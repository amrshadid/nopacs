package events

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fakeClock is a Clock the test moves: Now changes only with Advance, and
// its tickers tick only with Tick.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
	timers  []*fakeTimer
}

// newFakeClock returns a clock at a fixed time.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
}

// Now implements Clock.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTicker implements Clock.
func (c *fakeClock) NewTicker(time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{c: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	return t
}

// Advance moves the time on by d, and calls, each in its own goroutine as
// the system's clock does, every AfterFunc whose time has come.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			if t.stopped.CompareAndSwap(false, true) {
				go t.f()
			}
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// fakeTimer is an AfterFunc of fakeClock.
type fakeTimer struct {
	at      time.Time
	f       func()
	stopped atomic.Bool
}

// AfterFunc implements Clock.
func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool { return t.stopped.CompareAndSwap(false, true) }
}

// Tick ticks every ticker not stopped, as the timer would.
func (c *fakeClock) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tickers {
		if !t.stopped.Load() {
			select {
			case t.c <- c.now:
			default:
			}
		}
	}
}

// fakeTicker is a ticker of fakeClock.
type fakeTicker struct {
	c       chan time.Time
	stopped atomic.Bool
}

// C implements Ticker.
func (t *fakeTicker) C() <-chan time.Time { return t.c }

// Stop implements Ticker.
func (t *fakeTicker) Stop() { t.stopped.Store(true) }

// feedTypes are the types of a fakeFeed's events, by id: event i is of
// type feedTypes[i % 3].
var feedTypes = []tenant.EventType{tenant.EventReceptionStep, tenant.EventFileQuarantined, tenant.EventReceiverHealth}

// fakeFeed is one tenant's feed that holds nothing: event i, from 1 to the
// count written, is made when it is read, so writing costs no memory and
// the heap a test measures is the Broadcaster's alone. A cursor is the id
// in decimal; the feed follows tenant.Events's rules for cursors and
// pruning.
type fakeFeed struct {
	written atomic.Int64
	pruned  atomic.Int64
	reads   atomic.Int64
	down    atomic.Bool // its database does not answer
	// block, when set, makes a read wait until ctx ends; each read that
	// waits says so on blocked.
	block   atomic.Bool
	blocked chan struct{}
	// holding, when set, makes each read take its page, say so on blocked
	// and wait for a value on hold before it answers, as a read that is slow
	// to come back does: what is written meanwhile is not in its page.
	holding atomic.Bool
	hold    chan struct{}
	// recreated, when set, makes it a tenant created again under its name:
	// any cursor but the empty one is another tenant's.
	recreated atomic.Bool
	// history is the history id each page names, set before a watch reads
	// the feed.
	history string
}

// write adds n events.
func (f *fakeFeed) write(n int) { f.written.Add(int64(n)) }

// typeOf returns the type of event i.
func typeOf(i int64) tenant.EventType { return feedTypes[i%int64(len(feedTypes))] }

// keeps reports whether a filter keeps type t.
func keeps(types []tenant.EventType, t tenant.EventType) bool {
	if len(types) == 0 {
		return true
	}
	for _, k := range types {
		if k == t {
			return true
		}
	}
	return false
}

// EventCursorHistory implements Reader: every cursor but the empty one is
// of the feed's history.
func (f *fakeFeed) EventCursorHistory(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	return f.history, nil
}

// ReadEvents implements Reader.
func (f *fakeFeed) ReadEvents(ctx context.Context, after string, types []tenant.EventType, limit int) (tenant.EventPage, error) {
	f.reads.Add(1)
	if f.block.Load() {
		if f.blocked != nil {
			select {
			case f.blocked <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return tenant.EventPage{}, fault.Wrap(fault.Unavailable, ctx.Err(), "the read ended")
	}
	if f.down.Load() {
		return tenant.EventPage{}, fault.New(fault.Unavailable, "the tenant database does not answer")
	}
	if after != "" && f.recreated.Load() {
		return tenant.EventPage{}, tenant.ForeignCursor()
	}
	high, pruned := f.written.Load(), f.pruned.Load()
	pos := pruned
	if after != "" {
		v, err := strconv.ParseInt(after, 10, 64)
		if err != nil {
			return tenant.EventPage{}, fault.New(fault.Invalid, "not a cursor of this feed")
		}
		if v < pruned {
			return tenant.EventPage{}, tenant.CursorExpired()
		}
		pos = v
	}
	var out []tenant.Event
	next := max(pos, high)
	for i := pos + 1; i <= high; i++ {
		if !keeps(types, typeOf(i)) {
			continue
		}
		out = append(out, tenant.Event{Cursor: strconv.FormatInt(i, 10), Type: typeOf(i), Time: time.Unix(i, 0)})
		if len(out) == limit {
			next = i
			break
		}
	}
	if f.holding.Load() {
		f.blocked <- struct{}{}
		<-f.hold
	}
	return tenant.EventPage{Events: out, Cursor: strconv.FormatInt(next, 10), History: f.history}, nil
}

// EventCursor implements Reader.
func (f *fakeFeed) EventCursor(context.Context) (string, error) {
	if f.down.Load() {
		return "", fault.New(fault.Unavailable, "the tenant database does not answer")
	}
	return strconv.FormatInt(max(f.written.Load(), f.pruned.Load()), 10), nil
}

// fakeTenants is a list of tenants, each with its fake feed.
type fakeTenants struct {
	mu    sync.Mutex
	feeds map[string]*fakeFeed
}

// set adds or replaces a tenant.
func (t *fakeTenants) set(name string, f *fakeFeed) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.feeds == nil {
		t.feeds = map[string]*fakeFeed{}
	}
	t.feeds[name] = f
}

// remove removes a tenant.
func (t *fakeTenants) remove(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.feeds, name)
}

// TenantNames implements Tenants.
func (t *fakeTenants) TenantNames(context.Context) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for n := range t.feeds {
		out = append(out, n)
	}
	return out, nil
}

// Reader implements Tenants.
func (t *fakeTenants) Reader(_ context.Context, name string) (Reader, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.feeds[name]
	if !ok {
		return nil, fault.New(fault.NotFound, "no such tenant")
	}
	if f.down.Load() {
		return nil, fault.New(fault.Unavailable, "the tenant database does not answer")
	}
	return f, nil
}
