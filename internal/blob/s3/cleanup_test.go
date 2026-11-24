package s3

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestCleanupNeverFailsOpen guards that Open succeeds whatever the cleanup of
// old uploads meets: an abort answered "no such upload" (another process
// aborted it first) counts as done, one that fails is counted and does not
// stop the others, a listing that fails is reported in the notes, and in no
// case does Open return an error for it, on this start or the next.
func TestCleanupNeverFailsOpen(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	setup := func(f *fakeS3) {
		f.listMode = "honor"
		f.stale = []fakeUpload{{"old/a", "u1", old}, {"old/b", "u2", old}, {"old/c", "u3", time.Now()}}
	}
	for _, c := range []struct {
		name            string
		status          int
		code            string
		aborted, failed int
	}{
		{"another process aborted it first", 404, "NoSuchUpload", 2, 0},
		{"the server fails the abort", 500, "InternalError", 1, 1},
		{"the abort is refused", 403, "AccessDenied", 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			setup(f)
			f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "u1" {
					xmlError(w, c.status, c.code, "secret-server-words")
					return true
				}
				return false
			}
			for i := range 2 { // And again, as a restart would.
				first := i == 0
				s, err := Open(context.Background(), f.config())
				if err != nil {
					t.Fatalf("Open = %v", err)
				}
				rep := s.Notes().Cleanup
				// The server keeps the upload whose abort failed, so a restart meets
				// it again and must not fail either.
				if rep.Err != nil || (first && (rep.Aborted != c.aborted || rep.Failed != c.failed || rep.Seen != 3)) {
					t.Errorf("start %d: report %+v, want %d aborted, %d failed, 3 seen", i+1, rep, c.aborted, c.failed)
				}
				_ = s.Close()
			}
		})
	}
	t.Run("the listing fails", func(t *testing.T) {
		f := newFakeS3(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodGet && r.URL.Query().Has("uploads") {
				xmlError(w, 403, "AccessDenied", "secret-server-words")
				return true
			}
			return false
		}
		s, err := Open(context.Background(), f.config())
		if err != nil {
			t.Fatalf("Open = %v", err)
		}
		defer s.Close()
		rep := s.Notes().Cleanup
		if rep.Err == nil {
			t.Fatal("the report does not say the listing failed")
		}
		noLeak(t, "cleanup listing", rep.Err)
	})
	t.Run("an abort that fails exactly", func(t *testing.T) {
		f := newFakeS3(t)
		setup(f)
		f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodDelete && r.URL.Query().Has("uploadId") {
				xmlError(w, 500, "InternalError", "x")
				return true
			}
			return false
		}
		s, err := Open(context.Background(), f.config())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if rep := s.Notes().Cleanup; rep.Failed != 2 || rep.Aborted != 0 || rep.Seen != 3 {
			t.Errorf("report %+v, want 2 failed, 0 aborted, 3 seen", rep)
		}
	})
}

// uploadsFor makes n old unfinished uploads under prefix.
func uploadsFor(prefix string, n int, at time.Time) []fakeUpload {
	var out []fakeUpload
	for i := range n {
		out = append(out, fakeUpload{fmt.Sprintf("%s%05d", prefix, i), fmt.Sprintf("id-%s%05d", prefix, i), at})
	}
	return out
}

