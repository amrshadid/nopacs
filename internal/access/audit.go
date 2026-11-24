package access

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// dropNoticeEvery is how often at most the audit writer writes an entry
// saying entries were dropped. The first drop after this long without a
// notice is reported at the next tick; drops in between are counted into
// the next notice, and Close writes a last one for what is left, so every
// drop before Close is reported, and a flood of drops costs one entry a
// minute.
const dropNoticeEvery = time.Minute

// The method and details of the entries the audit writer writes itself.
const (
	dropNoticeMethod = "access.AuditWriter"
	detailDropped    = "dropped"
	// DetailRerouted is the detail an entry carries when it was meant for a
	// tenant's log, which did not answer, and was written to the master's log
	// instead, so the record of an allowed request is not lost with the
	// tenant's database.
	DetailRerouted = "audit_rerouted"
	reroutedValue  = "the tenant's log did not answer; written to the installation's log"
)

// The kinds the audit writer's accounts count.
const (
	auditWritten         = "written"
	auditFailed          = "failed"
	auditDroppedRefusals = "dropped refusals"
	auditDroppedRecords  = "dropped records"
	auditDroppedWrites   = "dropped writes"
	auditRerouted        = "rerouted"
	auditNotices         = "notices"
)

// Sink is an audit log the writer appends to: the master's, or a tenant's.
// Both master.Store and the tenant database's store satisfy it. A Sink must
// be comparable (a pointer, as both stores are), since the writer batches
// entries per sink; RecordTo refuses one that is not.
type Sink interface {
	// AppendAudit writes entries, all or none.
	AppendAudit(ctx context.Context, entries ...master.AuditEntry) error
}

// Facts are what a handler learned about a request after the check, which
// the check could not know: the server does not read a request's body
// before the check, so for a method with a fixed requirement the check's
// entry has no object and no tenant. A non-empty field replaces the check's;
// it is escaped and bounded as the check's facts are. The identity of the
// request (who made it, when, the method, the address and the request id)
// is the check's and cannot be changed through Facts.
type Facts struct {
	// Object is what the request acted on, such as a key's id or a
	// setting's name.
	Object string
	// TenantName is the tenant the request named, as it named it.
	TenantName string
	// Details replace the check's details when not nil. At most
	// MaxRequestDetails pairs are kept, the first in order of key.
	Details map[string]string
}

// apply returns e with f's non-empty fields in place of e's, escaped and
// bounded.
func (f Facts) apply(e master.AuditEntry) master.AuditEntry {
	if f.Object != "" {
		e.Object = auditText(f.Object, master.MaxAuditFieldLength)
	}
	if f.TenantName != "" {
		e.TenantName = clipTenantName(f.TenantName)
	}
	if f.Details != nil {
		keys := slices.Sorted(maps.Keys(f.Details))
		e.Details = make(map[string]string, min(len(keys), MaxRequestDetails))
		for _, k := range keys {
			if len(e.Details) == MaxRequestDetails {
				break
			}
			ck := auditText(k, master.MaxAuditDetailKeyLength)
			if ck == "" {
				continue
			}
			e.Details[ck] = auditText(f.Details[k], master.MaxAuditDetailValueLength)
		}
	}
	return e
}

// auditItem is an allowed request's entry waiting for the writer, with its
// destination.
type auditItem struct {
	sink  Sink
	entry master.AuditEntry
}

// AuditStats counts what became of the audit entries handed to the audit
// writer since New: refusals, and the allowed requests' entries given to
// Record and RecordTo. The entries of changes are not here: each is written
// in its change's transaction. Every entry handed in is in exactly one of
// Written, Failed, DroppedRefusals, DroppedRecords and DroppedWrites.
type AuditStats struct {
	// Written is how many entries a log took.
	Written uint64
	// Failed is how many entries a log refused one by one (an entry not
	// valid, an id already taken), or that were not valid entries when
	// handed in.
	Failed uint64
	// DroppedRefusals is how many refusals' entries found their queue full
	// or came after Close began.
	DroppedRefusals uint64
	// DroppedRecords is how many allowed requests' entries found their
	// queue full or came after Close began.
	DroppedRecords uint64
	// DroppedWrites is how many entries were taken by the writer and then
	// given up: the master did not answer after the retries, an append ran
	// out of time, or Close's deadline passed.
	DroppedWrites uint64
	// Rerouted is how many entries meant for a tenant's log were sent to
	// the master's because the tenant's did not answer. They are counted
	// again in Written, Failed or DroppedWrites, by what the master did.
	Rerouted uint64
	// Notices is how many entries the writer wrote to say entries were
	// dropped. They are not entries handed in and are in no other count.
	Notices uint64
}

