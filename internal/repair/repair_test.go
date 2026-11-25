package repair

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// run makes job's row when the test has not, then runs one claim on a new
// runner over cfg and returns its result and error.
func (f *fixture) run(cfg Config, job Job) (Result, error) {
	f.t.Helper()
	f.create(job)
	return f.runner(cfg).Run(f.ctx)
}

// wantCounts fails unless res counted exactly want.
func wantCounts(t *testing.T, res Result, want map[OutcomeKind]int64) {
	t.Helper()
	if fmt.Sprint(res.Counts) != fmt.Sprint(want) {
		t.Errorf("the run counted %v, want %v", res.Counts, want)
	}
}

// wantCleared fails unless the finding was cleared by job and its instance
// unmarked.
func (f *fixture) wantCleared(id, job string, i int) {
	f.t.Helper()
	got := f.findingOf(id)
	if got.Holds() || got.ClearJob != job {
		f.t.Errorf("the finding reads cleared %v by %q; want cleared by the repair job %q", got.Cleared, got.ClearJob, job)
	}
	if f.marked(i) {
		f.t.Error("the instance is still marked after its repair")
	}
}

// TestRepairsMissingObject guards the repair of an object missing on the
// primary: put back byte-identical from the replica in step, its repair
// recorded with nothing found, its finding cleared by the repair job and
// the instance's mark lifted.
func TestRepairsMissingObject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	if fd.Kind != tenant.FindingMissing || !f.marked(0) {
		t.Fatalf("the finding is %s, marked %v; want missing and marked", fd.Kind, f.marked(0))
	}
	job := f.manual(fd.ID)
	res, err := f.run(f.config(), job)
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v; want done", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	if res.Bytes != 4096 {
		t.Errorf("the run put %d bytes, want 4096", res.Bytes)
	}
	f.wantGood(0)
	f.wantCleared(fd.ID, job.ID, 0)
	reps := f.repairsOf(fd.ID)
	if len(reps) != 1 || !reps[0].Done() || reps[0].FoundSize != nil || reps[0].From != "copy" || reps[0].Job != job.ID ||
		reps[0].ID != tenant.RepairID(job.ID, fd.ID) || reps[0].SeriesUID != f.series || reps[0].Changed {
		t.Errorf("the repairs of the finding are %+v; want one ended, from copy, nothing found", reps)
	}
	row := f.job(job.ID)
	c := row.Counts
	if row.State != tenant.JobDone || c.Instances != 1 || c.InstancesSent != 1 || c.BytesSent != 4096 || c.InstancesRefused != 0 ||
		row.Repair == nil || row.Repair.Storage != "main" || len(row.Repair.Findings) != 1 {
		t.Errorf("the job's row reads %s with %+v, repair %+v; want done, one object repaired", row.State, c, row.Repair)
	}
	tallies, err := f.p.JobTallies(f.ctx, job.ID)
	if err != nil || fmt.Sprint(tallies) != fmt.Sprint([]tenant.JobTally{{Key: "repaired", Count: 1}}) {
		t.Errorf("the job's tallies are %+v, %v; want repaired 1", tallies, err)
	}
}

// TestRepairsDamagedObject guards the repair of a damaged (flipped) object:
// the damaged bytes' size and SHA-256 are recorded, the catalog's bytes put
// back at the same key through the storage's write gate, the finding
// cleared, and an audit entry written by the server.
func TestRepairsDamagedObject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.damage(f.primary.Store, f.keys[1])
	damagedSize, damagedSum, _ := f.measure(f.primary.Store, f.keys[1])
	fd := f.find(1)
	job := f.manual(fd.ID)
	res, err := f.run(f.config(), job)
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v; want done", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	f.wantGood(1)
	f.wantCleared(fd.ID, job.ID, 1)
	reps := f.repairsOf(fd.ID)
	if len(reps) != 1 || reps[0].FoundSize == nil || *reps[0].FoundSize != damagedSize || reps[0].FoundSHA256 != damagedSum ||
		reps[0].ExpectedSHA256 != sumOf(f.files[1]) {
		t.Errorf("the repair is %+v; want the damaged object's size and checksum and the catalog's", reps)
	}
	if got := f.primary.puts; len(got) != 1 || got[0] != f.keys[1] {
		t.Errorf("the primary was put %v; want the one damaged key alone", got)
	}
	all, _, err := f.p.ListAudit(f.ctx, master.AuditFilter{}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var entries []master.AuditEntry
	for _, e := range all {
		if e.Method == CompleteMethod {
			entries = append(entries, e)
		}
	}
	if len(entries) != 1 || entries[0].KeyID != "" || entries[0].Object != "main" || entries[0].Details["finding"] != fd.ID {
		t.Errorf("the repair's audit entries are %+v; want one of the server's for the storage", entries)
	}
}

