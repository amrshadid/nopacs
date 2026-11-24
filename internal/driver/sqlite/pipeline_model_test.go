package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// modelT0 is the time the model starts at.
var modelT0 = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

// modelEnv is one run of the model: its store, a read-only view of its
// file for the checks, and the ids it made.
type modelEnv struct {
	t   *testing.T
	p   *Tenant
	db  *sql.DB
	ctx context.Context
	seq int
}

// id returns a new id with the prefix.
func (e *modelEnv) id(p string) string { e.seq++; return fmt.Sprintf("%s%06d", p, e.seq) }

// audit returns a new audit entry.
func (e *modelEnv) audit() master.AuditEntry {
	return master.AuditEntry{ID: e.id("a"), Time: modelT0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
		Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// must fails the run on an error.
func (e *modelEnv) must(what string, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: %v", what, err)
	}
}

// storage adds a directory storage.
func (e *modelEnv) storage(name string) {
	_, err := e.p.AddStorage(e.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: name}}, modelT0, e.audit(), nil)
	e.must("AddStorage", err)
}

// receiver adds a receiver.
func (e *modelEnv) receiver(name, storage string, port int) {
	_, err := e.p.AddReceiver(e.ctx, tenant.Receiver{Name: name, Type: tenant.ReceiverDIMSE, Storage: storage,
		DIMSE:       tenant.DIMSEReceiver{Port: port, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes},
		modelT0, e.audit(), nil)
	e.must("AddReceiver", err)
}

// modelUID returns a valid UID unique to kind and n.
func modelUID(kind, n int) string { return fmt.Sprintf("2.25.%d.%d", kind, n+1) }

// modelSum returns a checksum made from n, never zero.
func modelSum(n int) tenant.SHA256 {
	var s tenant.SHA256
	for i := range s {
		s[i] = byte((n + i + 1) & 0xff)
	}
	s[0] = byte((n>>8)&0x7f) | 0x80
	return s
}

// modelFile is file n (UID n, checksum v) of study st, series se.
func modelFile(r tenant.Reception, st, se, n, v int) tenant.ReceivedFile {
	return tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: modelUID(3, n), SeriesUID: modelUID(2, se), StudyUID: modelUID(1, st),
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Size: int64(1000 + n + v), SHA256: modelSum(n*100 + v), Incoming: fmt.Sprintf("%s/f%06d_%d.dcm", r.ID, n, v),
		Caller:      tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"},
		Association: "assoc1"}
}

// modelClaim returns the claim of a received file.
func modelClaim(f tenant.ReceivedFile) tenant.InstanceClaim {
	return tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
}

// mfile is a received file in the model, and what became of it.
type mfile struct {
	f     tenant.ReceivedFile
	state string // "", "claimed", "committed", "dup", "quar", "own"
}

// mrec is a reception in the model.
type mrec struct {
	r          tenant.Reception
	files      []*mfile
	archived   int64
	archivedB  int64
	dup, dupB  int64
	ended      bool
	failed     bool
	retried    bool
	discarded  bool
	lastCommit *tenant.ScanCommit
}

// mheld is an instance the model holds.
type mheld struct {
	sha tenant.SHA256
	by  string
	st  string
	se  string
}

// mclaim is a claim the model records.
type mclaim struct {
	sha tenant.SHA256
	by  string
	st  string
	se  string
}

// mfileFor returns file uid (checksum v) of study st, in one of two series
// of the study, so UIDs are shared across studies and series.
func mfileFor(r tenant.Reception, st, uid, v int) tenant.ReceivedFile {
	f := modelFile(r, st, st*10+uid%2, uid, v)
	return f
}