// Dropped returns all the entries dropped, of every cause.
func (s AuditStats) Dropped() uint64 {
	return s.DroppedRefusals + s.DroppedRecords + s.DroppedWrites
}

// AuditStats returns the counts of the audit writer, so a server can report
// entries it dropped or could not write.
func (a *Access) AuditStats() AuditStats {
	c := &a.auditAcct
	return AuditStats{
		Written:         c.get(auditWritten),
		Failed:          c.get(auditFailed),
		DroppedRefusals: c.get(auditDroppedRefusals),
		DroppedRecords:  c.get(auditDroppedRecords),
		DroppedWrites:   c.get(auditDroppedWrites),
		Rerouted:        c.get(auditRerouted),
		Notices:         c.get(auditNotices),
	}
}

// Record hands an audit entry of an allowed request that changed nothing (a
// read, or a change that failed) to the audit writer for the master's log:
// the request concerned the installation. facts adds what the handler
// learned after the check. A change does not use it: it writes d.Entry with
// the change, in one transaction. Record never blocks and never fails; an
// entry it cannot queue is dropped and counted (AuditStats).
func (a *Access) Record(d Decision, outcome master.Outcome, reason string, facts Facts) {
	a.enqueueRecord(a.store, d.Entry(outcome, reason, facts))
}

// RecordTo is Record for an allowed request that concerned a tenant: its
// entry goes to that tenant's log, sink, beside the data it concerns. If the
// tenant's log does not answer, the entry is written to the master's log
// instead, with the detail DetailRerouted and the tenant's name. A sink that
// is not comparable is refused: the entry is counted as failed and logged.
func (a *Access) RecordTo(sink Sink, d Decision, outcome master.Outcome, reason string, facts Facts) {
	e := d.Entry(outcome, reason, facts)
	if sink == nil {
		a.enqueueRecord(a.store, e)
		return
	}
	if !reflect.TypeOf(sink).Comparable() {
		a.auditAcct.add(auditFailed, 1)
		a.log.Error("access: an audit sink is not comparable; the entry was not written", "audit_id", e.ID, "method", e.Method)
		return
	}
	a.enqueueRecord(sink, e)
}

