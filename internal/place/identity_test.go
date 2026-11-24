package place

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The name "café" written with "é" as one code point (NFC) and as "e"
// followed by a combining acute accent (NFD): two byte strings that a file
// system ignoring normalization (APFS, HFS+) takes for one name.
const (
	cafeComposed   = "café"
	cafeDecomposed = "café"
)

// foldingTemp returns a directory holding "café" made in its composed
// spelling, and skips the test where the file system does not find it by
// its decomposed spelling too.
func foldingTemp(t *testing.T) string {
	t.Helper()
	base := realTemp(t)
	if err := os.Mkdir(filepath.Join(base, cafeComposed), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, cafeDecomposed)); errors.Is(err, fs.ErrNotExist) {
		t.Skip("this file system keeps the two spellings of a name apart")
	}
	return base
}

// TestOverlapIdentity guards the comparison of two directories by what
// they are, not how they are written: a path through a symbolic link not
// followed beforehand (standing for a bind mount, which no name reveals)
// is the directory it leads to, inside it or around it, even where the
// part below does not exist yet; two directories apart, and two parts not
// made yet side by side under one directory, do not overlap.
func TestOverlapIdentity(t *testing.T) {
	base := realTemp(t)
	for _, d := range []string{"real/inner", "apart"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	symlink(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"one directory", filepath.Join(base, "alias"), filepath.Join(base, "real"), true},
		{"inside", filepath.Join(base, "alias", "inner"), filepath.Join(base, "real"), true},
		{"around", filepath.Join(base, "real"), filepath.Join(base, "alias", "inner"), true},
		{"inside, not made yet", filepath.Join(base, "alias", "new", "deeper"), filepath.Join(base, "real", "new"), true},
		{"apart", filepath.Join(base, "alias"), filepath.Join(base, "apart"), false},
		{"side by side, not made yet", filepath.Join(base, "alias", "x"), filepath.Join(base, "real", "y"), false},
	}
	for _, c := range cases {
		got, err := overlap(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// TestRootsSpelling guards every rule of where a root may lie against two
// spellings of one directory, on a file system that ignores Unicode
// normalization: an import root written decomposed is refused against a
// storage root, a guarded directory and another import root written
// composed, whether it exists or not; at its use, an import root stored
// decomposed is refused against the data directory written composed; and
// two storage roots so written are refused, when set and at their use.
func TestRootsSpelling(t *testing.T) {
	base := foldingTemp(t)
	composed := filepath.Join(base, cafeComposed)
	decomposed := filepath.Join(base, cafeDecomposed)
	storage := settings.DirectoryRoots{{Name: "fast", Path: composed}}
	if err := CheckImportRoots(settings.DirectoryRoots{{Name: "old", Path: decomposed}}, storage, nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an import root at a storage root by another spelling: %v", err)
	}
	if err := CheckImportRoots(settings.DirectoryRoots{{Name: "old", Path: filepath.Join(decomposed, "new")}}, storage, nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an import root not made yet inside a storage root by another spelling: %v", err)
	}
	if err := CheckImportRoots(settings.DirectoryRoots{{Name: "old", Path: decomposed}}, nil, []string{composed}); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an import root at the data directory by another spelling: %v", err)
	}
	two := settings.DirectoryRoots{{Name: "a", Path: composed}, {Name: "b", Path: filepath.Join(decomposed, "x")}}
	if err := CheckImportRoots(two, nil, nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("two import roots, one inside the other by another spelling: %v", err)
	}
	if err := CheckRoots(two); fault.KindOf(err) != fault.Precondition {
		t.Errorf("two storage roots, one inside the other by another spelling: %v", err)
	}
	if err := os.Mkdir(filepath.Join(composed, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRoot(two, two[1]); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a storage root used inside another by another spelling: %v", err)
	}
	kept, err := ResolveImportRoots(settings.DirectoryRoots{{Name: "old", Path: decomposed}})
	if err != nil {
		t.Fatal(err)
	}
	r := ImportRoots{Values: importValues{imports: kept}, Guarded: []string{composed}}
	if _, err := r.Root(t.Context(), "lab1", nil, "old"); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an import root used at the data directory by another spelling: %v", err)
	}
}