// runModel runs one seed of the model against one store, or two stores on
// one file when shared.
func runModel(t *testing.T, seed int64, shared bool) {
	path := filepath.Join(t.TempDir(), "tenant.db")
	var stores []*Tenant
	n := 1
	if shared {
		n = 2
	}
	for i := 0; i < n; i++ {
		s, err := OpenTenant(context.Background(), path, "rv")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, s)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &modelEnv{t: t, p: stores[0], db: db, ctx: context.Background()}
	rng := rand.New(rand.NewPCG(uint64(seed), 47)) //nolint:gosec // G115, G404: a seeded sequence for a test, from a positive seed.
	pick := func() *Tenant { return stores[rng.IntN(len(stores))] }
	e.storage("store")
	e.receiver("a", "store", 11112)
	e.receiver("b", "store", 11113)

	held := map[string]mheld{}
	claims := map[string]mclaim{}
	var recs []*mrec
	clock := modelT0
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }
	logf := func(format string, a ...any) { t.Logf("seed %d: "+format, append([]any{seed}, a...)...) }

	// expect is the claim's rule as the model states it: another
	// reception's claim is Claimed whatever its study; this reception's
	// claim is elsewhere, before, or different; a held instance is
	// elsewhere, archived before (by this reception), the same file, or
	// different.
	expect := func(rid string, c tenant.InstanceClaim, pending map[string]mclaim) tenant.DuplicateKind {
		if cl, ok := pending[c.SOPInstanceUID]; ok {
			switch {
			case cl.by != rid:
				return tenant.Claimed
			case cl.st != c.StudyUID || cl.se != c.SeriesUID:
				return tenant.UIDElsewhere
			case cl.sha == c.SHA256:
				return tenant.ClaimedBefore
			}
			return tenant.DifferentFile
		}
		if h, ok := held[c.SOPInstanceUID]; ok {
			switch {
			case h.st != c.StudyUID || h.se != c.SeriesUID:
				return tenant.UIDElsewhere
			case h.sha == c.SHA256 && h.by == rid:
				return tenant.ArchivedBefore
			case h.sha == c.SHA256:
				return tenant.SameFile
			}
			return tenant.DifferentFile
		}
		return tenant.ClaimedNow
	}

	releaseModel := func(rid string) {
		for u, c := range claims {
			if c.by == rid {
				delete(claims, u)
			}
		}
	}

	check := func(op string) {
		t.Helper()
		var a, b, c, d string
		_ = db.QueryRow(`SELECT subjects||','||sessions||','||scans||','||instances FROM catalog_totals`).Scan(&a)
		_ = db.QueryRow(`SELECT (SELECT count(*) FROM subjects)||','||(SELECT count(*) FROM sessions)||','||(SELECT count(*) FROM scans)||','||(SELECT count(*) FROM instances)`).Scan(&b)
		if a != b {
			t.Fatalf("seed %d after %s: totals %s, rows %s", seed, op, a, b)
		}
		_ = db.QueryRow(`SELECT count(*) FROM scans s WHERE instances <> (SELECT count(*) FROM instances i WHERE i.scan_id = s.id)
			OR bytes <> (SELECT coalesce(sum(size),0) FROM instances i WHERE i.scan_id = s.id)`).Scan(&c)
		if c != "0" {
			t.Fatalf("seed %d after %s: %s scans whose counts disagree with their rows", seed, op, c)
		}
		_ = db.QueryRow(`SELECT count(*) FROM instance_claims i LEFT JOIN receptions r ON r.id = i.reception_id
			WHERE r.id IS NULL OR r.ended = 1 OR r.storage = ''`).Scan(&d)
		if d != "0" {
			t.Fatalf("seed %d after %s: %s claims by ended or missing receptions", seed, op, d)
		}
		rows, err := db.Query(`SELECT sop_instance_uid, 'held', sha256, reception_id FROM instances
			UNION ALL SELECT sop_instance_uid, 'claimed', sha256, reception_id FROM instance_claims`)
		if err != nil {
			t.Fatal(err)
		}
		seenH, seenC := 0, 0
		for rows.Next() {
			var uid, state, by string
			var sum []byte
			if err := rows.Scan(&uid, &state, &sum, &by); err != nil {
				t.Fatal(err)
			}
			if state == "held" {
				seenH++
				h, ok := held[uid]
				if !ok || sqlkit.ToSHA256(sum) != h.sha || by != h.by {
					t.Fatalf("seed %d after %s: held row %s by %s differs from model %+v (%v)", seed, op, uid, by, h, ok)
				}
				continue
			}
			seenC++
			c, ok := claims[uid]
			if !ok || sqlkit.ToSHA256(sum) != c.sha || by != c.by {
				t.Fatalf("seed %d after %s: claim row %s by %s differs from model %+v (%v)", seed, op, uid, by, c, ok)
			}
		}
		_ = rows.Close()
		if seenH != len(held) || seenC != len(claims) {
			t.Fatalf("seed %d after %s: rows held %d claimed %d, model %d %d", seed, op, seenH, seenC, len(held), len(claims))
		}
		for _, m := range recs {
			r, _, err := stores[0].Reception(e.ctx, m.r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Counts.FilesArchived != m.archived || r.Counts.BytesArchived != m.archivedB {
				t.Fatalf("seed %d after %s: reception %s archived %d/%d, model %d/%d", seed, op, r.ID, r.Counts.FilesArchived, r.Counts.BytesArchived, m.archived, m.archivedB)
			}
			if r.State == reception.Done {
				c := r.Counts
				if c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined || c.BytesReceived != c.BytesArchived+c.BytesDuplicate+c.BytesQuarantined {
					t.Fatalf("seed %d after %s: identity broken for done %s: %+v", seed, op, r.ID, c)
				}
			}
		}
	}

	newRec := func(st int, files []tenant.ReceivedFile, r tenant.Reception) *mrec {
		m := &mrec{r: r}
		for _, f := range files {
			m.files = append(m.files, &mfile{f: f})
		}
		recs = append(recs, m)
		return m
	}

	open := func() {
		st := 1
		if rng.IntN(5) == 0 {
			st = 2
		}
		recv := []string{"a", "b"}[rng.IntN(2)]
		p := pick()
		r, opened, err := p.OpenReception(e.ctx, recv, modelUID(1, st), e.id("rc"), tick())
		e.must("open", err)
		if !opened {
			return
		}
		var files []tenant.ReceivedFile
		used := map[string]bool{}
		for k := 0; k < 1+rng.IntN(4); k++ {
			f := mfileFor(r, st, rng.IntN(6), rng.IntN(2))
			key := f.SOPInstanceUID + string(f.SHA256[:])
			if used[key] {
				continue
			}
			used[key] = true
			files = append(files, f)
		}
		_, err = p.AddReceivedFiles(e.ctx, files, tick())
		e.must("add", err)
		if rng.IntN(3) == 0 { // the batch again after a crash
			_, err = pick().AddReceivedFiles(e.ctx, files, tick())
			e.must("add again", err)
		}
		r, _, _ = p.Reception(e.ctx, r.ID)
		r, err = pick().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: tick(), LastFile: r.LastFile}, nil)
		e.must("quiet", err)
		r, err = pick().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: tick(),
			SubjectLabel: "sub-01", SessionLabel: "ses-" + r.StudyUID, Storage: "store"}, nil)
		e.must("identify", err)
		newRec(st, files, r)
		logf("open %s with %d files", r.ID, len(files))
	}

	live := func() []*mrec {
		var out []*mrec
		for _, m := range recs {
			if !m.ended {
				out = append(out, m)
			}
		}
		return out
	}
	failedRecs := func() []*mrec {
		var out []*mrec
		for _, m := range recs {
			if m.failed && !m.retried && !m.discarded {
				out = append(out, m)
			}
		}
		return out
	}

	doClaim := func(m *mrec) {
		var cls []tenant.InstanceClaim
		var idx []int
		for i, f := range m.files {
			// claim every file not yet decided, and sometimes all of them
			// again, as a step run again after a crash would
			if f.state == "" || rng.IntN(3) == 0 {
				cls = append(cls, modelClaim(f.f))
				idx = append(idx, i)
			}
		}
		if len(cls) == 0 {
			return
		}
		pending := map[string]mclaim{}
		for k, v := range claims {
			pending[k] = v
		}
		// The store claims in sorted order of UID, then checksum, whatever
		// order it is given, so the model decides in that order too.
		order := make([]int, len(cls))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int {
			if cls[a].SOPInstanceUID != cls[b].SOPInstanceUID {
				if cls[a].SOPInstanceUID < cls[b].SOPInstanceUID {
					return -1
				}
				return 1
			}
			return bytes.Compare(cls[a].SHA256[:], cls[b].SHA256[:])
		})
		want := make([]tenant.DuplicateKind, len(cls))
		for _, i := range order {
			c := cls[i]
			want[i] = expect(m.r.ID, c, pending)
			if want[i] == tenant.ClaimedNow {
				pending[c.SOPInstanceUID] = mclaim{sha: c.SHA256, by: m.r.ID, st: c.StudyUID, se: c.SeriesUID}
			}
		}
		got, err := pick().ClaimInstances(e.ctx, m.r.ID, cls, tick(), nil)
		e.must("claim", err)
		for i, d := range got {
			f := m.files[idx[i]]
			w := want[i]
			if d.Kind != w {
				t.Fatalf("seed %d: claim of %s by %s: got %v want %v (file state %q)", seed, cls[i].SOPInstanceUID, m.r.ID, d.Kind, w, f.state)
			}
			switch d.Kind {
			case tenant.ArchivedBefore:
				if f.state != "committed" {
					t.Fatalf("seed %d: archived before for a file not committed (state %q)", seed, f.state)
				}
			case tenant.ClaimedNow, tenant.ClaimedBefore:
				if f.state == "" {
					f.state = "claimed"
				}
			case tenant.SameFile:
				if f.state == "" {
					f.state = "dup"
					m.dup++
					m.dupB += f.f.Size
				}
			case tenant.DifferentFile, tenant.UIDElsewhere:
				if f.state == "" {
					q := tenant.QuarantinedFile{ID: "q" + m.r.ID + fmt.Sprint(idx[i]), Reason: tenant.QuarantineUIDElsewhere,
						Details:  map[string]string{"study_instance_uid": d.HeldStudyUID, "series_instance_uid": d.HeldSeriesUID},
						Receiver: m.r.Receiver, ReceptionID: m.r.ID, Size: f.f.Size, SHA256: f.f.SHA256,
						SOPClassUID: f.f.SOPClassUID, SOPInstanceUID: f.f.SOPInstanceUID, StudyUID: f.f.StudyUID, SeriesUID: f.f.SeriesUID,
						TransferSyntaxUID: f.f.TransferSyntaxUID, Incoming: f.f.Incoming, ContentKept: true, Quarantined: tick()}
					if d.Kind == tenant.DifferentFile {
						q.Reason, q.Details, q.KeptSHA256 = tenant.QuarantineDifferentFile, nil, d.HeldSHA256
					}
					_, err := pick().HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
					e.must("hold", err)
					f.state = "quar"
				}
			}
		}
		claims = pending
		logf("claim %s: %v", m.r.ID, got)
	}

	doCommit := func(m *mrec, again bool) {
		if again && m.lastCommit != nil {
			_, err := pick().CommitScan(e.ctx, *m.lastCommit)
			e.must("commit again", err)
			logf("commit again %s", m.r.ID)
			return
		}
		bySeries := map[string][]*mfile{}
		for _, f := range m.files {
			if f.state == "claimed" && rng.IntN(4) != 0 {
				bySeries[f.f.SeriesUID] = append(bySeries[f.f.SeriesUID], f)
			}
		}
		for se, fs := range bySeries {
			l, err := pick().CatalogLabels(e.ctx, m.r.StudyUID, []string{se})
			e.must("labels", err)
			p := label.Path{Subject: "sub-01", Session: "ses-" + m.r.StudyUID, Scan: "scan-" + se}
			storage := "store"
			if l.Session != nil {
				p.Subject, p.Session, storage = l.Session.SubjectLabel, l.Session.SessionLabel, l.Session.Storage
			}
			if v, ok := l.Scans[se]; ok {
				p.Scan = v
			}
			c := tenant.ScanCommit{ReceptionID: m.r.ID, StudyUID: m.r.StudyUID, SeriesUID: se, SubjectLabel: p.Subject,
				SessionLabel: p.Session, ScanLabel: p.Scan, Storage: storage, At: tick()}
			for _, f := range fs {
				key, _ := layout.InstanceKey(p, f.f.SOPInstanceUID)
				c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.f.SOPInstanceUID, Key: key, Length: f.f.Size, Size: f.f.Size, ArchivedSHA256: f.f.SHA256})
			}
			_, err = pick().CommitScan(e.ctx, c)
			e.must("commit", err)
			cc := c
			m.lastCommit = &cc
			for _, f := range fs {
				f.state = "committed"
				m.archived++
				m.archivedB += f.f.Size
				held[f.f.SOPInstanceUID] = mheld{sha: f.f.SHA256, by: m.r.ID, st: f.f.StudyUID, se: f.f.SeriesUID}
				delete(claims, f.f.SOPInstanceUID)
			}
			logf("commit %s series %s: %d", m.r.ID, se, len(fs))
		}
	}

	step := func(m *mrec, ev reception.Event, counts *tenant.StepCounts) error {
		r, _, _ := pick().Reception(e.ctx, m.r.ID)
		st := tenant.ReceptionStep{ID: r.ID, From: r.State, Event: ev, At: tick(), Counts: counts}
		if ev == reception.Fail {
			st.Failure = "storage unreachable"
		}
		r2, err := pick().StepReception(e.ctx, st, nil)
		if err == nil {
			m.r = r2
		}
		return err
	}

	doFinish := func(m *mrec) {
		for _, f := range m.files {
			if f.state == "" || f.state == "claimed" {
				return
			}
		}
		for _, ev := range []reception.Event{reception.Archive, reception.Verify, reception.Finish} {
			if m.r.State == reception.Identified && ev != reception.Archive || m.r.State == reception.Archived && ev == reception.Archive {
				continue
			}
			if m.r.State == reception.Verified && ev != reception.Finish {
				continue
			}
			e.must("finish "+string(ev), step(m, ev, &tenant.StepCounts{FilesDuplicate: m.dup, BytesDuplicate: m.dupB}))
		}
		if rng.IntN(3) == 0 {
			// The last step written again after a crash: the reception has
			// ended, the core refuses it as Ended, and nothing changes.
			err := step(m, reception.Finish, nil)
			var refusal *reception.Refusal
			if !errors.As(err, &refusal) || refusal.Kind != reception.Ended {
				t.Fatalf("seed %d: the finish step again: %v, want a refusal of kind Ended", seed, err)
			}
		}
		m.ended = true
		releaseModel(m.r.ID)
		logf("done %s", m.r.ID)
	}

	doFail := func(m *mrec) {
		e.must("fail", step(m, reception.Fail, nil))
		m.ended, m.failed = true, true
		releaseModel(m.r.ID)
		logf("fail %s", m.r.ID)
	}

	doRetry := func(m *mrec) {
		_, r2, err := pick().RetryReception(e.ctx, m.r.ID, e.id("rc"), "", tick(), e.audit(), nil)
		e.must("retry", err)
		if rng.IntN(3) == 0 {
			_, again, err := pick().RetryReception(e.ctx, m.r.ID, e.id("rc"), "", tick(), e.audit(), nil)
			e.must("retry again", err)
			if again.ID != r2.ID {
				t.Fatalf("seed %d: retry again made %s, first %s", seed, again.ID, r2.ID)
			}
		}
		m.retried = true
		r2, err = pick().StepReception(e.ctx, tenant.ReceptionStep{ID: r2.ID, From: reception.Complete, Event: reception.Identify, At: tick(),
			SubjectLabel: "sub-01", SessionLabel: "ses-" + r2.StudyUID, Storage: "store"}, nil)
		e.must("identify retry", err)
		var files []tenant.ReceivedFile
		for _, f := range m.files {
			g := f.f
			g.ReceptionID = r2.ID
			files = append(files, g)
		}
		newRec(0, files, r2)
		logf("retry %s -> %s", m.r.ID, r2.ID)
	}

	doDiscard := func(m *mrec) {
		_, err := pick().DiscardReception(e.ctx, m.r.ID, "operator", tick(), e.audit(), nil)
		e.must("discard", err)
		m.discarded = true
		logf("discard %s", m.r.ID)
	}

	check("start")
	for i := 0; i < 120; i++ {
		lv := live()
		op := rng.IntN(10)
		switch {
		case op < 2 || len(lv) == 0:
			if len(recs) < 10 {
				open()
				check("open")
			}
		case op < 5:
			doClaim(lv[rng.IntN(len(lv))])
			check("claim")
		case op < 7:
			doCommit(lv[rng.IntN(len(lv))], rng.IntN(4) == 0)
			check("commit")
		case op < 8:
			doFinish(lv[rng.IntN(len(lv))])
			check("finish")
		case op < 9:
			if rng.IntN(2) == 0 {
				doFail(lv[rng.IntN(len(lv))])
				check("fail")
			}
		default:
			if fr := failedRecs(); len(fr) > 0 {
				if rng.IntN(2) == 0 {
					doRetry(fr[rng.IntN(len(fr))])
					check("retry")
				} else {
					doDiscard(fr[rng.IntN(len(fr))])
					check("discard")
				}
			}
		}
	}
}

