package place

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// realTemp returns a temporary directory with its own links resolved, so a
// test compares what the code under test resolves (on macOS the temporary
// directory is under a link, /var to /private/var).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// symlink makes a symbolic link, or skips the test where the system does not
// let the test make one (Windows without the privilege).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("no symbolic links here: %v", err)
		}
		t.Fatal(err)
	}
}

// TestCheckRoots guards the rules of the roots that need the file system:
// two roots apart in their text but not once their links are followed are a
// precondition, whether the link is the root itself, a directory above it,
// or a link whose target is not made yet; roots that do not exist are
// compared where they would be; a path not absolute here is invalid; no
// message holds a path.
func TestCheckRoots(t *testing.T) {
	base := realTemp(t)
	fast := filepath.Join(base, "fast")
	if err := os.MkdirAll(filepath.Join(fast, "lab2"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(fast, "lab2"), filepath.Join(base, "into-lab2"))
	symlink(t, fast, filepath.Join(base, "fast-alias"))
	symlink(t, filepath.Join(fast, "not-yet"), filepath.Join(base, "dangling"))
	roots := func(paths ...string) settings.DirectoryRoots {
		out := settings.DirectoryRoots{{Name: "fast", Path: fast}}
		for i, p := range paths {
			out = append(out, settings.DirectoryRoot{Name: "r" + string(rune('a'+i)), Path: p})
		}
		return out
	}
	for name, p := range map[string]string{
		"a link into the root":            filepath.Join(base, "into-lab2"),
		"below a link to the root":        filepath.Join(base, "fast-alias", "x"),
		"a link whose target is not made": filepath.Join(base, "dangling"),
		"the same root by another path":   filepath.Join(base, "fast-alias"),
	} {
		err := CheckRoots(roots(p))
		if fault.KindOf(err) != fault.Precondition || strings.Contains(fault.Message(err), base) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CheckRoots(roots(filepath.Join(base, "slow"), filepath.Join(base, "cold", "deep"))); err != nil {
		t.Errorf("roots not made yet, apart: %v", err)
	}
	if err := CheckRoots(roots(filepath.Join(base, "slow"), filepath.Join(base, "slow", "deep"))); fault.KindOf(err) != fault.Precondition {
		t.Errorf("roots not made yet, nested: %v", err)
	}
	rel := settings.DirectoryRoots{{Name: "a", Path: "relative"}}
	if err := CheckRoots(rel); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a relative root: %v", err)
	}
}

// TestResolveLooseLoop guards a loop of links: it ends in an error, never
// in a resolution that runs on.
func TestResolveLooseLoop(t *testing.T) {
	base := realTemp(t)
	symlink(t, filepath.Join(base, "b"), filepath.Join(base, "a"))
	symlink(t, filepath.Join(base, "a"), filepath.Join(base, "b"))
	if _, err := resolveLoose(filepath.Join(base, "a", "x")); err == nil {
		t.Error("a loop of links resolved")
	}
}

// TestSegments guards the segments a resolved path is compared by: the
// volume first, no empty segment, and the root of a volume holding the
// volume alone, so it contains every path on it.
func TestSegments(t *testing.T) {
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	if s := segments(root); len(s) != 1 {
		t.Errorf("the root of a volume: %q", s)
	}
	p := filepath.Join(root, "srv", "fast")
	if s := segments(p + string(filepath.Separator)); len(s) != 3 || s[1] != "srv" || s[2] != "fast" {
		t.Errorf("%s: %q", p, s)
	}
}
