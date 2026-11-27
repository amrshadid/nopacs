package download

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// fakeClock is a clock whose sleeps move its time instead of waiting, and
// are recorded.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (f *fakeClock) clock() clock {
	return clock{
		now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t },
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if d > 0 {
				f.t = f.t.Add(d)
				f.slept = append(f.slept, d)
			}
			return nil
		},
	}
}

// advance moves the clock without a sleep.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// TestLimiterPacesTheBytesOfAllStreams guards the bandwidth bound: chunks
// from several callers are let through no faster than the rate in all, the
// first at once, with no burst saved up by a pause.
func TestLimiterPacesTheBytesOfAllStreams(t *testing.T) {
	fc := newFakeClock()
	l := newLimiter(1000, fc.clock())
	start := fc.clock().now()
	// Four chunks of 500 bytes from two "streams" take 2 seconds in all at
	// 1000 bytes per second, the first free.
	for i := 0; i < 4; i++ {
		if err := l.wait(context.Background(), 500); err != nil {
			t.Fatal(err)
		}
	}
	if got := fc.clock().now().Sub(start); got != 1500*time.Millisecond {
		t.Errorf("four chunks of 500 bytes at 1000 B/s took %s of waiting, want 1.5s (the last chunk is let through when the three before it would have finished)", got)
	}
	// A pause does not bank time: after 10 idle seconds the next chunks are
	// paced again, not let through in a burst.
	fc.advance(10 * time.Second)
	before := len(fc.slept)
	for i := 0; i < 3; i++ {
		_ = l.wait(context.Background(), 1000)
	}
	var waited time.Duration
	for _, d := range fc.slept[before:] {
		waited += d
	}
	if waited != 2*time.Second {
		t.Errorf("three chunks of 1000 bytes after a pause waited %s, want 2s", waited)
	}
	if err := (*limiter)(nil).wait(context.Background(), 1<<30); err != nil {
		t.Errorf("a nil limiter: %v", err)
	}
	if newLimiter(0, fc.clock()) != nil {
		t.Error("a zero rate made a limiter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.wait(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("a wait on an ended context returned %v", err)
	}
}

// TestGateBoundsStreamsAndSettlesAfterRefusals guards the worker bound and
// the answer to the server's refusal: never more places in use than the
// bound, a refusal lowers the bound below the places in use but never below
// one, and a quiet stretch lets it grow one at a time back to the most asked.
func TestGateBoundsStreamsAndSettlesAfterRefusals(t *testing.T) {
	fc := newFakeClock()
	g := newGate(4, fc.clock())
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			active.Add(-1)
			g.release()
		}()
	}
	wg.Wait()
	if peak.Load() > 4 {
		t.Errorf("%d streams were open at once, the bound is 4", peak.Load())
	}

	// Four streams open, the server refuses one: the bound drops to three,
	// and again to two, and never under one.
	for i := 0; i < 4; i++ {
		if err := g.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g.refused()
	if g.bound() != 3 {
		t.Errorf("after one refusal with 4 open the bound is %d, want 3", g.bound())
	}
	for i := 0; i < 3; i++ {
		g.release()
	}
	g.refused()
	g.refused()
	g.refused()
	if g.bound() != 1 {
		t.Errorf("after many refusals the bound is %d, want 1", g.bound())
	}
	g.release()

	// Quiet for the regrow time: one more place, then one more, up to 4.
	for want := 2; want <= 4; want++ {
		fc.advance(regrowAfter)
		if err := g.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		g.release()
		if g.bound() != want {
			t.Errorf("after a quiet stretch the bound is %d, want %d", g.bound(), want)
		}
	}
	fc.advance(10 * regrowAfter)
	_ = g.acquire(context.Background())
	g.release()
	if g.bound() != 4 {
		t.Errorf("the bound grew past what was asked: %d", g.bound())
	}
}

// TestBackoffDoublesToALimitAndHonorsTheServer guards the wait after a
// refusal: it doubles from half a second to the limit, and a Retry-After the
// server named wins.
func TestBackoffDoublesToALimitAndHonorsTheServer(t *testing.T) {
	var got []time.Duration
	for n := 1; n <= 9; n++ {
		got = append(got, backoff(n, 0))
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got[i], want[i])
		}
	}
	if d := backoff(5, 7*time.Second); d != 7*time.Second {
		t.Errorf("a named Retry-After of 7s gave %s", d)
	}
}

// TestRefusalIsRecognizedByKindAndByStatus guards what counts as the server
// asking to come back later: the API's exhausted kind, HTTP 429 and 503 with
// their Retry-After in seconds, and nothing else.
func TestRefusalIsRecognizedByKindAndByStatus(t *testing.T) {
	if _, ok := refusalOf(fault.New(fault.Exhausted, "the caller holds 16 streams")); !ok {
		t.Error("an exhausted error is not a refusal")
	}
	for _, k := range []fault.Kind{fault.Unavailable, fault.NotFound, fault.Internal, fault.Denied} {
		if _, ok := refusalOf(fault.New(k, "x")); ok {
			t.Errorf("a %s error counted as a refusal", k)
		}
	}
	if _, ok := refusalOf(nil); ok {
		t.Error("nil counted as a refusal")
	}
	res := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"3"}}}
	r := refusalOfStatus(res)
	if r == nil || r.after != 3*time.Second {
		t.Fatalf("a 429 with Retry-After 3: %+v", r)
	}
	if got, ok := refusalOf(r); !ok || got.after != 3*time.Second {
		t.Error("a status refusal was not recognized by refusalOf")
	}
	if r := refusalOfStatus(&http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}); r == nil || r.after != 0 {
		t.Errorf("a 503 without Retry-After: %+v", r)
	}
	if r := refusalOfStatus(&http.Response{StatusCode: http.StatusOK, Header: http.Header{}}); r != nil {
		t.Errorf("a 200 counted as a refusal: %+v", r)
	}
	if r := refusalOfStatus(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"9999"}}}); r == nil || r.after != maxBackoff {
		t.Errorf("a huge Retry-After was not bounded: %+v", r)
	}
}

// TestTrackerRateAndTimeLeft guards the progress numbers: the rate is the
// bytes of the last ten seconds over those seconds, and the time left is what
// remains at that rate, unknown when nothing moved.
func TestTrackerRateAndTimeLeft(t *testing.T) {
	var tr tracker
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.add(start, 0)
	if tr.rate() != 0 || remaining(1000, tr.rate()) != -1 {
		t.Errorf("one sample: rate %v, left %v", tr.rate(), remaining(1000, tr.rate()))
	}
	for s := 1; s <= 30; s++ {
		tr.add(start.Add(time.Duration(s)*time.Second), int64(s)*100)
	}
	if got := tr.rate(); got < 99 || got > 101 {
		t.Errorf("a steady 100 B/s measured as %v", got)
	}
	// The speed doubles: within a window the rate follows it.
	for s := 31; s <= 50; s++ {
		tr.add(start.Add(time.Duration(s)*time.Second), 3000+int64(s-30)*200)
	}
	if got := tr.rate(); got < 195 || got > 205 {
		t.Errorf("after the speed doubled the rate is %v, want about 200", got)
	}
	if got := remaining(2000, 200); got != 10*time.Second {
		t.Errorf("2000 bytes at 200 B/s: %s left, want 10s", got)
	}
	if remaining(0, 0) != 0 || remaining(-5, 100) != 0 {
		t.Error("nothing left must be zero time")
	}
}
