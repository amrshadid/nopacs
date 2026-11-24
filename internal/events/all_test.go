package events

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// TestWatchAllGoesOnPastATenantThatHangs guards that one tenant whose read
// does not return holds no other tenant: with several readers the others'
// events come while it hangs; with one reader they come once its read's
// deadline passes, and the tenant is named in the keep-alives while it is
// skipped.
func TestWatchAllGoesOnPastATenantThatHangs(t *testing.T) {
	for _, readers := range []int{1, DefaultTenantReaders} {
		t.Run(strconv.Itoa(readers), func(t *testing.T) {
			b, clock := newTest(t, Config{TenantReaders: readers, TenantReadTimeout: time.Second})
			hung, z := &fakeFeed{blocked: make(chan struct{}, 1)}, &fakeFeed{}
			hung.block.Store(true)
			ts := &fakeTenants{}
			ts.set("a", hung)
			ts.set("z", z)
			s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "0", "z": "0"}})
			s.keepAlive(t)
			<-hung.blocked // the first round reads a, which hangs
			z.write(3)
			b.Wake("z")
			if readers == 1 {
				clock.Advance(time.Second) // a's read passes its deadline
			}
			if ids := s.eventIDs(t, 3); fmt.Sprint(ids) != "[1 2 3]" {
				t.Errorf("tenant z gave %v while a hung, want [1 2 3]", ids)
			}
			clock.Advance(time.Second)
			b.idle(t, "")
			clock.Advance(DefaultKeepAlive)
			tick(t, b, clock)
			if k := s.keepAlive(t); fmt.Sprint(k.Unanswered) != "[a]" {
				t.Errorf("a keep-alive while a hangs names %v as unanswered, want [a]", k.Unanswered)
			}
		})
	}
}

// TestWatchAllKeepsACursorWhileATenantIsUnlisted guards that a tenant
// missing from one listing and listed again continues from its cursor: its
// events are not delivered a second time.
func TestWatchAllKeepsACursorWhileATenantIsUnlisted(t *testing.T) {
	b, clock := newTest(t, Config{})
	a := &fakeFeed{}
	a.write(3)
	ts := &fakeTenants{}
	ts.set("a", a)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "0"}})
	s.keepAlive(t)
	if ids := s.eventIDs(t, 3); fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("got %v", ids)
	}
	ts.remove("a")
	b.idle(t, "")
	tick(t, b, clock)
	b.idle(t, "")
	ts.set("a", a)
	a.write(1)
	tick(t, b, clock)
	if ids := s.eventIDs(t, 1); fmt.Sprint(ids) != "[4]" {
		t.Errorf("tenant a, listed again, gave %v, want only [4]", ids)
	}
	b.idle(t, "")
	s.empty(t)
}

// TestWatchAllStartsATenantCreatedAgainFromItsFirstEvent guards that a
// tenant created again under a name the stream follows, whose feed answers
// the old cursor as another tenant's, is followed from its first event.
func TestWatchAllStartsATenantCreatedAgainFromItsFirstEvent(t *testing.T) {
	b, clock := newTest(t, Config{})
	old := &fakeFeed{}
	old.write(3)
	ts := &fakeTenants{}
	ts.set("a", old)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "0"}})
	s.keepAlive(t)
	s.eventIDs(t, 3)
	again := &fakeFeed{}
	again.recreated.Store(true)
	again.write(2)
	b.idle(t, "")
	ts.set("a", again)
	tick(t, b, clock)
	if ids := s.eventIDs(t, 2); fmt.Sprint(ids) != "[1 2]" {
		t.Errorf("the tenant created again gave %v, want its first events [1 2]", ids)
	}
}

// TestWatchAllResumeStartsNewTenantsFromTheirFirstEvent guards resuming a
// stream of every tenant from a keep-alive: every tenant the stream follows
// is named, one with no event yet included; a tenant created after that
// keep-alive, whose events the stream had not read when it ended, is new to
// the client and starts from its first event when the stream is resumed;
// without Resume it would start from now.
func TestWatchAllResumeStartsNewTenantsFromTheirFirstEvent(t *testing.T) {
	b, _ := newTest(t, Config{})
	a := &fakeFeed{}
	ts := &fakeTenants{}
	ts.set("a", a)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts})
	k := s.keepAlive(t)
	if fmt.Sprint(k.TenantCursors) != "map[a:0]" {
		t.Errorf("the opening keep-alive names %v, want a with no event yet", k.TenantCursors)
	}
	c := &fakeFeed{}
	ts.set("c", c)
	c.write(4)
	s.stop()
	if err := <-s.done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	fresh := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: k.TenantCursors})
	fresh.keepAlive(t)
	b.idle(t, "")
	fresh.empty(t)
	fresh.stop()
	if err := <-fresh.done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	resumed := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: k.TenantCursors, Resume: true})
	if k := resumed.keepAlive(t); fmt.Sprint(k.TenantCursors) != "map[a:0 c:]" {
		t.Errorf("the resumed stream opens with %v, want a at 0 and c from its first event", k.TenantCursors)
	}
	if ids := resumed.eventIDs(t, 4); fmt.Sprint(ids) != "[1 2 3 4]" {
		t.Errorf("the tenant created after the keep-alive gave %v on resume, want [1 2 3 4]", ids)
	}
}

