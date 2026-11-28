package receive

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// batchItem is one landed file and what its check decided.
type batchItem struct {
	l landed
	d decision
}

// batcher writes the rows of landed files in batches: received files with
// AddReceivedFiles and holds with HoldFiles, each batch one transaction,
// flushed when it reaches the batch size or after the batch interval, so the
// database takes a few writes a second however many files arrive. A file's
// sender was answered when the file was durable on disk; its row may come
// later, and a crash between the two leaves the file landed with no row,
// which Recover finds and checks again (incoming: the .new name).
//
// After a batch commits, each file's landed name becomes its recorded name
// (incoming.Area.Commit), so that from then on a restart that finds it with
// no row knows it was let go, never owed.
type batcher struct {
	p  *Pipeline
	in chan batchItem
	// queued counts the files handed to the batcher and not yet written
	// or put on the retry list.
	queued atomic.Int64

	mu    sync.Mutex
	retry []landed // files whose row could not be written yet
}

func newBatcher(p *Pipeline) *batcher {
	return &batcher{p: p, in: make(chan batchItem, 4*p.cfg.BatchSize)}
}

// submit hands a checked file to the batcher. It waits while the batcher's
// queue is full, which slows the senders down to what the database takes;
// when ctx ends first, the file waits on the retry list instead, never
// dropped.
func (b *batcher) submit(ctx context.Context, it batchItem) {
	b.queued.Add(1)
	select {
	case b.in <- it:
	case <-ctx.Done():
		b.queued.Add(-1)
		b.later(it.l)
	}
}

// idle reports whether the batcher holds nothing: no file queued, none
// waiting on the retry list.
func (b *batcher) idle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queued.Load() == 0 && len(b.retry) == 0
}

// pending reports whether a file has been handed to the batcher and its row
// is not written yet. The sender of such a file was answered already, so the
// reception it belongs to is not complete while this holds.
func (b *batcher) pending() bool { return b.queued.Load() > 0 }

// later puts a landed file on the retry list, checked again after the retry
// interval.
func (b *batcher) later(l landed) {
	b.mu.Lock()
	b.retry = append(b.retry, l)
	b.mu.Unlock()
}

