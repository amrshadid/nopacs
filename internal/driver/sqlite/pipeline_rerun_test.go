package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// fixtureSeq makes ids unique across this file's tests.
var fixtureSeq atomic.Int64

// fixtureID returns a new id with the prefix.
func fixtureID(prefix string) string { return fmt.Sprintf("%s%08d", prefix, fixtureSeq.Add(1)) }

// fixtureAt returns the suite's start time plus seconds.
func fixtureAt(s int) time.Time { return tenanttest.T0.Add(time.Duration(s) * time.Second) }

// fixtureAudit returns a new audit entry.
func fixtureAudit() master.AuditEntry {
	return master.AuditEntry{ID: fixtureID("a"), Time: tenanttest.T0, Method: "Test", Outcome: master.OutcomeDone}
}

// fixtureSum returns a checksum made from n, never zero.
func fixtureSum(n int) tenant.SHA256 {
	var s tenant.SHA256
	for i := range s {
		s[i] = byte((n + i + 1) & 0xff)
	}
	s[0] = byte((n>>8)&0x7f) | 0x80
	return s
}

// fixtureSetup adds a storage and the named receivers to a store.
func fixtureSetup(t *testing.T, p *Tenant, receivers ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.AddStorage(ctx, tenant.Storage{Name: "store", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: "store"}}, tenanttest.T0, fixtureAudit(), nil); err != nil {
		t.Fatal(err)
	}
	for i, name := range receivers {
		if _, err := p.AddReceiver(ctx, tenant.Receiver{Name: name, Type: tenant.ReceiverDIMSE, Storage: "store",
			DIMSE:       tenant.DIMSEReceiver{Port: 11112 + i, AETitle: "ARCHIVE"},
			QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes},
			tenanttest.T0, fixtureAudit(), nil); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureFile returns received file n of study for reception r.
func fixtureFile(r tenant.Reception, study, n int) tenant.ReceivedFile {
	return tenant.ReceivedFile{
		ReceptionID: r.ID, SOPInstanceUID: fmt.Sprintf("2.25.3.%d", n+1), SeriesUID: fmt.Sprintf("2.25.2.%d", study+1), StudyUID: r.StudyUID,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Size: int64(1000 + n), SHA256: fixtureSum(n), Incoming: fmt.Sprintf("%s/f%06d.dcm", r.ID, n),
		Caller: tenant.Caller{CallingAETitle: "SCANNER1", Address: "192.0.2.7:51234"}, Association: "assoc1",
	}
}

// TestStepHookRunsAgainWithItsBatch guards what a step's hook and its
// change do when the transaction they were grouped into is undone by a
// neighbor (one whose statement is interrupted) and run again: the hook may
// be called more than once, and the step is still taken once, with the time
// it was given. The port says a hook must be safe to repeat; this shows the
// driver does repeat it, which the contract suite cannot provoke through
// the port.
func TestStepHookRunsAgainWithItsBatch(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	fixtureSetup(t, p, "a")
	r, _, err := p.OpenReception(ctx, "a", "2.25.1.1", fixtureID("rc"), fixtureAt(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ready(ctx); err != nil {
		t.Fatal(err)
	}
	h := holdWriter(t, &p.base)
	var calls atomic.Int32
	stepped := submit(t, &p.base, 1, func() error {
		_, err := p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: fixtureAt(70)},
			func(context.Context, tenant.Reception, tenant.Reception) error { calls.Add(1); return nil })
		return err
	})
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted := submit(t, &p.base, 2, func() error {
		return p.writeTx(stopped, "test", func(tx *sql.Tx) error {
			time.AfterFunc(50*time.Millisecond, cancel)
			_, err := tx.ExecContext(stopped, `INSERT INTO reception_associations (reception_id, association)
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
				SELECT 'x', 'a' || i FROM n`)
			return err
		})
	})
	h.letGo(t)
	if res := answer(t, "step", stepped); res.err != nil {
		t.Fatal(res.err)
	}
	answer(t, "interrupted", interrupted)
	got, _, err := p.Reception(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the hook was called %d times for one step", calls.Load())
	if calls.Load() < 1 || got.State != reception.Complete || !got.StepTime.Equal(fixtureAt(70)) {
		t.Errorf("after a step run again: hook called %d times, reception %s at %v; want the step once", calls.Load(), got.State, got.StepTime)
	}
	var n int
	if err := p.p.read.QueryRow(`SELECT count(*) FROM reception_associations WHERE reception_id = 'x'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the interrupted neighbor left %d rows (%v)", n, err)
	}
}

// TestPipelineRandomizedAcrossInstances guards the pipeline's identities
// under many goroutines on two instances of one database at once, in short
// bursts (see below why): opens,
// received batches (some written twice), holds (some written twice),
// releases, rejections, quiet steps, and claims and commits of identified
// receptions. Afterwards each reception's counts equal a count over its
// rows, the quarantine's and the catalog's totals equal a count over theirs,
// arrivals are unique and without gaps, no two receptions of one receiver
// and study are open, no instance is held and claimed by two receptions,
// the receptions per step equal a count of them, and each open, hold and
// decision wrote its event once.
func TestPipelineRandomizedAcrossInstances(t *testing.T) {
	ctx := context.Background()
	a0, b0 := openTenantPair(t, tenanttest.SuiteTenant)
	a, b := a0.(*Tenant), b0.(*Tenant)
	fixtureSetup(t, a, "a", "b")
	stores := []*Tenant{a, b}
	// Two stores on one file are two writing processes as far as SQLite's
	// locking goes, and that locking is not fair: a writer that finds the
	// file locked sleeps and tries again, and one that keeps writing can
	// keep it out past the busy timeout (doc.go, "One process writes a
	// SQLite database"). A server has one writing process, so a starved
	// second one is not what this test guards. The workers therefore run in
	// short bursts, every worker of both stores at once (so the stores do
	// contend), with all of them waiting at the end of each burst, which
	// bounds how long one store can keep the other out; and a change that
	// still finds the file busy is tried again a few times and counted.
	const workers, ops, burst, tries = 16, 150, 10, 5
	var busyRetries atomic.Int64
	retry := func(op func() error) error {
		for n := 1; ; n++ {
			err := op()
			if err == nil || !isBusy(err) || n == tries {
				return err
			}
			busyRetries.Add(1)
		}
	}
	var qids [workers][]string
	var rngs [workers]uint64
	for w := range workers {
		rngs[w] = uint64(w)*7919 + 1 //nolint:gosec // G115: w is a worker number from 0 to 15.
	}
	both := 0 // bursts in which both stores committed: the stores did contend
	for start := 0; start < ops; start += burst {
		ca, cb := a.w.commits.Load(), b.w.commits.Load()
		errs := make(chan error, workers)
		for w := range workers {
			go func(w int) {
				p := stores[w%2]
				next := func(n int) int {
					rngs[w] ^= rngs[w] << 13
					rngs[w] ^= rngs[w] >> 7
					rngs[w] ^= rngs[w] << 17
					return int(rngs[w] % uint64(n)) //nolint:gosec // G115: n is a small positive count and the result is below it.
				}
				for i := start; i < start+burst; i++ {
					if err := randomChange(ctx, p, &qids[w], i, next, retry); err != nil {
						errs <- err
						return
					}
				}
				errs <- nil
			}(w)
		}
		for range workers {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if a.w.commits.Load() > ca && b.w.commits.Load() > cb {
			both++
		}
	}
	t.Logf("both stores committed in %d of %d bursts (%d and %d commits); %d changes found the file busy and were tried again",
		both, ops/burst, a.w.commits.Load(), b.w.commits.Load(), busyRetries.Load())
	if both == 0 {
		t.Error("the stores never wrote in the same burst, so they never contended")
	}
	db := a.p.read
	check := func(what, query string) {
		t.Helper()
		var n int64
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows break it", what, n)
		}
	}
	// A reception's received count is its received files and the files it
	// held that have no received file, while it keeps its received files
	// (a reception ended done or quarantined has none left); its
	// quarantined count, its records less those released back into it.
	check("each reception's counts equal a count over its rows", `SELECT count(*) FROM receptions AS p WHERE
		(p.state NOT IN ('done', 'quarantined') AND (files_received <> (SELECT count(*) FROM received_files WHERE reception_id = p.id)
			+ (SELECT count(*) FROM quarantine AS q WHERE q.reception_id = p.id AND NOT EXISTS (SELECT 1 FROM received_files AS r
				WHERE r.reception_id = p.id AND r.sop_instance_uid = q.sop_instance_uid AND r.sha256 = q.sha256))
		OR files_released <> (SELECT count(*) FROM received_files WHERE reception_id = p.id AND released = 1)))
		OR files_quarantined <> (SELECT count(*) FROM quarantine AS q WHERE q.reception_id = p.id
			AND NOT (q.state = 'released' AND q.release_reception = p.id))
		OR files_archived <> (SELECT count(*) FROM instances WHERE reception_id = p.id)
		OR associations <> (SELECT count(*) FROM reception_associations WHERE reception_id = p.id)`)
	// A reception's released count is the same however far it got: the
	// quarantine records released into it (the received files that say so
	// are gone once it ended done or quarantined).
	check("each reception's released count equals the records released into it", `SELECT count(*) FROM receptions AS p WHERE
		files_released <> (SELECT count(*) FROM quarantine AS q WHERE q.state = 'released' AND q.release_reception = p.id)`)
	check("the quarantine's totals", `SELECT count(*) FROM quarantine_totals WHERE
		files <> (SELECT count(*) FROM quarantine WHERE state = 'held')
		OR bytes <> (SELECT coalesce(sum(size), 0) FROM quarantine WHERE state = 'held' AND content_kept = 1)`)
	check("the catalog's totals", `SELECT count(*) FROM catalog_totals WHERE subjects <> (SELECT count(*) FROM subjects)
		OR sessions <> (SELECT count(*) FROM sessions) OR scans <> (SELECT count(*) FROM scans)
		OR instances <> (SELECT count(*) FROM instances)`)
	check("each scan's counts", `SELECT count(*) FROM scans AS s WHERE
		instances <> (SELECT count(*) FROM instances WHERE scan_id = s.id)
		OR bytes <> (SELECT coalesce(sum(size), 0) FROM instances WHERE scan_id = s.id)`)
	check("no claim outlives its reception", `SELECT count(*) FROM instance_claims AS i JOIN receptions AS p ON p.id = i.reception_id
		WHERE p.ended = 1`)
	check("a reception that ended done or quarantined keeps no received file", `SELECT count(*) FROM received_files AS r
		JOIN receptions AS p ON p.id = r.reception_id WHERE p.state IN ('done', 'quarantined')`)
	check("arrivals unique", `SELECT count(*) FROM (SELECT study_uid, arrival FROM receptions GROUP BY 1, 2 HAVING count(*) > 1)`)
	check("arrivals without gaps", `SELECT count(*) FROM (SELECT study_uid FROM receptions GROUP BY 1 HAVING max(arrival) <> count(*))`)
	check("one open reception per receiver and study", `SELECT count(*) FROM (SELECT receiver, study_uid FROM receptions
		WHERE state = 'receiving' GROUP BY 1, 2 HAVING count(*) > 1)`)
	check("the receptions per step equal a count of them", `SELECT count(*) FROM (SELECT state, count(*) AS n FROM receptions GROUP BY state) AS c
		LEFT JOIN reception_steps AS s USING (state) WHERE s.receptions IS NOT c.n`)
	check("no step counts receptions it does not have", `SELECT count(*) FROM reception_steps AS s
		WHERE s.receptions <> (SELECT count(*) FROM receptions WHERE state = s.state)`)
	// Every reception opened once, every record held once, every decision
	// made once: each wrote its event once, whatever the batches it was
	// grouped into.
	check("one receiving event per reception", `SELECT abs((SELECT count(*) FROM receptions)
		- (SELECT count(*) FROM events WHERE type = 'reception_step' AND json_extract(payload, '$.step') = 'receiving'))`)
	check("one event per quarantine record", `SELECT abs((SELECT count(*) FROM quarantine)
		- (SELECT count(*) FROM events WHERE type = 'file_quarantined'))`)
	check("one event per quarantine decision", `SELECT abs((SELECT count(*) FROM quarantine WHERE state <> 'held')
		- (SELECT count(*) FROM events WHERE type = 'quarantine_decided'))`)
	var receptions, held, instances int64
	_ = db.QueryRow(`SELECT count(*) FROM receptions`).Scan(&receptions)
	_ = db.QueryRow(`SELECT count(*) FROM quarantine`).Scan(&held)
	_ = db.QueryRow(`SELECT count(*) FROM instances`).Scan(&instances)
	t.Logf("%d receptions, %d quarantine records, %d instances held", receptions, held, instances)
	if instances == 0 {
		t.Error("no instance was archived, so the claims and commits were not tested")
	}
}

// claimAndCommit takes one identified reception, if there is one, through
// its claims and the commits of what it claimed, ignoring the refusals a
// reception moved meanwhile by another worker earns.
func claimAndCommit(ctx context.Context, p *Tenant, next func(int) int) error {
	rs, _, err := p.ListReceptions(ctx, tenant.ReceptionFilter{States: []reception.State{reception.Identified}}, master.Page{Limit: 5})
	if err != nil || len(rs) == 0 {
		return err
	}
	r := rs[next(len(rs))]
	files, _, err := p.ReceivedFiles(ctx, r.ID, master.Page{Limit: 1000})
	if err != nil || len(files) == 0 {
		return err
	}
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	answers, err := p.ClaimInstances(ctx, r.ID, claims, fixtureAt(500), nil)
	if fault.KindOf(err) == fault.Conflict {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	path := label.Path{Subject: "sub-01", Session: "ses-" + r.StudyUID, Scan: "scan-" + files[0].SeriesUID}
	l, err := p.CatalogLabels(ctx, r.StudyUID, []string{files[0].SeriesUID})
	if err != nil {
		return err
	}
	if l.Session != nil {
		path.Subject, path.Session = l.Session.SubjectLabel, l.Session.SessionLabel
	}
	c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: files[0].SeriesUID, SubjectLabel: path.Subject,
		SessionLabel: path.Session, ScanLabel: path.Scan, Storage: "store", At: fixtureAt(501)}
	for i, f := range files {
		if k := answers[i].Kind; k != tenant.ClaimedNow && k != tenant.ClaimedBefore {
			continue
		}
		key, err := layout.InstanceKey(path, f.SOPInstanceUID)
		if err != nil {
			return err
		}
		c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256})
	}
	if len(c.Instances) == 0 {
		return nil
	}
	if _, err := p.CommitScan(ctx, c); err != nil && fault.KindOf(err) != fault.Conflict {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// randomChange makes one change of TestPipelineRandomizedAcrossInstances,
// chosen by next, on store p at step i: an open with received batches (some
// written twice), a hold (some written twice), a release, a rejection,
// steps of receptions in progress, or a claim and commit. Each change that
// can find the file busy goes through retry. Refusals a change earns from
// another worker's change meanwhile are expected and ignored.
func randomChange(ctx context.Context, p *Tenant, qids *[]string, i int, next func(int) int, retry func(func() error) error) error {
	study := next(5)
	recv := []string{"a", "b"}[next(2)]
	open := func() (tenant.Reception, error) {
		var r tenant.Reception
		err := retry(func() error {
			var err error
			r, _, err = p.OpenReception(ctx, recv, fmt.Sprintf("2.25.1.%d", study+1), fixtureID("rc"), fixtureAt(i))
			return err
		})
		return r, err
	}
	switch next(7) {
	case 0, 1:
		r, err := open()
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		var batch []tenant.ReceivedFile
		for k := range next(5) + 1 {
			batch = append(batch, fixtureFile(r, study, study*1000+next(20)+k))
		}
		for range next(2) + 1 {
			err := retry(func() error { _, err := p.AddReceivedFiles(ctx, batch, fixtureAt(i), nil); return err })
			if err != nil && !strings.Contains(err.Error(), "no longer receiving") {
				return fmt.Errorf("received: %w", err)
			}
		}
	case 2:
		r, err := open()
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		id := fixtureID("q")
		if next(3) == 0 && len(*qids) > 0 {
			id = (*qids)[next(len(*qids))]
		}
		q := tenant.QuarantinedFile{ID: id, Reason: tenant.QuarantineNotDICOM, Receiver: recv, ReceptionID: r.ID,
			Size: int64(next(1000)), SHA256: fixtureSum(next(1000)), Incoming: "quarantine/" + id, ContentKept: true, Quarantined: fixtureAt(i)}
		if next(2) == 0 {
			// A record that names its instance, series and study is
			// receivable: its release puts it into a reception, which counts
			// it released there, so the counter the checks below read is
			// exercised.
			f := fixtureFile(r, study, study*1000+next(20))
			q.SOPInstanceUID, q.SeriesUID, q.StudyUID, q.SOPClassUID, q.TransferSyntaxUID = f.SOPInstanceUID, f.SeriesUID, f.StudyUID,
				f.SOPClassUID, f.TransferSyntaxUID
		}
		if err := retry(func() error { _, err := p.HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil); return err }); err != nil {
			return fmt.Errorf("hold: %w", err)
		}
		*qids = append(*qids, id)
	case 3:
		if len(*qids) > 0 {
			id := (*qids)[next(len(*qids))]
			_ = retry(func() error {
				_, err := p.ReleaseQuarantined(ctx, id, tenant.ReleaseAsIs, "", fixtureID("rc"), fixtureAt(i), fixtureAudit(), nil)
				return err
			})
		}
	case 4:
		if len(*qids) > 0 {
			id := (*qids)[next(len(*qids))]
			_ = retry(func() error {
				_, err := p.RejectQuarantined(ctx, id, "no", fixtureAt(i), fixtureAudit(), nil)
				return err
			})
		}
	case 5:
		rs, _, err := p.ListReceptions(ctx, tenant.ReceptionFilter{InProgress: true}, master.Page{Limit: 5})
		if err != nil {
			return err
		}
		for _, r := range rs {
			switch {
			case r.State == reception.Receiving && next(2) == 0:
				_ = retry(func() error {
					_, err := p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quiet, At: fixtureAt(i + 100)}, nil)
					return err
				})
			case r.State == reception.Complete:
				_ = retry(func() error {
					_, err := p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Identify,
						At: fixtureAt(i + 100), SubjectLabel: "sub-01", SessionLabel: "ses-" + r.StudyUID, Storage: "store"}, nil)
					return err
				})
			}
		}
	case 6:
		if err := retry(func() error { return claimAndCommit(ctx, p, next) }); err != nil {
			return err
		}
	}
	return nil
}
