package api

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestFramesEventIsAnswered guards the frame tables pass's event on the
// API: its type maps to EVENT_TYPE_FRAMES_ADDED both ways (an event read
// and a Watch filter), and its payload is answered field for field.
func TestFramesEventIsAnswered(t *testing.T) {
	p := &tenant.FramesAdded{SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "C1", Instances: 3, ScansDone: 2, InstancesDone: 7}
	m := eventMessage("lab1", tenant.Event{Cursor: "c", Type: tenant.EventFramesAdded, Payload: p})
	got := m.GetFramesAdded()
	if m.GetType() != v1.EventType_EVENT_TYPE_FRAMES_ADDED || got == nil || got.GetSubjectLabel() != "S1" || got.GetSessionLabel() != "E1" ||
		got.GetScanLabel() != "C1" || got.GetInstances() != 3 || got.GetScansDone() != 2 || got.GetInstancesDone() != 7 || got.GetFinished() {
		t.Fatalf("answered as %v", m)
	}
	end := eventMessage("lab1", tenant.Event{Cursor: "d", Type: tenant.EventFramesAdded, Payload: &tenant.FramesAdded{ScansDone: 2, InstancesDone: 7, Finished: true}})
	if !end.GetFramesAdded().GetFinished() || end.GetFramesAdded().GetScanLabel() != "" {
		t.Errorf("the end answered as %v", end)
	}
	types, err := eventTypesOf([]v1.EventType{v1.EventType_EVENT_TYPE_FRAMES_ADDED})
	if err != nil || len(types) != 1 || types[0] != tenant.EventFramesAdded {
		t.Errorf("a filter on the type reads as %v, %v", types, err)
	}
}