// TestRecordsBeforePut guards the order of a repair: the record of what
// the damaged object was is written before the put, so a put that fails
// leaves the repair recorded and the finding open, the object untouched;
// the next attempt puts it back under the same record.
func TestRecordsBeforePut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	_, damagedSum, _ := f.measure(f.primary.Store, f.keys[0])
	fd := f.find(0)
	f.primary.set(func(s *store) { s.failPut = true })
	job := f.manual(fd.ID)
	res, err := f.run(f.config(), job)
	if !errors.Is(err, ErrTryAgain) || res.Done {
		t.Fatalf("Run with a failing put = %+v, %v; want tried again", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Retry: 1})
	reps := f.repairsOf(fd.ID)
	if len(reps) != 1 || reps[0].Done() || reps[0].FoundSHA256 != damagedSum {
		t.Fatalf("after a failed put the repairs are %+v; want one recorded, not ended, with the damaged checksum", reps)
	}
	if !f.findingOf(fd.ID).Holds() || !f.marked(0) {
		t.Error("a failed put cleared the finding or lifted the mark")
	}
	if _, sum, _ := f.measure(f.primary.Store, f.keys[0]); sum != damagedSum {
		t.Error("a failed put changed the primary's object")
	}
	f.primary.set(func(s *store) { s.failPut = false })
	if _, err := f.run(f.config(), job); !errors.Is(err, ErrNotDue) {
		t.Fatalf("Run before the next attempt is due = %v; want not due", err)
	}
	f.clock.Add(time.Hour)
	res, err = f.run(f.config(), job)
	if err != nil || !res.Done || res.Job != job.ID {
		t.Fatalf("Run of the next attempt = %+v, %v; want done", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	f.wantGood(0)
	f.wantCleared(fd.ID, job.ID, 0)
	if reps := f.repairsOf(fd.ID); len(reps) != 1 || !reps[0].Done() || reps[0].FoundSHA256 != damagedSum {
		t.Errorf("after the next attempt the repairs are %+v; want the first record, ended", reps)
	}
}

// TestRefusesWrongReplica guards a replica whose object is not the
// catalog's bytes: it is never put on the primary, it is recorded as the
// replica's damage (its scan no longer in step), and the finding is left
// open as having no good copy.
func TestRefusesWrongReplica(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	_, damagedSum, _ := f.measure(f.primary.Store, f.keys[0])
	fd := f.find(0)
	f.damage(f.replica.Store, f.keys[0])
	res, err := f.run(f.config(), f.manual(fd.ID))
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v; want done, the object refused", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{NoGoodCopy: 1})
	if _, puts := f.primary.counts(); puts != 0 {
		t.Errorf("the primary was put %d times; want none", puts)
	}
	if _, sum, _ := f.measure(f.primary.Store, f.keys[0]); sum != damagedSum {
		t.Error("the primary's object changed")
	}
	row, err := f.p.ReplicaScan(f.ctx, "copy", f.series)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica's row is %s with %d damaged; want damaged with one", row.State, row.Damaged)
	}
	if !f.findingOf(fd.ID).Holds() || len(f.repairsOf(fd.ID)) != 0 {
		t.Error("a refused repair cleared the finding or recorded a repair")
	}
	if row := f.job(res.Job); row.State != tenant.JobPartial || row.Counts.InstancesRefused != 1 || row.Counts.InstancesSent != 0 {
		t.Errorf("the job's row reads %s with %+v; want partial, one object refused", row.State, row.Counts)
	}
}

