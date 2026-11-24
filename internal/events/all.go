package events

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The stream of every tenant reads its tenants through a small pool of
// readers it owns (Config.TenantReaders), each read bounded by its own
// deadline (Config.TenantReadTimeout), and hands each page to the one sender
// that delivers it. The choice, against the two others:
//
//   - one read after another, each with a deadline, would let one tenant
//     that does not answer cost every other tenant that deadline on every
//     round, and a database server that stops answering for many tenants
//     at once would cost the sum;
//   - a goroutine per tenant would cost, with thousands of tenants and up
//     to a thousand streams, millions of goroutines, most of them idle;
//   - a pool of a few readers per stream keeps the stream going while up
//     to that many tenants hang, costs a few goroutines that live only
//     while a round reads, and holds at most one page per reader: a reader
//     that has read a page waits for the sender to take it, so a slow client
//     holds the readers, not a growing queue.
//
// A tenant whose read fails or runs past its deadline is skipped, and tried
// again after a wait that doubles from one tick up to Config.MaxBackoff; it
// is named in the keep-alive while it is skipped, so it is never silent.

// follow is one tenant as a stream of every tenant follows it.
type follow struct {
	cursor  string
	fromNow bool // start from the tenant's newest event when first read
	known   bool // the cursor is set ("" is its first event)
	// sent is the cursor the last keep-alive gave for the tenant, and
	// sentKnown whether one gave any: a later keep-alive names the tenant
	// only when its cursor differs.
	sent      string
	sentKnown bool
	// history is the id of the feed's history cursor belongs to, set with
	// it and sent with it: "" for the empty cursor, or one given at the
	// start whose database did not answer yet. sentHistory is the one the
	// last keep-alive gave: a cursor given at the start is sent again with
	// its history once a read has learned it, though the cursor is the same.
	history, sentHistory string
	listed               bool // in the latest listing; a tenant not listed is paused, its cursor kept
	// failing is set while the tenant is skipped after a read that failed
	// or did not answer; it is tried again at retry, after backoff.
	failing bool
	retry   time.Time
	backoff time.Duration
}

// readJob is one page to read for one tenant, from the cursor the stream
// holds for it.
type readJob struct {
	name   string
	cursor string
	known  bool
}

// readResult is what a reader brings back for a job.
type readResult struct {
	name   string
	cursor string // the cursor read from, resolved when it was not known
	page   tenant.EventPage
	err    error
}

// readTenant reads one page of a job's tenant, bounded by the read
// deadline.
func (b *Broadcaster) readTenant(ctx context.Context, ts Tenants, types []tenant.EventType, j readJob) readResult {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := b.cfg.Clock.AfterFunc(b.cfg.TenantReadTimeout, cancel)
	defer stop()
	res := readResult{name: j.name, cursor: j.cursor}
	r, err := ts.Reader(rctx, j.name)
	if err == nil && !j.known {
		res.cursor, err = r.EventCursor(rctx)
	}
	if err == nil {
		res.page, err = r.ReadEvents(rctx, res.cursor, types, b.cfg.PageSize)
	}
	res.err = err
	return res
}

