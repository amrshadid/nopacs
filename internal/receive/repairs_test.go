package receive

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// markDamaged records, through a scrub job of the storage "main", that the
// one instance the tenant holds is damaged, as a scrub that read it would,
// and returns the finding.
func (e *env) markDamaged() tenant.ScrubFinding {
	e.t.Helper()
	scans, err := e.store.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	if err != nil || len(scans.Items) != 1 {
		e.t.Fatalf("the scans: %v, %v", scans.Items, err)
	}
	s := scans.Items[0]
	p := label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
	held, _, err := e.store.ListHeldInstances(e.ctx, p, master.Page{Limit: 10})
	if err != nil || len(held) != 1 {
		e.t.Fatalf("the instances: %v, %v", held, err)
	}
	in := held[0]
	if _, err := e.store.CreateJob(e.ctx, tenant.NewJob{ID: "scrub1", Kind: tenant.JobScrub, Storage: "main", Created: e.clock.Now()},
		e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
	w, err := e.store.ClaimWork(e.ctx, tenant.ScrubWorkKind, "scrubber", e.clock.Now(), time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	size, found := in.Size, in.Size
	f := tenant.ScrubFinding{Kind: tenant.FindingDamaged, Storage: "main", Key: in.Key, Path: p, SOPInstanceUID: in.SOPInstanceUID,
		Revision: 1, ExpectedSize: &size, FoundSize: &found, ExpectedSHA256: in.ArchivedSHA256, FoundSHA256: tenant.SHA256{0: 1}}
	f.ID = tenant.ScrubFindingID("scrub1", f.Storage, f.Key, f.Kind)
	got, _, err := e.store.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: e.clock.Now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// TestTheSameFileForAMarkedInstanceIsHeldAsARepair guards an arrival whose
// SOP Instance UID and received checksum are those of an instance a scrub
// marked damaged: it is not ignored as a duplicate, whose bytes are let go,
// but held with the reason repairs_damaged naming the finding, its bytes
// kept in the incoming area for the replace that repairs the instance; the
// instance stays as it is, marked.
func TestTheSameFileForAMarkedInstanceIsHeldAsARepair(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send("MR_small.dcm")
	e.settle(1)
	f := e.markDamaged()
	e.clock.Advance(time.Minute)
	e.send("MR_small.dcm")
	rs := byArrival(e.settle(2))
	second := rs[1]
	if second.Counts.FilesDuplicate != 0 || second.Counts.FilesQuarantined != 1 || second.Counts.FilesArchived != 0 {
		t.Fatalf("the second arrival's counts %+v, want the file held, not a duplicate", second.Counts)
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineRepairsDamaged || held[0].Details["finding"] != f.ID || !held[0].ContentKept {
		t.Fatalf("held %+v, want one repair naming finding %s with its bytes kept", held, f.ID)
	}
	if n := e.incomingFiles(); n != 1 {
		t.Errorf("the incoming area holds %d files, want the repair's", n)
	}
	marks, err := e.store.DamagedInstances(e.ctx, []string{f.SOPInstanceUID})
	if err != nil || marks[f.SOPInstanceUID] != f.ID {
		t.Errorf("after the arrival the marks are %v, %v; want the instance still marked until a replace", marks, err)
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	if totals.Instances != 1 {
		t.Errorf("the catalog holds %d instances, want 1", totals.Instances)
	}
}

// TestTheSameRepairSentAgainIsADuplicate guards that one copy of the bytes
// that repair a marked instance is held, however many times they are sent:
// the third arrival of the same file, while the second's held copy waits
// for its replace, is counted a duplicate of it and its bytes let go, not
// held as a copy of its own; the instance stays marked.
func TestTheSameRepairSentAgainIsADuplicate(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send("MR_small.dcm")
	e.settle(1)
	f := e.markDamaged()
	e.clock.Advance(time.Minute)
	e.send("MR_small.dcm")
	e.settle(2)
	e.clock.Advance(time.Minute)
	e.send("MR_small.dcm")
	rs := byArrival(e.settle(3))
	third := rs[2]
	if third.Counts.FilesDuplicate != 1 || third.Counts.FilesQuarantined != 0 || third.Counts.FilesArchived != 0 {
		t.Errorf("the third arrival's counts %+v, want a duplicate of the held repair", third.Counts)
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineRepairsDamaged || held[0].ReceptionID != rs[1].ID {
		t.Errorf("held %+v, want the second arrival's repair alone", held)
	}
	if n := e.incomingFiles(); n != 1 {
		t.Errorf("the incoming area holds %d files, want the one held repair's", n)
	}
	marks, err := e.store.DamagedInstances(e.ctx, []string{f.SOPInstanceUID})
	if err != nil || marks[f.SOPInstanceUID] != f.ID {
		t.Errorf("the marks are %v, %v; want the instance still marked", marks, err)
	}
}
