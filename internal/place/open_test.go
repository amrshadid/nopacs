package place

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
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

// versionedS3 is listingS3 in front of a bucket that keeps versions and has
// no object lock, so what a purge will do there depends only on the
// storage's own purge_versions.
type versionedS3 struct{ listingS3 }

func (f *versionedS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Has("versioning"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
	case r.Method == http.MethodGet && q.Has("object-lock"):
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>ObjectLockConfigurationNotFoundError</Code><Message>none</Message></Error>`))
	default:
		f.listingS3.ServeHTTP(w, r)
	}
}

// TestOpenCarriesPurgeVersions guards that Open hands an S3 storage's
// purge_versions to the driver: on a versioned bucket, a storage set to
// keep older versions expects its purges to hide an object behind a delete
// marker ("purge_versions off"), and one left at the default expects every
// version deleted. Without it a purge deleted versions a person asked to
// keep.
func TestOpenCarriesPurgeVersions(t *testing.T) {
	srv := httptest.NewServer(&versionedS3{})
	t.Cleanup(srv.Close)
	o := Opener{Values: fixedValues{endpoints: settings.S3Endpoints{{URL: srv.URL, PlainHTTP: true, PrivateAddress: true}}}, Keys: plainKeys{}, StaleUploadAge: -1}
	lab := Tenant{ID: "t1", Name: "lab", Secrets: oneSecret{}}
	for _, c := range []struct {
		keep bool
		want blob.Disposition
		why  string
	}{
		{false, blob.Removed, "versioned bucket, every version deleted"},
		{true, blob.Hidden, "versioned bucket, purge_versions off"},
	} {
		s := tenant.Storage{Name: "cold", Type: tenant.StorageS3, S3: tenant.S3Storage{Endpoint: srv.URL, Region: "us-east-1", Bucket: "cold-bucket",
			AccessKeyID: "a", PathStyle: true, KeepVersions: c.keep}}
		bs, err := o.Open(context.Background(), lab, s)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		pe, ok := bs.(blob.PurgeExpecter)
		if !ok {
			t.Fatal("an S3 store says nothing of what its purge will do")
		}
		if got, why := pe.PurgeExpected(context.Background()); got != c.want || why != c.why {
			t.Errorf("keep versions %v: a purge is expected %s (%s), want %s (%s)", c.keep, got, why, c.want, c.why)
		}
		_ = bs.Close()
	}
}
