package master

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestKeyRef guards the check of a key id a caller gives and what the log
// records for it: a key id passes and is recorded as it is; a whole key, a
// value that starts as one does, and one longer than a key id are Invalid
// without the value in the message and are recorded as NotAKeyID, which no
// key id can equal, so the log never holds the secret nor reads as naming a
// real key.
func TestKeyRef(t *testing.T) {
	if err := ValidateKeyRef("key id", "k8Zp2QvXn5Rt"); err != nil {
		t.Fatalf("a key id was refused: %v", err)
	}
	if got := KeyObject("k8Zp2QvXn5Rt"); got != "k8Zp2QvXn5Rt" {
		t.Errorf("KeyObject of a key id = %q", got)
	}
	if got := KeyObject(""); got != "" {
		t.Errorf("KeyObject of none = %q, want empty", got)
	}
	whole := "nopacs_k8Zp2QvXn5Rt_" + strings.Repeat("s", 44)
	for _, bad := range []string{whole, "nopacs_short", strings.Repeat("k", 13), "not an id"} {
		err := ValidateKeyRef("key id", bad)
		if fault.KindOf(err) != fault.Invalid || strings.Contains(fault.Message(err), bad) {
			t.Errorf("%.12s...: %v; want Invalid without the value", bad, err)
		}
		if got := KeyObject(bad); got != NotAKeyID {
			t.Errorf("KeyObject(%.12s...) = %q, want %q", bad, got, NotAKeyID)
		}
	}
	if ValidateID("key id", NotAKeyID) == nil {
		t.Errorf("%q is a valid id, so a log entry could read as naming a key", NotAKeyID)
	}
}
