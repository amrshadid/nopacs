package tenanttest

// The feed's cursors and what pruning does to them.

import (
	"errors"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testEventCursors guards the cursor: bound to the tenant and the feed (a
// cursor of another tenant's feed, of another list, altered or cut short is
// Invalid), and EventCursor starts a reader after every event committed.
func testEventCursors(e *eenv) {
	e.appendAll(e.health("a", 1), e.health("a", 2))
	now := e.now()
	if got := e.read(now, 10); len(got.Events) != 0 {
		e.Errorf("a read from the current cursor answered %d events", len(got.Events))
	}
	e.appendAll(e.health("a", 3))
	if got := e.read(now, 10); len(got.Events) != 1 || !got.Events[0].Time.Equal(at(3)) {
		e.Errorf("a read from the cursor before the last append answered %d events", len(got.Events))
	}
	if all, _ := e.readAll("", 10); len(all) != 3 {
		e.Errorf("a read from the empty cursor answered %d events, want every one kept", len(all))
	}
	other := e.openEv(e.T, OtherTenant)
	oc, err := other.EventCursor(e.ctx)
	e.ok("EventCursor of another tenant", err)
	_, err = e.ev.ReadEvents(e.ctx, oc, nil, 10)
	e.wantIs("ReadEvents with another tenant's cursor", err, tenant.ErrForeignCursor, fault.Invalid)
	_, err = other.ReadEvents(e.ctx, now, nil, 10)
	e.wantIs("ReadEvents of another tenant with this one's cursor", err, tenant.ErrForeignCursor, fault.Invalid)
	e.addReceiver("a", "store", 11112)
	e.openReception("a", 1, at(1))
	e.openReception("a", 2, at(2))
	_, list, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1})
	e.ok("ListReceptions", err)
	_, err = e.ev.ReadEvents(e.ctx, list, nil, 10)
	e.wantKind("ReadEvents with a list's cursor", err, fault.Invalid)
	if errors.Is(err, tenant.ErrForeignCursor) {
		e.Error("a list's cursor is answered as another tenant's feed's")
	}
	for _, bad := range []string{now[:len(now)-2], now + "A", "not a cursor", strings.Repeat("A", 2000)} {
		_, err = e.ev.ReadEvents(e.ctx, bad, nil, 10)
		e.wantKind("ReadEvents with an altered cursor", err, fault.Invalid)
		if errors.Is(err, tenant.ErrForeignCursor) {
			e.Error("an altered cursor is answered as another tenant's feed's")
		}
	}
	// A cursor after the newest event the feed holds, as a database
	// restored from an older backup meets: another database of the same
	// tenant with more events makes one.
	restored := e.openEv(e.T, SuiteTenant)
	var batch []tenant.EventRecord
	for i := range 10 {
		batch = append(batch, e.health("a", 100+i))
	}
	e.appendTo(restored, batch...)
	ahead, err := restored.EventCursor(e.ctx)
	e.ok("EventCursor", err)
	_, err = e.ev.ReadEvents(e.ctx, ahead, nil, 10)
	e.wantIs("ReadEvents with a cursor after the newest event", err, tenant.ErrCursorExpired, fault.OutOfRange)
}

