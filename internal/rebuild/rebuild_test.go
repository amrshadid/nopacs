package rebuild

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// drivers runs fn once on SQLite and once on Postgres (skipped when there
// is none to use).
func drivers(t *testing.T, fn func(t *testing.T, postgres bool)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, false) })
	t.Run("postgres", func(t *testing.T) { fn(t, true) })
}

// sqliteExec runs statements on the SQLite file at path, on a connection
// of its own.
func sqliteExec(tb testing.TB, path string, stmts ...string) {
	tb.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
}

// TestRebuildCompareEqualsTheCatalog guards the round trip on the real
// pipeline's output: a catalog made again from what the storage holds,
// read through its metadata and manifests alone, equals the catalog in use
// in every column of every table, so a compare ends done with no
// difference, reads every instance, and writes nothing to the storage or
// the catalog in use.
func TestRebuildCompareEqualsTheCatalog(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		before := r.catalogText()
		totals, err := r.p.CatalogTotals(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
		if j.State != tenant.JobDone || j.Counts.InstancesSent != totals.Instances || j.Counts.InstancesRefused != 0 ||
			j.Counts.Scans != totals.Scans {
			t.Errorf("the compare ended %+v, the catalog holds %+v", j, totals)
		}
		if len(tallies) != 0 {
			t.Errorf("the compare tallied %v, want nothing: the storage and the catalog differ", tallies)
		}
		if writes.Load() != 0 {
			t.Errorf("a rebuild wrote %d times to a storage", writes.Load())
		}
		if got := r.catalogText(); got != before {
			t.Errorf("a compare changed the catalog in use")
		}
	})
}

// TestRebuildReplaceMakesTheCatalogAgain guards the replace: with every
// catalog row of the tenant's database gone and the storage as it was, a
// replace makes the catalog again equal, as its reads answer it, to the
// one before, and tallies its activation.
func TestRebuildReplaceMakesTheCatalogAgain(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		before := r.catalogText()
		r.forgetCatalog()
		if totals, err := r.p.CatalogTotals(r.ctx); err != nil || totals != (tenant.CatalogCounts{}) {
			t.Fatalf("the catalog after it was removed: %+v %v", totals, err)
		}
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildReplace)
		if j.State != tenant.JobDone || j.Rebuild == nil || !j.Rebuild.Activated || tallies[tenant.TallyActivated] != 1 {
			t.Fatalf("the replace ended %+v with tallies %v", j, tallies)
		}
		if got := r.catalogText(); got != before {
			t.Errorf("the catalog made again differs from the one before:\n%s\nwant\n%s", got, before)
		}
		if writes.Load() != 0 {
			t.Errorf("a rebuild wrote %d times to a storage", writes.Load())
		}
	})
}

