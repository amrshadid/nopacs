package scrub

import (
	"context"
	"io"
	"sync"
	"time"
)

// pacer holds the reads of one storage to a rate, whichever unit reads it:
// each piece read reserves its share of the time at the rate, after the
// reservations before it, and the reader waits until its share has passed.
// So the average over any run of reads is at most the rate, and a scrub
// leaves the rest of the storage's bandwidth to reception and reads.
type pacer struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// next is when the reservations made so far have passed.
	next time.Time
}

// maxBurst is how far behind the clock a pacer's reservations may fall, so
// a storage left idle does not bank an unbounded burst.
const maxBurst = time.Second

// take reserves the time n bytes take at rate bytes a second and waits
// until it has passed, or ctx ends.
func (p *pacer) take(ctx context.Context, n int, rate int64) error {
	if n <= 0 || rate <= 0 {
		return ctx.Err()
	}
	share := time.Duration(float64(n) / float64(rate) * float64(time.Second))
	p.mu.Lock()
	now := p.now()
	if p.next.Before(now.Add(-maxBurst)) {
		p.next = now.Add(-maxBurst)
	}
	p.next = p.next.Add(share)
	wait := p.next.Sub(now)
	p.mu.Unlock()
	return p.sleep(ctx, wait)
}

// pacedReader is a reader whose pieces are paced.
type pacedReader struct {
	ctx  context.Context
	r    io.Reader
	p    *pacer
	rate int64
	// n counts the bytes read.
	n int64
}

// Read implements io.Reader.
func (r *pacedReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += int64(n)
	if perr := r.p.take(r.ctx, n, r.rate); perr != nil && err == nil {
		err = perr
	}
	return n, err
}
