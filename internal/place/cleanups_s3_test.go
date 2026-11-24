package place

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// plainKeys opens a sealed secret as itself.
type plainKeys struct{}

func (plainKeys) Open(_ string, sealed []byte) ([]byte, error) { return sealed, nil }

// oneSecret hands back the same sealed secret for every storage.
type oneSecret struct{}

func (oneSecret) StorageSecret(context.Context, string) ([]byte, error) { return []byte("secret"), nil }

// listingS3 is the least of an S3 server an open to write needs: an empty
// bucket, a probe upload the credentials may not make (which the driver
// takes as a note, not a failure), and a listing of unfinished uploads
// that is refused while refuse is set and empty otherwise. listings counts
// the listings of uploads, which only the cleanup makes.
type listingS3 struct {
	refuse   atomic.Bool
	listings atomic.Int64
}

func (f *listingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Has("uploads"):
		f.listings.Add(1)
		if f.refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListMultipartUploadsResult><Bucket>b</Bucket><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`))
	case r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`))
	default:
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
	}
}

// TestAnS3SweepThatIsNotWholeRunsAgain guards that Cleanups counts an S3
// storage's cleanup of old uploads as done only when it was whole: an open
// whose listing of uploads was refused (or, alike, cut at its bound, or
// whose aborts failed) leaves the sweep to the next open, which lists
// again; once a sweep is whole, later opens of the process list nothing.
func TestAnS3SweepThatIsNotWholeRunsAgain(t *testing.T) {
	f := &listingS3{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o := Opener{
		Values:   fixedValues{endpoints: settings.S3Endpoints{{URL: srv.URL, PlainHTTP: true, PrivateAddress: true}}},
		Keys:     plainKeys{},
		Cleanups: &Cleanups{},
	}
	s := tenant.Storage{Name: "cold", Type: tenant.StorageS3,
		S3: tenant.S3Storage{Endpoint: srv.URL, Region: "us-east-1", Bucket: "cold-bucket", AccessKeyID: "a", PathStyle: true}}
	lab := Tenant{ID: "t1", Name: "lab", Secrets: oneSecret{}}
	open := func() {
		t.Helper()
		bs, err := o.Open(context.Background(), lab, s)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		_ = bs.Close()
	}

	f.refuse.Store(true)
	open()
	open()
	if n := f.listings.Load(); n != 2 {
		t.Fatalf("%d listings over two opens whose listing was refused, want 2: a sweep that was not whole was not run again", n)
	}
	f.refuse.Store(false)
	open()
	open()
	open()
	if n := f.listings.Load(); n != 3 {
		t.Fatalf("%d listings, want 3: the opens after a whole sweep swept again", n)
	}
}
