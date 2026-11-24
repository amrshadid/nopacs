package pathname

import (
	"strings"
	"testing"
)

// TestValidName guards the rule of a name: the characters, the first
// letter, the bound, and the device names Windows reserves.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "lab-1", "con1", "comx", "lpt", "nul-x", strings.Repeat("a", MaxNameLength)} {
		if !ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1a", "-a", "A", "a_b", "a.b", "a b", strings.Repeat("a", MaxNameLength+1),
		"con", "prn", "aux", "nul", "com0", "com9", "lpt1"} {
		if ValidName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if !WellFormedName("con") {
		t.Error("a device name is well formed; only ValidName refuses it")
	}
}

// TestDeviceName guards the device names of a path's segment: any case,
// with or without an extension, and nothing that only starts like one.
func TestDeviceName(t *testing.T) {
	for _, dev := range []string{"con", "CON", "Nul", "nul.txt", "COM1", "com1.tar.gz", "lpt9", "aux.", "prn.x"} {
		if !DeviceName(dev) {
			t.Errorf("%q is not seen as a device", dev)
		}
	}
	for _, not := range []string{"", "co", "cons", "console", "com", "com10", "lptx", "x.con", "nul-1", "comA"} {
		if DeviceName(not) {
			t.Errorf("%q is seen as a device", not)
		}
	}
}

// TestTrailingDotOrSpace guards the segments Windows shortens.
func TestTrailingDotOrSpace(t *testing.T) {
	for _, s := range []string{"mri.", "mri..", "mri ", ".", "a. "} {
		if !TrailingDotOrSpace(s) {
			t.Errorf("%q not seen", s)
		}
	}
	for _, s := range []string{"", "mri", ".mri", "a.b"} {
		if TrailingDotOrSpace(s) {
			t.Errorf("%q seen", s)
		}
	}
}

// TestNest guards the comparison of two paths by segments: one directory,
// either inside the other, case folded, and a common start of a segment
// that is not a common segment.
func TestNest(t *testing.T) {
	split := func(p string) []string {
		if p == "" {
			return nil
		}
		return strings.Split(p, "/")
	}
	for _, c := range []struct {
		a, b string
		nest bool
	}{
		{"srv", "srv", true},
		{"srv", "srv/x", true},
		{"srv/x", "srv", true},
		{"SRV/X", "srv/x/y", true},
		{"", "a/b", true},
		{"srv", "srv2", false},
		{"srv/a", "srv/b", false},
		{"a/b", "b/a", false},
	} {
		if got := Nest(split(c.a), split(c.b)); got != c.nest {
			t.Errorf("Nest(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
