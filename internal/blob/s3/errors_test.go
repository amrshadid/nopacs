package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestNotOnline guards the reading of an object held in an archive storage
// class: S3 answers its GET with 403 InvalidObjectState
// (docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html). A whole read
// and a ranged one must both fail as Unavailable with blob.ErrNotOnline in
// the chain, and not as refused credentials (which a scrub retries without
// end) nor as a missing object (which a reader would call data loss); the
// server's words stay out of the error, and another key still reads.
func TestNotOnline(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	ctx := context.Background()
	for _, k := range []string{"cold", "warm"} {
		if _, err := s.Put(ctx, k, strings.NewReader("bytes of "+k)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/cold") {
			return false
		}
		xmlError(w, http.StatusForbidden, "InvalidObjectState", "secret-server-words GLACIER")
		return true
	}
	for name, read := range map[string]func() error{
		"whole":  func() error { return second(s.Read(ctx, "cold", 0, -1)) },
		"ranged": func() error { return second(s.Read(ctx, "cold", 2, 3)) },
	} {
		err := read()
		if !errors.Is(err, blob.ErrNotOnline) {
			t.Errorf("%s read of a cold object = %v, want blob.ErrNotOnline in its chain", name, err)
		}
		if k := fault.KindOf(err); k != fault.Unavailable {
			t.Errorf("%s read of a cold object: kind %v, want Unavailable", name, k)
		}
		if errors.Is(err, ErrRefused) {
			t.Errorf("%s read of a cold object reads as refused: %v", name, err)
		}
		noLeak(t, name+" read of a cold object", err)
	}
	rc, err := s.Read(ctx, "warm", 0, -1)
	if err != nil {
		t.Fatalf("Read warm: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != "bytes of warm" {
		t.Errorf("Read warm = %q, %v; want its bytes", got, err)
	}
	// An AccessDenied 403 stays a refusal, never not online.
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		xmlError(w, http.StatusForbidden, "AccessDenied", "no")
		return true
	}
	if err := second(s.Read(ctx, "warm", 0, -1)); errors.Is(err, blob.ErrNotOnline) || !errors.Is(err, ErrRefused) {
		t.Errorf("a read refused with AccessDenied = %v, want refused and not online", err)
	}
}
