package events

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// waitLimit bounds every wait of these tests for something the code is to
// do: only a failure waits this long, so it is set for a machine many times
// slower than any it runs on (thousands of watches under the race detector
// on one processor), never as a guess at how long the code takes.
const waitLimit = 2 * time.Minute

// newTest returns a Broadcaster on a fake clock, closed when t ends.
func newTest(t testing.TB, cfg Config) (*Broadcaster, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg.Clock = clock
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, clock
}

// stream is one watch run in its own goroutine, its messages collected.
type stream struct {
	msgs chan Message
	done chan error
	stop context.CancelFunc
	// cursors is every tenant's cursor as a client keeps it: a full
	// keep-alive replaces it, a later one is applied to it.
	cursors map[string]string
}

// start runs Watch (or WatchAll, when all is not nil) with a send that puts
// each message on msgs.
func start(t testing.TB, b *Broadcaster, w Watch, all *WatchAll) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{msgs: make(chan Message, 1<<16), done: make(chan error, 1), stop: cancel}
	send := func(ctx context.Context, m Message) error {
		select {
		case s.msgs <- m:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() {
		if all != nil {
			s.done <- b.WatchAll(ctx, *all, send)
		} else {
			s.done <- b.Watch(ctx, w, send)
		}
	}()
	t.Cleanup(cancel)
	return s
}

// next returns the stream's next message, failing the test after waitLimit.
func (s *stream) next(t testing.TB) Message {
	t.Helper()
	select {
	case m := <-s.msgs:
		return m
	case err := <-s.done:
		t.Fatalf("the stream ended: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("no message came")
	}
	return Message{}
}

// empty fails the test if a message has come. It is called once the watch
// is idle (Broadcaster.idle): every message it was going to send for what
// happened before is sent by then.
func (s *stream) empty(t testing.TB) {
	t.Helper()
	select {
	case m := <-s.msgs:
		t.Fatalf("a message came: %+v", m)
	default:
	}
}

// until polls cond, which reads state the code records, until it holds,
// failing the test after waitLimit. It waits for something the code does,
// never for a length of time.
func until(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		// A pause between looks, so that looking (which takes the
		// Broadcaster's lock) does not slow what it waits for.
		time.Sleep(time.Millisecond)
	}
}

// idleNow reports whether the tenant's watches (every-tenant streams for
// the empty name), at least one, all wait for a wake with none pending.
func (b *Broadcaster) idleNow(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.byTenant[name]
	if name == "" {
		list = b.all
	}
	for _, w := range list {
		if !w.waiting || w.signals != w.take {
			return false
		}
	}
	return len(list) > 0
}

// idle waits until the tenant's watches are idle: each has handled every
// wake given it, sent what that called for, and waits. Only then does a
// test move the clock or check that nothing came, so the watch's next round
// is certain to see what the test did next and nothing the test did is
// missed by a round already under way.
func (b *Broadcaster) idle(t testing.TB, name string) {
	t.Helper()
	until(t, "the watches of "+strconv.Quote(name)+" becoming idle", func() bool { return b.idleNow(name) })
}

// tick ticks the fake clock and waits until the Broadcaster has handled the
// tick: every watch has been woken by it.
func tick(t testing.TB, b *Broadcaster, clock *fakeClock) {
	t.Helper()
	b.mu.Lock()
	before := b.ticks
	b.mu.Unlock()
	clock.Tick()
	until(t, "the tick being handled", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.ticks > before
	})
}

// keepAlive returns the next message, failing unless it is a keep-alive.
func (s *stream) keepAlive(t testing.TB) *KeepAlive {
	t.Helper()
	m := s.next(t)
	if m.KeepAlive == nil {
		t.Fatalf("got %+v, want a keep-alive", m)
	}
	k := m.KeepAlive
	if k.Full || s.cursors == nil {
		s.cursors = map[string]string{}
	}
	maps.Copy(s.cursors, k.TenantCursors)
	return k
}