// run is the batcher's loop. It ends when stop is closed, after writing what
// it holds.
func (b *batcher) run(stop <-chan struct{}) {
	ctx := context.Background()
	var pending []batchItem
	var timer <-chan time.Time
	retryTick := b.p.cfg.Clock.After(b.p.cfg.RetryInterval)
	flush := func() {
		if len(pending) > 0 {
			b.flush(ctx, pending)
			// Each file's row is written, and the database counts it
			// against the tenant's quota from now on; the gate keeps
			// counting it in memory until its next read of the use
			// has seen the row (quota.go).
			for _, it := range pending {
				it.l.quota.written()
			}
			b.queued.Add(-int64(len(pending)))
			pending = nil
		}
		timer = nil
	}
	for {
		select {
		case it := <-b.in:
			pending = append(pending, it)
			if timer == nil {
				timer = b.p.cfg.Clock.After(b.p.cfg.BatchInterval)
			}
			if len(pending) >= b.p.cfg.BatchSize {
				flush()
			}
		case <-timer:
			flush()
		case <-retryTick:
			for _, it := range b.recheck(ctx) {
				b.queued.Add(1)
				pending = append(pending, it)
				if len(pending) >= b.p.cfg.BatchSize {
					flush()
				}
			}
			flush()
			retryTick = b.p.cfg.Clock.After(b.p.cfg.RetryInterval)
		case <-stop:
			for {
				select {
				case it := <-b.in:
					pending = append(pending, it)
					if len(pending) >= b.p.cfg.BatchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

// recheck checks again every file on the retry list and returns those it
// could decide; the rest stay on the list.
func (b *batcher) recheck(ctx context.Context) []batchItem {
	b.mu.Lock()
	list := b.retry
	b.retry = nil
	b.mu.Unlock()
	var out []batchItem
	for _, l := range list {
		d, err := b.p.check(ctx, l)
		if err != nil {
			b.later(l)
			continue
		}
		out = append(out, batchItem{l: l, d: d})
	}
	return out
}

// flush writes one batch: the holds, then the received files.
func (b *batcher) flush(ctx context.Context, items []batchItem) {
	var holds, received []batchItem
	for _, it := range items {
		if it.d.hold != nil {
			holds = append(holds, it)
		} else {
			received = append(received, it)
		}
	}
	if len(holds) > 0 {
		b.writeHolds(ctx, holds)
	}
	if len(received) > 0 {
		b.writeReceived(ctx, received)
	}
}

// writeHolds records the holds, then gives each file its recorded name.
func (b *batcher) writeHolds(ctx context.Context, items []batchItem) {
	records := make([]tenant.QuarantinedFile, len(items))
	for i, it := range items {
		records[i] = *it.d.hold
	}
	b.p.at("batch.before-hold")
	written, err := b.p.store.HoldFiles(ctx, records, nil)
	if err != nil {
		b.p.log.Warn("received files could not be held; they are kept and tried again", "files", len(items), "reason", safe(err))
		for _, it := range items {
			b.later(it.l)
		}
		return
	}
	b.p.at("batch.after-hold")
	b.p.wake()
	for _, it := range items {
		if written < len(items) && !b.heldAsWritten(ctx, *it.d.hold, it.l.file.Path) {
			// The store skipped this record: its id was taken by a record
			// that does not keep this file (one released, say). Its bytes
			// are touched no further; they keep their landed name, which
			// the next start checks again.
			b.p.log.Error("a held file's record was not written, since its id is taken by another; the file keeps its landed name",
				"record", it.d.hold.ID)
			continue
		}
		b.p.stats.Held.Add(1)
		if !it.d.hold.ContentKept {
			// A record that keeps no bytes: they go now that it is
			// written. A crash first leaves the landed file with no
			// row keeping it; a restart checks it again, writes the
			// same record (its id is made from the file's), and
			// removes it.
			if err := b.p.area.Remove(it.l.file.Path); err != nil {
				b.p.log.Warn("the bytes of a file held without them could not be removed; a restart removes them", "reason", safe(err))
			}
			continue
		}
		b.commitName(it.l)
	}
}

// writeReceived records the received files. A batch is all or none in the
// store, so one whose reception closed meanwhile (ErrReceptionClosed) is
// split by reception: the others are written, and the files of the closed
// one are checked again, which finds them the study's next arrival. Before
// the write, the catalog is asked whether it already holds each file
// (CheckDuplicates): a file it holds under the same UID and checksum is
// still recorded, so its reception counts it received and its archive step
// counts it a duplicate, but its bytes are let go once its row is written,
// since nothing will ever be written from them.
func (b *batcher) writeReceived(ctx context.Context, items []batchItem) {
	early := b.earlyDuplicates(ctx, items)
	files := make([]tenant.ReceivedFile, len(items))
	for i, it := range items {
		files[i] = *it.d.received
	}
	b.p.at("batch.before-add")
	added, err := b.p.store.AddReceivedFiles(ctx, files, b.p.now(), nil)
	if errors.Is(err, tenant.ErrReceptionClosed) && len(items) > 1 {
		byReception := map[string][]batchItem{}
		var order []string
		for _, it := range items {
			id := it.d.received.ReceptionID
			if _, ok := byReception[id]; !ok {
				order = append(order, id)
			}
			byReception[id] = append(byReception[id], it)
		}
		if len(order) > 1 {
			for _, id := range order {
				b.writeReceived(ctx, byReception[id])
			}
			return
		}
	}
	if errors.Is(err, tenant.ErrReceptionClosed) {
		closed := items[0].d.received.ReceptionID
		b.p.receptions.drop(closed)
		var again []batchItem
		for _, it := range items {
			d, cerr := b.p.check(ctx, it.l)
			if cerr != nil || (d.received != nil && d.received.ReceptionID == closed) {
				b.later(it.l)
				continue
			}
			again = append(again, batchItem{l: it.l, d: d})
		}
		if len(again) > 0 {
			b.flush(ctx, again)
		}
		return
	}
	if err != nil {
		b.p.log.Warn("received files could not be recorded; they are kept and tried again", "files", len(items), "reason", safe(err))
		for _, it := range items {
			b.later(it.l)
		}
		return
	}
	b.p.at("batch.after-add")
	skipped := map[int]bool{}
	if added < len(items) {
		skipped = b.skipped(ctx, items)
	}
	for i, it := range items {
		if skipped[i] {
			// The store skipped it: its reception already has a received
			// file of this UID and checksum, a copy sent again before
			// the first was acknowledged. This copy is let go.
			if err := b.p.area.Remove(it.l.file.Path); err != nil {
				b.p.log.Warn("the bytes of a file received twice could not be removed; a restart removes them", "reason", safe(err))
			}
			continue
		}
		b.p.stats.Recorded.Add(1)
		if early[i] {
			b.p.stats.EarlyDuplicates.Add(1)
			if err := b.p.area.Remove(it.l.file.Path); err != nil {
				b.p.log.Warn("the bytes of a file the catalog holds could not be removed; a restart removes them", "reason", safe(err))
				b.commitName(it.l)
			}
			continue
		}
		b.commitName(it.l)
	}
}

// skipped returns, by index, the files of a written batch whose rows the
// store skipped because their reception already had a received file of the
// same UID and checksum at another place in the incoming area. It lists the
// receptions concerned, which happens only when a batch wrote fewer rows
// than it was given. A failure to list answers none: the copies are then
// left for a restart to remove.
func (b *batcher) skipped(ctx context.Context, items []batchItem) map[int]bool {
	type key struct {
		reception, uid string
		sum            tenant.SHA256
	}
	where := map[key]string{}
	listed := map[string]bool{}
	out := map[int]bool{}
	for i, it := range items {
		f := it.d.received
		if !listed[f.ReceptionID] {
			listed[f.ReceptionID] = true
			err := b.p.eachReceived(ctx, f.ReceptionID, func(r tenant.ReceivedFile) error {
				where[key{r.ReceptionID, r.SOPInstanceUID, r.SHA256}] = r.Incoming
				return nil
			})
			if err != nil {
				return map[int]bool{}
			}
		}
		if at, ok := where[key{f.ReceptionID, f.SOPInstanceUID, f.SHA256}]; ok && at != f.Incoming {
			out[i] = true
		}
	}
	return out
}

// earlyDuplicates asks the catalog about each file and returns, by index,
// those it holds under the same UID, study, series and checksum, but those
// whose instance a scrub marked damaged. A failure to ask answers none: the
// claim is the authority, and decides them later.
func (b *batcher) earlyDuplicates(ctx context.Context, items []batchItem) []bool {
	out := make([]bool, len(items))
	qs := make([]tenant.DuplicateQuery, len(items))
	for i, it := range items {
		f := it.d.received
		qs[i] = tenant.DuplicateQuery{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	ds, err := b.p.store.CheckDuplicates(ctx, qs)
	if err != nil || len(ds) != len(items) {
		return out
	}
	var same []string
	for i, d := range ds {
		out[i] = d.Kind == tenant.SameFile
		if out[i] {
			same = append(same, qs[i].SOPInstanceUID)
		}
	}
	if len(same) == 0 {
		return out
	}
	// The same file as an instance a scrub marked damaged keeps its bytes:
	// the archive step holds it as one that repairs the instance. A failure
	// to ask keeps every one: the archive step decides.
	marks, err := b.p.store.DamagedInstances(ctx, same)
	for i := range out {
		if _, marked := marks[qs[i].SOPInstanceUID]; out[i] && (err != nil || marked) {
			out[i] = false
		}
	}
	return out
}

// commitName gives a file whose row has committed its recorded name. A
// failure leaves the landed name, which a restart commits.
func (b *batcher) commitName(l landed) {
	if err := b.p.area.Commit(l.file.Path); err != nil {
		b.p.log.Warn("a recorded file keeps its landed name until a restart", "reason", safe(err))
	}
}

// heldAsWritten reports whether the store holds the record rec as this
// batch wrote it, for the bytes at path: held, keeping its bytes (and at
// path) when rec does. A record the store skipped as already held is the
// same record when it is, since its id is made from the file's, and another
// one when its id was taken by a record in another state.
func (b *batcher) heldAsWritten(ctx context.Context, rec tenant.QuarantinedFile, path string) bool {
	q, err := b.p.store.QuarantinedFile(ctx, rec.ID)
	if err != nil {
		return false
	}
	if q.State != tenant.QuarantineHeld || q.ContentKept != rec.ContentKept {
		return false
	}
	return !rec.ContentKept || q.Incoming == path
}
