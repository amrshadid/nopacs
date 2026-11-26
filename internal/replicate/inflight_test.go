package replicate

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
)

// deadlineStore is the replica's storage, remembering the time each put's
// context had left when the put began.
type deadlineStore struct {
	blob.Store
	mu   sync.Mutex
	left []time.Duration
}

// Put records the time its context has left, then puts.
func (d *deadlineStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.mu.Lock()
		d.left = append(d.left, time.Until(dl))
		d.mu.Unlock()
	}
	return d.Store.Put(ctx, key, r)
}

// Close keeps the storage open: the fixture closes it.
func (*deadlineStore) Close() error { return nil }

// TestPutEndsBeforeTheLeaseDue guards the deadline of each put: it is the
// due the lease's claim or last renewal set, less the margin, not a fixed
// share of the lease counted from the put's start, so a copy whose lease
// was ended under it (a purge's start, a delete's publish), and which can
// renew it no more, lands nothing after that due. A quarter of the lease
// has passed since the claim when the puts begin, too little for a
// renewal, so each has at most the lease less that quarter and the margin.
func TestPutEndsBeforeTheLeaseDue(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 2, 1000))
	f.attach()
	var once atomic.Bool
	f.primary.set(func(st *store) {
		st.onRead = func(key string) {
			if key == mustKey(layout.ManifestKey(s.path)) && once.CompareAndSwap(false, true) {
				f.clock.Add(DefaultLease / 4)
			}
		}
	})
	ds := &deadlineStore{Store: f.replica}
	cfg := f.config()
	cfg.Parallel = 1
	open := cfg.Open
	cfg.Open = func(ctx context.Context, name string) (blob.Store, error) {
		if name == "copy" {
			return ds, nil
		}
		return open(ctx, name)
	}
	w := f.worker(cfg)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	want := DefaultLease - DefaultLease/4 - w.cfg.PutMargin
	if len(ds.left) == 0 {
		t.Fatal("no put was bounded by a deadline")
	}
	for i, left := range ds.left {
		if left > want || left < want-5*time.Second {
			t.Errorf("put %d had %v left; want the lease's due less the margin, %v, counted from the claim", i, left, want)
		}
	}
}

// pauseRecorder is a write gate that admits every write and counts the
// writes inside it, by storage.
type pauseRecorder struct {
	mu     sync.Mutex
	inside map[string]int
	// refuse, when set, is answered instead of admitting.
	refuse error
}

// Enter admits a write to storage, or answers the refusal.
func (p *pauseRecorder) Enter(_ context.Context, storage string) (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse != nil {
		return nil, p.refuse
	}
	p.inside[storage]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.inside[storage]--
			p.mu.Unlock()
		})
	}, nil
}

// in returns the writes inside the gate of storage now.
func (p *pauseRecorder) in(storage string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inside[storage]
}

// TestPutsEnterTheReplicasGate guards the write gate of the replica: every
// put of a copy, its objects, its sidecar and its manifest, is made inside
// the replica's gate, which a switch waits on before it swaps the
// replica's place; a gate that refuses the write fails the copy with
// nothing put.
func TestPutsEnterTheReplicasGate(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 3, 1000))
	f.attach()
	gate := &pauseRecorder{inside: map[string]int{}, refuse: errors.New("the test's gate refuses")}
	var outside, inside atomic.Int64
	f.replica.set(func(st *store) {
		st.onPut = func(string) {
			if gate.in("copy") > 0 {
				inside.Add(1)
			} else {
				outside.Add(1)
			}
		}
	})
	cfg := f.config()
	cfg.Pause = gate
	cfg.RetryMin, cfg.RetryMax = time.Millisecond, time.Millisecond
	w := f.worker(cfg)
	if p := f.runOnce(w); p.Failed != 1 || p.Bytes != 0 {
		t.Fatalf("the pass through a gate that refuses is %+v, want the scan failed with nothing put", p)
	}
	if n := len(f.replica.putKeys()); n != 0 {
		t.Fatalf("%d objects were put through a gate that refused", n)
	}
	gate.mu.Lock()
	gate.refuse = nil
	gate.mu.Unlock()
	f.clock.Add(time.Minute)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	if inside.Load() != 5 || outside.Load() != 0 {
		t.Errorf("%d puts were made inside the replica's gate and %d outside; want every one of the 5 inside", inside.Load(), outside.Load())
	}
	if n := gate.in("copy"); n != 0 {
		t.Errorf("%d writes are left inside the replica's gate after the pass", n)
	}
}
