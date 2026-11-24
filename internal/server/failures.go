package server

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// The limits on failed calls. Only failures are counted, never a request
// that succeeded, so a busy key or a busy proxy is never slowed however many
// requests it makes; and only refusals are slowed, never a request that
// will succeed.
//
// The counts are held in memory, per process: a restart forgets them, and
// two servers on one master count apart. For a limit on guessing that is
// acceptable: a restart costs an attacker one more window's worth of
// guesses against a secret of 256 bits, and keeping the counts in the
// master would cost a write per failure, which a flood would turn against
// the master itself.
const (
	// failureDelay is how long a refusal is held before it is answered once
	// its caller has failed more than the limit within the window.
	failureDelay = time.Second
	// maxTrackedAddresses and maxTrackedKeys bound the callers counted at
	// once, and the blocks held at once, so a flood from many addresses
	// cannot make the server hold unbounded memory: each entry is a few
	// dozen bytes.
	maxTrackedAddresses = 1 << 16
	maxTrackedKeys      = 1 << 16
)

// failureCount is one caller's failed calls in its current window.
type failureCount struct {
	start time.Time
	count int
}

// verdict is what a failure leads to.
type verdict struct {
	// slow holds the refusal for failureDelay before it is answered.
	slow bool
	// blocked is set on the failure that starts a block, which ends at
	// until; that failure is audited and raised as an event.
	blocked bool
	until   time.Time
}

// failureTable counts the failed calls of one kind of caller (addresses, or
// keys), bounded in size. Its methods are safe for concurrent use.
//
// The counts and the blocks are held apart, each under its own lock. A
// flood of failures from new callers takes only the counts' lock, and
// makes room by forgetting the caller counted longest ago, in constant
// time; the blocks are read under a read lock that only the start of a
// block writes, so the check of every request (blocked) never waits on a
// flood. A block is never forgotten to make room, so a flood of new
// callers cannot lift one.
type failureTable struct {
	max int

	mu     sync.Mutex
	counts map[string]*failureCount
	// countOrder holds the counted callers in the order they were first
	// counted; its oldest is forgotten to make room.
	countOrder fifo

	bmu    sync.RWMutex
	blocks map[string]time.Time
	// blockOrder holds the blocked callers in the order their blocks
	// started, which is nearly the order they end.
	blockOrder fifo

	// untracked counts blocks that found the table full of blocks still
	// running, and were not started: the refusal is slowed, not blocked.
	untracked atomic.Uint64
}

// newFailureTable returns a table that counts at most max callers and
// holds at most max blocks.
func newFailureTable(max int) *failureTable {
	return &failureTable{
		max:    max,
		counts: make(map[string]*failureCount), countOrder: newFIFO(max),
		blocks: make(map[string]time.Time), blockOrder: newFIFO(max),
	}
}

// blocked reports whether who is blocked at now, and until when. It takes
// only the blocks' read lock.
func (t *failureTable) blocked(who string, now time.Time) (bool, time.Time) {
	t.bmu.RLock()
	until, ok := t.blocks[who]
	t.bmu.RUnlock()
	if !ok || !now.Before(until) {
		return false, time.Time{}
	}
	return true, until
}

// fail counts one failed call of who at now under the limits, and says what
// it leads to: past limit failures within window, a refusal is slowed; at
// twice limit, who is blocked for block and its count starts again.
func (t *failureTable) fail(who string, now time.Time, limit int, window, block time.Duration) verdict {
	slow, blocks := t.count(who, now, limit, window)
	if !blocks {
		return verdict{slow: slow}
	}
	until := now.Add(block)
	if !t.block(who, now, until) {
		t.untracked.Add(1)
		return verdict{slow: true}
	}
	return verdict{slow: true, blocked: true, until: until}
}

// count counts one failure of who and reports whether it is past limit
// within window (slow), and whether it reached twice limit (blocks), which
// starts its count again.
func (t *failureTable) count(who string, now time.Time, limit int, window time.Duration) (slow, blocks bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.counts[who]
	if !ok {
		if len(t.counts) >= t.max {
			delete(t.counts, t.countOrder.pop())
		}
		c = &failureCount{start: now}
		t.counts[who] = c
		t.countOrder.push(who)
	}
	if now.Sub(c.start) > window {
		c.start, c.count = now, 0
	}
	c.count++
	// Twice the limit, written so it cannot overflow an int on a 32-bit
	// build whatever limit a caller passes (the setting is bounded far
	// below, but a limit near the int's top would make 2*limit negative and
	// block at the first refusal).
	if c.count > limit && c.count-limit >= limit {
		c.start, c.count = now, 0
		return true, true
	}
	return c.count > limit, false
}

// block starts a block of who that ends at until, and reports whether it
// could: when the table holds max blocks, the block started longest ago is
// forgotten only if it has ended, and a few are looked at, in constant
// time, before a table full of running blocks refuses the new one.
func (t *failureTable) block(who string, now, until time.Time) bool {
	t.bmu.Lock()
	defer t.bmu.Unlock()
	if _, ok := t.blocks[who]; ok {
		t.blocks[who] = until
		return true
	}
	for tries := 0; len(t.blocks) >= t.max && tries < blockLookahead; tries++ {
		oldest := t.blockOrder.pop()
		if !now.Before(t.blocks[oldest]) {
			delete(t.blocks, oldest)
			break
		}
		// Still running (a block started again, or a longer block time):
		// it goes to the back, and the next is looked at.
		t.blockOrder.push(oldest)
	}
	if len(t.blocks) >= t.max {
		return false
	}
	t.blocks[who] = until
	t.blockOrder.push(who)
	return true
}

// blockLookahead bounds how many blocks a new block looks at for one that
// has ended when the table is full.
const blockLookahead = 8

// size returns how many callers are counted, and how many blocks were not
// started for want of room.
func (t *failureTable) size() (int, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.counts), t.untracked.Load()
}

// fifo is a bounded first-in, first-out queue of names, a ring: push and
// pop take constant time. Its user never pushes more than its capacity.
type fifo struct {
	names      []string
	head, size int
}

// newFIFO returns an empty queue of capacity n.
func newFIFO(n int) fifo { return fifo{names: make([]string, n)} }

// push adds name at the back.
func (q *fifo) push(name string) {
	q.names[(q.head+q.size)%len(q.names)] = name
	q.size++
}

// pop removes and returns the name at the front.
func (q *fifo) pop() string {
	name := q.names[q.head]
	q.names[q.head] = ""
	q.head = (q.head + 1) % len(q.names)
	q.size--
	return name
}

// addressKey returns the key an address is counted under: an IPv4 address
// alone, an IPv6 address by its /64, the smallest network a site is given
// (RFC 6177), so a caller holding a /64 cannot make 2^64 callers of itself.
func addressKey(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.Unmap().String()
}
