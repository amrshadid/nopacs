package tenanttest

import (
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// checkNewHistory checks the feed after an activation that began the
// history named, from old, a cursor taken before it in oldHistory: each
// cursor names its own history, read from the cursor alone; old is refused as a
// cursor of another history, whose message begins with the contract's
// reason; the empty cursor reads from the new history's first event, the
// catalog_rebuilt of job, with no event of the old history before it; a
// page and a status name the new history, and the status's cursor is
// taken; and pruning removes the old history's events, whatever the
// checkpoints say, without changing what a read answers.
func (e *renv) checkNewHistory(old, oldHistory, history, job string) {
	e.Helper()
	if h, err := e.ev.EventCursorHistory(old); err != nil || h != oldHistory {
		e.Errorf("EventCursorHistory of a cursor of the history before = %q, %v; want %q", h, err, oldHistory)
	}
	if h, err := e.ev.EventCursorHistory(""); err != nil || h != "" {
		e.Errorf("EventCursorHistory of the empty cursor = %q, %v; want none", h, err)
	}
	if _, err := e.ev.EventCursorHistory("not a cursor"); fault.KindOf(err) != fault.Invalid {
		e.Errorf("EventCursorHistory of a text no feed made = %v, want Invalid", err)
	}
	_, err := e.ev.ReadEvents(e.ctx, old, nil, 10)
	e.wantIs("ReadEvents from a cursor of the history before", err, tenant.ErrHistoryChanged, fault.OutOfRange)
	e.wantIs("ReadEvents from a cursor of the history before", err, tenant.ErrCursorExpired, fault.OutOfRange)
	if !strings.HasPrefix(fault.Message(err), tenant.ReasonHistoryChanged+":") {
		e.Errorf("the refusal of a cursor of the history before says %q, want it to begin with %s", fault.Message(err), tenant.ReasonHistoryChanged)
	}
	first := func(what string) {
		e.Helper()
		p := e.read("", 100)
		if p.History != history {
			e.Errorf("%s: a page names the history %q, want %q", what, p.History, history)
		}
		if len(p.Events) == 0 {
			e.Fatalf("%s: the new history has no event", what)
		}
		r, ok := p.Events[0].Payload.(*tenant.CatalogRebuilt)
		if !ok || r.Job != job || !r.NewHistory {
			e.Errorf("%s: the new history's first event is %s %+v, want catalog_rebuilt of %s", what, p.Events[0].Type, p.Events[0].Payload, job)
		}
		for _, ev := range p.Events[1:] {
			if ev.Type == tenant.EventCatalogRebuilt {
				e.Errorf("%s: a second catalog_rebuilt follows the first", what)
			}
		}
	}
	first("before pruning")
	s, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	if s.History != history {
		e.Errorf("a status names the history %q, want %q", s.History, history)
	}
	if p := e.read(s.EventCursor, 10); p.History != history || len(p.Events) != 0 {
		e.Errorf("a read from the status's cursor answered %d events in history %q", len(p.Events), p.History)
	}
	if h, err := e.ev.EventCursorHistory(s.EventCursor); err != nil || h != history {
		e.Errorf("EventCursorHistory of the status's cursor = %q, %v; want %q", h, err, history)
	}
	removed, more, err := e.ev.PruneEvents(e.ctx, at(-3600), tenant.MaxPruneBatch)
	e.ok("PruneEvents", err)
	if removed == 0 || more {
		e.Errorf("pruning after a new history removed %d events (more %v), want the old history's", removed, more)
	}
	if again, _, err := e.ev.PruneEvents(e.ctx, at(-3600), tenant.MaxPruneBatch); err != nil || again != 0 {
		e.Errorf("pruning again removed %d events (%v), want none", again, err)
	}
	first("after pruning")
	_, err = e.ev.ReadEvents(e.ctx, old, nil, 10)
	e.wantIs("ReadEvents from a cursor of the history before, after pruning", err, tenant.ErrHistoryChanged, fault.OutOfRange)
}