// TestWatchAllReadsAFewPagesOfATenantAtATime guards that tenants are read
// in turn, a page at a time: a tenant created while another delivers a long
// backlog is read within a few pages, not after the whole backlog.
func TestWatchAllReadsAFewPagesOfATenantAtATime(t *testing.T) {
	b, _ := newTest(t, Config{TenantReaders: 1, PageSize: 10})
	busy := &fakeFeed{}
	busy.write(1000)
	ts := &fakeTenants{}
	ts.set("a", busy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paused, release := make(chan struct{}), make(chan struct{})
	var order []string
	done := make(chan error, 1)
	go func() {
		done <- b.WatchAll(ctx, WatchAll{Tenants: ts, Cursors: map[string]string{"a": "0"}}, func(_ context.Context, m Message) error {
			if m.Event == nil {
				return nil
			}
			order = append(order, m.Tenant)
			if len(order) == 5 {
				close(paused)
				<-release
			}
			if m.Tenant == "c" {
				return errReachedEnd
			}
			return nil
		})
	}()
	<-paused
	created := &fakeFeed{}
	created.write(1)
	ts.set("c", created)
	b.Wake("c")
	close(release)
	if err := <-done; !errors.Is(err, errReachedEnd) {
		t.Fatal(err)
	}
	// The page being delivered when c was woken, the one a reader may
	// already hold, and one taken before the wake: at most three pages.
	if before := len(order) - 1; before > 3*10 {
		t.Errorf("the tenant created came after %d events of the other, want at most %d", before, 3*10)
	}
}

// TestWatchAllRefusesTooManyTenants guards the bound of tenants one stream
// follows, which keeps its keep-alive within one message.
func TestWatchAllRefusesTooManyTenants(t *testing.T) {
	b, _ := newTest(t, Config{})
	ts := &fakeTenants{}
	for i := range MaxStreamTenants + 1 {
		ts.set("t"+strconv.Itoa(i), &fakeFeed{})
	}
	err := b.WatchAll(context.Background(), WatchAll{Tenants: ts}, func(context.Context, Message) error { return nil })
	if !errors.Is(err, ErrTooManyTenants) {
		t.Errorf("a stream of %d tenants = %v, want ErrTooManyTenants", MaxStreamTenants+1, err)
	}
}

// TestWatchAllRereadsATenantWokenDuringItsRead guards a wake that comes
// while the tenant it names is being read: the read under way took its page
// before the events that woke the tenant, so the tenant is read again once
// that read comes back, and its events are not left until the next tick.
func TestWatchAllRereadsATenantWokenDuringItsRead(t *testing.T) {
	b, _ := newTest(t, Config{})
	slow := &fakeFeed{blocked: make(chan struct{}, 1), hold: make(chan struct{})}
	ts := &fakeTenants{}
	ts.set("a", slow)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "0"}})
	s.keepAlive(t)
	b.idle(t, "")
	slow.holding.Store(true)
	b.Wake("a")
	<-slow.blocked // the read has taken its page, empty, and not come back
	slow.write(2)
	b.Wake("a")
	// The stream takes that wake while the read is still out.
	until(t, "the wake being taken", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.all) == 1 && b.all[0].signals == b.all[0].take
	})
	slow.holding.Store(false)
	slow.hold <- struct{}{}
	if ids := s.eventIDs(t, 2); fmt.Sprint(ids) != "[1 2]" {
		t.Errorf("tenant a gave %v, want [1 2] without waiting for a tick", ids)
	}
}

