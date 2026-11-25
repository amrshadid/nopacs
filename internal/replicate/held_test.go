package replicate

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// inStepThenArrival archives a scan, copies it in step, then archives one
// more instance into it, so the scan is pending outside a backfill with one
// object the replica lacks. It returns the worker, its queue and the new
// object's key.
func inStepThenArrival(t *testing.T, f *fixture) (*Worker, *queue, string) {
	t.Helper()
	f.archive(1, "scan-a", synthetic(1, 1, 2, 1000))
	f.attach()
	q := f.newQueue()
	cfg := f.config()
	cfg.Queue = q
	w := f.worker(cfg)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the first pass is %+v, want the scan in step", p)
	}
	next := synthetic(1, 10, 1, 1000)
	s := f.archive(1, "scan-a", next)
	return w, q, mustKey(layout.InstanceKey(s.path, next[0].uid))
}

// wantRepairedAfter runs a pass against a true replica once the scan is
// due, and fails unless the scan ends in step with the replica holding the
// primary's bytes.
func wantRepairedAfter(t *testing.T, f *fixture, w *Worker) {
	t.Helper()
	f.replica.set(func(st *store) { st.lie, st.failRead, st.onRead = false, nil, nil })
	f.clock.Add(DefaultRetryMax + DefaultLease)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass against a true replica is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// TestFailedDamageRecordLeavesObjectUnverified guards a read back that
// finds other bytes when the damage cannot be recorded: the copy fails,
// and the next pass does not take the object on its size, but puts it
// again, so the scan never ends in step with wrong bytes.
func TestFailedDamageRecordLeavesObjectUnverified(t *testing.T) {
	f := newFixture(t)
	w, q, key := inStepThenArrival(t, f)
	f.replica.set(func(st *store) { st.lie = true })
	q.set(func(q *queue) { q.damageErr = errors.New("the test's database does not answer") })
	if p := f.runOnce(w); p.Failed != 1 || p.InStep != 0 {
		t.Fatalf("the pass is %+v, want the copy failed", p)
	}
	if r := f.row("scan-a"); r.Damaged != 0 || r.State == tenant.ReplicaInStep {
		t.Fatalf("the row is %+v, want not in step with no damage recorded", r)
	}
	q.set(func(q *queue) { q.damageErr = nil })
	wantRepairedAfter(t, f, w)
	if !f.replicaHas(key) {
		t.Error("the new object is not on the replica")
	}
}

// TestStopBeforeReadBackLeavesObjectUnverified guards a stop between an
// object's put and its read back, the put having kept other bytes: the
// next pass verifies the object rather than taking it on its size, and
// puts it again.
func TestStopBeforeReadBackLeavesObjectUnverified(t *testing.T) {
	f := newFixture(t)
	w, _, key := inStepThenArrival(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.replica.set(func(st *store) {
		st.lie = true
		st.onRead = func(k string) {
			if k == key {
				cancel()
			}
		}
	})
	p, err := w.RunOnce(ctx)
	if err != nil || p.Stopped != 1 {
		t.Fatalf("the pass is %+v, %v, want the scan stopped", p, err)
	}
	wantRepairedAfter(t, f, w)
}

// TestFailedReadBackLeavesObjectUnverified guards a read back that fails
// with the replica's own error, the put having kept other bytes: the copy
// fails, and the next pass verifies the object and puts it again.
func TestFailedReadBackLeavesObjectUnverified(t *testing.T) {
	f := newFixture(t)
	w, _, key := inStepThenArrival(t, f)
	f.replica.set(func(st *store) { st.lie, st.failRead = true, map[string]bool{key: true} })
	if p := f.runOnce(w); p.Failed != 1 || p.InStep != 0 {
		t.Fatalf("the pass is %+v, want the copy failed", p)
	}
	wantRepairedAfter(t, f, w)
}

// TestReusedKeyIsPutAgain guards a key that holds other bytes than the
// replica's verified manifest lists for it (an instance purged and
// received again under the same labels): the replica's object, of the same
// size, is not taken as copied, and is put again with the new bytes.
func TestReusedKeyIsPutAgain(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 2, 900))
	f.attach()
	q := f.newQueue()
	cfg := f.config()
	cfg.Queue = q
	if p := f.runOnce(f.worker(cfg)); p.InStep != 1 {
		t.Fatalf("the first pass is %+v, want the scan in step", p)
	}
	// The primary's first instance now holds other bytes of the same size,
	// under a manifest and a catalog row that name them.
	reused, other := s.confirmed[0], s.confirmed[1]
	newer := entry{uid: reused.SOPInstanceUID, size: reused.Size, seed: 0xA5}
	f.putPrimary(reused.Key, readAll(t, pattern(newer.seed, newer.size)))
	newSum := sumOf(newer)
	lines := make([]manifest.File, len(s.lines))
	copy(lines, s.lines)
	for i := range lines {
		if lines[i].SOPInstanceUID == reused.SOPInstanceUID {
			lines[i].SHA256 = newSum.String()
		}
	}
	digest := f.putManifest(s.path, lines)
	q.set(func(q *queue) { q.claim = func(c *tenant.ReplicaClaim) { c.ManifestSHA256 = digest } })
	cfg.Catalog = catalog{Catalog: f.p, edit: func(in *tenant.ScanInstance) {
		if in.SOPInstanceUID == reused.SOPInstanceUID {
			in.ArchivedSHA256 = newSum
		}
	}}
	// Damage on the other instance makes the scan due outside a backfill.
	if _, err := f.p.RecordReplicaDamage(f.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: s.series,
		Damage: tenant.ReplicaDamage{SOPInstanceUID: other.SOPInstanceUID, Revision: 1, Found: f.clock.Now()}}); err != nil {
		t.Fatal(err)
	}
	if p := f.runOnce(f.worker(cfg)); p.Claimed != 1 || p.Failed != 0 {
		t.Fatalf("the pass is %+v, want the scan copied", p)
	}
	if got := f.tree(f.replica.Store)[reused.Key]; got != newSum {
		t.Errorf("the replica's object at the reused key is %s, want the new bytes %s", got, newSum)
	}
}

// TestSidecarOfAReceptionInFlightIsNotInStep guards the sidecar's tie to
// the claimed manifest: a reception that writes its next sidecar and
// manifest while the copy runs, and has not committed, never lets the
// copy end in step at the old manifest with the new sidecar; once it
// commits, the scan ends in step at the new manifest.
func TestSidecarOfAReceptionInFlightIsNotInStep(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 3, 1500))
	f.attach()
	cfg := f.config()
	cfg.Parallel = 1
	var commit func()
	var once atomic.Bool
	prefix := mustKey(layout.InstancePrefix(s.path))
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if strings.HasPrefix(key, prefix) && once.CompareAndSwap(false, true) {
				commit = f.stage(1, "scan-a", synthetic(1, 50, 1, 1500))
			}
		}
	})
	w := f.worker(cfg)
	if p := f.runOnce(w); p.InStep != 0 || p.Failed != 1 {
		t.Fatalf("the pass is %+v, want the copy failed as a change", p)
	}
	if r := f.row("scan-a"); r.State == tenant.ReplicaInStep || !strings.Contains(r.LastError, "not the one claimed") {
		t.Errorf("the row is %+v, want not in step with the change as its reason", r)
	}
	f.replica.set(func(st *store) { st.onPut = nil })
	commit()
	f.clock.Add(DefaultChangedDelay)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass after the commit is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// readAll reads r whole, failing the test on an error.
func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
