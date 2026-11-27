package download

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

const (
	// DefaultParallel is how many streams a download holds open when the
	// caller does not say.
	DefaultParallel = 4
	// MaxParallel is the most streams a download may be asked to hold. The
	// server refuses a caller past its own bound (16 by default) rather than
	// queueing it, so asking for more only buys refusals.
	MaxParallel = 16

	// regrowAfter is how long a download that lowered its streams after a
	// refusal waits, without another, before it tries one more.
	regrowAfter = 30 * time.Second
	// closeGrace is how long a stream's place is kept after the stream was
	// closed at the end of its lane, for the server to release its own count.
	closeGrace = 25 * time.Millisecond
	// firstBackoff and maxBackoff bound the wait after a refusal: it doubles
	// from the first up to the largest, unless the server says how long.
	firstBackoff = 500 * time.Millisecond
	maxBackoff   = 30 * time.Second
	// refusalPatience is how long a stream may be refused in a row, with
	// no progress of its own, before the download gives up and says so.
	refusalPatience = 10 * time.Minute
)

// gate bounds how many streams are open at once and lowers the bound when
// the server refuses one, so a download settles at what the server allows
// its caller instead of failing or hammering. The bound grows again, one
// at a time, after a quiet stretch, in case the refusal was another
// caller's load.
type gate struct {
	mu      sync.Mutex
	limit   int
	most    int
	active  int
	changed chan struct{}
	refusal time.Time
	clk     clock
}

// newGate returns a gate that lets most streams run.
func newGate(most int, clk clock) *gate {
	return &gate{limit: most, most: most, changed: make(chan struct{}), clk: clk}
}

// wake lets every waiter look again; the caller holds the lock.
func (g *gate) wake() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquire waits for a free place.
func (g *gate) acquire(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.limit < g.most && g.clk.now().Sub(g.refusal) >= regrowAfter {
			g.limit++
			g.refusal = g.clk.now()
		}
		if g.active < g.limit {
			g.active++
			g.mu.Unlock()
			return nil
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// release gives a place back.
func (g *gate) release() {
	g.mu.Lock()
	g.active--
	g.wake()
	g.mu.Unlock()
}

// refused is called by a holder of a place that the server refused: the
// bound drops below the places in use, never below one.
func (g *gate) refused() {
	g.mu.Lock()
	g.limit = max(1, min(g.limit, g.active)-1)
	g.refusal = g.clk.now()
	g.wake()
	g.mu.Unlock()
}

// bound returns the streams allowed now.
func (g *gate) bound() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// refusal is the server telling a caller to come back later: the API's
// resource exhausted (the caller or the server holds as many reads as it
// serves) or HTTP 429 or 503 on the plain route (RFC 9110 15.5.4, 15.6.4).
type refusal struct {
	// after is the wait the server named in Retry-After (RFC 9110 10.2.3),
	// or zero.
	after time.Duration
	err   error
}

// Error says what the server refused.
func (r *refusal) Error() string { return r.err.Error() }

// Unwrap returns the underlying error.
func (r *refusal) Unwrap() error { return r.err }

// refusalOf reports whether err is the server refusing a read for now, and
// how long it asked to wait.
func refusalOf(err error) (*refusal, bool) {
	if err == nil {
		return nil, false
	}
	if r, ok := err.(*refusal); ok { //nolint:errorlint // refusal is returned unwrapped by this package.
		return r, true
	}
	if fault.KindOf(err) == fault.Exhausted {
		return &refusal{err: err}, true
	}
	return nil, false
}

// refusalOfStatus makes a refusal of an HTTP answer whose status asks the
// caller to come back later, or returns nil for any other status.
func refusalOfStatus(res *http.Response) *refusal {
	if res.StatusCode != http.StatusTooManyRequests && res.StatusCode != http.StatusServiceUnavailable {
		return nil
	}
	r := &refusal{err: fault.New(fault.Exhausted, "the server answered "+strconv.Itoa(res.StatusCode)+" "+strings.ToLower(http.StatusText(res.StatusCode))+": it holds as many reads open for this caller as it serves")}
	if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s > 0 {
		r.after = min(time.Duration(s)*time.Second, maxBackoff)
	}
	return r
}

// backoff is the wait after the n-th refusal in a row (counting from one):
// what the server asked for, or a doubling from firstBackoff.
func backoff(n int, asked time.Duration) time.Duration {
	if asked > 0 {
		return asked
	}
	d := firstBackoff
	for i := 1; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}
