package ascii

import "testing"

// TestPrintable guards the class: the bounds of printable ASCII, a space,
// control characters and bytes of UTF-8.
func TestPrintable(t *testing.T) {
	for _, ok := range []string{"", "!", "~", "https://s3.example.com", "AKIA0123"} {
		if !Printable(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{" ", "a b", "a\tb", "a\x00", "\x7f", "é", "a\n"} {
		if Printable(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
