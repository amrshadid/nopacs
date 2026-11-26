// Package pace holds the background work that reads or writes a storage to
// a rate in bytes a second (a scrub's reads, a replica's copies), so that
// work leaves the rest of the storage's bandwidth to reception and reads.
package pace

import (
	"context"
	"io"
	"sync"
	"time"
)

// MaxBurst is how far behind the clock a Pacer's reservations may fall, so
// a storage left idle does not bank an unbounded burst.
const MaxBurst = time.Second

// Pacer holds the pieces of one storage's work to a rate, whichever worker
// moves them: each piece reserves its share of the time at the rate, after
// the reservations before it, and the worker waits until its share has
// passed. So the average over any run of pieces is at most the rate. It is
// safe for concurrent use.
type Pacer struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// next is when the reservations made so far have passed.
	next time.Time
}

// New returns a pacer on the clock now that waits with sleep; a nil now is
// time.Now and a nil sleep a timer.
func New(now func() time.Time, sleep func(ctx context.Context, d time.Duration) error) *Pacer {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = Sleep
	}
	return &Pacer{now: now, sleep: sleep}
}

// Take reserves the time n bytes take at rate bytes a second and waits
// until it has passed, or ctx ends. A rate of zero or less takes no time.
func (p *Pacer) Take(ctx context.Context, n int, rate int64) error {
	if n <= 0 || rate <= 0 {
		return ctx.Err()
	}
	share := time.Duration(float64(n) / float64(rate) * float64(time.Second))
	p.mu.Lock()
	now := p.now()
	if p.next.Before(now.Add(-MaxBurst)) {
		p.next = now.Add(-MaxBurst)
	}
	p.next = p.next.Add(share)
	wait := p.next.Sub(now)
	p.mu.Unlock()
	return p.sleep(ctx, wait)
}

// Reader is a reader whose pieces are paced by P at Rate bytes a second.
type Reader struct {
	// Ctx ends a wait; R is read; P and Rate pace it.
	Ctx  context.Context
	R    io.Reader
	P    *Pacer
	Rate int64
	// N counts the bytes read.
	N int64
}

// Read implements io.Reader: it reads, then waits for the piece's share.
func (r *Reader) Read(b []byte) (int, error) {
	n, err := r.R.Read(b)
	r.N += int64(n)
	if perr := r.P.Take(r.Ctx, n, r.Rate); perr != nil && err == nil {
		err = perr
	}
	return n, err
}

// Sleep waits d, or until ctx ends, and returns ctx's error then.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