// FlushAudit waits until the audit writer has written, or given up, every
// entry handed to it before the call, or until ctx is done. A server calls
// it before a backup of the master; a test calls it to read what a refusal
// wrote. After Close it returns at once.
func (a *Access) FlushAudit(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case a.auditFlush <- reply:
	case <-a.auditDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-a.auditDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// valid counts and logs an entry the master would refuse, which must not
// join a batch it would fail, and reports whether e is valid.
func (a *Access) valid(e master.AuditEntry) bool {
	if err := e.Validate(); err != nil {
		a.auditAcct.add(auditFailed, 1)
		a.log.Error("access: an audit entry is not valid and was not written", "audit_id", e.ID, "method", e.Method, "error", err)
		return false
	}
	return true
}

// enqueueRefusal hands a refusal's entry to the writer without waiting.
// When its queue is full, or Close has begun, it is dropped and counted.
func (a *Access) enqueueRefusal(e master.AuditEntry) {
	if !a.valid(e) {
		return
	}
	a.sendMu.RLock()
	defer a.sendMu.RUnlock()
	if a.closed {
		a.auditAcct.add(auditDroppedRefusals, 1)
		return
	}
	select {
	case a.refusals <- e:
	default:
		a.auditAcct.add(auditDroppedRefusals, 1)
	}
}

// enqueueRecord hands an allowed request's entry to the writer without
// waiting, on its own queue, so refusals never push it out. When the queue
// is full, or Close has begun, it is dropped and counted.
func (a *Access) enqueueRecord(sink Sink, e master.AuditEntry) {
	if !a.valid(e) {
		return
	}
	a.sendMu.RLock()
	defer a.sendMu.RUnlock()
	if a.closed {
		a.auditAcct.add(auditDroppedRecords, 1)
		return
	}
	select {
	case a.records <- auditItem{sink: sink, entry: e}:
	default:
		a.auditAcct.add(auditDroppedRecords, 1)
	}
}

// auditWriter is the audit writer: it gathers queued entries into a batch
// per sink and appends a batch when it is full, when the interval ticks,
// when FlushAudit asks, and when Close stops it, after taking what is
// queued.
func (a *Access) auditWriter() {
	defer close(a.auditDone)
	w := &batchWriter{a: a}
	tick := time.NewTicker(a.auditInterval)
	defer tick.Stop()
	for {
		select {
		case e := <-a.refusals:
			w.add(a.store, e)
		case it := <-a.records:
			w.add(it.sink, it.entry)
		case <-tick.C:
			w.flushAll()
			w.notice(false)
		case reply := <-a.auditFlush:
			w.drain()
			w.flushAll()
			w.notice(false)
			close(reply)
		case <-a.stop:
			w.drain()
			w.flushAll()
			w.notice(true)
			return
		}
	}
}

// sinkBatch is the entries waiting for one sink.
type sinkBatch struct {
	sink    Sink
	entries []master.AuditEntry
}

// batchWriter is the audit writer's state, used by its goroutine only.
type batchWriter struct {
	a          *Access
	batches    []*sinkBatch
	noticed    uint64
	lastNotice time.Time
}

// add puts e in its sink's batch, and writes the batch when it is full.
func (w *batchWriter) add(sink Sink, e master.AuditEntry) {
	if !w.a.auditAcct.take(auditDroppedWrites) {
		return
	}
	var b *sinkBatch
	for _, c := range w.batches {
		if c.sink == sink {
			b = c
			break
		}
	}
	if b == nil {
		b = &sinkBatch{sink: sink}
		w.batches = append(w.batches, b)
	}
	b.entries = append(b.entries, e)
	if len(b.entries) >= w.a.auditBatch {
		w.write(b.sink, b.entries)
		b.entries = nil
	}
}

// drain takes every entry queued now, of both kinds, writing each batch
// that fills.
func (w *batchWriter) drain() {
	for {
		select {
		case e := <-w.a.refusals:
			w.add(w.a.store, e)
		case it := <-w.a.records:
			w.add(it.sink, it.entry)
		default:
			return
		}
	}
}

// flushAll writes every batch and forgets the sinks, so the list does not
// grow with the number of tenants ever written to.
func (w *batchWriter) flushAll() {
	for _, b := range w.batches {
		if len(b.entries) > 0 {
			w.write(b.sink, b.entries)
		}
	}
	w.batches = w.batches[:0]
}

// isMaster reports whether sink is the master's log.
func (w *batchWriter) isMaster(sink Sink) bool {
	return sink == Sink(w.a.store)
}

// write appends entries to sink and counts how each ended.
//
//   - Done: Written.
//   - A failure of an entry (Invalid, or master.ErrDuplicateAuditID): each
//     entry is appended alone, once, so the one the log refuses is Failed
//     and the others are written. A refused entry is never tried again.
//   - Unavailable or another failure of the store: the batch is tried
//     again, up to the retries, waiting the backoff, doubled each time,
//     between tries; never as single entries.
//   - An append that ran out of time, or the retries spent: the batch is
//     given up. A tenant's batch goes to the master's log instead; the
//     master's is DroppedWrites. No batch is tried again after a timeout,
//     so a store that hangs holds the writer one timeout per batch.
func (w *batchWriter) write(sink Sink, entries []master.AuditEntry) {
	a := w.a
	backoff := a.auditBackoff
	for attempt := 0; ; attempt++ {
		if a.workCtx.Err() != nil {
			a.auditAcct.resolve(auditDroppedWrites, len(entries))
			return
		}
		timedOut, err := a.appendWithin(sink, entries...)
		switch {
		case err == nil:
			a.auditAcct.resolve(auditWritten, len(entries))
			return
		case entryFailure(err):
			w.oneByOne(sink, entries)
			return
		case timedOut || attempt >= a.auditRetries:
			w.giveUp(sink, entries, err)
			return
		}
		if !w.wait(backoff) {
			w.giveUp(sink, entries, err)
			return
		}
		backoff *= 2
	}
}

// oneByOne appends each entry alone after a batch failed for one of its
// entries. The first failure that is not an entry's (the store stopped
// answering) gives up the rest as one batch: never one call per entry for a
// store that is down.
func (w *batchWriter) oneByOne(sink Sink, entries []master.AuditEntry) {
	a := w.a
	for i, e := range entries {
		if a.workCtx.Err() != nil {
			a.auditAcct.resolve(auditDroppedWrites, len(entries)-i)
			return
		}
		_, err := a.appendWithin(sink, e)
		switch {
		case err == nil:
			a.auditAcct.resolve(auditWritten, 1)
		case entryFailure(err):
			a.auditAcct.resolve(auditFailed, 1)
			a.log.Error("access: an audit entry could not be written", "audit_id", e.ID, "method", e.Method,
				"outcome", string(e.Outcome), "reason", e.Reason, "kind", fault.KindOf(err).String(), "error", err)
		default:
			w.giveUp(sink, entries[i:], err)
			return
		}
	}
}

// giveUp ends a batch its sink did not take: a tenant's goes to the master's
// log, each entry marked with DetailRerouted so it is not lost; the master's
// is dropped, counted and logged once.
func (w *batchWriter) giveUp(sink Sink, entries []master.AuditEntry, err error) {
	a := w.a
	if a.workCtx.Err() != nil {
		// Close's deadline cut the batch short: it is dropped, not moved
		// to the master's log, which Close no longer waits for either.
		a.auditAcct.resolve(auditDroppedWrites, len(entries))
		return
	}
	if !w.isMaster(sink) {
		moved := make([]master.AuditEntry, len(entries))
		for i, e := range entries {
			e.Details = maps.Clone(e.Details)
			if e.Details == nil {
				e.Details = map[string]string{}
			}
			if len(e.Details) < master.MaxAuditDetails {
				e.Details[DetailRerouted] = reroutedValue
			}
			moved[i] = e
		}
		a.auditAcct.add(auditRerouted, len(moved))
		a.log.Warn("access: a tenant's audit log did not answer; its entries go to the installation's log",
			"entries", len(moved), "kind", fault.KindOf(err).String(), "error", err)
		w.write(a.store, moved)
		return
	}
	a.auditAcct.resolve(auditDroppedWrites, len(entries))
	a.log.Error("access: the installation's audit log did not take a batch; its entries are dropped",
		"entries", len(entries), "kind", fault.KindOf(err).String(), "error", err)
}

// wait waits d, and reports false when Close's deadline cancels the wait.
func (w *batchWriter) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.a.workCtx.Done():
		return false
	}
}

