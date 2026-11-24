package place

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestResolveBound guards MaxDirectoryLength: a directory of exactly the
// bound resolves, one byte more is a precondition whose message holds no
// path, and nothing is made either way.
func TestResolveBound(t *testing.T) {
	base := realTemp(t)
	roots := settings.DirectoryRoots{{Name: "fast", Path: base}}
	// <base>/lab/<path>: the path's length that makes the whole the bound.
	room := MaxDirectoryLength - len(base) - len("/lab/")
	path := func(n int) string {
		var segs []string
		for n > 0 {
			k := min(n, 100)
			if n-k == 1 {
				k-- // A segment of one byte after a separator: keep both.
			}
			segs = append(segs, strings.Repeat("a", k))
			n -= k + 1
		}
		return strings.Join(segs, "/")
	}
	at := path(room)
	if len(at) != room {
		t.Fatalf("the test's path is %d bytes, want %d", len(at), room)
	}
	if _, err := Resolve(roots, "lab", tenant.DirectoryStorage{Root: "fast", Path: at}); err != nil {
		t.Errorf("a directory of the bound: %v", err)
	}
	_, err := Resolve(roots, "lab", tenant.DirectoryStorage{Root: "fast", Path: at + "b"})
	if fault.KindOf(err) != fault.Precondition || strings.Contains(fault.Message(err), base) {
		t.Errorf("a directory past the bound: %v", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("resolving made something: %v", entries)
	}
}

// TestResolveRefusesNestedRootOnUse guards the check made when a root is
// used: a root that came to lie inside another through a link made after
// the setting was written is refused, and so is the other root.
func TestResolveRefusesNestedRootOnUse(t *testing.T) {
	base := realTemp(t)
	fast := filepath.Join(base, "fast")
	if err := os.MkdirAll(filepath.Join(fast, "lab2"), 0o700); err != nil {
		t.Fatal(err)
	}
	slow := filepath.Join(base, "slow")
	roots := settings.DirectoryRoots{{Name: "fast", Path: fast}, {Name: "slow", Path: slow}}
	if err := CheckRoots(roots); err != nil {
		t.Fatalf("apart when written: %v", err)
	}
	symlink(t, filepath.Join(fast, "lab2"), slow)
	for _, r := range []string{"fast", "slow"} {
		_, err := Resolve(roots, "data", tenant.DirectoryStorage{Root: r})
		if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "symbolic links") {
			t.Errorf("the root %s: %v", r, err)
		}
	}
}
