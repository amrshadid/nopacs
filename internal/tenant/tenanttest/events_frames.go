package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testFramesEventRoundTrips guards the frame tables pass's event, which the
// server appends: a scan's progress and a pass's end read back with their
// payloads, a filter on the type answers them alone, and a payload that
// names a scan on the event that ends a pass is refused before anything is
// written.
func testFramesEventRoundTrips(e *eenv) {
	start := e.now()
	scan, err := tenant.NewEvent(&tenant.FramesAdded{SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "C1",
		Instances: 3, ScansDone: 1, InstancesDone: 3}, at(1))
	e.ok("NewEvent of a scan", err)
	end, err := tenant.NewEvent(&tenant.FramesAdded{ScansDone: 1, InstancesDone: 3, Finished: true}, at(2))
	e.ok("NewEvent of an end", err)
	e.appendAll(e.health("a", 1), scan, end)
	got, _ := e.readAll(start, 10, tenant.EventFramesAdded)
	if len(got) != 2 {
		e.Fatalf("the filter answered %d events, want 2", len(got))
	}
	first, ok := got[0].Payload.(*tenant.FramesAdded)
	if !ok || *first != (tenant.FramesAdded{SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "C1", Instances: 3, ScansDone: 1, InstancesDone: 3}) {
		e.Errorf("the scan's event read back as %#v", got[0].Payload)
	}
	last, ok := got[1].Payload.(*tenant.FramesAdded)
	if !ok || !last.Finished || last.ScanLabel != "" || last.InstancesDone != 3 {
		e.Errorf("the end's event read back as %#v", got[1].Payload)
	}
	if _, err := tenant.NewEvent(&tenant.FramesAdded{ScanLabel: "C1", ScansDone: 1, Finished: true}, at(3)); err == nil {
		e.Errorf("an end naming a scan was made")
	}
	if _, err := tenant.NewEvent(&tenant.FramesAdded{SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "C1", Instances: 2}, at(3)); err == nil {
		e.Errorf("a scan's event whose totals do not hold it was made")
	}
}
