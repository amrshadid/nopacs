package download

import (
	"context"
	"sync"
	"time"
)

// clock is the time a download waits and measures by. The real one is used
// everywhere but in tests, which pass a clock that waits for nothing so a
// backoff of minutes or a bound on bandwidth is shown in microseconds.
type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// realClock is the wall clock; its sleep ends early, with the context's
// error, when the context ends.
func realClock() clock {
	return clock{now: time.Now, sleep: func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

// limiter bounds the bytes per second of every stream of one download
// together. Each chunk is let through at the time the chunks before it,
// at the configured rate, would have finished, so the average over any
// stretch longer than one chunk never exceeds the rate and no bucket of
// saved-up time lets a burst through after a pause. A nil limiter lets
// everything through.
type limiter struct {
	mu   sync.Mutex
	rate float64
	next time.Time
	clk  clock
}

// newLimiter returns a limiter of bytesPerSecond, or nil when it is not
// above zero.
func newLimiter(bytesPerSecond int64, clk clock) *limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	return &limiter{rate: float64(bytesPerSecond), clk: clk}
}

// wait blocks until n more bytes may be written, or returns the context's
// error when it ends first.
func (l *limiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := l.clk.now()
	if l.next.Before(now) {
		l.next = now
	}
	start := l.next
	l.next = start.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	return l.clk.sleep(ctx, start.Sub(now))
}
