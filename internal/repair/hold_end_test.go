package repair

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPutPastLeaseLeavesNothingUnrecorded guards a worker whose lease's due
// passes while its put streams (it lost the database, or its renewals are
// late). Past the due, within the grace, a delete and purge of the
// instance are refused as the scan being repaired. Once the grace too has
// passed they are taken, the instance's object removed and the manifest
// rewritten; the put that then lands puts bytes back no catalog row names,
// so the repair ends changed, its finding left holding, and the key is
// recorded as an orphan of the scan for a scrub to read again: never bytes
// on the storage nothing records.
func TestPutPastLeaseLeavesNothingUnrecorded(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	cfg := f.config()
	var late, after deletion
	f.onSecondRead(f.keys[0], func() {
		f.clock.Add(cfg.Lease + time.Second)
		late = f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzt", 0, true)
		f.clock.Add(tenant.RepairHoldGrace)
		after = f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzs", 0, true)
	})
	var reasons []string
	cfg.Outcome = func(o Outcome) { reasons = append(reasons, string(o.Kind)+": "+o.Reason) }
	res, err := f.run(cfg, f.manual(fd.ID))
	if err != nil {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !errors.Is(late.err, tenant.ErrScanRepairing) || late.committed {
		t.Errorf("a delete past the lease's due, within the grace = %+v; want refused as the scan being repaired", late)
	}
	if after.err != nil || !after.purged {
		t.Fatalf("a delete once the grace ran out = %+v; want published and purged", after)
	}
	if len(reasons) != 1 || reasons[0] != string(Skipped)+": "+tenant.RepairCatalogChanged {
		t.Errorf("the outcomes are %q; want the object skipped as the catalog changed", reasons)
	}
	if !f.findingOf(fd.ID).Holds() {
		t.Error("a repair ended after its instance was deleted cleared its finding")
	}
	_, _, present := f.measure(f.primary.Store, f.keys[0])
	if !present {
		t.Fatal("the put did not land; the scenario was not reached")
	}
	stray := f.findingOf(tenant.ScrubFindingID(tenant.RepairID(res.Job, fd.ID), "main", f.keys[0], tenant.FindingOrphan))
	if stray.Kind != tenant.FindingOrphan || !stray.Holds() || stray.Path != f.path || stray.Job != res.Job {
		t.Errorf("the bytes the put left are recorded as %+v; want an orphan of the scan that holds", stray)
	}
}

// TestPutCutBeforeLeaseDue guards the deadline of a put to the primary: it
// is the due of the unit's lease less the margin, not the bound of one
// object's read or put, which may be far longer, so a worker that lost the
// database has ended its put before its hold of the scan runs out.
func TestPutCutBeforeLeaseDue(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	cfg := f.config()
	cfg.ObjectTimeout = time.Hour
	var mu sync.Mutex
	var left time.Duration
	var bounded bool
	f.primary.set(func(s *store) {
		s.onPut = func(ctx context.Context, _ string) {
			d, ok := ctx.Deadline()
			mu.Lock()
			left, bounded = time.Until(d), ok
			mu.Unlock()
		}
	})
	res, err := f.run(cfg, f.manual(fd.ID))
	if err != nil || res.Counts[Repaired] != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := cfg.Lease - cfg.PutMargin; !bounded || left > want || left <= 0 {
		t.Errorf("the put had %v left (bounded %v); want at most the lease less the margin, %v", left, bounded, want)
	}
}

// TestFailedPutLetsScanGo guards the way out of an object whose put failed:
// the repair's hold of the scan is let go at once, so a delete of another
// instance of the scan is taken right after, not refused for the rest of
// the job or until the hold runs out.
func TestFailedPutLetsScanGo(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	f.primary.set(func(s *store) { s.failPut = true })
	res, err := f.run(f.config(), f.manual(fd.ID))
	if !errors.Is(err, ErrTryAgain) || res.Counts[Retry] != 1 {
		t.Fatalf("Run with a failing put = %+v, %v; want the object tried again", res, err)
	}
	if d := f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzr", 1, false); d.err != nil || !d.committed {
		t.Errorf("a delete of another instance right after the failed put = %+v; want published", d)
	}
	rs := f.repairsOf(fd.ID)
	if len(rs) != 1 || rs[0].Done() || !rs[0].HeldUntil.IsZero() {
		t.Errorf("the repair after its failed put reads %+v; want one, not ended, holding nothing", rs)
	}
}

// TestCanceledJobPutsNothingMore guards a job canceled while it runs: the
// record of its next object is refused, the walk ends there, and the unit
// ends with the job, so nothing more is put.
func TestCanceledJobPutsNothingMore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.remove(f.primary.Store, f.keys[0])
	f.remove(f.primary.Store, f.keys[1])
	first, second := f.find(0), f.find(1)
	job := f.manual(first.ID, second.ID)
	var once sync.Once
	var cancelErr error
	f.replica.set(func(s *store) {
		s.onRead = func(string) {
			once.Do(func() {
				_, cancelErr = f.p.CancelJob(f.ctx, job.ID, f.clock.Now(), f.audit(), nil)
			})
		}
	})
	res, err := f.run(f.config(), job)
	if cancelErr != nil {
		t.Fatalf("CancelJob: %v", cancelErr)
	}
	if err != nil || !res.Done {
		t.Fatalf("the Run of a job canceled while it ran = %+v, %v; want ended", res, err)
	}
	if _, puts := f.primary.counts(); puts != 0 {
		t.Errorf("the primary was put %d times after the job was canceled", puts)
	}
	if res.Counts[Repaired] != 0 {
		t.Errorf("the Run counted %v; want nothing repaired", res.Counts)
	}
	if j := f.job(job.ID); j.State != tenant.JobCanceled {
		t.Errorf("the job reads %s; want canceled", j.State)
	}
	if w, err := f.p.Work(f.ctx, tenant.JobUnitID(job.ID, "")); err != nil || w.State != tenant.WorkDone {
		t.Errorf("the canceled job's unit reads %+v, %v; want done", w, err)
	}
}
