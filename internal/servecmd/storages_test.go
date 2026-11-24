package servecmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// oneTenant is a master that knows one tenant, lab.
type oneTenant struct{ master.Store }

func (oneTenant) TenantByName(_ context.Context, name string) (master.Tenant, error) {
	return master.Tenant{ID: "T0000000000001", Name: name}, nil
}

// oneStorage is a tenant's store with one storage row, which a test changes.
type oneStorage struct {
	tenant.Pipeline
	row tenant.Storage
}

func (s *oneStorage) Storage(context.Context, string) (tenant.Storage, error) { return s.row, nil }

// roots answers the installation's directory roots.
type roots settings.DirectoryRoots

func (r roots) DirectoryRoots(context.Context) (settings.DirectoryRoots, error) {
	return settings.DirectoryRoots(r), nil
}
func (roots) S3Endpoints(context.Context) (settings.S3Endpoints, error) { return nil, nil }

// TestThePipelinesStorageIsKeptOpenAndCheckedAtEveryUse guards
// tenantStorages: the same store is handed to every step while the
// storage's row is unchanged; a changed row gets a new store while the old
// one stays open for a step that holds it; a storage no longer allowed (its
// directory replaced by a link) is refused at its next use though a store
// for it is open; and Close closes every store and refuses later uses.
func TestThePipelinesStorageIsKeptOpenAndCheckedAtEveryUse(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "lab", "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	st := &oneStorage{row: tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}, Updated: t1}}
	s := newTenantStorages(place.Opener{Values: roots{{Name: "fast", Path: root}}}, oneTenant{}, "lab", st)

	a, err := s.Open(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(ctx, "main")
	if err != nil || b != a {
		t.Fatalf("a second use of an unchanged storage got another store (%v)", err)
	}
	st.row.Updated = t1.Add(time.Second)
	c, err := s.Open(ctx, "main")
	if err != nil || c == a {
		t.Fatalf("a changed row kept the old store (%v)", err)
	}
	if _, err := a.Stat(ctx, "data/none"); err == nil || !isNotFound(err) {
		t.Fatalf("the replaced store was closed while a step may hold it: %v", err)
	}

	// A storage's directory swapped for a link under the open store is
	// refused at the next use. On Windows the swap cannot happen: a
	// directory a process holds open cannot be removed, which protects the
	// store there by itself.
	if runtime.GOOS != "windows" {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), dir); err != nil {
			t.Fatalf("making the link: %v", err)
		}
		if _, err := s.Open(ctx, "main"); err == nil {
			t.Fatal("a storage whose directory became a link was handed out from the open store")
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, "data/none"); err == nil || isNotFound(err) {
		t.Fatalf("a store is still open after Close: %v", err)
	}
	// The link, where the swap was made; the directory itself, with what
	// the store wrote in it, where it was not (Windows).
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "main"); err == nil {
		t.Fatal("a use after Close was answered")
	}
}

// isNotFound reports whether a storage answered that a key is not there:
// the store is open and working.
func isNotFound(err error) bool { return fault.KindOf(err) == fault.NotFound }
