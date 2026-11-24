package access

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// markTimeout bounds one last-use write, so a master that hangs holds the
// worker for this long at most and the queue drops instead of growing.
const markTimeout = 5 * time.Second

// markEvery is how stale a key's last-use time must be before a check asks
// for a new one: the store's own throttle (master.Store.MarkKeyUsed). A
// check that skips the call spares the master's single write connection,
// which the call takes even when its throttle writes nothing.
const markEvery = time.Minute

// The kinds the last-use worker's accounts count.
const (
	markWritten = "written"
	markDropped = "dropped"
	markFailed  = "failed"
)

// mark is one last-use write waiting for the worker.
type mark struct {
	keyID   string
	at      time.Time
	address string
}

// MarkStats counts what became of the last-use marks since New. A check
// whose key was marked within the minute asks for none, and is not counted.
// Every mark asked for is in exactly one count.
type MarkStats struct {
	// Written is how many marks the store took, whether or not its own
	// throttle let them write.
	Written uint64
	// Dropped is how many marks found the queue full, came after Close
	// began, or were left when Close's deadline passed.
	Dropped uint64
	// Failed is how many marks the store refused or failed.
	Failed uint64
}

// MarkStats returns the counts of last-use marks, so a server can report a
// worker that falls behind.
func (a *Access) MarkStats() MarkStats {
	return MarkStats{
		Written: a.markAcct.get(markWritten),
		Dropped: a.markAcct.get(markDropped),
		Failed:  a.markAcct.get(markFailed),
	}
}

// needsMark reports whether a key last used at lastUsed should be marked
// used at now: never marked, or marked a minute or more before.
func needsMark(lastUsed, now time.Time) bool {
	return lastUsed.IsZero() || now.Sub(lastUsed) >= markEvery
}

// queueMark hands a last-use mark to the worker without waiting: when the
// queue is full, or Close has begun, the mark is dropped and counted. The
// mark is to within a minute anyway, and the next request with the key asks
// again, so a drop delays the mark and never the request.
//
// address is the caller's address as the request's audit entry records it,
// written beside the time (master.Store.MarkKeyUsed).
func (a *Access) queueMark(k master.Key, at time.Time, address string) {
	if !needsMark(k.LastUsed, at) {
		return
	}
	a.sendMu.RLock()
	defer a.sendMu.RUnlock()
	if a.closed {
		a.markAcct.add(markDropped, 1)
		return
	}
	select {
	case a.marks <- mark{keyID: k.ID, at: at, address: address}:
	default:
		a.markAcct.add(markDropped, 1)
	}
}

// markWorker writes queued marks one at a time until Close, then writes
// what is still queued. The channel is never closed: a sender checks closed
// under sendMu instead.
func (a *Access) markWorker() {
	defer close(a.marksDone)
	for {
		select {
		case m := <-a.marks:
			a.writeMark(m)
		case <-a.stop:
			for {
				select {
				case m := <-a.marks:
					a.writeMark(m)
				default:
					return
				}
			}
		}
	}
}

// writeMark writes one mark, bounded by markTimeout. A failure is counted
// and logged with the key's id and the error's kind, never returned: no
// request waits for it. Once Close has canceled the workers, what is left
// is dropped without a call.
func (a *Access) writeMark(m mark) {
	if !a.markAcct.take(markDropped) {
		return
	}
	if a.workCtx.Err() != nil {
		a.markAcct.resolve(markDropped, 1)
		return
	}
	ctx, cancel := context.WithTimeout(a.workCtx, markTimeout)
	defer cancel()
	if _, err := a.store.MarkKeyUsed(ctx, m.keyID, m.at, m.address); err != nil {
		a.markAcct.resolve(markFailed, 1)
		if !errors.Is(err, context.Canceled) {
			a.log.Warn("access: a key's last use was not recorded", "key_id", m.keyID, "kind", fault.KindOf(err).String(), "error", err)
		}
		return
	}
	a.markAcct.resolve(markWritten, 1)
}
