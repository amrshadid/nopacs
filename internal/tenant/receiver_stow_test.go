package tenant

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestStowReceiverRefusesAWholeKey guards the key ids a stow receiver
// lists: a key's id is taken, and a whole key pasted in its place, a value
// that starts as one does, or one longer than a key id, is refused as
// Invalid without the value in the message, so a secret is never kept in a
// receiver's configuration or repeated in an error.
func TestStowReceiverRefusesAWholeKey(t *testing.T) {
	ok := STOWReceiver{Keys: []string{"k8Zp2QvXn5Rt"}, MaxRequestBytes: DefaultMaxRequestBytes}
	if err := ok.validate(); err != nil {
		t.Fatalf("a key id was refused: %v", err)
	}
	whole := "nopacs_k8Zp2QvXn5Rt_" + strings.Repeat("s", 44)
	for _, bad := range []string{whole, "nopacs_short", strings.Repeat("k", 13)} {
		s := STOWReceiver{Keys: []string{"k8Zp2QvXn5Rt", bad}, MaxRequestBytes: DefaultMaxRequestBytes}
		err := s.validate()
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("%.12s...: %v, want Invalid", bad, err)
			continue
		}
		if strings.Contains(err.Error(), bad) || strings.Contains(fault.Message(err), bad) {
			t.Errorf("the refusal repeats the value: %q", fault.Message(err))
		}
	}
}