// TestWatchAllKeepAliveNamesOnlyWhatChanged guards the size of a mostly idle
// stream: its opening keep-alive is full and names every tenant; a later
// one, with nothing read since, names none; one after a tenant's events
// names that tenant alone, with its new cursor.
func TestWatchAllKeepAliveNamesOnlyWhatChanged(t *testing.T) {
	b, clock := newTest(t, Config{})
	ts := &fakeTenants{}
	feeds := map[string]*fakeFeed{}
	for _, n := range []string{"a", "b", "c"} {
		feeds[n] = &fakeFeed{}
		feeds[n].write(1)
		ts.set(n, feeds[n])
	}
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts})
	if k := s.keepAlive(t); !k.Full || len(k.TenantCursors) != 3 {
		t.Fatalf("the opening keep-alive: full %v, %v; want full with the 3 tenants", k.Full, k.TenantCursors)
	}
	b.idle(t, "")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	if k := s.keepAlive(t); k.Full || len(k.TenantCursors) != 0 {
		t.Errorf("an idle stream's keep-alive: full %v, %v; want no tenant named", k.Full, k.TenantCursors)
	}
	feeds["b"].write(2)
	b.Wake("b")
	if ids := s.eventIDs(t, 2); fmt.Sprint(ids) != "[2 3]" {
		t.Fatalf("b gave %v, want [2 3]", ids)
	}
	b.idle(t, "")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	if k := s.keepAlive(t); k.Full || fmt.Sprint(k.TenantCursors) != "map[b:3]" {
		t.Errorf("the keep-alive after b's events: full %v, %v; want b at 3 alone", k.Full, k.TenantCursors)
	}
	if fmt.Sprint(s.cursors) != "map[a:1 b:3 c:1]" {
		t.Errorf("the cursors a client keeps are %v, want a and c at 1 and b at 3", s.cursors)
	}
}

// TestWatchAllKeepAliveNamesHistories guards the histories a stream of
// every tenant gives, each with its cursor, as the contract names them: the
// opening keep-alive names the history of every cursor it names, those
// started from now and those given alike; an idle one names neither; one
// after a tenant's events names that tenant's cursor and history alone.
func TestWatchAllKeepAliveNamesHistories(t *testing.T) {
	b, clock := newTest(t, Config{})
	ts := &fakeTenants{}
	feeds := map[string]*fakeFeed{}
	for _, n := range []string{"a", "b"} {
		feeds[n] = &fakeFeed{history: "history-" + n}
		feeds[n].write(1)
		ts.set(n, feeds[n])
	}
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"a": "1"}})
	k := s.keepAlive(t)
	if fmt.Sprint(k.TenantCursors) != "map[a:1 b:1]" || fmt.Sprint(k.TenantHistories) != "map[a:history-a b:history-b]" {
		t.Errorf("the opening keep-alive names the cursors %v and histories %v, want each tenant's cursor with its history", k.TenantCursors,
			k.TenantHistories)
	}
	b.idle(t, "")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	if k := s.keepAlive(t); len(k.TenantHistories) != 0 || len(k.TenantCursors) != 0 {
		t.Errorf("an idle keep-alive names the cursors %v and histories %v, want none", k.TenantCursors, k.TenantHistories)
	}
	feeds["b"].write(1)
	b.Wake("b")
	s.eventIDs(t, 1)
	b.idle(t, "")
	clock.Advance(DefaultKeepAlive)
	tick(t, b, clock)
	if k := s.keepAlive(t); fmt.Sprint(k.TenantCursors) != "map[b:2]" || fmt.Sprint(k.TenantHistories) != "map[b:history-b]" {
		t.Errorf("the keep-alive after b's event: cursors %v, histories %v; want b at 2 with its history alone", k.TenantCursors, k.TenantHistories)
	}
}

// TestWatchAllSendsAHistoryLearnedLater guards a cursor given at the start
// for a tenant whose database does not answer: the opening keep-alive names
// it with an empty history, and once the tenant answers and is read, a
// keep-alive names the same cursor again with its history.
func TestWatchAllSendsAHistoryLearnedLater(t *testing.T) {
	b, clock := newTest(t, Config{})
	ts := &fakeTenants{}
	c := &fakeFeed{history: "history-c"}
	c.write(1)
	c.down.Store(true)
	ts.set("c", c)
	s := start(t, b, Watch{}, &WatchAll{Tenants: ts, Cursors: map[string]string{"c": "1"}})
	if k := s.keepAlive(t); fmt.Sprint(k.TenantCursors) != "map[c:1]" || fmt.Sprint(k.TenantHistories) != "map[c:]" {
		t.Fatalf("the opening keep-alive names %v with histories %v, want c at 1 with no history yet", k.TenantCursors, k.TenantHistories)
	}
	c.down.Store(false)
	for range 5 {
		b.idle(t, "")
		clock.Advance(DefaultMaxBackoff)
		tick(t, b, clock)
		if k := s.keepAlive(t); k.TenantHistories["c"] == "history-c" {
			if k.TenantCursors["c"] != "1" {
				t.Errorf("the history came with the cursor %q, want 1", k.TenantCursors["c"])
			}
			return
		}
	}
	t.Fatal("no keep-alive named c's history once it answered")
}
