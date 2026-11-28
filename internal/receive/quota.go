package receive

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReasonOverQuota is the reason of a file refused because the tenant holds
// as much as its quota allows (quota.bytes, quota.instances): a temporary
// failure, answered A700H to a C-STORE (PS3.4 B.2.3) and to a STOW-RS part,
// and a wait to an import, so the sender keeps its copy and sends it again
// once the tenant is under its bound. A fixed phrase a source compares.
const ReasonOverQuota = "the tenant's storage quota is reached"

// QuotaBounds reads the tenant's quota settings as they apply on it now
// (tenant.QuotaBounds). It is read again at most every HeaderLimitInterval,
// so a changed bound applies within a second without a read per file.
type QuotaBounds func(ctx context.Context) (tenant.QuotaBounds, error)

// quotaGate admits each new file against the tenant's quota, before any
// byte of it is kept. This is where the check sits, and why:
//
//   - At the start of a file, not at its archive. A file admitted is
//     received whole and archived whatever the quota says by then, since
//     its sender was answered success; so a reception in progress always
//     finishes what it was sent, and only files that arrive once the bound
//     is reached are refused, as temporary failures their senders retry.
//   - The database is read outside the gate's semaphore, and only the
//     arithmetic is done under it. A read that hangs holds up its own file
//     and nobody else's, and the semaphore is taken with the caller's
//     context, so a caller that gave up leaves at once. Two files at the
//     edge are still admitted one after the other, the second seeing the
//     first, because each admission is counted in memory under the
//     semaphore before the next one is decided.
//
// The use is the database's (tenant.Quotas: what the catalog holds, what
// corrections own and have not purged, what receptions recorded and have
// not decided), read again at most every HeaderLimitInterval like the
// bounds, plus the files admitted here that the cached read does not yet
// count. Which those are is the point of the release stamps below: an
// admission counts in memory from its admission until a read of the use
// that began after its row was written has been installed. Giving it back
// when its batch commits, as soon as the row is written, would leave a
// window in which the cached use (read before the row) and the memory (the
// admission gone) both miss it, and an instance bound could be passed.
// Counting it until the next read makes the opposite error instead: a file
// whose row a read caught while it was still held in memory counts twice
// for up to one interval, which can only refuse early.
//
// The slack: a file admitted counts as one instance at once, so an
// instance bound is never passed. Its bytes are counted once it has landed,
// since a C-STORE or a STOW-RS part does not say its size first: a bytes
// bound can be passed by the files in flight when it is reached, at most
// one per store being received at that moment, each at most the receiver's
// largest file. A file whose row could not be written (the database
// refusing the batch) counts here until the next read of the use, then
// stops while it waits to be written again.
type quotaGate struct {
	once sync.Once
	// sem is a semaphore of one, taken with the caller's context. It
	// guards every field below and is never held across a database read.
	sem chan struct{}
	// held is the files admitted that the cached use does not count, by
	// ticket; files and bytes are their sum.
	held         map[*quotaTicket]struct{}
	files, bytes int64
	// clock stamps the writing of rows: a ticket written when it read n
	// is counted by any read of the use that started at clock n or later.
	clock atomic.Uint64
	// reads numbers the reads of the use, so a slow read that finishes
	// after a newer one is not installed over it.
	reads atomic.Uint64
	// bounds is the bounds last read, when, and whether one was.
	bounds      tenant.QuotaBounds
	boundsAt    time.Time
	boundsKnown bool
	// use is the use last read, when, the number of the read, and whether
	// one was.
	use      tenant.QuotaUse
	useAt    time.Time
	useRead  uint64
	useKnown bool
}

// quotaTicket is one file's admission, given back once the file's row is
// written and a read of the use has counted it, or when the file is not
// kept. A nil ticket (no quota, or a file a restart found) gives back
// nothing.
type quotaTicket struct {
	g     *quotaGate
	bytes int64
	// stamp is the gate's clock when the row was written, 0 while it is
	// not; gone says the ticket is no longer held.
	stamp uint64
	gone  bool
}

// init makes the semaphore the first time the gate is used.
func (g *quotaGate) init() {
	g.once.Do(func() {
		g.sem = make(chan struct{}, 1)
		g.held = map[*quotaTicket]struct{}{}
	})
}

