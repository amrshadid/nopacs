package receive

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestParallelCallsEachIndexWithinTheBound guards the archive step's
// parallel puts and read-backs: every index is called once, never more
// than archiveParallel at a time.
func TestParallelCallsEachIndexWithinTheBound(t *testing.T) {
	const n = 100
	var seen [n]atomic.Int32
	var running, most atomic.Int32
	err := parallel(context.Background(), n, func(_ context.Context, i int) error {
		r := running.Add(1)
		for {
			m := most.Load()
			if r <= m || most.CompareAndSwap(m, r) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		seen[i].Add(1)
		running.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		if c := seen[i].Load(); c != 1 {
			t.Fatalf("index %d was called %d times", i, c)
		}
	}
	if m := most.Load(); m > archiveParallel || m < 2 {
		t.Fatalf("at most %d calls ran at once, want 2 to %d", m, archiveParallel)
	}
}

// TestParallelStopsAtTheFirstError guards that a failed put or read-back
// ends its scan's work: the first error is returned, the calls still
// running see their context end, and no call starts after it.
func TestParallelStopsAtTheFirstError(t *testing.T) {
	boom := errors.New("the storage refused")
	var started atomic.Int32
	err := parallel(context.Background(), 1000, func(ctx context.Context, i int) error {
		started.Add(1)
		if i == 3 {
			return boom
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("a running call's context did not end after another failed")
		}
		return ctx.Err()
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the first error", err)
	}
	if s := started.Load(); s > 3+archiveParallel {
		t.Fatalf("%d calls started, though the fourth failed with at most %d running", s, archiveParallel)
	}
}

// TestParallelEndsWithItsContext guards that a step stopped from outside
// (the pipeline stopping) starts nothing more and says why.
func TestParallelEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	err := parallel(ctx, 1000, func(ctx context.Context, i int) error {
		if started.Add(1) == 5 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the context's end", err)
	}
	if s := started.Load(); s >= 1000 {
		t.Fatal("every call started though the context ended")
	}
}
