package s3

import (
	"context"
	"sync"
)

// budget is the bytes of part buffers all the Puts of a store may hold
// together. It is a counting semaphore of bytes with a queue, so that a Put
// that needs a whole part is not starved by many that need a little.
//
// It cannot deadlock the Puts that use it, because of how they use it: a Put
// takes one part from it before it reads a byte, waiting for that alone, and
// then only takes more with tryTake, which never waits. A Put that holds a
// part therefore always makes progress by itself (its own parts finish and
// come back to it), and gives its parts back when it ends, so whoever waits
// in the queue is waiting for Puts that are not waiting for anything.
type budget struct {
	mu      sync.Mutex
	avail   int64
	waiting []*waiter
}

type waiter struct {
	n     int64
	ready chan struct{}
}

func newBudget(n int64) *budget { return &budget{avail: n} }

// take waits until n bytes are free, in order of arrival, or ctx ends.
func (b *budget) take(ctx context.Context, n int64) error {
	b.mu.Lock()
	if len(b.waiting) == 0 && b.avail >= n {
		b.avail -= n
		b.mu.Unlock()
		return nil
	}
	w := &waiter{n: n, ready: make(chan struct{})}
	b.waiting = append(b.waiting, w)
	b.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		defer b.mu.Unlock()
		select {
		case <-w.ready: // Granted as the context ended: give it back.
			b.avail += n
			b.grant()
		default:
			for i, x := range b.waiting {
				if x == w {
					b.waiting = append(b.waiting[:i], b.waiting[i+1:]...)
					break
				}
			}
			b.grant() // The head may have been this waiter.
		}
		return ctx.Err()
	}
}

// tryTake takes n bytes if they are free now and nobody is waiting for
// them, and says whether it did.
func (b *budget) tryTake(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.waiting) == 0 && b.avail >= n {
		b.avail -= n
		return true
	}
	return false
}

// give returns n bytes.
func (b *budget) give(n int64) {
	b.mu.Lock()
	b.avail += n
	b.grant()
	b.mu.Unlock()
}

// grant wakes the waiters at the head of the queue that now fit. The caller
// holds mu.
func (b *budget) grant() {
	for len(b.waiting) > 0 && b.waiting[0].n <= b.avail {
		w := b.waiting[0]
		b.waiting = b.waiting[1:]
		b.avail -= w.n
		close(w.ready)
	}
}