// testEventsExpire guards pruning: it removes, in order and in bounded
// batches, every event up to the newest checkpoint older than its bound,
// says whether more are due, stops at the checkpoint whatever the events'
// own times, and a cursor it passed is refused with ErrCursorExpired while
// one at its mark or after reads on.
func testEventsExpire(e *eenv) {
	start := e.now()
	var batch []tenant.EventRecord
	for i := range 6 {
		batch = append(batch, e.health("a", i))
	}
	e.appendAll(batch...)
	e.ok("CheckpointEvents after six", e.ev.CheckpointEvents(e.ctx, at(100)))
	batch = nil
	for i := range 4 {
		batch = append(batch, e.health("a", 6+i))
	}
	e.appendAll(batch...)
	e.ok("CheckpointEvents after ten", e.ev.CheckpointEvents(e.ctx, at(200)))
	first := e.read(start, 3)
	sixth := e.read(start, 6)
	n, more, err := e.ev.PruneEvents(e.ctx, at(50), 100)
	e.ok("PruneEvents before every checkpoint", err)
	if n != 0 || more {
		e.Errorf("a pruning before every checkpoint removed %d (more %v)", n, more)
	}
	n, more, err = e.ev.PruneEvents(e.ctx, at(150), 2)
	e.ok("PruneEvents", err)
	if n != 2 || !more {
		e.Errorf("a pruning bounded to 2 removed %d (more %v), want 2 and more", n, more)
	}
	if got := e.read(first.Cursor, 100); len(got.Events) != 7 {
		e.Errorf("a cursor past what was pruned read %d events, want 7", len(got.Events))
	}
	n, more, err = e.ev.PruneEvents(e.ctx, at(150), 100)
	e.ok("PruneEvents", err)
	if n != 4 || more {
		e.Errorf("the second pruning removed %d (more %v), want 4 and no more: up to the first checkpoint", n, more)
	}
	_, err = e.ev.ReadEvents(e.ctx, first.Cursor, nil, 10)
	e.wantIs("ReadEvents from a cursor that pruning passed", err, tenant.ErrCursorExpired, fault.OutOfRange)
	_, err = e.ev.ReadEvents(e.ctx, start, []tenant.EventType{tenant.EventStorageSpace}, 10)
	e.wantIs("a filtered ReadEvents from before every event pruned", err, tenant.ErrCursorExpired, fault.OutOfRange)
	if got := e.read(sixth.Cursor, 100); len(got.Events) != 4 || !got.Events[0].Time.Equal(at(6)) {
		e.Errorf("a cursor at the pruning's mark read %d events, want 4 from 6 s", len(got.Events))
	}
	if got, _ := e.readAll("", 100); len(got) != 4 {
		e.Errorf("the empty cursor read %d events, want the 4 kept", len(got))
	}
	n, more, err = e.ev.PruneEvents(e.ctx, at(150), 100)
	e.ok("PruneEvents with nothing more under the checkpoint", err)
	if n != 0 || more {
		e.Errorf("a pruning with nothing more under its checkpoint removed %d (more %v)", n, more)
	}
	n, _, err = e.ev.PruneEvents(e.ctx, at(1000), 100)
	e.ok("PruneEvents up to the last checkpoint", err)
	if n != 4 {
		e.Errorf("pruning up to the last checkpoint removed %d, want 4", n)
	}
	// With every event gone, the feed's place does not go back: a new
	// event is after every cursor made before.
	now := e.now()
	_, err = e.ev.ReadEvents(e.ctx, sixth.Cursor, nil, 10)
	e.wantIs("ReadEvents from a cursor before an emptied feed's mark", err, tenant.ErrCursorExpired, fault.OutOfRange)
	e.appendAll(e.health("a", 2000))
	if got := e.read(now, 10); len(got.Events) != 1 || !got.Events[0].Time.Equal(at(2000)) {
		e.Errorf("after the feed was emptied a new event read %d events from the cursor before it", len(got.Events))
	}
}

// testEventCheckpoints guards the checkpoints retention measures by: an
// idle feed adds none, so an event committed after a checkpoint is never
// pruned by it; one earlier than the newest is a precondition failure, as
// is one at the same time with events since; and a time out of range is
// invalid.
func testEventCheckpoints(e *eenv) {
	e.appendAll(e.health("a", 1))
	e.ok("CheckpointEvents", e.ev.CheckpointEvents(e.ctx, at(100)))
	e.ok("CheckpointEvents of an idle feed", e.ev.CheckpointEvents(e.ctx, at(300)))
	e.appendAll(e.health("a", 2))
	// The idle checkpoint at 300 s was not written: a pruning before 400 s
	// removes only what the checkpoint at 100 s covers.
	n, _, err := e.ev.PruneEvents(e.ctx, at(400), 100)
	e.ok("PruneEvents", err)
	if n != 1 {
		e.Errorf("an idle feed's checkpoint let a pruning remove %d events, want 1", n)
	}
	err = e.ev.CheckpointEvents(e.ctx, at(50))
	e.wantKind("CheckpointEvents earlier than the newest", err, fault.Precondition)
	err = e.ev.CheckpointEvents(e.ctx, at(100))
	e.wantKind("CheckpointEvents at the newest's time, events since", err, fault.Precondition)
	e.ok("CheckpointEvents later", e.ev.CheckpointEvents(e.ctx, at(500)))
	n, _, err = e.ev.PruneEvents(e.ctx, at(600), 100)
	e.ok("PruneEvents", err)
	if n != 1 {
		e.Errorf("a pruning after the later checkpoint removed %d events, want 1", n)
	}
	err = e.ev.CheckpointEvents(e.ctx, time.Time{})
	e.wantKind("CheckpointEvents with no time", err, fault.Invalid)
}