// eventIDs reads n events and returns their ids, failing on a keep-alive.
func (s *stream) eventIDs(t testing.TB, n int) []int64 {
	t.Helper()
	out := make([]int64, 0, n)
	for len(out) < n {
		m := s.next(t)
		if m.Event == nil {
			continue // a keep-alive between events is allowed
		}
		id, err := strconv.ParseInt(m.Event.Cursor, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// settleGoroutines waits until the number of goroutines is at most want.
func settleGoroutines(t testing.TB, want int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines remain, want at most %d:\n%s", runtime.NumGoroutine(), want, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStalledWatchHoldsNoEvents guards that a watch that stops reading
// costs no memory however much is written: while its send blocks, 50,000
// events are written and woken one by one, and the heap does not grow with
// them; when it reads again it gets every event, in order, exactly once.
// A Broadcaster that queued events per watch would grow by every event.
func TestStalledWatchHoldsNoEvents(t *testing.T) {
	b, clock := newTest(t, Config{})
	feed := &fakeFeed{}
	release := make(chan struct{})
	got := make(chan int64, 1<<17)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalled := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	go func() {
		done <- b.Watch(ctx, Watch{Tenant: "lab", Reader: feed, Cursor: "0"}, func(ctx context.Context, m Message) error {
			if m.Event == nil {
				return nil
			}
			once.Do(func() {
				close(stalled)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			id, _ := strconv.ParseInt(m.Event.Cursor, 10, 64)
			got <- id
			return nil
		})
	}()
	feed.write(1)
	b.Wake("lab")
	<-stalled
	heap := func() uint64 {
		var m runtime.MemStats
		for range 3 {
			runtime.GC()
		}
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	const n = 50000
	for i := range n {
		feed.write(1)
		b.Wake("lab")
		if i%1000 == 0 {
			tick(t, b, clock)
		}
	}
	after := heap()
	growth := int64(after) - int64(before) //nolint:gosec // G115: a test's heap is far below 2^63 bytes.
	t.Logf("the heap grew by %d bytes while %d events were written to a stalled watch (from %d to %d)", growth, n, before, after)
	if growth > 256<<10 {
		t.Errorf("the heap grew by %d bytes for %d events written to a stalled watch: events are held per watch", growth, n)
	}
	close(release)
	want := int64(1)
	deadline := time.After(waitLimit)
	for want <= n+1 {
		select {
		case id := <-got:
			if id != want {
				t.Fatalf("the watch got event %d, want %d: out of order, twice or missing", id, want)
			}
			want++
		case <-deadline:
			t.Fatalf("the watch got events up to %d of %d", want-1, n+1)
		}
	}
	b.idle(t, "lab")
	select {
	case id := <-got:
		t.Fatalf("the watch got event %d after the last", id)
	default:
	}
}

// TestManyWatches guards watches with different cursors and filters
// against a writer, run under the race detector: 1, 100 and 4000 watches
// (the detector allows 8128 goroutines), and 10,000 without it. Each gets
// exactly the events after its cursor that its filter keeps, in order,
// once.
func TestManyWatches(t *testing.T) {
	sizes := []int{1, 100, 4000}
	if !raceDetector && !testing.Short() {
		sizes = append(sizes, 10000)
	}
	for _, n := range sizes {
		t.Run(strconv.Itoa(n), func(t *testing.T) { manyWatches(t, n) })
	}
}

// manyWatches runs n watches against one writer and checks what each got.
func manyWatches(t *testing.T, n int) {
	b, clock := newTest(t, Config{PageSize: 50, MaxPerOwner: n, MaxPerTenant: n, MaxTotal: n})
	feed := &fakeFeed{}
	const total = 600
	feed.write(total / 3)
	rng := rand.New(rand.NewPCG(uint64(n), 7)) //nolint:gosec // G404: the test's own choice of cursors, not a secret.
	type watch struct {
		from  int64
		types []tenant.EventType
		got   []int64
		done  chan error
	}
	watches := make([]*watch, n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range watches {
		w := &watch{from: rng.Int64N(total / 3), done: make(chan error, 1)}
		for _, typ := range feedTypes {
			if rng.IntN(2) == 0 {
				w.types = append(w.types, typ)
			}
		}
		watches[i] = w
		go func() {
			// A watch ends itself once its cursor reaches the last event.
			w.done <- b.Watch(ctx, Watch{Tenant: "lab", Owner: "k", Reader: feed, Cursor: strconv.FormatInt(w.from, 10), Types: w.types},
				func(_ context.Context, m Message) error {
					c := ""
					if m.Event != nil {
						id, _ := strconv.ParseInt(m.Event.Cursor, 10, 64)
						w.got = append(w.got, id)
						c = m.Event.Cursor
					} else {
						c = m.KeepAlive.Cursor
					}
					if c == strconv.Itoa(total) {
						return errReachedEnd
					}
					return nil
				})
		}()
	}
	// Every watch is open before the writer starts, so none opens late,
	// after the clock has moved, and needs a keep-alive interval more.
	until(t, "every watch opening", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.byTenant["lab"]) == n
	})
	for written := total / 3; written < total; written += 20 {
		feed.write(20)
		b.Wake("lab")
	}
	// Every watch that has not seen the last event (its filter left it
	// out) learns it reached the end from a keep-alive: once every watch
	// still open is idle, so none is still reading or recording when it
	// last sent, the clock moves past the keep-alive interval and ticks.
	finished := 0
	collect := func() {
		for _, w := range watches {
			if w.done == nil {
				continue
			}
			select {
			case err := <-w.done:
				if !errors.Is(err, errReachedEnd) {
					t.Fatalf("a watch ended with %v", err)
				}
				w.done = nil
				finished++
			default:
			}
		}
	}
	for round := 0; finished < n; round++ {
		if round == 3 {
			t.Fatalf("%d of %d watches reached the end after %d keep-alive intervals", finished, n, round)
		}
		// A watch leaves the Broadcaster before its goroutine hands back
		// its answer, so every watch no longer open must have answered
		// before the rest are known to be all there is.
		settled := func() bool {
			collect()
			b.mu.Lock()
			open := len(b.byTenant["lab"])
			b.mu.Unlock()
			return finished == n-open && (open == 0 || b.idleNow("lab"))
		}
		until(t, "every watch answering or waiting", settled)
		if finished == n {
			break
		}
		clock.Advance(DefaultKeepAlive)
		tick(t, b, clock)
		until(t, "every watch reaching the end or waiting again", settled)
	}
	for i, w := range watches {
		var want []int64
		for id := w.from + 1; id <= total; id++ {
			if keeps(w.types, typeOf(id)) {
				want = append(want, id)
			}
		}
		if fmt.Sprint(w.got) != fmt.Sprint(want) {
			t.Fatalf("watch %d from %d with %v got %d events, want %d (first %v)", i, w.from, w.types, len(w.got), len(want), w.got[:min(5, len(w.got))])
		}
	}
	t.Logf("%d watches, %d reads of the feed", n, feed.reads.Load())
}

// errReachedEnd ends a test's watch when it has read everything.
var errReachedEnd = errors.New("the watch reached the end")

// TestTimerCoversMissedWake guards the timer: an event written with no wake
// is not read until the timer ticks, and then is.
func TestTimerCoversMissedWake(t *testing.T) {
	b, clock := newTest(t, Config{})
	feed := &fakeFeed{}
	s := start(t, b, Watch{Tenant: "lab", Reader: feed}, nil)
	s.keepAlive(t)
	feed.write(1) // written by another process: no wake
	b.idle(t, "lab")
	s.empty(t)
	tick(t, b, clock)
	if ids := s.eventIDs(t, 1); ids[0] != 1 {
		t.Errorf("after a tick the watch got event %d, want 1", ids[0])
	}
}

// TestWakeReachesOnlyItsTenant guards Wake: it wakes the watches of the
// tenant named, and an event of another tenant waits for its own wake or
// the timer.
func TestWakeReachesOnlyItsTenant(t *testing.T) {
	b, _ := newTest(t, Config{})
	a, other := &fakeFeed{}, &fakeFeed{}
	sa := start(t, b, Watch{Tenant: "a", Reader: a}, nil)
	so := start(t, b, Watch{Tenant: "b", Reader: other}, nil)
	sa.keepAlive(t)
	so.keepAlive(t)
	a.write(1)
	other.write(1)
	b.Wake("a")
	sa.eventIDs(t, 1)
	b.idle(t, "b")
	so.empty(t)
	b.Wake("b")
	so.eventIDs(t, 1)
}

// TestKeepAlive guards the keep-alive: a stream opens with one carrying its
// starting cursor; an idle stream gets another once the interval passes
// with nothing sent, and not before; its cursor, past events the filter
// left out, resumes a new stream exactly after what was read. Each names
// the feed's history its cursor belongs to.
func TestKeepAlive(t *testing.T) {
	b, clock := newTest(t, Config{})
	feed := &fakeFeed{history: "h1"}
	feed.write(4)
	s := start(t, b, Watch{Tenant: "lab", Reader: feed, Types: []tenant.EventType{tenant.EventReceiverHealth}}, nil)
	if k := s.keepAlive(t); k.Cursor != "4" || k.History != "h1" {
		t.Errorf("the opening keep-alive's cursor and history = %q, %q, want 4 (from now) and h1", k.Cursor, k.History)
	}
	feed.write(4) // 5 to 8: 5 and 8 are health events
	b.Wake("lab")
	if ids := s.eventIDs(t, 2); fmt.Sprint(ids) != "[5 8]" {
		t.Errorf("the filtered watch got events %v, want [5 8]", ids)
	}
	feed.write(2) // 9 and 10: no health event
	b.Wake("lab")
	b.idle(t, "lab")
	clock.Advance(DefaultKeepAlive - time.Second)
	tick(t, b, clock)
	b.idle(t, "lab")
	s.empty(t)
	clock.Advance(time.Second)
	tick(t, b, clock)
	k := s.keepAlive(t)
	if k.Cursor != "10" || k.History != "h1" || !k.Time.Equal(clock.Now()) {
		t.Errorf("the idle keep-alive = %+v, want cursor 10, past what the filter left out, of history h1, at %v", k, clock.Now())
	}
	s.stop()
	if err := <-s.done; !errors.Is(err, context.Canceled) {
		t.Errorf("a stream stopped by its caller ended with %v", err)
	}
	feed.write(2)
	r := start(t, b, Watch{Tenant: "lab", Reader: feed, Cursor: k.Cursor}, nil)
	if k := r.keepAlive(t); k.Cursor != "10" {
		t.Errorf("the resumed stream opens at %q, want 10", k.Cursor)
	}
	if ids := r.eventIDs(t, 2); fmt.Sprint(ids) != "[11 12]" {
		t.Errorf("the resumed stream got %v, want [11 12]", ids)
	}
}

// TestCursorExpired guards a watch whose cursor is older than what the
// tenant keeps: it ends with tenant.ErrCursorExpired before it opens, and
// a watch whose cursor pruning passes while it waits ends with it too.
func TestCursorExpired(t *testing.T) {
	b, clock := newTest(t, Config{})
	feed := &fakeFeed{}
	feed.write(10)
	feed.pruned.Store(5)
	err := b.Watch(context.Background(), Watch{Tenant: "lab", Reader: feed, Cursor: "3"}, func(context.Context, Message) error { return nil })
	if !errors.Is(err, tenant.ErrCursorExpired) {
		t.Errorf("a watch from a pruned cursor = %v, want ErrCursorExpired", err)
	}
	s := start(t, b, Watch{Tenant: "lab", Reader: feed, Cursor: "10"}, nil)
	s.keepAlive(t)
	feed.pruned.Store(20)
	feed.write(20)
	tick(t, b, clock)
	select {
	case err := <-s.done:
		if !errors.Is(err, tenant.ErrCursorExpired) {
			t.Errorf("a watch that pruning passed ended with %v, want ErrCursorExpired", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("a watch that pruning passed did not end")
	}
}

// TestBounds guards the bounds of open watches: per owner, per tenant and
// in all, each refused with ErrTooManyWatches, and a place freed when a
// watch ends.
func TestBounds(t *testing.T) {
	b, _ := newTest(t, Config{MaxPerOwner: 2, MaxPerTenant: 3, MaxTotal: 4})
	feed := &fakeFeed{}
	refused := func(w Watch) error {
		return b.Watch(context.Background(), w, func(context.Context, Message) error { return nil })
	}
	s1 := start(t, b, Watch{Tenant: "a", Owner: "k1", Reader: feed}, nil)
	s1.keepAlive(t)
	start(t, b, Watch{Tenant: "a", Owner: "k1", Reader: feed}, nil).keepAlive(t)
	if err := refused(Watch{Tenant: "b", Owner: "k1", Reader: feed}); !errors.Is(err, ErrTooManyWatches) || fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a third watch of one owner = %v, want ErrTooManyWatches", err)
	}
	start(t, b, Watch{Tenant: "a", Owner: "k2", Reader: feed}, nil).keepAlive(t)
	if err := refused(Watch{Tenant: "a", Owner: "k3", Reader: feed}); !errors.Is(err, ErrTooManyWatches) {
		t.Errorf("a fourth watch of one tenant = %v, want ErrTooManyWatches", err)
	}
	start(t, b, Watch{Tenant: "b", Owner: "k3", Reader: feed}, nil).keepAlive(t)
	if err := refused(Watch{Tenant: "c", Owner: "k4", Reader: feed}); !errors.Is(err, ErrTooManyWatches) {
		t.Errorf("a fifth watch in all = %v, want ErrTooManyWatches", err)
	}
	s1.stop()
	if err := <-s1.done; !errors.Is(err, context.Canceled) {
		t.Errorf("a stream stopped by its caller ended with %v", err)
	}
	start(t, b, Watch{Tenant: "c", Owner: "k1", Reader: feed}, nil).keepAlive(t)
}

// TestCancelLeavesNoGoroutine guards that a watch ended at any point ends
// its goroutine and leaves none behind: its context ended while it waits,
// while its send blocks, while a read blocks, before it opens, and Close
// with watches in each of those states, which returns once they ended.
func TestCancelLeavesNoGoroutine(t *testing.T) {
	for _, point := range []string{"waiting", "sending", "reading", "opening"} {
		t.Run(point, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			b, err := New(Config{Clock: newFakeClock()})
			if err != nil {
				t.Fatal(err)
			}
			feed := &fakeFeed{blocked: make(chan struct{}, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			reached := make(chan struct{}, 1)
			send := func(ctx context.Context, m Message) error {
				select {
				case reached <- struct{}{}:
				default:
				}
				if point == "sending" && m.Event != nil {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			if point == "opening" {
				feed.block.Store(true)
			}
			done := make(chan error, 1)
			go func() { done <- b.Watch(ctx, Watch{Tenant: "lab", Reader: feed}, send) }()
			if point != "opening" {
				<-reached // the opening keep-alive
			}
			switch point {
			case "waiting":
				b.idle(t, "lab")
			case "opening":
				<-feed.blocked
			case "sending":
				feed.write(1)
				b.Wake("lab")
				<-reached
			case "reading":
				feed.block.Store(true)
				b.Wake("lab")
				<-feed.blocked
			}
			cancel()
			select {
			case err := <-done:
				t.Logf("ended with: %v", err)
			case <-time.After(waitLimit):
				t.Fatal("the watch did not end when its context did")
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			settleGoroutines(t, baseline)
		})
	}
	t.Run("close", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		b, err := New(Config{Clock: newFakeClock()})
		if err != nil {
			t.Fatal(err)
		}
		// One watch waiting, one whose send blocks, one whose read blocks,
		// and a stream of every tenant waiting: each is known to be in its
		// state before Close.
		idle, stuck, blocked := &fakeFeed{}, &fakeFeed{}, &fakeFeed{blocked: make(chan struct{}, 1)}
		stuck.write(1)
		sending := make(chan struct{}, 1)
		var ends []chan error
		for _, w := range []struct {
			tenant string
			feed   *fakeFeed
			send   Send
		}{
			{"lab-idle", idle, func(context.Context, Message) error { return nil }},
			{"lab-stuck", stuck, func(ctx context.Context, m Message) error {
				if m.Event != nil {
					sending <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}},
			{"lab-blocked", blocked, func(context.Context, Message) error { blocked.block.Store(true); return nil }},
		} {
			done := make(chan error, 1)
			ends = append(ends, done)
			go func() {
				done <- b.Watch(context.Background(), Watch{Tenant: w.tenant, Reader: w.feed, Cursor: "0"}, w.send)
			}()
		}
		every := make(chan error, 1)
		ts := &fakeTenants{}
		ts.set("a", &fakeFeed{})
		go func() {
			every <- b.WatchAll(context.Background(), WatchAll{Tenants: ts}, func(context.Context, Message) error { return nil })
		}()
		ends = append(ends, every)
		b.idle(t, "lab-idle")
		<-sending
		b.idle(t, "lab-blocked")
		b.Wake("lab-blocked")
		<-blocked.blocked
		b.idle(t, "")
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		for _, done := range ends {
			select {
			case err := <-done:
				if !errors.Is(err, ErrClosed) {
					t.Errorf("a watch ended by Close returned %v, want ErrClosed", err)
				}
			case <-time.After(waitLimit):
				// Close returns once every watch has unregistered; the
				// goroutine that called Watch then has only its return
				// left, so its answer follows at once.
				t.Error("a watch did not return after Close")
			}
		}
		if err := b.Watch(context.Background(), Watch{Tenant: "lab", Reader: idle}, func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
			t.Errorf("a watch after Close = %v, want ErrClosed", err)
		}
		settleGoroutines(t, baseline)
	})
}

// TestWatchAll guards the stream of every tenant: it opens with a
// keep-alive of the tenants' cursors (from the cursor given, or from now);
// each tenant's events come in order; a tenant whose database does not
// answer is skipped while the others go on, named in keep-alives, tried
// again after its wait and followed from then on; a tenant created joins
// from its first event; a tenant missing from a listing keeps its cursor; a
// tenant's expired cursor ends the stream naming it.
func TestWatchAll(t *testing.T) {
	b, clock := newTest(t, Config{})
	a, down := &fakeFeed{}, &fakeFeed{}
	a.write(5)
	down.write(3)
	down.down.Store(true)
	ts := &fakeTenants{}
	ts.set("a", a)
	ts.set("b", down)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "2"}})
	if k := s.keepAlive(t); fmt.Sprint(k.TenantCursors) != "map[a:2]" {
		t.Errorf("the opening keep-alive = %v, want a at 2 and b not read", k.TenantCursors)
	}
	if ids := s.eventIDs(t, 3); fmt.Sprint(ids) != "[3 4 5]" {
		t.Errorf("tenant a gave %v, want [3 4 5]", ids)
	}
	a.write(1)
	b.Wake("a")
	if m := s.next(t); m.Tenant != "a" || m.Event == nil || m.Event.Cursor != "6" {
		t.Errorf("after a wake of a: %+v", m)
	}
	// b did not answer when the stream opened, so "from now" is when it
	// first answers: the stream reads its place then, and follows it. A
	// tenant that failed is tried again once its wait (a tick, the first
	// time) has passed, and is named in a keep-alive meanwhile.
	b.idle(t, "")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	if k := s.keepAlive(t); fmt.Sprint(k.Unanswered) != "[b]" {
		t.Errorf("a keep-alive while b does not answer names %v as unanswered, want [b]", k.Unanswered)
	}
	down.down.Store(false)
	b.idle(t, "")
	clock.Advance(2 * DefaultTick) // its wait doubled when it failed again on the last tick
	tick(t, b, clock)
	b.idle(t, "")
	down.write(2)
	b.Wake("b")
	got := map[string][]string{}
	for range 2 {
		m := s.next(t)
		got[m.Tenant] = append(got[m.Tenant], m.Event.Cursor)
	}
	if fmt.Sprint(got) != "map[b:[4 5]]" {
		t.Errorf("tenant b, once it answered, gave %v; want 4 and 5, written after it answered", got)
	}
	created := &fakeFeed{}
	created.write(2)
	ts.set("c", created)
	b.Wake("c")
	if ids := s.eventIDs(t, 2); fmt.Sprint(ids) != "[1 2]" {
		t.Errorf("the tenant created gave %v, want its first events [1 2]", ids)
	}
	// A tenant missing from a listing is paused, its cursor kept.
	b.idle(t, "")
	ts.remove("a")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	// The keep-alive names only the tenants whose place changed since the
	// last one; applied to what the client keeps, a is still at 6.
	if k := s.keepAlive(t); k.Full || fmt.Sprint(k.TenantCursors) != "map[b:5 c:2]" || len(k.Unanswered) != 0 {
		t.Errorf("after a left the listing the keep-alive = %v (full %v, unanswered %v), want b at 5 and c at 2 only", k.TenantCursors, k.Full, k.Unanswered)
	}
	if fmt.Sprint(s.cursors) != "map[a:6 b:5 c:2]" {
		t.Errorf("the cursors a client keeps are %v, want a still at 6, b at 5 and c at 2", s.cursors)
	}
	created.pruned.Store(10)
	created.write(10)
	b.Wake("c")
	select {
	case err := <-s.done:
		if !errors.Is(err, tenant.ErrCursorExpired) || fault.Message(err) == "" || !strings.Contains(fault.Message(err), "tenant c") {
			t.Errorf("an expired cursor of c ended the stream with %v, want ErrCursorExpired naming c", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("an expired cursor did not end the stream")
	}
}

// TestConfigRefused guards New: a page size over the port's bound and a
// negative bound are Invalid.
func TestConfigRefused(t *testing.T) {
	for _, cfg := range []Config{{PageSize: tenant.MaxEventPage + 1}, {MaxTotal: -1}, {Tick: -time.Second}} {
		if _, err := New(cfg); fault.KindOf(err) != fault.Invalid {
			t.Errorf("New(%+v) = %v, want Invalid", cfg, err)
		}
	}
}

// BenchmarkWake measures one Wake of a tenant with 1, 100 and 10,000
// watches waiting: what a writer pays after each commit that wrote an
// event.
func BenchmarkWake(b *testing.B) {
	for _, n := range []int{1, 100, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			br, _ := newTest(b, Config{MaxPerOwner: n, MaxPerTenant: n, MaxTotal: n})
			watchers := make([]*watcher, n)
			for i := range watchers {
				w, err := br.register("lab", "k")
				if err != nil {
					b.Fatal(err)
				}
				watchers[i] = w
			}
			b.Cleanup(func() {
				for _, w := range watchers {
					br.unregister(w)
				}
			})
			b.ReportAllocs()
			for b.Loop() {
				br.Wake("lab")
				// Each watch takes its wake, as a waiting watch does, so
				// every Wake fills every slot again.
				for _, w := range watchers {
					<-w.wake
				}
			}
		})
	}
}