// acquire takes the semaphore, or returns the context's error at once when
// the caller has given up (a canceled caller never wins the race against a
// free semaphore).
func (g *quotaGate) acquire(ctx context.Context) error {
	g.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lock takes the semaphore without a context, for the bookkeeping of a
// ticket, which does arithmetic only and so never waits long.
func (g *quotaGate) lock() {
	g.init()
	g.sem <- struct{}{}
}

// unlock gives the semaphore back.
func (g *quotaGate) unlock() { <-g.sem }

// fresh reports whether a read made at then is still kept at now. A clock
// that went back makes it stale.
func fresh(now, then time.Time) bool {
	return !now.Before(then) && now.Sub(then) < HeaderLimitInterval
}

// admitQuota admits one new file, or answers why not: ReasonOverQuota when
// the tenant is at or past a bound, reasonUnavailable when its bounds or
// its use cannot be read, or the caller has given up (refused rather than
// let through: a quota that could not be read is not known to have room,
// whatever an earlier read said). It returns a nil ticket when no bound is
// set, when the pipeline is given no bounds, or when the store keeps no
// counts.
func (p *Pipeline) admitQuota(ctx context.Context) (*quotaTicket, string) {
	if p.cfg.Quota == nil {
		return nil, ""
	}
	counts, ok := p.store.(tenant.Quotas)
	if !ok {
		return nil, ""
	}
	g := &p.quota
	now := p.cfg.Clock.Now()

	// What is cached, read under the semaphore and nothing more.
	if err := g.acquire(ctx); err != nil {
		return nil, reasonUnavailable
	}
	b, boundsFresh := g.bounds, g.boundsKnown && fresh(now, g.boundsAt)
	useFresh := g.useKnown && fresh(now, g.useAt)
	g.unlock()

	if !boundsFresh {
		var err error
		if b, err = p.cfg.Quota(ctx); err != nil {
			return nil, reasonUnavailable
		}
	}
	var (
		use     tenant.QuotaUse
		useRead uint64
		clock   uint64
		readUse bool
	)
	if b.Bounded() && !useFresh {
		// The clock is read before the use, so every row written by then
		// is in what the read returns.
		clock = g.clock.Load()
		useRead = g.reads.Add(1)
		var err error
		if use, err = counts.QuotaUse(ctx); err != nil {
			return nil, reasonUnavailable
		}
		readUse = true
	}

	if err := g.acquire(ctx); err != nil {
		return nil, reasonUnavailable
	}
	defer g.unlock()
	if !boundsFresh {
		g.bounds, g.boundsAt, g.boundsKnown = b, now, true
	}
	if readUse && useRead > g.useRead {
		g.use, g.useAt, g.useRead, g.useKnown = use, now, useRead, true
		// What the read counts is no longer held in memory.
		for t := range g.held {
			if t.stamp != 0 && t.stamp <= clock {
				g.drop(t)
			}
		}
	}
	if !b.Bounded() {
		return nil, ""
	}
	if !g.useKnown {
		return nil, reasonUnavailable
	}
	if b.Reached(g.use.Plus(g.files, g.bytes)) {
		return nil, ReasonOverQuota
	}
	t := &quotaTicket{g: g}
	g.held[t] = struct{}{}
	g.files++
	return t, ""
}

// drop stops holding a ticket. The semaphore is held.
func (g *quotaGate) drop(t *quotaTicket) {
	if t.gone {
		return
	}
	t.gone = true
	delete(g.held, t)
	g.files--
	g.bytes -= t.bytes
}

// landed counts the bytes of the file the ticket admitted, now that they
// are known.
func (t *quotaTicket) landed(size int64) {
	if t == nil {
		return
	}
	t.g.lock()
	defer t.g.unlock()
	if !t.gone {
		t.bytes = size
		t.g.bytes += size
	}
}

// written records that the file's row is written: the database counts it
// from now on, and the gate stops holding it once a read of the use that
// began after this has been installed.
func (t *quotaTicket) written() {
	if t == nil {
		return
	}
	t.g.lock()
	defer t.g.unlock()
	if !t.gone && t.stamp == 0 {
		t.stamp = t.g.clock.Add(1)
	}
}

// release gives the admission back at once: the file was not kept, so no
// row will ever count it. A second call does nothing.
func (t *quotaTicket) release() {
	if t == nil {
		return
	}
	t.g.lock()
	defer t.g.unlock()
	t.g.drop(t)
}

// admittedForQuota returns the files the quota counts as admitted and not
// yet counted by the use, and their landed bytes, for a test.
func (p *Pipeline) admittedForQuota() (int64, int64) {
	g := &p.quota
	g.lock()
	defer g.unlock()
	return g.files, g.bytes
}
