package replicate

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRenewalFailureIsLeftToItsLease guards a renewal of the lease that
// fails because the database does not answer: the copy stops, nothing is
// recorded on the row (no failure, no reason), and the replica's wait
// before its next try is not raised, since the replica did nothing wrong.
func TestRenewalFailureIsLeftToItsLease(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 4, 1000))
	f.attach()
	q := f.newQueue()
	q.set(func(q *queue) { q.renewErr = errors.New("the test's database does not answer") })
	cfg := f.config()
	cfg.Queue, cfg.Parallel = q, 1
	var once atomic.Bool
	f.primary.set(func(st *store) {
		st.onRead = func(key string) {
			if key == s.confirmed[1].Key && once.CompareAndSwap(false, true) {
				f.clock.Add(DefaultLease / 2)
			}
		}
	})
	w := f.worker(cfg)
	if p := f.runOnce(w); p.Stopped != 1 || p.Failed != 0 {
		t.Fatalf("the pass is %+v, want the scan stopped and no failure recorded", p)
	}
	if r := f.row("scan-a"); r.State != tenant.ReplicaCopying || r.LastError != "" {
		t.Errorf("the row is %+v, want copying under its lease, untouched", r)
	}
	if n := w.failures["copy"]; n != 0 {
		t.Errorf("the replica's failures in a row are %d after the database failed, want 0", n)
	}
	if f.replicaHas(mustKey(layout.ManifestKey(s.path))) {
		t.Error("the manifest was put after the lease could not be renewed")
	}
}

// TestTimerKeepsTheLeaseThroughABlockedPut guards the renewal from a
// timer: a put that reads nothing for longer than a third of the lease (a
// storage buffering the body before it answers) still has its lease
// renewed meanwhile, and the scan ends in step.
func TestTimerKeepsTheLeaseThroughABlockedPut(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 3, 1000))
	f.attach()
	q := f.newQueue()
	cfg := f.config()
	cfg.Queue, cfg.Parallel, cfg.Lease = q, 1, 600*time.Millisecond
	var during atomic.Int64
	var once atomic.Bool
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if key == s.confirmed[1].Key && once.CompareAndSwap(false, true) {
				before := q.renewed.Load()
				time.Sleep(350 * time.Millisecond)
				during.Store(q.renewed.Load() - before)
			}
		}
	})
	if p := f.runOnce(f.worker(cfg)); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	if during.Load() < 1 {
		t.Errorf("no renewal while a put blocked past a third of the lease")
	}
}

// TestNoWriteAfterTheLeaseIsLost guards a lease taken over while a put
// blocks: the timer's renewal finds it lost and cancels the copy, after
// which nothing more is put on the replica (that object, the others, the
// sidecar or the manifest) and nothing is written to the row.
func TestNoWriteAfterTheLeaseIsLost(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 5, 1000))
	f.attach()
	q := f.newQueue()
	cfg := f.config()
	cfg.Queue, cfg.Parallel, cfg.Lease = q, 1, 300*time.Millisecond
	var noticed atomic.Bool
	var at atomic.Int64
	var once atomic.Bool
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if key != s.confirmed[1].Key || !once.CompareAndSwap(false, true) {
				return
			}
			// Another worker claims the scan once its lease ran out by the
			// database's clock; the worker's own clock is then set back, so
			// only the timer can find the lease lost.
			f.clock.Add(time.Second)
			if c, err := f.p.ClaimReplicaScans(f.ctx, "another", f.clock.Now(), time.Hour, 1); err != nil || len(c) != 1 {
				t.Errorf("the takeover claimed %v, %v", c, err)
			}
			f.clock.Add(-time.Second)
			select {
			case <-q.lost:
				noticed.Store(true)
			case <-time.After(2 * time.Second):
			}
			at.Store(int64(len(f.replica.putKeys())))
		}
	})
	if p := f.runOnce(f.worker(cfg)); p.Stopped != 1 {
		t.Fatalf("the pass is %+v, want the scan stopped", p)
	}
	if !noticed.Load() {
		t.Fatal("the timer never found the lease lost")
	}
	if n := int64(len(f.replica.putKeys())) - at.Load(); n != 0 {
		t.Errorf("%d objects were put after the lease was found lost: %v", n, f.replica.putKeys())
	}
	for _, k := range f.replica.putKeys() {
		if strings.HasSuffix(k, mustKey(layout.ManifestKey(s.path))) {
			t.Error("the manifest was put after the lease was lost")
		}
	}
	if r := f.row("scan-a"); r.Attempt != 2 || r.LastError != "" {
		t.Errorf("the row is %+v, want the takeover's attempt, untouched", r)
	}
}
