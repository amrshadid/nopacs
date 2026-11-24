package sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
)

// Group commit. With synchronous=FULL every commit waits for the disk to
// confirm the write-ahead log is on it (sqlite.org/pragma.html#pragma_synchronous),
// and that wait, not SQLite's work, is what a change costs: one commit per
// change caps a store at about as many changes a second as the disk takes
// syncs, however many callers wait. So the changes of every caller waiting
// at the same moment are written in one transaction and one sync, each in a
// savepoint of its own, and each caller is given its own result.
//
// There is no timer. A change that finds the writer idle is written at once,
// by its own caller, alone, exactly as a transaction of its own would be.
// Changes that arrive while a transaction is running or syncing queue behind
// it, and the next transaction takes them all, up to maxBatch. Batches form
// from the time the disk takes, and only when callers are already waiting.

// maxBatch bounds the changes in one transaction, so that the time one
// transaction holds the writer, the memory it holds, and what it adds to the
// write-ahead log stay bounded however long the queue grows. Its value is
// measured: with 256 callers each setting a setting with its audit entry, on
// a laptop's SSD with full sync, a bound of 64 wrote about 8,000 changes a
// second with a mean wait of 30 ms, and a bound of 256 about 15,000 with 17
// ms. A change costs SQLite tens of microseconds and a sync milliseconds, so
// 256 changes still take less time than the sync they share.
const maxBatch = 256

// The states of a change: waiting in the queue, taken by the writer to run,
// or given up by its caller before the writer took it.
const (
	changeQueued int32 = iota
	changeTaken
	changeAbandoned
)

// change is one caller's change waiting to be written: its function, the
// caller's context and message for classifying a failure, and where its
// result goes.
type change struct {
	ctx     context.Context
	message string
	fn      func(tx *sql.Tx) error
	// state is changeQueued until the writer takes the change to run it, or
	// its caller gives up first; whichever moves it first decides whether
	// the change can still be written.
	state atomic.Int32
	// done holds the one result; it is buffered, so the writer never waits
	// for a caller that has left.
	done chan outcome
	// ran is set by the writer, alone, once fn has run in a transaction.
	ran bool
	// held is the failure of a change that failed while a neighbor's
	// uncommitted write was in the transaction, kept by the writer until the
	// transaction's outcome is known; failed tells it from a success.
	held   outcome
	failed bool
}

// outcome is a change's result: its error, or the value it panicked with,
// which its caller panics with again in its own goroutine.
type outcome struct {
	err      error
	panicked bool
	value    any
}

// committer is the queue of changes waiting to be written and whether a
// writer goroutine is running. Its zero value is ready.
type committer struct {
	mu      sync.Mutex
	queue   []*change
	running bool

	// commits counts the transactions committed, for tests that check how
	// changes were grouped.
	commits atomic.Uint64
	// beforeCommit, when set by a test, runs after every change of a batch
	// has run and before its commit, with the changes whose answers wait
	// for it: those written, and those whose failure is held.
	beforeCommit func(waiting []*change)
}

// writeTx runs fn in a write transaction, which begins with BEGIN IMMEDIATE
// on the one write connection, possibly shared with the changes of other
// callers waiting at the same moment, and returns once that transaction is
// committed, and so synced, or fn's change is undone. Every changing
// operation and its audit entry go through here together, which is what
// makes them both or neither: when fn fails or panics nothing it wrote
// stays, and the other changes in the transaction are not touched.
//
// fn may run more than once, each time in a new transaction after the last
// was undone whole, so it must not keep anything from a run but its
// assignments; every change of this package only assigns.
//
// When ctx ends while the change waits in the queue, the caller is answered
// at once with Unavailable and the change is never written. When it ends
// after the change has started, the change is written whole or not at all,
// never in part, and the caller is answered with which, as soon as that is
// known: nil only when it was committed.
func (b *base) writeTx(ctx context.Context, message string, fn func(tx *sql.Tx) error) error {
	if err := b.open(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return b.fail(ctx, err, message)
	}
	c := &change{ctx: ctx, message: message, fn: fn, done: make(chan outcome, 1)}
	b.w.mu.Lock()
	if !b.w.running {
		// Alone: the caller writes its own change in its own goroutine, as
		// a transaction of its own, and then hands the writer on to the
		// changes queued meanwhile.
		b.w.running = true
		b.w.mu.Unlock()
		b.commitBatch(ctx, []*change{c})
		b.handOff()
		return (<-c.done).result()
	}
	b.w.queue = append(b.w.queue, c)
	b.w.mu.Unlock()
	select {
	case o := <-c.done:
		return o.result()
	case <-ctx.Done():
	}
	// A change not yet taken is given up, and its caller answered at once.
	// One the writer has taken is in a transaction already: its caller is
	// answered with what became of it once that is known, at the latest
	// when the transaction ends, which is soon, since its statements fail
	// once its context has ended. An error must mean nothing was written,
	// so the answer is never a guess.
	if c.state.CompareAndSwap(changeQueued, changeAbandoned) {
		return b.fail(ctx, ctx.Err(), message)
	}
	return (<-c.done).result()
}

