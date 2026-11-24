package api

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// rootsMaster is a master that answers the installation's directory roots
// and the default of every other setting.
type rootsMaster struct {
	master.Store
	roots string
}

func (m rootsMaster) Setting(_ context.Context, name string) (string, error) {
	if name == settings.StorageDirectoryRoots {
		return m.roots, nil
	}
	return "", fault.New(fault.NotFound, "no such setting")
}

// TestAStorageTestsPutSurvivesTheServersFirstOpen guards the open a
// storage test makes (openToTest): it shares the server's record of
// cleaned storages, so when the server's own first open of the storage (a
// reception pipeline's) comes while the test's Put is in flight, that open
// is beside the test's and removes nothing. Opened beside with a record of
// its own, the test's Put lost its temporary file to the pipeline's
// cleanup and failed at its rename.
func TestAStorageTestsPutSurvivesTheServersFirstOpen(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lab", "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := &place.Cleanups{}
	c := &core{settings: settings.MustBuiltin(), master: rootsMaster{roots: "fast=" + root}, cleanups: shared}
	row := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}}
	lab := place.Tenant{ID: "T0000000000001", Name: "lab"}

	tested, err := c.openToTest(ctx, lab, row)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tested.Close() }()
	pr, pw := io.Pipe()
	put := make(chan error, 1)
	go func() {
		_, err := tested.Put(ctx, "storage-test/probe", pr)
		_ = pr.CloseWithError(io.ErrClosedPipe) // A Put that ended early does not leave the writes below blocked.
		put <- err
	}()
	if _, err := pw.Write([]byte("first part ")); err != nil {
		t.Fatal(err)
	}
	pipeline, err := place.Opener{Values: installationPlaces{c}, Cleanups: shared}.Open(ctx, lab, row)
	if err != nil {
		t.Fatalf("the pipeline's open: %v", err)
	}
	defer func() { _ = pipeline.Close() }()
	if _, err := pw.Write([]byte("last part")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if err := <-put; err != nil {
		t.Fatalf("the storage test's Put, in flight while the pipeline opened the storage: %v", err)
	}
}
