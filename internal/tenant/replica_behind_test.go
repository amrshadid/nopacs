package tenant

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestReplicaBehindEvent guards the event of a replica falling behind: it
// is the server's to append, a filter may name it, it reads back as it was
// written, and a payload with a bad name or a negative lag is refused.
func TestReplicaBehindEvent(t *testing.T) {
	if !ServerEventType(EventReplicaBehind) || !EventReplicaBehind.Valid() {
		t.Fatal("replica_behind is not a type the server appends")
	}
	if err := ValidateEventTypes([]EventType{EventReplicaBehind}); err != nil {
		t.Fatalf("a filter of replica_behind: %v", err)
	}
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	want := ReplicaBehind{Replica: "copy", Primary: "main", LagMicros: (90 * time.Minute).Microseconds(), ScansBehind: 3}
	e, err := NewEvent(&want, at)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := DecodeEvent(e.Type(), e.Payload())
	got, isBehind := p.(*ReplicaBehind)
	if !ok || !isBehind || *got != want {
		t.Errorf("read back %+v %v from %s; want %+v", p, ok, e.Payload(), want)
	}
	for _, bad := range []ReplicaBehind{{Replica: "", Primary: "main"}, {Replica: "copy", Primary: "main", LagMicros: -1},
		{Replica: "copy", Primary: "main", ScansBehind: -1}} {
		if _, err := NewEvent(&bad, at); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%+v: %v, want invalid", bad, err)
		}
	}
}