// TestNoReplicaInStep guards a scan no replica is in step for: refused with
// no good copy, the replica never read, nothing recorded or put.
func TestNoReplicaInStep(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	if _, err := f.p.BackfillReplica(f.ctx, "copy", f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if f.inStep() {
		t.Fatal("the replica is still in step after a backfill was asked")
	}
	before, _ := f.replica.counts()
	res, err := f.run(f.config(), f.manual(fd.ID))
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v; want done, the object refused", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{NoGoodCopy: 1})
	if reads, _ := f.replica.counts(); reads != before {
		t.Errorf("a replica not in step was read %d times", reads)
	}
	if _, puts := f.primary.counts(); puts != 0 || len(f.repairsOf(fd.ID)) != 0 || !f.findingOf(fd.ID).Holds() {
		t.Error("a repair with no replica in step wrote something")
	}
}

// TestReplicaChangedBeforePut guards the put: a replica whose object reads
// good when checked and other bytes when streamed to the primary leaves the
// primary as it was, since only the catalog's bytes are ever kept.
func TestReplicaChangedBeforePut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	_, damagedSum, _ := f.measure(f.primary.Store, f.keys[0])
	fd := f.find(0)
	seen := 0
	f.replica.set(func(s *store) {
		s.onRead = func(key string) {
			if key != f.keys[0] {
				return
			}
			if seen++; seen == 2 {
				f.replica.set(func(s *store) { s.flipRead = map[string]bool{key: true} })
			}
		}
	})
	res, err := f.run(f.config(), f.manual(fd.ID))
	if !errors.Is(err, ErrTryAgain) {
		t.Fatalf("Run = %+v, %v; want tried again", res, err)
	}
	if _, sum, _ := f.measure(f.primary.Store, f.keys[0]); sum != damagedSum {
		t.Error("bytes other than the catalog's were kept on the primary")
	}
	if !f.findingOf(fd.ID).Holds() {
		t.Error("the finding was cleared though nothing good was put")
	}
}

// TestReadsBack guards the read back: a primary that keeps other bytes than
// it was given, while it reports those given, leaves the finding open and
// the instance marked, never cleared on the put's word.
func TestReadsBack(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	f.primary.set(func(s *store) { s.lie = true })
	res, err := f.run(f.config(), f.manual(fd.ID))
	if !errors.Is(err, ErrTryAgain) {
		t.Fatalf("Run = %+v, %v; want tried again", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Retry: 1})
	if !f.findingOf(fd.ID).Holds() || !f.marked(0) {
		t.Error("an object read back wrong had its finding cleared or its mark lifted")
	}
}

// TestStopLeavesUnit guards a stop in the middle of a repair: the unit is
// left to its lease, the repair recorded and not ended, and once the lease
// ends the next Run takes the unit over and ends the repair under the same
// record.
func TestStopLeavesUnit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	fd := f.find(0)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	reads := 0
	f.replica.set(func(s *store) {
		s.onRead = func(key string) {
			if key == f.keys[0] {
				if reads++; reads == 2 {
					cancel() // the stream to the primary starts on an ended context
				}
			}
		}
	})
	job := f.manual(fd.ID)
	f.create(job)
	if _, err := f.runner(f.config()).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped Run = %v; want canceled", err)
	}
	reps := f.repairsOf(fd.ID)
	if len(reps) != 1 || reps[0].Done() || !f.findingOf(fd.ID).Holds() {
		t.Fatalf("after a stop the repairs are %+v; want one recorded and not ended, the finding holding", reps)
	}
	f.replica.set(func(s *store) { s.onRead = nil })
	if _, err := f.run(f.config(), job); !errors.Is(err, ErrNotDue) {
		t.Fatalf("a Run while the stopped one's lease holds = %v; want not due", err)
	}
	f.clock.Add(time.Minute)
	res, err := f.run(f.config(), job)
	if err != nil || !res.Done {
		t.Fatalf("the Run after the lease ended = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	f.wantGood(0)
	f.wantCleared(fd.ID, job.ID, 0)
	if reps := f.repairsOf(fd.ID); len(reps) != 1 || !reps[0].Done() {
		t.Errorf("the repairs are %+v; want the one record, ended", reps)
	}
}

// movingDB is the tenant's database with a switch's swap run right after
// each repair is recorded.
type movingDB struct {
	DB
	f *fixture
}

// RecordRepair records, then swaps "main" with its replica.
func (m movingDB) RecordRepair(ctx context.Context, r tenant.RepairRecord) (tenant.Repair, bool, error) {
	out, made, err := m.DB.RecordRepair(ctx, r)
	if err != nil {
		return out, made, err
	}
	f := m.f
	if _, _, err := f.p.BeginSwitch(ctx, tenant.SwitchStart{ID: "sw-1", Primary: "main", Replica: "copy", Token: "tok-1",
		Now: f.clock.Now()}, f.audit(), nil); err != nil {
		f.t.Error(err)
	}
	a := f.audit()
	a.KeyID = ""
	if _, err := f.p.SwapStorages(ctx, tenant.SwitchSwap{ID: "sw-1", Now: f.clock.Now()}, a); err != nil {
		f.t.Error(err)
	}
	return out, made, nil
}

// TestSwitchMovesFinding guards a finding a switch's swap moved to the other
// storage between the repair's record and its end: it is not cleared, the
// repair not ended.
func TestSwitchMovesFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	fd := f.find(0)
	cfg := f.config()
	cfg.DB = movingDB{DB: f.p, f: f}
	res, err := f.run(cfg, f.manual(fd.ID))
	if err != nil {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Skipped: 1})
	if got := f.findingOf(fd.ID); got.Storage != "copy" || !got.Holds() {
		t.Errorf("the moved finding reads on %s, holding %v; want holding on copy", got.Storage, got.Holds())
	}
	if reps := f.repairsOf(fd.ID); len(reps) != 1 || reps[0].Done() {
		t.Errorf("the repairs are %+v; want one recorded and not ended", reps)
	}
}