// testPruneUnderReader guards retention under a reader that pages through
// the feed while it is pruned: each page it reads is exact, and once
// pruning passes its cursor its next read is ErrCursorExpired, never an
// answer that skips events.
func testPruneUnderReader(e *eenv) {
	start := e.now()
	var batch []tenant.EventRecord
	for i := range 100 {
		batch = append(batch, e.health("a", i))
	}
	e.appendAll(batch...)
	e.ok("CheckpointEvents", e.ev.CheckpointEvents(e.ctx, at(500)))
	cursor, seen := start, 0
	for {
		p, err := e.ev.ReadEvents(e.ctx, cursor, nil, 10)
		if errors.Is(err, tenant.ErrCursorExpired) {
			break
		}
		e.ok("ReadEvents", err)
		for i, ev := range p.Events {
			if !ev.Time.Equal(at(seen + i)) {
				e.Fatalf("the reader got the event at %v as its event %d: a gap", ev.Time, seen+i)
			}
		}
		seen += len(p.Events)
		cursor = p.Cursor
		if len(p.Events) == 0 {
			e.Fatal("the reader reached the end before pruning passed it")
		}
		// Pruning moves 15 events ahead for every 10 the reader reads.
		_, _, err = e.ev.PruneEvents(e.ctx, at(1000), 15)
		e.ok("PruneEvents", err)
	}
	if seen == 0 || seen >= 100 {
		e.Errorf("the reader read %d events before its cursor expired", seen)
	}
}

// testPruneFollowsFeedOrder guards that retention reads no event's own
// time: an event dated far ahead, first in the feed, and one dated long ago,
// last, are pruned or kept by the checkpoints alone, with the events
// around them.
func testPruneFollowsFeedOrder(e *eenv) {
	start := e.now()
	far, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "store", State: tenant.StorageRecovered}, time.Date(9000, 1, 1, 0, 0, 0, 0, time.UTC))
	e.ok("NewEvent", err)
	batch := []tenant.EventRecord{far}
	for i := range 700 {
		batch = append(batch, e.space("store", i))
	}
	e.appendAll(batch...)
	e.ok("CheckpointEvents", e.ev.CheckpointEvents(e.ctx, at(1000)))
	e.appendAll(e.space("store", 1))
	n, more, err := e.ev.PruneEvents(e.ctx, at(2000), tenant.MaxPruneBatch)
	e.ok("PruneEvents", err)
	if n != 701 || more {
		e.Errorf("a pruning after the checkpoint removed %d (more %v), want 701: the one dated far ahead with the rest", n, more)
	}
	if kept, _ := e.readAll("", 100); len(kept) != 1 || !kept[0].Time.Equal(at(1)) {
		e.Errorf("after the pruning %d events are kept, want the one dated long ago, written after the checkpoint", len(kept))
	}
	_, err = e.ev.ReadEvents(e.ctx, start, nil, 10)
	e.wantIs("ReadEvents from before the pruned events", err, tenant.ErrCursorExpired, fault.OutOfRange)
	n, _, err = e.ev.PruneEvents(e.ctx, at(2000), 10)
	e.ok("PruneEvents again", err)
	if n != 0 {
		e.Errorf("a second pruning removed %d, want none", n)
	}
}