// WatchAll streams every tenant's events to send, each tenant's in its
// commit order, until ctx ends, Close is called, send fails, a tenant's
// cursor can no longer be answered from (tenant.ErrCursorExpired, the
// tenant named in the message), or the installation has more tenants than
// MaxStreamTenants.
//
// The first message is a keep-alive with the cursor of every tenant it
// follows (Full); a later one names the tenants whose cursor changed since
// the one before, each with the history its cursor belongs to. A
// tenant whose cursor belongs to another history of its feed ends the
// stream as an expired one does (tenant.ErrHistoryChanged, which holds
// tenant.ErrCursorExpired). A tenant named in Cursors starts from its cursor; one not named
// starts from now, or with Resume from its first event kept; a tenant
// listed later starts from its first event. A tenant missing from a listing
// is paused, not forgotten, and continues from its cursor when listed
// again. A tenant created again under a name the stream follows answers the
// old cursor as another tenant's (tenant.ErrForeignCursor), and on exactly
// that the stream starts it from its first event. A tenant whose read fails
// or does not answer within TenantReadTimeout is skipped while the others
// go on, named in each keep-alive's Unanswered, and tried again after a
// wait. A keep-alive follows whenever nothing was sent for the keep-alive
// interval.
func (b *Broadcaster) WatchAll(ctx context.Context, w WatchAll, send Send) error {
	if w.Tenants == nil || send == nil {
		return fault.New(fault.Invalid, "a watch of every tenant has a list of tenants and a send function")
	}
	if err := tenant.ValidateEventTypes(w.Types); err != nil {
		return err
	}
	if len(w.Cursors) > MaxStreamTenants {
		return fault.Wrap(fault.Invalid, ErrTooManyTenants, "a stream of every tenant names at most "+strconv.Itoa(MaxStreamTenants)+" cursors")
	}
	me, err := b.register("", w.Owner)
	if err != nil {
		return err
	}
	defer b.unregister(me)
	ctx, cancel := b.watchContext(ctx)
	defer cancel()

	follows := map[string]*follow{}
	listed := false
	// list reads the tenants. Those there at the start begin from the
	// cursor given, or from now, or with Resume from their first event;
	// those listed later from their first event; those not listed now are
	// paused. It reports a listing larger than the stream may follow.
	list := func() error {
		names, err := w.Tenants.TenantNames(ctx)
		if err != nil {
			return nil // tried again on the next tick
		}
		if len(names) > MaxStreamTenants {
			return fault.Wrap(fault.Unavailable, ErrTooManyTenants,
				"the installation has more tenants than one stream follows ("+strconv.Itoa(MaxStreamTenants)+")")
		}
		for _, f := range follows {
			f.listed = false
		}
		for _, n := range names {
			if f, ok := follows[n]; ok {
				f.listed = true
				continue
			}
			f := &follow{listed: true}
			switch c, given := w.Cursors[n]; {
			case !listed && given:
				f.cursor, f.known = c, true
			case !listed && !w.Resume:
				f.fromNow = true
			default:
				f.known = true // from its first event: the empty cursor
			}
			follows[n] = f
		}
		listed = true
		return nil
	}
	// keepAlive names every tenant followed when full, and otherwise
	// those whose cursor changed since the last keep-alive: at a thousand
	// tenants, a mostly idle stream's keep-alive is a few bytes rather
	// than the whole map every interval.
	keepAlive := func(full bool) Message {
		k := &KeepAlive{Time: b.cfg.Clock.Now(), Full: full, TenantCursors: map[string]string{}, TenantHistories: map[string]string{}}
		for n, f := range follows {
			if f.known && (full || !f.sentKnown || f.sent != f.cursor || f.sentHistory != f.history) {
				// A history is sent with every cursor sent, and never
				// alone.
				k.TenantCursors[n], k.TenantHistories[n] = f.cursor, f.history
				f.sent, f.sentHistory, f.sentKnown = f.cursor, f.history, true
			}
			if f.failing && f.listed {
				k.Unanswered = append(k.Unanswered, n)
			}
		}
		sort.Strings(k.Unanswered)
		return Message{KeepAlive: k}
	}
	// skip marks a tenant skipped after a failed or late read, until a
	// wait that doubles each time.
	skip := func(f *follow) {
		f.backoff = min(max(2*f.backoff, b.cfg.Tick), b.cfg.MaxBackoff)
		f.failing, f.retry = true, b.cfg.Clock.Now().Add(f.backoff)
	}

	// The stream opens with a keep-alive that holds the cursor of every
	// tenant it follows, those starting from now resolved where they
	// answer.
	if err := list(); err != nil {
		return err
	}
	for n, f := range follows {
		if !f.fromNow && f.cursor == "" {
			continue
		}
		r, err := w.Tenants.Reader(ctx, n)
		if err != nil {
			continue
		}
		if f.fromNow {
			c, err := r.EventCursor(ctx)
			if err != nil {
				continue
			}
			f.cursor, f.known, f.fromNow = c, true, false
		}
		// The history a cursor names, read from the cursor alone; a cursor
		// that is not this feed's is refused at its first read, and goes
		// out with no history until then.
		f.history, _ = r.EventCursorHistory(f.cursor)
	}
	if ctx.Err() != nil {
		return ended(ctx)
	}
	if err := send(ctx, keepAlive(true)); err != nil {
		return sendFailed(ctx, err)
	}
	lastSent := b.cfg.Clock.Now()

	// The readers, which live as long as the stream and end with it.
	rctx, stopReaders := context.WithCancel(ctx)
	jobs, results := make(chan readJob), make(chan readResult)
	var readers sync.WaitGroup
	for range b.cfg.TenantReaders {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := range jobs {
				res := b.readTenant(rctx, w.Tenants, w.Types, j)
				select {
				case results <- res:
				case <-rctx.Done():
					return
				}
			}
		}()
	}
	defer func() {
		stopReaders()
		close(jobs)
		readers.Wait()
	}()

	// The tenants to read, in turn: a tenant whose page was full goes to
	// the back, so a tenant with a long backlog is read a page at a time
	// between the others; a tenant is read by one reader at a time, so its
	// events stay in order.
	var queue []string
	// rewake holds the tenants woken while a reader was reading them: that
	// read may have begun before the events that woke them, so each is read
	// again once its read comes back, or the wake would be lost.
	queued, reading, rewake := map[string]bool{}, map[string]bool{}, map[string]bool{}
	enqueue := func(n string) {
		f := follows[n]
		switch {
		case f == nil || !f.listed || queued[n]:
		case reading[n]:
			rewake[n] = true
		case !f.failing || !b.cfg.Clock.Now().Before(f.retry):
			queue, queued[n] = append(queue, n), true
		}
	}
	// woken takes what woke the stream: a listing on the timer's tick or
	// for a tenant not followed yet, and the tenants woken.
	woken := func() error {
		me.mu.Lock()
		rescan, dirty := me.rescan, me.dirty
		me.rescan, me.dirty = false, map[string]struct{}{}
		me.mu.Unlock()
		for n := range dirty {
			if _, ok := follows[n]; !ok {
				rescan = true
			}
		}
		if rescan {
			if err := list(); err != nil {
				return err
			}
		}
		names := make([]string, 0, len(follows))
		for n := range follows {
			if _, ok := dirty[n]; ok || rescan {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			enqueue(n)
		}
		return nil
	}
	// keepAliveDue sends a keep-alive when nothing was sent for the
	// interval, whether or not a read is under way.
	keepAliveDue := func() error {
		if now := b.cfg.Clock.Now(); now.Sub(lastSent) >= b.cfg.KeepAlive {
			if err := send(ctx, keepAlive(false)); err != nil {
				return sendFailed(ctx, err)
			}
			lastSent = now
		}
		return nil
	}
	if err := woken(); err != nil { // register asked for a first listing
		return err
	}
	for {
		if len(queue) == 0 && len(reading) == 0 {
			if err := keepAliveDue(); err != nil {
				return err
			}
			if err := b.wait(ctx, me); err != nil {
				return err
			}
			if err := woken(); err != nil {
				return err
			}
			if err := keepAliveDue(); err != nil {
				return err
			}
			continue
		}
		var next chan<- readJob
		var job readJob
		if len(queue) > 0 {
			f := follows[queue[0]]
			next, job = jobs, readJob{name: queue[0], cursor: f.cursor, known: f.known}
		}
		select {
		case next <- job:
			queue = queue[1:]
			delete(queued, job.name)
			reading[job.name] = true
		case <-me.wake:
			b.took(me)
			if err := woken(); err != nil {
				return err
			}
			if err := keepAliveDue(); err != nil {
				return err
			}
		case res := <-results:
			delete(reading, res.name)
			again := rewake[res.name]
			delete(rewake, res.name)
			f := follows[res.name]
			switch {
			case ctx.Err() != nil:
				return ended(ctx)
			case errors.Is(res.err, tenant.ErrCursorExpired):
				return fault.Wrap(fault.KindOf(res.err), res.err, fault.Message(res.err)+" (tenant "+res.name+")")
			case errors.Is(res.err, tenant.ErrForeignCursor):
				// The name is another tenant now, created again: its feed
				// is followed from its first event.
				f.cursor, f.history, f.known, f.fromNow = "", "", true, false
				enqueue(res.name)
				continue
			case res.err != nil:
				skip(f)
				continue
			}
			// The page was read from res.cursor, so both are of its history.
			f.cursor, f.history, f.known, f.fromNow, f.failing, f.backoff = res.cursor, res.page.History, true, false, false, 0
			for i := range res.page.Events {
				if err := send(ctx, Message{Tenant: res.name, Event: &res.page.Events[i]}); err != nil {
					return sendFailed(ctx, err)
				}
				lastSent = b.cfg.Clock.Now()
			}
			f.cursor, f.history = res.page.Cursor, res.page.History
			if again || len(res.page.Events) == b.cfg.PageSize {
				enqueue(res.name)
			}
		case <-ctx.Done():
			return ended(ctx)
		}
	}
}