// TestRunTwice guards repeats: a job ended leaves nothing due, a new job
// naming a finding repaired already is refused, one of all meets nothing,
// and a second repair of a storage is refused while one is queued.
func TestRunTwice(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	job := f.manual(fd.ID)
	f.create(job)
	again := Job{ID: f.id("rj"), Storage: "main", Mode: ModeManual, All: true}
	if _, err := f.p.CreateJob(f.ctx, again.NewJob(f.clock.Now()), f.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a second repair of the storage while one is queued = %v; want a precondition failure", err)
	}
	if res, err := f.run(f.config(), job); err != nil || res.Counts[Repaired] != 1 {
		t.Fatalf("the first Run = %+v, %v", res, err)
	}
	if res, err := f.runner(f.config()).Run(f.ctx); !errors.Is(err, ErrNotDue) || len(res.Counts) != 0 {
		t.Errorf("a Run after the job ended = %+v, %v; want nothing due", res, err)
	}
	other := f.manual(fd.ID)
	if _, err := f.p.CreateJob(f.ctx, other.NewJob(f.clock.Now()), f.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a job naming a finding repaired already = %v; want a precondition failure", err)
	}
	res, err := f.run(f.config(), again)
	if err != nil || !res.Done {
		t.Fatalf("a job of all = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{})
	if _, puts := f.primary.counts(); puts != 1 {
		t.Errorf("the primary was put %d times in all; want once", puts)
	}
	if reps := f.repairsOf(fd.ID); len(reps) != 1 {
		t.Errorf("the finding has %d repairs; want one", len(reps))
	}
	f.wantCleared(fd.ID, job.ID, 0)
}

// TestAlreadyGood guards an object that reads whole again by the time the
// job meets it: no put, its finding cleared with a record whose bytes found
// are the catalog's.
func TestAlreadyGood(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.damage(f.primary.Store, f.keys[0])
	fd := f.find(0)
	f.damage(f.primary.Store, f.keys[0]) // flips the byte back
	job := f.manual(fd.ID)
	res, err := f.run(f.config(), job)
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{AlreadyGood: 1})
	if _, puts := f.primary.counts(); puts != 0 {
		t.Errorf("an object already good was put %d times", puts)
	}
	f.wantCleared(fd.ID, job.ID, 0)
	if reps := f.repairsOf(fd.ID); len(reps) != 1 || reps[0].FoundSHA256 != sumOf(f.files[0]) {
		t.Errorf("the repairs are %+v; want one finding the catalog's bytes", reps)
	}
}

// TestModes guards the selection of findings: an automatic job mends the
// findings that mark an instance and leaves a damaged manifest, which a
// person's job of all mends, put back at the catalog's digest.
func TestModes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[1])
	missing := f.find(1)
	key := mustKey(layout.ManifestKey(f.path))
	scan, err := f.p.CatalogScan(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.damage(f.primary.Store, key)
	size, sum, _ := f.measure(f.primary.Store, key)
	man := f.record(tenant.ScrubFinding{Kind: tenant.FindingManifestDamaged, Storage: "main", Key: key, Path: f.path,
		ExpectedSHA256: scan.ManifestSHA256, FoundSize: &size, FoundSHA256: sum})
	auto := Job{ID: f.id("rj"), Storage: "main", Mode: ModeAuto}
	res, err := f.run(f.config(), auto)
	if err != nil || !res.Done {
		t.Fatalf("the automatic Run = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	f.wantGood(1)
	f.wantCleared(missing.ID, auto.ID, 1)
	if !f.findingOf(man.ID).Holds() {
		t.Error("an automatic job mended a manifest")
	}
	all := Job{ID: f.id("rj"), Storage: "main", Mode: ModeManual, All: true}
	res, err = f.run(f.config(), all)
	if err != nil || !res.Done {
		t.Fatalf("the Run of all = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	if _, got, _ := f.measure(f.primary.Store, key); got != scan.ManifestSHA256 {
		t.Error("the manifest was not put back at the catalog's digest")
	}
	if got := f.findingOf(man.ID); got.Holds() || got.ClearJob != all.ID {
		t.Errorf("the manifest's finding reads holding %v, cleared by %q", got.Holds(), got.ClearJob)
	}
}

// TestJobValidate guards what a job must name: a manual job findings or
// instances or all, an automatic one none of them.
func TestJobValidate(t *testing.T) {
	t.Parallel()
	for name, j := range map[string]Job{
		"manual naming nothing":   {ID: "rj-1", Storage: "main", Mode: ModeManual},
		"manual naming both":      {ID: "rj-1", Storage: "main", Mode: ModeManual, All: true, Findings: []string{"sf-1"}},
		"automatic naming a one":  {ID: "rj-1", Storage: "main", Mode: ModeAuto, Findings: []string{"sf-1"}},
		"no mode":                 {ID: "rj-1", Storage: "main", All: true},
		"a replica badly named":   {ID: "rj-1", Storage: "main", Mode: ModeAuto, From: "Bad Name"},
		"a finding id not an id":  {ID: "rj-1", Storage: "main", Mode: ModeManual, Findings: []string{"a b"}},
		"no storage":              {ID: "rj-1", Mode: ModeAuto},
		"an id that is not an id": {ID: "", Storage: "main", Mode: ModeAuto},
	} {
		if err := j.Validate(); err == nil {
			t.Errorf("a job with %s is valid", name)
		}
	}
	if m, err := ParseMode(""); err != nil || m != ModeManual {
		t.Errorf("the default mode is %q, %v; want manual", m, err)
	}
	if _, err := ParseMode("sometimes"); err == nil {
		t.Error("a mode of neither manual nor auto parsed")
	}
}

// TestLargeObjectBoundedHeap guards memory: a 64 MiB object is checked on
// the replica, put back and read back as a stream, the live heap growing by
// far less than the object.
func TestLargeObjectBoundedHeap(t *testing.T) {
	f := newFixture(t, synthetic(1, 64<<20))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	stop := testkit.HeapPeak()
	res, err := f.run(f.config(), f.manual(fd.ID))
	peak := stop()
	if err != nil || res.Counts[Repaired] != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	f.wantGood(0)
	if peak > base.HeapAlloc+16<<20 {
		t.Errorf("the live heap grew by %d MiB repairing a 64 MiB object; want under 16", (peak-base.HeapAlloc)>>20)
	}
}

// TestRunnerConfig guards what a runner needs: a database, an opener, a
// clock, an owner and a tenant's name.
func TestRunnerConfig(t *testing.T) {
	t.Parallel()
	ok := Config{DB: movingDB{}, Tenant: "lab", Open: func(context.Context, string) (blob.Store, error) { return nil, errTest },
		Now: time.Now, Owner: "x"}
	if _, err := New(ok); err != nil {
		t.Fatalf("New of a whole config = %v", err)
	}
	for name, change := range map[string]func(c *Config){
		"no database": func(c *Config) { c.DB = nil },
		"no opener":   func(c *Config) { c.Open = nil },
		"no clock":    func(c *Config) { c.Now = nil },
		"no owner":    func(c *Config) { c.Owner = "" },
		"no tenant":   func(c *Config) { c.Tenant = "" },
		"a lease too short": func(c *Config) {
			c.Lease = time.Millisecond
		},
	} {
		c := ok
		change(&c)
		if _, err := New(c); err == nil {
			t.Errorf("New with %s succeeded", name)
		}
	}
}
