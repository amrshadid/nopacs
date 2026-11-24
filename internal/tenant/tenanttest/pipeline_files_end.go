package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReceivedFilesAtEnd guards which receptions keep their received files
// once they end (tenant.KeepsReceivedFiles): one done and one quarantined
// keep none, removed by the step that ends them, while a failed one keeps
// every file for its retry and its discard, and one in progress keeps
// them all; a reception's counts stay as they were whichever it is.
func testReceivedFilesAtEnd(e *penv) {
	e.addReceiver("a", "store", 11112)
	type end struct {
		state reception.State
		keeps bool
	}
	for i, c := range []end{{reception.Done, false}, {reception.Quarantined, false}, {reception.Failed, true}, {reception.Identified, true}} {
		r := e.openReception("a", 10+i, at(1))
		_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 10+i, 1), file(r, 10+i, 2)}, at(2))
		e.ok("AddReceivedFiles", err)
		switch c.state {
		case reception.Quarantined, reception.Failed:
			r = e.stepTo(r, reception.Complete, "store")
			ev := reception.Quarantine
			if c.state == reception.Failed {
				ev = reception.Fail
			}
			r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: ev, At: at(3), Failure: "storage unreachable"}, nil)
			e.ok("StepReception "+string(ev), err)
		default:
			r = e.stepTo(r, c.state, "store")
		}
		files, _, err := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
		e.ok("ReceivedFiles", err)
		if want := map[bool]int{true: 2, false: 0}[c.keeps]; len(files) != want {
			e.Errorf("a reception %s lists %d received files, want %d", c.state, len(files), want)
		}
		got, _, err := e.p.Reception(e.ctx, r.ID)
		e.ok("Reception", err)
		if got.State != c.state || got.Counts.FilesReceived != 2 {
			e.Errorf("a reception %s after its end: %s with %d files received, want 2", c.state, got.State, got.Counts.FilesReceived)
		}
	}
}