// entryFailure reports whether err is a failure of an entry, not of the
// store: an entry the log refuses whatever the retry.
func entryFailure(err error) bool {
	return fault.KindOf(err) == fault.Invalid || errors.Is(err, master.ErrDuplicateAuditID)
}

// appendWithin appends entries to sink, bounded by the audit timeout, and
// reports whether the time ran out.
func (a *Access) appendWithin(sink Sink, entries ...master.AuditEntry) (timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(a.workCtx, a.auditTimeout)
	defer cancel()
	err = sink.AppendAudit(ctx, entries...)
	return err != nil && ctx.Err() != nil, err
}

// notice writes the entry that reports drops not yet reported, when one is
// due: at most one a minute, and always at the end (final), when Close
// stops the writer. The time of the last notice, and what it reported, move
// only once the notice is written, so a notice the master did not take is
// tried again.
func (w *batchWriter) notice(final bool) {
	a := w.a
	dropped := a.AuditStats().Dropped()
	if dropped <= w.noticed || a.workCtx.Err() != nil {
		return
	}
	now := a.clock()
	if !final && !w.lastNotice.IsZero() && now.Sub(w.lastNotice) < dropNoticeEvery {
		return
	}
	id := a.newID()
	if master.ValidateID("audit entry id", id) != nil {
		return
	}
	n := strconv.FormatUint(dropped-w.noticed, 10)
	e := master.AuditEntry{
		ID: id, Time: now, Method: dropNoticeMethod, Outcome: master.OutcomeFailed,
		Reason:  n + " audit entries of refusals or reads were dropped: a queue was full, a log did not answer, or the server was closing",
		Details: map[string]string{detailDropped: n},
	}
	if _, err := a.appendWithin(a.store, e); err != nil {
		a.log.Error("access: the notice of dropped audit entries could not be written", "dropped", n, "error", err)
		return
	}
	w.noticed, w.lastNotice = dropped, now
	a.auditAcct.add(auditNotices, 1)
}