// TestCleanupFindsUploadsPastTheFirstThousand guards that the cleanup pages
// through the uploads and starts where the storage's prefix sorts: with more
// than a page of other users' uploads sorting before this storage's, on a
// server that honors the prefix, on one that matches a prefix only against a
// whole key (MinIO), and on one that honors neither, this storage's old
// uploads are aborted and the others are not touched. The work of one call
// is bounded, the report says when uploads remain unseen, and the next call
// continues where the last stopped.
func TestCleanupFindsUploadsPastTheFirstThousand(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	ours := func() []fakeUpload {
		return []fakeUpload{{"t/obj/a", "ours-1", old}, {"t/obj/b", "ours-2", old}, {"t/obj/young", "ours-3", time.Now()}}
	}
	foreign := uploadsFor("a/", 2500, old)
	for _, mode := range []string{"honor", "exact", "wide", "paging-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeS3(t)
			f.listMode = mode
			f.stale = append(append(slices.Clone(foreign), ours()...), uploadsFor("u/", 100, old)...)
			s, err := Open(context.Background(), func() Config { c := f.config(); c.Prefix = "t"; return c }())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			rep := s.Notes().Cleanup
			t.Logf("%s: %+v after %d requests", mode, rep, f.count(http.MethodGet, "uploads"))
			f.mu.Lock()
			aborted := slices.Clone(f.abortedIDs)
			f.mu.Unlock()
			if rep.More {
				t.Errorf("%+v: 2,500 others precede ours and a call looks at 10 pages, so all should be seen", rep)
			}
			if mode != "paging-only" && f.count(http.MethodGet, "uploads") > 3 {
				t.Errorf("%d listing requests: starting where the prefix sorts should reach ours at once", f.count(http.MethodGet, "uploads"))
			}
			slices.Sort(aborted)
			if !slices.Equal(aborted, []string{"ours-1", "ours-2"}) {
				t.Errorf("aborted %v, want only this storage's two old uploads", aborted)
			}
			if rep.Aborted != 2 || rep.Failed != 0 || rep.Err != nil || rep.Seen != 3 {
				t.Errorf("report %+v", rep)
			}
			if got := f.count(http.MethodGet, "uploads"); got > sweepPages+1 {
				t.Errorf("%d listing requests for one call, want at most %d", got, sweepPages+1)
			}
		})
	}
	t.Run("beyond the bound", func(t *testing.T) {
		f := newFakeS3(t)
		f.listMode = "paging-only"
		f.stale = append(uploadsFor("a/", sweepPages*sweepPageSize+500, old), ours()...)
		s, err := Open(context.Background(), func() Config { c := f.config(); c.Prefix = "t"; return c }())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		rep := s.Notes().Cleanup
		if !rep.More || rep.Aborted != 0 || rep.Seen != 0 {
			t.Fatalf("first call %+v, want More with nothing of ours seen yet", rep)
		}
		if got := f.count(http.MethodGet, "uploads"); got > sweepPages+1 {
			t.Errorf("%d listing requests, want at most %d", got, sweepPages+1)
		}
		rep = s.AbortStaleUploads(context.Background())
		if rep.More || rep.Aborted != 2 || rep.Seen != 3 {
			t.Errorf("the next call %+v, want it to continue and reach this storage's uploads", rep)
		}
		// Having reached the end, a call starts again at the beginning.
		rep = s.AbortStaleUploads(context.Background())
		if !rep.More || rep.Aborted != 0 {
			t.Errorf("the call after the end %+v, want a new pass from the start", rep)
		}
	})
}

// TestCleanupOnServersSeesPastTheFirstThousand guards, on a real server, that
// this storage's old uploads are aborted and other users' are not, with a
// few uploads on every server, and, on a server whose listing does not match
// a prefix as a start (MinIO's), with 1,100 other uploads sorting before
// this storage's, which is more than a page. A server that honors the
// prefix is not made to hold the 1,100: the driver does not look past its
// prefix there, and a thousand uploads cost such a server a lot of time.
func TestCleanupOnServersSeesPastTheFirstThousand(t *testing.T) {
	cfg := serverConfig(t)
	cfg.StaleUploadAge = -1
	maker := openAt(t, cfg)
	ctx := context.Background()
	other := "aaa-" + uniquePrefix()
	var made []struct{ key, id string }
	var mu sync.Mutex
	begin := func(key string) {
		out, err := maker.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String(maker.set.bucket), Key: aws.String(key)})
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		made = append(made, struct{ key, id string }{key, aws.ToString(out.UploadId)})
		mu.Unlock()
	}
	t.Cleanup(func() {
		for _, m := range made {
			if strings.HasPrefix(m.key, other) {
				_ = maker.abort(ctx, aws.String(m.key), aws.String(m.id))
			}
		}
	})
	for i := range 5 {
		begin(fmt.Sprintf("%sk%04d", other, i))
	}
	// Does the server match an upload listing's prefix as a start?
	honors := len(mustUploads(t, maker, other)) > 0
	if !honors {
		var wg sync.WaitGroup
		work := make(chan string)
		for range 8 {
			wg.Go(func() {
				for k := range work {
					begin(k)
				}
			})
		}
		for i := 5; i < 1100; i++ {
			work <- fmt.Sprintf("%sk%04d", other, i)
		}
		close(work)
		wg.Wait()
	}
	t.Logf("the server honors a prefix in an upload listing: %v; %d other uploads made", honors, len(made))
	begin(maker.set.prefix + "obj/one")
	begin(maker.set.prefix + "obj/two")
	time.Sleep(1100 * time.Millisecond) // S3 times to the second.
	cfg.StaleUploadAge = time.Second
	s := openAt(t, cfg)
	rep := s.Notes().Cleanup
	if got := unfinished(t, maker); len(got) != 0 {
		t.Errorf("this storage's old uploads are left: %v (report %+v)", got, rep)
	}
	if rep.Aborted != 2 || rep.Failed != 0 || rep.Err != nil {
		t.Errorf("report %+v", rep)
	}
	var left int
	for _, u := range uploadsUnder(t, &Store{set: settings{prefix: other, bucket: maker.set.bucket}, client: maker.client}) {
		_ = u
		left++
	}
	if left != len(made)-2 {
		t.Errorf("the others' uploads were touched: %d of %d left", left, len(made)-2)
	}
}

// mustUploads lists the uploads the server returns for a prefix in the
// request itself.
func mustUploads(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	out, err := s.client.ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{Bucket: aws.String(s.set.bucket), Prefix: aws.String(prefix)})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, u := range out.Uploads {
		keys = append(keys, aws.ToString(u.Key))
	}
	return keys
}
