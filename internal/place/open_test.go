package place

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCheckAppliesTheRulesOfOpen guards Check, which the reception pipeline
// runs at every use of a storage it keeps open: it passes while Open would,
// and refuses, as Open does, a storage whose directory was replaced by a
// symbolic link below the root, one whose root is no longer listed, and one
// whose directory is gone; it makes and opens nothing.
func TestCheckAppliesTheRulesOfOpen(t *testing.T) {
	ctx := context.Background()
	root := realTemp(t)
	dir := filepath.Join(root, "lab", "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listed := fixedValues{roots: settings.DirectoryRoots{{Name: "fast", Path: root}}}
	s := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}}
	lab := Tenant{ID: "t1", Name: "lab"}
	o := Opener{Values: listed}
	if err := o.Check(ctx, lab, s); err != nil {
		t.Fatalf("Check of an allowed storage: %v", err)
	}
	bs, err := o.Open(ctx, lab, s)
	if err != nil {
		t.Fatalf("Open of an allowed storage: %v", err)
	}
	_ = bs.Close()

	if err := (Opener{Values: fixedValues{}}).Check(ctx, lab, s); err == nil {
		t.Error("Check passed a storage whose root is no longer listed")
	}

	elsewhere := realTemp(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	symlink(t, elsewhere, dir)
	errCheck := o.Check(ctx, lab, s)
	_, errOpen := o.Open(ctx, lab, s)
	if errCheck == nil || errOpen == nil {
		t.Fatalf("a link planted at the storage's directory: Check %v, Open %v; both must refuse", errCheck, errOpen)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := o.Check(ctx, lab, s); err == nil {
		t.Error("Check passed a storage whose directory is gone")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Error("Check made the storage's directory")
	}
}

// TestOpensForReadingNeverFailBesideWrites guards the storage a download
// opens per request: many opens for reading at once, beside a store that
// keeps writing, never fail and never fail a write. An open that writes (a
// probe file, and the removal of every temporary file it finds) made two
// downloads at once fail with "the storage's directory cannot be written"
// when one removed the other's probe, and could remove the temporary file
// of a Put in progress. The opened store reads, and refuses to write.
func TestOpensForReadingNeverFailBesideWrites(t *testing.T) {
	ctx := context.Background()
	root := realTemp(t)
	if err := os.MkdirAll(filepath.Join(root, "lab", "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}}
	lab := Tenant{ID: "t1", Name: "lab"}
	o := Opener{Values: fixedValues{roots: settings.DirectoryRoots{{Name: "fast", Path: root}}}}
	writer, err := o.Open(ctx, lab, s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.Put(ctx, "a/obj", strings.NewReader("held")); err != nil {
		t.Fatal(err)
	}

	const readers, rounds = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, readers*rounds+rounds)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Each object is large enough that its temporary file lives across
		// many opens.
		body := strings.Repeat("x", 1<<20)
		for i := range rounds {
			if _, err := writer.Put(ctx, fmt.Sprintf("w/%d", i), strings.NewReader(body)); err != nil {
				errs <- fmt.Errorf("a Put beside the readers: %w", err)
			}
		}
	}()
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				r, err := o.OpenForReading(ctx, lab, s)
				if err != nil {
					errs <- fmt.Errorf("an open for reading: %w", err)
					continue
				}
				rc, err := r.Read(ctx, "a/obj", 0, -1)
				if err == nil {
					var b []byte
					b, err = io.ReadAll(rc)
					_ = rc.Close()
					if err == nil && string(b) != "held" {
						err = fmt.Errorf("read %q", b)
					}
				}
				if err != nil {
					errs <- fmt.Errorf("a read: %w", err)
				}
				_ = r.Close()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	r, err := o.OpenForReading(ctx, lab, s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := r.Put(ctx, "b/obj", strings.NewReader("x")); err == nil {
		t.Error("a store opened for reading took a Put")
	}
	if err := r.Delete(ctx, "a/obj"); err == nil {
		t.Error("a store opened for reading took a Delete")
	}
}
