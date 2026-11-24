package scrub

import (
	"bytes"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// markedIn counts the instances of the scan the catalog answers marked.
func (f *fixture) markedIn(p label.Path) int {
	f.t.Helper()
	n := 0
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rows, next, err := f.p.ListHeldInstances(f.ctx, p, page)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, in := range rows {
			if in.Damaged {
				n++
			}
		}
		if next == "" {
			return n
		}
		page.Cursor = next
	}
}

// TestScrubClearsEveryMarkReadGoodAgain guards that a scrub clears the
// mark of every instance it reads good again, however many findings its
// scan holds: 1003 instances' objects deleted are found missing and
// marked; put back as they were, a later scrub reads each good, clears
// every finding and removes every mark, past the bound of one read of a
// scan's findings.
func TestScrubClearsEveryMarkReadGoodAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("about 3000 commits, each synced to the disk: not a short test")
	}
	f := newFixture(t)
	files := synthetic("2.25.7", tenant.MaxScanFindings+3)
	p := f.archiveEntries("scan-a", "2.25.100", "2.25.101", files)
	for _, e := range files {
		if err := f.store.Delete(f.ctx, f.keys[e.uid]); err != nil {
			t.Fatal(err)
		}
	}
	cfg := f.config()
	first := f.createScrub("main")
	s := f.start(cfg)
	if j := f.waitEnded(s, first.ID); j.Counts.InstancesRefused != int64(len(files)) {
		t.Fatalf("the first scrub ended as %+v, want every instance refused", j.Counts)
	}
	if n := f.markedIn(p); n != len(files) {
		t.Fatalf("after the first scrub %d instances are marked, want %d", n, len(files))
	}
	for _, e := range files {
		if _, err := f.store.Put(f.ctx, f.keys[e.uid], bytes.NewReader(e.b)); err != nil {
			t.Fatal(err)
		}
	}
	second := f.createScrub("main")
	j := f.waitEnded(s, second.ID)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != int64(len(files)) {
		t.Errorf("the scrub after the objects were put back ended as %s %+v", j.State, j.Counts)
	}
	if n := f.markedIn(p); n != 0 {
		t.Errorf("after the objects were put back and read good, %d instances are still marked", n)
	}
	if n := f.tallies(second.ID)[tenant.TallyCleared]; n != int64(len(files)) {
		t.Errorf("the later job cleared %d findings, want %d", n, len(files))
	}
}

// TestScrubClearsAMarkReadGoodBeforeTheUnitResumes guards that an instance
// read good again is unmarked by that read, not at the scan's end: the
// unit stops (its tenant let go) after reading the repaired instance and
// before the scan's end, and the unit that resumes after its cursor never
// reads the instance again; it is unmarked all the same, its finding
// cleared by the job.
func TestScrubClearsAMarkReadGoodBeforeTheUnitResumes(t *testing.T) {
	f := newFixture(t)
	files := synthetic("2.25.8", 3)
	p := f.archiveEntries("scan-a", "2.25.100", "2.25.101", files)
	repaired := files[0]
	if err := f.store.Delete(f.ctx, f.keys[repaired.uid]); err != nil {
		t.Fatal(err)
	}
	first := f.createScrub("main")
	s := f.start(f.config())
	f.waitEnded(s, first.ID)
	if !f.marked(p, repaired.uid) {
		t.Fatal("the first scrub did not mark the instance deleted")
	}
	if _, err := f.store.Put(f.ctx, f.keys[repaired.uid], bytes.NewReader(repaired.b)); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(f.ctx); err != nil {
		t.Fatal(err)
	}
	stopNext := f.keys[files[1].uid]
	var sup *Supervisor
	var letGo atomic.Bool
	cfg := withRead(f.config(), func(key string, attempt int) {
		if key != stopNext || !letGo.CompareAndSwap(false, true) {
			return
		}
		// The tenant is let go while the unit reads its second instance:
		// the unit writes what it read and gives its unit back before the
		// third, and a new worker resumes after the cursor.
		sup.mu.Lock()
		run := sup.tenants["lab"]
		delete(sup.tenants, "lab")
		sup.mu.Unlock()
		run.retire()
		sup.TenantRemoved("lab")
	})
	second := f.createScrub("main")
	var err error
	if sup, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Stop(f.ctx) })
	j := f.waitEnded(sup, second.ID)
	if !letGo.Load() {
		t.Fatal("the tenant was never let go while the unit read")
	}
	if j.State != tenant.JobDone {
		t.Errorf("the scrub ended %s %+v", j.State, j.Counts)
	}
	if f.marked(p, repaired.uid) {
		t.Error("the instance read good before the unit resumed is still marked")
	}
	if n := f.tallies(second.ID)[tenant.TallyCleared]; n != 1 {
		t.Errorf("the job cleared %d findings, want 1", n)
	}
}