// TestPipelineModel guards the claim, the catalog and the counts against a
// model of what they must be: each seed opens receptions of two studies
// whose files share UIDs across studies and series, and in a random order
// claims (and claims again, as a step run again after a crash), commits
// (and commits again), finishes (and writes the last step again), fails,
// retries and discards, half the seeds on two stores sharing one file.
// After every operation every claim answer, every instance row, the
// catalog's totals, each scan's counts, each reception's archived counts
// and the count identity of each reception done equal the model's; no claim
// is left by a reception that ended. Each operation is a synced commit, so a
// seed takes about half a second: 8 seeds run here (four on one store, four
// on two), and all 150 in TestPipelineModelAllSeeds. Under the race detector, which slows each seed
// several times over, 6 run (three on one store, three on two): the detector
// is there to see the two stores' goroutines, not to widen the model, and the
// package has to fit the time a test run is given.
func TestPipelineModel(t *testing.T) {
	last := int64(8)
	if raceDetector {
		last = 6
	}
	runSeeds(t, 1, last)
}

// allSeedsEnv is the variable that makes TestPipelineModelAllSeeds run.
const allSeedsEnv = "NOPACS_TEST_ALL_SEEDS"

// TestPipelineModelAllSeeds runs the model's 150 seeds, which take minutes,
// when allSeedsEnv is set (make test-scale sets it, and CI runs that). The
// race detector slows each seed several times over, so the run is made
// without it.
func TestPipelineModelAllSeeds(t *testing.T) {
	switch {
	case testing.Short():
		t.Skip("150 seeds: skipped under -short")
	case os.Getenv(allSeedsEnv) == "":
		t.Skip("150 seeds: set " + allSeedsEnv + "=1 (make test-scale does)")
	case raceDetector:
		t.Skip("150 seeds: run without the race detector")
	}
	runSeeds(t, 1, 150)
}

// runSeeds runs the model's seeds first to last, the even ones on two
// stores sharing one file.
func runSeeds(t *testing.T, first, last int64) {
	for seed := first; seed <= last; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runModel(t, seed, seed%2 == 0)
		})
	}
}
