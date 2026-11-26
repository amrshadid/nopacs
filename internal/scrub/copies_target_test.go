package scrub

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// scrubOf runs a scrub of the one storage named to its end.
func (r *replicaRig) scrubOf(storage string, cfg Config) tenant.Job {
	r.t.Helper()
	j := r.createScrub(storage)
	s := r.start(cfg)
	got := r.waitEnded(s, j.ID)
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(stopCtx); err != nil {
		r.t.Fatal(err)
	}
	return got
}

// allFindings returns every finding of the tenant, cleared ones too.
func (r *replicaRig) allFindings() []tenant.ScrubFinding {
	r.t.Helper()
	got, _, err := r.p.ListScrubFindings(r.ctx, tenant.ScrubFindingFilter{IncludeCleared: true}, master.Page{Limit: 100})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// TestScrubOfAReplicaReadsItsScans guards that a scrub of one storage that
// is a replica (what the schedule makes for each storage) lists its
// primary's scans and reads each of them on the replica alone: an object
// changed on the replica is found, the replica's row is damaged, and the
// primary is not read for the scan.
func TestScrubOfAReplicaReadsItsScans(t *testing.T) {
	r := newReplicaRig(t)
	_, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.521")
	copy1 := r.addReplica("copy")
	r.copyAll()
	r.putOn(copy1, keys[1], []byte("bytes that are not what was archived"))

	j := r.scrubOf("copy", r.config())
	if j.Counts.Scans != 1 {
		t.Errorf("the scrub of the replica listed %d scans; want its primary's one scan", j.Counts.Scans)
	}
	if got := r.tallies(j.ID)[TallyReplicaDamaged]; got != 1 {
		t.Errorf("the scrub of the replica tallied %d objects damaged on it; want 1", got)
	}
	if row := r.row("copy", series); row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("after the scrub of the replica its row is %s with %d damaged; want damaged with 1", row.State, row.Damaged)
	}
	if r.replicaReads["copy"].Load() == 0 {
		t.Error("the scrub of the replica read nothing on it")
	}
}

// TestReplicaDamageIsListedAndClearedWithIt guards that a replica's damage
// a scrub finds is also a finding on the replica's name, naming its primary
// and marking no instance, so a list of findings shows it; and that the
// replicator's verified copy, which mends the damage, clears that finding
// with it, by no job.
func TestReplicaDamageIsListedAndClearedWithIt(t *testing.T) {
	r := newReplicaRig(t)
	p, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.522")
	copy1 := r.addReplica("copy")
	r.copyAll()
	good := r.read(copy1, keys[0])
	r.putOn(copy1, keys[0], []byte("bytes that are not what was archived"))

	j := r.scrubAll(r.config())
	all := r.allFindings()
	if len(all) != 1 {
		t.Fatalf("the scrub holds findings %+v; want the replica's damage, once", all)
	}
	f := all[0]
	if f.Kind != tenant.FindingDamaged || f.Storage != "copy" || f.ReplicaOf != "main" || f.Key != keys[0] || f.Path != p ||
		!f.Holds() || f.Job != j.ID {
		t.Errorf("the finding is %+v; want damaged on copy, of main, at its key, held, by the scrub", f)
	}
	for _, in := range r.held(p) {
		if in.Damaged {
			t.Errorf("the instance %s is marked; a finding on a replica marks none", in.SOPInstanceUID)
		}
	}
	if got := r.tallies(j.ID)[tenant.ScrubTally(tenant.FindingDamaged)]; got != 1 {
		t.Errorf("the scrub tallied %d damaged findings; want the replica's one", got)
	}

	// The replicator copies the damaged object again from the primary.
	r.putOn(copy1, keys[0], good)
	r.copyAll()
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Fatalf("after the copy the replica's row is %s with %d damaged", row.State, row.Damaged)
	}
	all = r.allFindings()
	if len(all) != 1 || all[0].Holds() || all[0].ClearJob != "" {
		t.Errorf("after the copy the findings are %+v; want the one cleared with the damage, by no job", all)
	}
}

// TestReplicaFindingClearedByTheScrubThatReadsItGood guards that a scrub
// that reads a replica's damaged object good again clears, with the
// damage, the finding recorded with it, by its own job.
func TestReplicaFindingClearedByTheScrubThatReadsItGood(t *testing.T) {
	r := newReplicaRig(t)
	_, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.523")
	copy1 := r.addReplica("copy")
	r.copyAll()
	good := r.read(copy1, keys[1])
	r.putOn(copy1, keys[1], []byte("bytes that are not what was archived"))
	r.scrubAll(r.config())
	r.putOn(copy1, keys[1], good)

	j := r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after the second scrub the replica's row is %s with %d damaged", row.State, row.Damaged)
	}
	all := r.allFindings()
	if len(all) != 1 || all[0].Holds() || all[0].ClearJob != j.ID {
		t.Errorf("after the second scrub the findings are %+v; want the one cleared by job %s", all, j.ID)
	}
}
