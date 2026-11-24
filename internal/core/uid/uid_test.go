package uid

import (
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
)

// TestCheck guards each rule of PS3.5 9.1 at its edges.
func TestCheck(t *testing.T) {
	sixtyFour := "1." + strings.Repeat("2", 62)
	for _, c := range []struct {
		in   string
		want error
	}{
		{"1.2.840.10008.1.2.1", nil},
		{"0", nil},
		{"1.0.2", nil},
		{"9", nil},
		{"2.25.329800735698586629295641978511506172918", nil},
		{sixtyFour, nil},
		{sixtyFour + "3", ErrTooLong},
		{"", ErrEmpty},
		{".1", ErrComponent},
		{"1.", ErrComponent},
		{"1..2", ErrComponent},
		{".", ErrComponent},
		{"1.02", ErrLeadingZero},
		{"01", ErrLeadingZero},
		{"1.00", ErrLeadingZero},
		{"1.2a", ErrCharacter},
		{"1.2 ", ErrCharacter},
		{"1.2\x00", ErrCharacter},
		{"1,2", ErrCharacter},
		{"-1", ErrCharacter},
	} {
		err := Check(c.in)
		if c.want == nil {
			if err != nil {
				t.Errorf("%q: %v", c.in, err)
			}
		} else if !errors.Is(err, c.want) {
			t.Errorf("%q: got %v, want %v", c.in, err, c.want)
		}
	}
}

// FuzzCheck guards that a value Check accepts is also a valid label, which
// is what lets a key use a UID as a path segment unchanged, and that Check
// never panics.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{"1.2.3", "0", "1..2", "01", "1.2\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if Valid(s) && !label.Valid(s) {
			t.Fatalf("UID %q is not a valid label", s)
		}
	})
}
