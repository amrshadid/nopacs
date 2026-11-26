package replicate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/pace"
)

// paceClock is the clock of the pacing of puts, which each wait advances,
// so the waits are measured without sleeping.
type paceClock struct {
	mu     sync.Mutex
	now    time.Time
	waited time.Duration
}

// Now returns the clock's time.
func (c *paceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// sleep records d and advances the clock by it.
func (c *paceClock) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waited += d
	c.now = c.now.Add(d)
	return ctx.Err()
}

// total returns the waits so far.
func (c *paceClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waited
}

// TestPutsArePacedByReplicaRate guards replica.rate: a pass that puts N
// bytes on a replica waits about N divided by the rate (less the second a
// pacer lets an idle storage bank), across every copy to it at once; the
// rate is read at each claim, so once it is set to "0" the next pass puts
// without waiting.
func TestPutsArePacedByReplicaRate(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 5, 30000))
	f.archive(2, "scan-b", synthetic(2, 1, 3, 70000))
	f.attach()
	const rate = 64 << 10
	var current atomic.Int64
	current.Store(rate)
	clock := &paceClock{now: time.Unix(1_800_000_000, 0)}
	cfg := f.config()
	cfg.Rate = func(context.Context) (int64, error) { return current.Load(), nil }
	cfg.seams = seams{now: clock.Now, sleep: clock.sleep}
	w := f.worker(cfg)
	p := f.runOnce(w)
	if p.InStep != 2 || p.Bytes < 250000 {
		t.Fatalf("the pass is %+v, want both scans in step", p)
	}
	low := time.Duration(float64(p.Bytes)/rate*float64(time.Second)) - pace.MaxBurst - time.Millisecond
	high := time.Duration(float64(p.Bytes)/rate*float64(time.Second)) + 10*time.Millisecond
	if waited := clock.total(); waited < low || waited > high {
		t.Errorf("putting %d bytes at %d bytes a second waited %v, want %v to %v", p.Bytes, rate, waited, low, high)
	}
	f.wantSameTrees()

	current.Store(0)
	before := clock.total()
	f.archive(3, "scan-c", synthetic(3, 1, 4, 50000))
	if p := f.runOnce(w); p.InStep != 1 || p.Bytes < 200000 {
		t.Fatalf("the pass after is %+v, want the new scan in step", p)
	}
	if waited := clock.total() - before; waited != 0 {
		t.Errorf("with replica.rate 0 the pass waited %v; want no wait", waited)
	}
}
