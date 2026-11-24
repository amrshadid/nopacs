package servecmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
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

// TestAStoreOpenedBesideAnotherKeepsItsPutInProgress guards writingOpener,
// which every store the server opens to write a storage takes: the
// pipeline's and a corrections engine's stores of one storage, each opened
// at its own time, never remove each other's work. A server start opened
// the engine's store while the pipeline's was putting; the engine's open
// removed every file in the storage's temporary directory, the Put's
// among them, and the Put failed at its rename ("the put's temporary file
// was removed before it was put in place"). What a crash left before the
// start is still removed by the first open.
func TestAStoreOpenedBesideAnotherKeepsItsPutInProgress(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "lab", "main")
	tmp := filepath.Join(dir, blob.ReservedPrefix+"tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	crash := filepath.Join(tmp, "left-by-a-crash")
	if err := os.WriteFile(crash, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"},
		Updated: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	opener := writingOpener(roots{{Name: "fast", Path: root}}, nil, &place.Cleanups{})
	pipeline := newTenantStorages(opener, oneTenant{}, "lab", &oneStorage{row: row})
	corrections := newTenantStorages(opener, oneTenant{}, "lab", &oneStorage{row: row})
	defer func() { _ = pipeline.Close(); _ = corrections.Close() }()

	a, err := pipeline.Open(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(crash); err == nil {
		t.Fatal("the first open left what a crash left in the temporary directory")
	}
	// The Put has made its temporary file once the pipe's first write has
	// been read.
	pr, pw := io.Pipe()
	put := make(chan error, 1)
	go func() {
		_, err := a.Put(ctx, "s/scan/1.dcm", pr)
		_ = pr.CloseWithError(io.ErrClosedPipe) // A Put that ended early does not leave the writes below blocked.
		put <- err
	}()
	if _, err := pw.Write([]byte("first part ")); err != nil {
		t.Fatal(err)
	}
	b, err := corrections.Open(ctx, "main")
	if err != nil {
		t.Fatalf("the second store's open: %v", err)
	}
	if _, err := pw.Write([]byte("last part")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if err := <-put; err != nil {
		t.Fatalf("the Put in progress while another store opened failed: %v", err)
	}
	rc, err := b.Read(ctx, "s/scan/1.dcm", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != "first part last part" {
		t.Fatalf("the object read through the other store = %q, %v", got, err)
	}
	if _, err := b.Put(ctx, "s/scan/2.dcm", strings.NewReader("x")); err != nil {
		t.Fatalf("a Put through the store opened beside: %v", err)
	}
}

// isNotFound reports whether a storage answered that a key is not there:
// the store is open and working.
func isNotFound(err error) bool { return fault.KindOf(err) == fault.NotFound }