// result returns the error, or panics again with the value the change
// panicked with.
func (o outcome) result() error {
	if o.panicked {
		panic(o.value)
	}
	return o.err
}

// handOff, called by the writer when it is done with a transaction of its
// own, starts a writer goroutine for the changes queued meanwhile, or marks
// the writer idle when there are none.
func (b *base) handOff() {
	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	if len(b.w.queue) == 0 {
		b.w.running = false
		return
	}
	go b.drain()
}

// drain writes the queue, a batch at a time, until it is empty, then marks
// the writer idle. Its callers wait in writeTx, where a context that ends
// is answered at once.
func (b *base) drain() {
	for {
		b.w.mu.Lock()
		n := min(len(b.w.queue), maxBatch)
		if n == 0 {
			b.w.running = false
			b.w.queue = nil
			b.w.mu.Unlock()
			return
		}
		batch := make([]*change, n)
		copy(batch, b.w.queue)
		b.w.queue = append(b.w.queue[:0], b.w.queue[n:]...)
		b.w.mu.Unlock()
		for len(batch) > 0 {
			batch = b.commitBatch(context.Background(), batch)
		}
	}
}

// answer gives c its result. The failure is classified with c's own context
// and message, as a transaction of its own would have been.
func (b *base) answer(c *change, err error) {
	give(c, outcome{err: b.fail(c.ctx, err, c.message)})
}

// give puts o where c's caller waits. A change has one answer; a second
// would mean the writer lost track of which changes it answered, so it
// stops with a message instead of blocking every write behind a full
// channel.
func give(c *change, o outcome) {
	select {
	case c.done <- o:
	default:
		panic("sqlite: a change was answered twice")
	}
}

// Savepoint statements. One name serves every change, since a change's
// savepoint is released or rolled back before the next one opens
// (sqlite.org/lang_savepoint.html).
const (
	savepointOpen     = "SAVEPOINT change"
	savepointRelease  = "RELEASE change"
	savepointRollback = "ROLLBACK TO change"
)

