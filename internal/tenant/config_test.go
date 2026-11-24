package tenant

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestDirectoryPathWindowsForms guards the segments of a directory
// storage's path that a file system on Windows reads as another name: a
// final dot, which Windows drops, so "mri." is "mri" there, and a device
// name in any case, with or without an extension, which opens the device
// instead of a directory. Each is refused on every system, so a data
// directory moved to Windows holds the same storages.
func TestDirectoryPathWindowsForms(t *testing.T) {
	for _, bad := range []string{"mri.", "mri..", "a/mri.", "con", "CON", "nul.txt", "com1", "Lpt9.tar", "a/aux/b", "prn."} {
		if err := validateDirectoryPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"mri", "a.b", "con1", "console", "com10", "x.con", "a/b_c-d.e"} {
		if err := validateDirectoryPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	if err := validateDirectoryPath(strings.Repeat("a/", MaxDirectorySegments-1) + "a"); err != nil {
		t.Errorf("the most segments refused: %v", err)
	}
}

// TestDirectorySegments guards the segments of a storage's path: none for
// the tenant's directory, so that directory contains every other.
func TestDirectorySegments(t *testing.T) {
	if s := DirectorySegments(""); len(s) != 0 {
		t.Errorf("the empty path has segments: %q", s)
	}
	if s := DirectorySegments("a/b"); len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Errorf("a/b: %q", s)
	}
}

// TestPrefixBound guards the longest S3 prefix: with the longest key the
// storage port accepts (896 bytes), a prefix of 128 keeps every object's key
// within the 1024 bytes S3 allows, and one byte more does not.
func TestPrefixBound(t *testing.T) {
	if err := validatePrefix(strings.Repeat("a/", 64)); err != nil {
		t.Errorf("a prefix of 128 bytes: %v", err)
	}
	if err := validatePrefix("a" + strings.Repeat("a/", 64)); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a prefix of 129 bytes: %v, want invalid", err)
	}
}
