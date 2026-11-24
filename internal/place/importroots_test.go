package place

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// importValues answers fixed settings for ImportRoots.
type importValues struct{ imports, storage settings.DirectoryRoots }

func (v importValues) ImportRoots(context.Context) (settings.DirectoryRoots, error) {
	return v.imports, nil
}
func (v importValues) DirectoryRoots(context.Context) (settings.DirectoryRoots, error) {
	return v.storage, nil
}

// TestCheckImportRoots guards where an import root may lie: not at, inside
// or around a storage root, a guarded directory of the server's or another
// import root, once links are followed, whether the link is the root itself
// or one leads into a guarded directory; a relative path is invalid; a root
// apart from all of them passes; no message holds a path.
func TestCheckImportRoots(t *testing.T) {
	base := realTemp(t)
	for _, d := range []string{"fast", "data/incoming", "old", "other"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	symlink(t, filepath.Join(base, "data"), filepath.Join(base, "data-alias"))
	symlink(t, filepath.Join(base, "fast"), filepath.Join(base, "fast-alias"))
	storage := settings.DirectoryRoots{{Name: "fast", Path: filepath.Join(base, "fast")}}
	guarded := []string{filepath.Join(base, "data"), filepath.Join(base, "data", "incoming")}
	cases := []struct {
		name  string
		roots settings.DirectoryRoots
		kind  fault.Kind
	}{
		{"apart", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "old")}, {Name: "other", Path: filepath.Join(base, "other")}}, 0},
		{"a storage root", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "fast")}}, fault.Precondition},
		{"inside a storage root by a link", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "fast-alias", "x")}}, fault.Precondition},
		{"around a storage root", settings.DirectoryRoots{{Name: "old", Path: base}}, fault.Precondition},
		{"the data directory by a link", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "data-alias")}}, fault.Precondition},
		{"inside the incoming area", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "data", "incoming", "a")}}, fault.Precondition},
		{"one inside another", settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "old")}, {Name: "inner", Path: filepath.Join(base, "old", "in")}}, fault.Precondition},
		{"relative", settings.DirectoryRoots{{Name: "old", Path: "old"}}, fault.Invalid},
	}
	for _, c := range cases {
		err := CheckImportRoots(c.roots, storage, guarded)
		if c.kind == 0 {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if fault.KindOf(err) != c.kind {
			t.Errorf("%s: %v, want %v", c.name, err, c.kind)
		}
		if err != nil && strings.Contains(err.Error(), base) {
			t.Errorf("%s: the message holds a path: %v", c.name, err)
		}
	}
}

// TestImportRootsRoot guards a root's use: it resolves to its directory
// when named, allotted and apart; a root not named, not allotted, not a
// directory, or one whose path came to pass into the data directory through
// a link made after it was set, is a precondition failure naming no path.
func TestImportRootsRoot(t *testing.T) {
	base := realTemp(t)
	for _, d := range []string{"data", "old", "fast"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(base, "data"), filepath.Join(base, "moved"))
	v := importValues{
		imports: settings.DirectoryRoots{{Name: "old", Path: filepath.Join(base, "old")}, {Name: "gone", Path: filepath.Join(base, "nowhere")},
			{Name: "file", Path: filepath.Join(base, "file")}, {Name: "moved", Path: filepath.Join(base, "moved")}},
		storage: settings.DirectoryRoots{{Name: "fast", Path: filepath.Join(base, "fast")}},
	}
	r := ImportRoots{Values: v, Guarded: []string{filepath.Join(base, "data")},
		Allotted: func(context.Context, tenant.Store) ([]string, error) {
			return []string{"old", "gone", "file", "moved"}, nil
		}}
	got, err := r.Root(t.Context(), "lab1", nil, "old")
	if err != nil || got != filepath.Join(base, "old") {
		t.Fatalf("old: %q, %v", got, err)
	}
	for _, name := range []string{"none", "gone", "file", "moved"} {
		_, err := r.Root(t.Context(), "lab1", nil, name)
		if fault.KindOf(err) != fault.Precondition {
			t.Errorf("%s: %v, want a precondition", name, err)
		}
		if err != nil && strings.Contains(err.Error(), base) {
			t.Errorf("%s: the message holds a path: %v", name, err)
		}
	}
	// A root kept resolved, whose directory is then replaced by a link to
	// another directory apart from everything: it resolves elsewhere now.
	if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "swap"), 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := ResolveImportRoots(settings.DirectoryRoots{{Name: "swap", Path: filepath.Join(base, "swap")}})
	if err != nil || kept[0].Path != filepath.Join(base, "swap") {
		t.Fatalf("resolve: %v %v", kept, err)
	}
	r.Values = importValues{imports: kept, storage: v.storage}
	r.Allotted = func(context.Context, tenant.Store) ([]string, error) { return []string{"swap"}, nil }
	if _, err := r.Root(t.Context(), "lab1", nil, "swap"); err != nil {
		t.Fatalf("swap before the change: %v", err)
	}
	if err := os.Remove(filepath.Join(base, "swap")); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(base, "elsewhere"), filepath.Join(base, "swap"))
	if _, err := r.Root(t.Context(), "lab1", nil, "swap"); fault.KindOf(err) != fault.Precondition || !strings.Contains(err.Error(), "another directory") {
		t.Errorf("a root resolving elsewhere now: %v", err)
	}
	r.Values = v
	r.Allotted = func(context.Context, tenant.Store) ([]string, error) { return nil, nil }
	if _, err := r.Root(t.Context(), "lab1", nil, "old"); fault.KindOf(err) != fault.Precondition || !strings.Contains(err.Error(), "import.allowed-roots") {
		t.Errorf("a root not allotted: %v", err)
	}
}