// TestRebuildReplaceKeepsIDs guards that a replace over a catalog that
// holds the same subjects, sessions and scans keeps their integer ids, so a
// table that names one by id (a send job's scans) still names the same one
// after the activation.
func TestRebuildReplaceKeepsIDs(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	ids := func() string {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+r.dbPath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var out string
		if err := db.QueryRow(`SELECT group_concat(k, ' ') FROM (SELECT 'subject ' || id || ' ' || label AS k FROM subjects
			UNION ALL SELECT 'session ' || id || ' ' || study_uid FROM sessions UNION ALL SELECT 'scan ' || id || ' ' || series_uid FROM scans
			ORDER BY 1)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := ids()
	var writes atomic.Int64
	if j, _ := r.run(r.config(&writes), tenant.RebuildReplace); j.State != tenant.JobDone {
		t.Fatalf("the replace ended %+v", j)
	}
	if after := ids(); after != before {
		t.Errorf("the replace changed the catalog's ids:\n%s\nwant\n%s", after, before)
	}
}

// recordOf returns a correction record of kind delete at level of the scan
// at p, in state.
func (r *rig) recordOf(id string, p label.Path, level string, state record.State, instances []string) record.Correction {
	r.t.Helper()
	se, err := r.p.CatalogSession(r.ctx, p.Subject, p.Session)
	if err != nil {
		r.t.Fatal(err)
	}
	sc, err := r.p.CatalogScan(r.ctx, p)
	if err != nil {
		r.t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	c := record.Correction{Format: record.Format, ID: id, Kind: record.KindDelete, Level: level, State: state,
		ReasonSHA256: record.ReasonDigest("consent withdrawn"), Created: created, Instances: instances,
		Scans: []record.Scan{{Subject: p.Subject, Session: p.Session, Scan: p.Scan, StudyUID: se.StudyUID, SeriesUID: sc.SeriesUID}}}
	if state != record.StatePrepared && state != record.StateAbandoned {
		c.CommittedAt, c.PurgeAfter = created.Add(time.Minute), created.Add(time.Hour)
	}
	return c
}

// putRecord writes a correction record to the storage, as a correction
// does before anything else.
func (r *rig) putRecord(c record.Correction) {
	r.t.Helper()
	b, err := record.EncodeCorrection(c, rigTenantID, r.keys)
	if err != nil {
		r.t.Fatal(err)
	}
	r.putRaw(c, b)
}

// putRaw writes b as c's correction record to the storage, whatever b is.
func (r *rig) putRaw(c record.Correction, b []byte) {
	r.t.Helper()
	key, err := c.Key()
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.storage.Put(r.ctx, key, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
}

// TestRebuildHidesWhatACorrectionDeleted guards the correction records:
// read before any scan, a delete still prepared hides its scan
// (fail-closed) and is tallied unresolved, an instance delete hides its
// instance and keeps the scan's others, and a delete undone hides nothing.
func TestRebuildHidesWhatACorrectionDeleted(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		paths := r.scanPaths()
		if len(paths) < 3 {
			t.Fatalf("the corpus made %d scans, want at least 3", len(paths))
		}
		whole, err := r.p.CatalogScan(r.ctx, paths[0])
		if err != nil {
			t.Fatal(err)
		}
		var part []tenant.ScanInstance
		for _, p := range paths[1:] {
			ins, _, err := r.p.ListHeldInstances(r.ctx, p, pageOf(1000))
			if err != nil {
				t.Fatal(err)
			}
			if len(ins) > 1 {
				part = ins
				r.putRecord(r.recordOf("01j00000000000000000000002", p, record.LevelInstance, record.StateCommitted,
					[]string{ins[0].SOPInstanceUID}))
				break
			}
		}
		if part == nil {
			t.Fatal("no scan of the corpus has two instances")
		}
		r.putRecord(r.recordOf("01j00000000000000000000001", paths[0], record.LevelScan, record.StatePrepared, nil))
		r.putRecord(r.recordOf("01j00000000000000000000003", paths[len(paths)-1], record.LevelScan, record.StateUndone, nil))
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
		if tallies[tenant.TallyLeftOutDeleted] != 2 || tallies[tenant.TallyUnresolvedCorrection] != 2 {
			t.Errorf("the tallies are %v, want two scans left out by deletes and two deletes unresolved", tallies)
		}
		if got := tallies[tenant.DifferTally("scans", "removed")]; got != 1 {
			t.Errorf("the compare found %d scans removed, want the one the prepared delete hides (tallies %v)", got, tallies)
		}
		if got, want := tallies[tenant.DifferTally("instances", "removed")], whole.Instances+1; got != want {
			t.Errorf("the compare found %d instances removed, want %d", got, want)
		}
		if j.Counts.InstancesRefused != whole.Instances+1 || j.State != tenant.JobPartial {
			t.Errorf("the job ended %+v, want %d instances left out", j, whole.Instances+1)
		}
		if writes.Load() != 0 {
			t.Errorf("a rebuild wrote %d times to a storage", writes.Load())
		}
	})
}

// macField matches a record's MAC field, the last of its fields.
var macField = regexp.MustCompile(`,"mac":"[0-9a-f]{64}"`)

// forgedRecords returns, for c, the bytes of records in the form whose MAC
// does not verify for the rig's tenant: no MAC, a byte of the content
// changed after signing, the MAC of another tenant, the MAC of another
// operation id, and the MAC of another installation's key file.
func (r *rig) forgedRecords(t *testing.T, c record.Correction) map[string][]byte {
	t.Helper()
	enc := func(c record.Correction, tenantID string, keys record.Keyring) []byte {
		b, err := record.EncodeCorrection(c, tenantID, keys)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	good := enc(c, rigTenantID, r.keys)
	other := c
	other.ID = "01j0000000000000000000009z"
	digest := record.ReasonDigest("consent withdrawn")
	flipped := digest[:63] + map[bool]string{true: "1", false: "0"}[digest[63] == '0']
	out := map[string][]byte{
		"no mac":               macField.ReplaceAll(good, nil),
		"one byte":             bytes.Replace(good, []byte(digest), []byte(flipped), 1),
		"another tenant":       enc(c, "T0000000000002", r.keys),
		"another operation":    bytes.Replace(enc(other, rigTenantID, r.keys), []byte(other.ID), []byte(c.ID), 1),
		"another installation": enc(c, rigTenantID, testkit.InstallationKeys(t)),
	}
	for name, b := range out {
		if bytes.Equal(b, good) {
			t.Fatalf("%s: the forged record is the good one", name)
		}
		if _, err := record.DecodeCorrection(bytes.NewReader(b), rigTenantID, r.keys); !errors.Is(err, record.ErrUnauthenticated) {
			t.Fatalf("%s: the forged record reads %v, want unauthenticated", name, err)
		}
	}
	return out
}

// TestRebuildHidesNothingForAnUnauthenticatedRecord guards that a record on
// a storage whose MAC is missing or does not verify for the tenant, here a
// committed delete of each scan written five ways by someone without the
// key file, hides nothing: no scan is left out, the compare finds the
// catalog whole, nothing is counted unresolved, and each record is tallied
// unauthenticated.
func TestRebuildHidesNothingForAnUnauthenticatedRecord(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		paths := r.scanPaths()
		probe := r.recordOf("01j00000000000000000000010", paths[0], record.LevelScan, record.StateCommitted, nil)
		names := slices.Sorted(maps.Keys(r.forgedRecords(t, probe)))
		n := 0
		// Each way of forging on its own record, the scans taken in turn so
		// every scan is named by at least one.
		for k := 0; k < max(len(names), len(paths)); k++ {
			c := r.recordOf(fmt.Sprintf("01j000000000000000000000%02d", k+10), paths[k%len(paths)], record.LevelScan, record.StateCommitted, nil)
			r.putRaw(c, r.forgedRecords(t, c)[names[k%len(names)]])
			n++
		}
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
		if tallies[tenant.TallyUnauthenticatedCorrection] != int64(n) || tallies[tenant.TallyLeftOutDeleted] != 0 ||
			tallies[tenant.TallyUnresolvedCorrection] != 0 {
			t.Errorf("the tallies are %v, want %d records unauthenticated and nothing left out or unresolved", tallies, n)
		}
		if j.State != tenant.JobDone || j.Counts.InstancesRefused != 0 {
			t.Errorf("the job ended %s with %d instances left out, want done with none (tallies %v)", j.State, j.Counts.InstancesRefused, tallies)
		}
	})
}

// TestRebuildNeedsRecordKeys guards that no supervisor is made without the
// installation keys or a way to learn a tenant's id, so no rebuild applies
// a correction record it has not checked.
func TestRebuildNeedsRecordKeys(t *testing.T) {
	r := &rig{t: t, keys: testkit.InstallationKeys(t)}
	var writes atomic.Int64
	for name, change := range map[string]func(*Config){
		"no keys":      func(c *Config) { c.Keys = nil },
		"empty keys":   func(c *Config) { c.Keys = &secret.Keys{} },
		"no tenant id": func(c *Config) { c.TenantID = nil },
	} {
		cfg := r.config(&writes)
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: a supervisor was made", name)
		}
	}
	if _, err := New(r.config(&writes)); err != nil {
		t.Fatalf("the whole configuration is refused: %v", err)
	}
}

// TestRebuildLeavesOutAnUnknownProfile guards that a scan whose metadata
// names a profile version the tenant does not have is left out and
// tallied, never cataloged as archived as received; the other scans are
// cataloged.
func TestRebuildLeavesOutAnUnknownProfile(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	p := r.scanPaths()[0]
	key, err := layout.MetadataKey(p)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.storage.Read(r.ctx, key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	head := sidecar.NewReader(rc)
	if _, _, err := head.Next(); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	rc, err = r.storage.Read(r.ctx, key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := sidecar.Upgrade(&out, rc, *head.Scan(), func(in sidecar.Instance) (sidecar.Instance, error) {
		in.Profile, in.ProfileSHA256 = "gone@1", ""
		return in, nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if _, err := r.storage.Put(r.ctx, key, &out); err != nil {
		t.Fatal(err)
	}
	sc, err := r.p.CatalogScan(r.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
	if tallies[tenant.TallyLeftOutProfileUnknown] != 1 || tallies[tenant.DifferTally("scans", "removed")] != 1 ||
		tallies[tenant.DifferTally("instances", "removed")] != sc.Instances {
		t.Errorf("the tallies are %v, want the scan of the unknown profile left out", tallies)
	}
	if j.Counts.InstancesRefused != sc.Instances || j.Counts.InstancesSent == 0 {
		t.Errorf("the job counts %+v", j.Counts)
	}
}

// TestRebuildResumesAfterAStop guards that a build stopped between two
// scans goes on, in another process's supervisor, from the last scan
// written to the shadow rather than from the first, and ends equal.
func TestRebuildResumesAfterAStop(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	j := r.createRebuild(tenant.RebuildCompare)
	var writes atomic.Int64
	var first []string
	hold, held := make(chan struct{}), make(chan struct{})
	cfg := withAfterScan(r.config(&writes), func(key string) {
		first = append(first, key)
		if len(first) == 2 {
			close(held)
			<-hold
		}
	})
	s := r.start(cfg)
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = s.Stop(ctx) // Cut short: the unit is held between two scans.
	cancel()
	close(hold)
	stopped, err := r.p.Job(r.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State.Ended() {
		t.Fatalf("the job ended though its unit was stopped: %+v", stopped)
	}
	var second []string
	s2 := r.start(withAfterScan(r.config(&writes), func(key string) { second = append(second, key) }))
	end := r.waitEnded(s2, j.ID)
	if end.State != tenant.JobDone || len(r.tallies(j.ID)) != 0 {
		t.Errorf("the resumed compare ended %+v with %v", end, r.tallies(j.ID))
	}
	for _, k := range second {
		for _, f := range first {
			if k == f {
				t.Errorf("the resumed build wrote the scan %q again", k)
			}
		}
	}
	if len(second) == 0 || len(first)+len(second) != len(r.scanPaths()) {
		t.Errorf("the two runs wrote %d and %d scans of %d", len(first), len(second), len(r.scanPaths()))
	}
}

// TestHidesOfARelabel guards how a relabel's record is applied: once
// published, the old tree is not the archive's and is hidden; before, the
// new tree, which its copy may hold in part, is hidden; and a delete undone
// or abandoned hides nothing.
func TestHidesOfARelabel(t *testing.T) {
	old := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "t1"}
	moved := label.Path{Subject: "sub-02", Session: "ses-1", Scan: "t1"}
	rec := record.Correction{Kind: record.KindRelabel, Relabel: &record.Relabel{Level: record.LevelSubject, From: "sub-01", To: "sub-02"},
		Scans: []record.Scan{{Subject: "sub-01", Session: "ses-1", Scan: "t1", StudyUID: "1.2.3", SeriesUID: "1.2.3.4"}}}
	for _, c := range []struct {
		state  record.State
		hidden label.Path
	}{{record.StateCommitted, old}, {record.StatePurged, old}, {record.StatePrepared, moved}, {record.StateAbandoned, moved}} {
		h := &hides{scans: map[string]bool{}, instances: map[string]map[string]bool{}}
		rec.State = c.state
		h.add(rec)
		if len(h.scans) != 1 || !h.scans[hideKey(c.hidden, "1.2.3.4")] || h.unresolved != 0 {
			t.Errorf("a relabel %s hides %v, want %v", c.state, h.scans, c.hidden)
		}
	}
	for _, s := range []record.State{record.StateUndone, record.StateAbandoned} {
		h := &hides{scans: map[string]bool{}, instances: map[string]map[string]bool{}}
		h.add(record.Correction{Kind: record.KindDelete, Level: record.LevelScan, State: s, Scans: rec.Scans})
		if len(h.scans) != 0 || h.unresolved != 0 {
			t.Errorf("a delete %s hides %v", s, h.scans)
		}
	}
}

// TestUnreadableRecordStopsTheBuild guards the fail-closed rule for a
// record that cannot be read: the unit's attempt fails and nothing of the
// storage is written to the shadow, since what the record hides is not
// known.
func TestUnreadableRecordStopsTheBuild(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:2]...)
	key, err := layout.CorrectionKey("01j00000000000000000000009")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.storage.Put(r.ctx, key, bytes.NewReader([]byte(`{"format":1,"id":"cut`))); err != nil {
		t.Fatal(err)
	}
	j := r.createRebuild(tenant.RebuildCompare)
	var writes atomic.Int64
	var scans atomic.Int64
	s := r.start(withAfterScan(r.config(&writes), func(string) { scans.Add(1) }))
	deadline := time.Now().Add(30 * time.Second)
	for {
		w, err := r.p.Work(r.ctx, tenantUnit(j.ID))
		if err == nil && w.LastError == reasonRecord {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit did not fail on the unreadable record: %+v %v", w, err)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	if scans.Load() != 0 {
		t.Errorf("%d scans were written though a correction record could not be read", scans.Load())
	}
	if got, err := r.p.Job(r.ctx, j.ID); err != nil || got.State.Ended() {
		t.Errorf("the job after the failed attempt: %+v %v", got, err)
	}
}

// TestRebuildDropsTheShadowOfAJobGivenUp guards a rebuild whose unit's
// attempts ran out: the job ends failed with its reason, and the
// supervisor removes the shadow its last worker left, though no unit of
// the job is ever claimed again.
func TestRebuildDropsTheShadowOfAJobGivenUp(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:2]...)
	j := r.createRebuild(tenant.RebuildCompare)
	now := time.Now()
	w, err := r.p.ClaimWork(r.ctx, tenant.RebuildWorkKind, "gone", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.StartRebuildUnit(r.ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.PrepareShadow(r.ctx, w.Fence()); err != nil {
		t.Fatal(err)
	}
	shadow := r.dbPath + ".rebuild"
	if _, err := os.Stat(shadow); err != nil {
		t.Fatalf("no shadow made: %v", err)
	}
	// Every attempt fails, the last one far in the past, as a server
	// that kept failing and then went away leaves them.
	for i := 0; ; i++ {
		got, err := r.p.FailWork(r.ctx, w.Fence(), reasonStorage, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == tenant.WorkFailed {
			break
		}
		if i > tenant.JobUnitAttempts {
			t.Fatal("the unit is not held failed")
		}
		now = now.Add(48 * time.Hour)
		if w, err = r.p.ClaimWork(r.ctx, tenant.RebuildWorkKind, "gone", now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.p.Job(r.ctx, j.ID); err != nil || got.State != tenant.JobFailed || got.Reason != tenant.JobReasonAttemptsRanOut {
		t.Fatalf("the job given up is %+v (%v)", got, err)
	}
	var writes atomic.Int64
	s := r.start(r.config(&writes))
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := os.Stat(shadow)
		left, lerr := r.p.ShadowsLeft(r.ctx)
		if os.IsNotExist(err) && lerr == nil && len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shadow of the job given up is still there: %v, left %v %v", err, left, lerr)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
}

// tenantUnit returns the id of a rebuild job's one unit.
func tenantUnit(jobID string) string { return tenant.JobUnitID(jobID, "") }
