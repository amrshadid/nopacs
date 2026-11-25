package scrub

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// switchPlaces switches "main" onto its replica "copy" through the tenant
// port, passing over the checks of the replica, and swaps the stores the
// rig hands out by name, as the swap points each name at the other place.
func (r *replicaRig) switchPlaces() {
	r.t.Helper()
	if _, _, err := r.p.BeginSwitch(r.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "main", Replica: "copy", Token: "tok-1",
		SkipVerification: true, SkipReason: "a test passes over the checks", Now: time.Now()}, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.p.SwapStorages(r.ctx, tenant.SwitchSwap{ID: "sw-1", Now: time.Now()}, r.audit()); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.p.FinishSwitch(r.ctx, "sw-1", time.Now()); err != nil {
		r.t.Fatal(err)
	}
	r.store, r.replicas["copy"] = r.replicas["copy"], r.store
}

// TestMovedFindingClearsWhenTheReplicaReadsGood guards that a finding a
// switch moved to the replica's name with its place is cleared by a scrub
// that reads the replica's object good once the replicator copied it
// again: the replica's row is in step with no damage by then, and the
// finding would otherwise stand and refuse every later switch.
func TestMovedFindingClearsWhenTheReplicaReadsGood(t *testing.T) {
	r := newReplicaRig(t)
	_, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.511")
	r.addReplica("copy")
	r.copyAll()
	r.putOn(r.store, keys[0], []byte("bytes that are not what was archived"))
	r.scrubAll(r.config())
	if got := r.findingsOn("main"); len(got) != 1 || got[0] != "damaged "+keys[0] {
		t.Fatalf("after the scrub the findings on the primary are %v; want its object damaged", got)
	}

	r.switchPlaces()
	if got := r.findingsOn("copy"); len(got) != 1 || got[0] != "damaged "+keys[0] {
		t.Fatalf("after the switch the findings on the replica are %v; want the damaged object's, moved with its place", got)
	}
	r.copyAll()
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Fatalf("after the copy again the replica's row is %s with %d damaged; want in step", row.State, row.Damaged)
	}

	r.scrubAll(r.config())
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("after a scrub read the replica good the findings on it are %v; want none", got)
	}
	facts, err := r.p.SwitchFacts(r.ctx, "main", "copy")
	if err != nil {
		t.Fatal(err)
	}
	if facts.ReplicaFindings != 0 {
		t.Errorf("the switch facts count %d findings on the replica; want none, so a later switch is not refused", facts.ReplicaFindings)
	}
}

// countingPipeline counts the clears of a replica's damage and of an
// object's findings a scrub writes.
type countingPipeline struct {
	tenant.Pipeline
	damageClears, findingClears atomic.Int64
}

// ClearReplicaDamage counts a clear of a replica's damage.
func (c *countingPipeline) ClearReplicaDamage(ctx context.Context, fence *tenant.WorkFence, replica, seriesUID, sopUID string,
	now time.Time) (bool, error) {
	c.damageClears.Add(1)
	return c.Pipeline.ClearReplicaDamage(ctx, fence, replica, seriesUID, sopUID, now)
}

// ClearObjectFindings counts a clear of an object's findings.
func (c *countingPipeline) ClearObjectFindings(ctx context.Context, fence tenant.WorkFence, storage, key string,
	kinds []tenant.ScrubFindingKind, now time.Time) (int, error) {
	c.findingClears.Add(1)
	return c.Pipeline.ClearObjectFindings(ctx, fence, storage, key, kinds, now)
}

// TestReplicaReadGoodWritesOnlyWhatItClears guards that a scrub reading a
// damaged replica's scan good again writes a clear only for the damage the
// row counts, and no clear of findings no one recorded: not two writes for
// every object of the scan.
func TestReplicaReadGoodWritesOnlyWhatItClears(t *testing.T) {
	r := newReplicaRig(t)
	files := synthetic("1.2.826.0.1.3680043.10.512", 4)
	r.archiveEntries("s1", "1.2.826.0.1.3680043.10.512.90", "1.2.826.0.1.3680043.10.512.80", files)
	series := "1.2.826.0.1.3680043.10.512.80"
	copy1 := r.addReplica("copy")
	r.copyAll()
	key := r.keys[files[0].uid]
	good := r.read(copy1, key)
	r.putOn(copy1, key, []byte("bytes that are not what was archived"))
	r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Fatalf("after the first scrub the replica's row is %s with %d damaged; want damaged with 1", row.State, row.Damaged)
	}

	r.putOn(copy1, key, good)
	counting := &countingPipeline{Pipeline: r.p}
	cfg := r.config()
	cfg.Tenants = &oneTenant{p: counting}
	r.scrubAll(cfg)
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after the second scrub the replica's row is %s with %d damaged; want in step", row.State, row.Damaged)
	}
	if n := counting.damageClears.Load(); n != 1 {
		t.Errorf("the scrub wrote %d clears of the replica's damage; want 1, for its one damaged object", n)
	}
	if n := counting.findingClears.Load(); n != 0 {
		t.Errorf("the scrub wrote %d clears of findings; want none, since none was recorded", n)
	}
}
