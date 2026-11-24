package send

import (
	"context"
	"io"
	"sync"
	"time"
)

// assocLimit counts the associations (for a stow sender, requests) open
// against a bound that a setting gives and may change: a take past the
// bound is refused, never queued, and a lowered bound takes effect as the
// associations above it end.
type assocLimit struct {
	mu        sync.Mutex
	max, used int
}

// newAssocLimit returns a limit of max.
func newAssocLimit(max int) *assocLimit { return &assocLimit{max: max} }

// setMax changes the bound.
func (l *assocLimit) setMax(max int) {
	l.mu.Lock()
	l.max = max
	l.mu.Unlock()
}

// take counts one more association, or reports false at the bound.
func (l *assocLimit) take() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used >= l.max {
		return false
	}
	l.used++
	return true
}

// give ends one association.
func (l *assocLimit) give() {
	l.mu.Lock()
	l.used--
	l.mu.Unlock()
}

// rateLimit is a sender's bound on bytes per second over all its
// associations together, as an average over rateWindow: each read waits
// until the bytes it passed fit the budget.
type rateLimit struct {
	mu     sync.Mutex
	rate   int64
	tokens float64
	last   time.Time
}

// rateWindow is the span the average of a byte rate is taken over: the
// most bytes that may pass at once after an idle spell is the rate times
// it.
const rateWindow = 2 * time.Second

// setRate sets the bound in bytes per second; 0 removes it.
func (r *rateLimit) setRate(rate int64) {
	r.mu.Lock()
	r.rate = rate
	r.mu.Unlock()
}

// wait spends n bytes of the budget, waiting while it is overdrawn, or
// until ctx ends.
func (r *rateLimit) wait(ctx context.Context, n int) error {
	r.mu.Lock()
	if r.rate <= 0 {
		r.mu.Unlock()
		return nil
	}
	now := time.Now()
	burst := float64(r.rate) * rateWindow.Seconds()
	if r.last.IsZero() {
		r.tokens = burst
	} else {
		r.tokens = min(burst, r.tokens+now.Sub(r.last).Seconds()*float64(r.rate))
	}
	r.last = now
	r.tokens -= float64(n)
	var d time.Duration
	if r.tokens < 0 {
		d = time.Duration(-r.tokens / float64(r.rate) * float64(time.Second))
	}
	r.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// paced passes reads of r under the rate limit.
type paced struct {
	ctx  context.Context
	r    io.Reader
	rate *rateLimit
}

// Read reads from r, then waits for the bytes it read to fit the rate.
func (p *paced) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		if werr := p.rate.wait(p.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}