// commitBatch runs the changes of batch in one transaction and commits it,
// answering every change it can; it returns the changes that must run again
// in a new transaction.
//
// Every answer, success or failure, describes the database as committed.
// A change sees the writes of the changes before it in the transaction,
// committed or not, so what a change saw is only true once those writes
// are: a success is answered after the commit, and so is a failure that
// happened while another change's write was in the transaction (a taken
// name, a key found revoked, a row not found, each can come from a
// neighbor's write). A failure that happened while no other change's write
// was in the transaction saw only what was committed, and is answered at
// once.
//
// Each change runs in a savepoint when there are several: one that fails or
// panics is rolled back to its savepoint, which undoes its writes and
// nothing else, while the others go on to the commit. A savepoint costs two
// statements and no sync; the other way to isolate a failure, running the
// batch again without the failed change, would cost every change before it
// a second run and the whole batch a second transaction each time one
// fails. A change alone runs without one, exactly as a transaction of its
// own.
//
// When the transaction is undone instead of committed, what the changes in
// it saw may no longer be true, so the changes that were waiting for its
// outcome, written or failed, run again in a new transaction:
//
//   - Some errors make SQLite roll the whole transaction back on its own: an
//     interrupted INSERT, UPDATE or DELETE, which is how the driver stops a
//     statement whose context ended, and a full disk, an I/O error or memory
//     running out (sqlite.org/lang_transaction.html, "Response To Errors
//     Within A Transaction"; sqlite.org/c3ref/interrupt.html). Then the
//     savepoint is gone with it and rolling back to it fails. The change
//     whose statement did it is answered with that error, which is about its
//     own statement or its own context, and nothing of it is written; the
//     others, with those not yet run, are returned.
//   - When COMMIT fails, nothing of the transaction is written. The changes
//     written in it are answered with that error, which is true; those whose
//     failure was held are returned, since the writes they failed on are
//     gone.
//
// Each return leaves out at least one change of batch, so the caller's loop
// ends.
//
// txCtx is the context the transaction begins with, on whose end
// database/sql rolls the transaction back: the caller's own when the batch
// is its change alone, and otherwise one that never ends, since one caller
// giving up must not undo the others' changes.
func (b *base) commitBatch(txCtx context.Context, batch []*change) []*change {
	tx, err := b.p.write.BeginTx(txCtx, nil)
	if err != nil {
		for _, c := range batch {
			b.answer(c, err)
		}
		return nil
	}
	// Runs on every path out; after Commit it does nothing. A transaction
	// left open would keep the pool's only write connection, and the
	// database's write lock, for good.
	defer func() { _ = tx.Rollback() }()

	several := len(batch) > 1
	// waiting are the changes run in this transaction whose answers wait
	// for its outcome, in the order they ran; written counts the successes
	// among them.
	var waiting []*change
	written := 0
	// fail ends the transaction and answers every change not yet answered
	// with err, which is about the database itself and true of each: none
	// is written.
	fail := func(rest []*change, err error) []*change {
		_ = tx.Rollback()
		for _, c := range append(waiting, rest...) {
			b.answer(c, err)
		}
		return nil
	}
	for i, c := range batch {
		if !b.take(c) {
			continue
		}
		c.failed, c.held = false, outcome{}
		if several {
			if _, err := tx.Exec(savepointOpen); err != nil {
				return fail(batch[i:], err)
			}
		}
		o := run(c, tx)
		if o.err == nil && !o.panicked {
			if several {
				if _, err := tx.Exec(savepointRelease); err != nil {
					return fail(batch[i:], err)
				}
			}
			waiting = append(waiting, c)
			written++
			continue
		}
		if o.err != nil {
			o.err = b.fail(c.ctx, o.err, c.message)
		}
		if !several {
			_ = tx.Rollback()
			give(c, o)
			return nil
		}
		if _, err := tx.Exec(savepointRollback); err != nil {
			// The transaction is gone, with every write in it.
			_ = tx.Rollback()
			give(c, o)
			return append(waiting, batch[i+1:]...)
		}
		if _, err := tx.Exec(savepointRelease); err != nil {
			give(c, o)
			return fail(batch[i+1:], err)
		}
		if written == 0 {
			give(c, o)
			continue
		}
		c.failed, c.held = true, o
		waiting = append(waiting, c)
	}
	if written == 0 {
		return nil
	}
	if b.w.beforeCommit != nil {
		b.w.beforeCommit(waiting)
	}
	if err := tx.Commit(); err != nil {
		var again []*change
		for _, c := range waiting {
			if c.failed {
				again = append(again, c)
			} else {
				b.answer(c, err)
			}
		}
		return again
	}
	b.w.commits.Add(1)
	for _, c := range waiting {
		give(c, c.held)
	}
	return nil
}

// take reports whether c is to run now. A change whose caller has given up,
// or whose context has ended, is not run: one never run is answered with
// its context's error (its caller may have left already), and one run
// before, in a transaction since undone, is absent and answered the same.
func (b *base) take(c *change) bool {
	if c.ctx.Err() != nil {
		c.state.CompareAndSwap(changeQueued, changeAbandoned)
		b.answer(c, c.ctx.Err())
		return false
	}
	if c.ran {
		return true
	}
	if !c.state.CompareAndSwap(changeQueued, changeTaken) {
		return false
	}
	c.ran = true
	return true
}

// run runs c's function in tx, turning a panic into an outcome so that it
// reaches c's caller and not the writer, which serves every caller.
func run(c *change, tx *sql.Tx) (o outcome) {
	defer func() {
		if v := recover(); v != nil {
			o = outcome{panicked: true, value: v}
		}
	}()
	return outcome{err: c.fn(tx)}
}
