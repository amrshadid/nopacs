package s3

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
)

// versionedBucket makes the fake answer as a bucket with the versioning
// status given ("" for never enabled, "deny" for refusing to say) and
// object lock when lock is set, holding versions v1 and v2 and the delete
// marker m1 of key, and version w1 of key+".x"; a delete of a version in
// locked is refused. It records the versions deleted.
func versionedBucket(f *fakeS3, status, key string, lock bool, locked map[string]bool) (deleted func() []string) {
	var mu sync.Mutex
	var gone []string
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && q.Has("versioning"):
			if status == "deny" {
				xmlError(w, http.StatusForbidden, "AccessDenied", "denied")
				return true
			}
			fmt.Fprintf(w, `<VersioningConfiguration>%s</VersioningConfiguration>`, map[bool]string{true: "<Status>" + status + "</Status>"}[status != ""])
			return true
		case r.Method == http.MethodGet && q.Has("object-lock"):
			if !lock {
				xmlError(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "none")
				return true
			}
			fmt.Fprint(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
			return true
		case r.Method == http.MethodGet && q.Has("versions"):
			fmt.Fprintf(w, `<ListVersionsResult><Name>%s</Name><IsTruncated>false</IsTruncated>`+
				`<Version><Key>%s</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest></Version>`+
				`<Version><Key>%s</Key><VersionId>v2</VersionId><IsLatest>false</IsLatest></Version>`+
				`<Version><Key>%s.x</Key><VersionId>w1</VersionId><IsLatest>true</IsLatest></Version>`+
				`<DeleteMarker><Key>%s</Key><VersionId>m1</VersionId><IsLatest>true</IsLatest></DeleteMarker></ListVersionsResult>`,
				fakeBucket, key, key, key, key)
			return true
		case r.Method == http.MethodGet && q.Has("retention"):
			fmt.Fprint(w, `<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>2031-02-03T04:05:06Z</RetainUntilDate></Retention>`)
			return true
		case r.Method == http.MethodDelete && q.Has("versionId"):
			v := q.Get("versionId")
			if locked[v] {
				xmlError(w, http.StatusForbidden, "AccessDenied", "object lock")
				return true
			}
			mu.Lock()
			gone = append(gone, strings.TrimPrefix(r.URL.Path, "/"+fakeBucket+"/")+"@"+v)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), gone...)
	}
}

// TestPurgeSaysWhatTheBucketDid guards that a purge never claims more than
// the bucket gave: an unversioned bucket's object removed; on a versioned
// bucket every version and delete marker of exactly the key deleted by its
// version id, the one object lock refuses retained with its retention's
// end, a sibling key untouched; with KeepVersions only a delete marker,
// hidden; a bucket whose versioning cannot be read, unknown. The preview's
// expectation says the same.
func TestPurgeSaysWhatTheBucketDid(t *testing.T) {
	ctx := context.Background()
	const key = "data/s/a/b/1.2.3.dcm"
	t.Run("unversioned", func(t *testing.T) {
		f := newFakeS3(t)
		versionedBucket(f, "", key, false, nil)
		s := openFake(t, f, nil)
		if _, err := s.Put(ctx, key, strings.NewReader("bytes")); err != nil {
			t.Fatal(err)
		}
		if d, why := s.PurgeExpected(ctx); d != blob.Removed || why != "unversioned bucket" {
			t.Fatalf("expected %s (%s)", d, why)
		}
		r, err := s.Purge(ctx, key)
		if err != nil || len(r) != 1 || r[0].Disposition != blob.Removed || r[0].Version != "" {
			t.Fatalf("Purge = %+v, %v; want one removal, removed", r, err)
		}
		if _, err := s.Stat(ctx, key); err == nil {
			t.Fatal("the purged object is still there")
		}
	})
	t.Run("versioned with a locked version", func(t *testing.T) {
		f := newFakeS3(t)
		deleted := versionedBucket(f, "Enabled", key, true, map[string]bool{"v2": true})
		s := openFake(t, f, nil)
		if d, _ := s.PurgeExpected(ctx); d != blob.Retained {
			t.Fatalf("a locked bucket is expected %s", d)
		}
		r, err := s.Purge(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]blob.Disposition{"v1": blob.Removed, "v2": blob.Retained, "m1": blob.Removed}
		if len(r) != 3 {
			t.Fatalf("Purge = %+v, want three versions", r)
		}
		for _, x := range r {
			if want[x.Version] != x.Disposition {
				t.Errorf("version %s answered %s, want %s", x.Version, x.Disposition, want[x.Version])
			}
			if x.Version == "v2" && !x.RetainUntil.Equal(time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)) {
				t.Errorf("the retained version's end is %v", x.RetainUntil)
			}
		}
		if got := deleted(); len(got) != 2 || got[0] != key+"@v1" || got[1] != key+"@m1" {
			t.Fatalf("the versions deleted are %q, want only the key's own", got)
		}
	})
	t.Run("versioned, versions kept", func(t *testing.T) {
		f := newFakeS3(t)
		deleted := versionedBucket(f, "Enabled", key, false, nil)
		s := openFake(t, f, func(c *Config) { c.KeepVersions = true })
		if _, err := s.Put(ctx, key, strings.NewReader("bytes")); err != nil {
			t.Fatal(err)
		}
		r, err := s.Purge(ctx, key)
		if err != nil || len(r) != 1 || r[0].Disposition != blob.Hidden {
			t.Fatalf("Purge = %+v, %v; want hidden", r, err)
		}
		if len(deleted()) != 0 {
			t.Fatal("a purge that keeps versions deleted one")
		}
		// The key now holds nothing, but the bucket keeps its versions: a
		// purge asked again (a purge resumed after a stop) says so.
		if r, err := s.Purge(ctx, key); err != nil || len(r) != 1 || r[0].Disposition != blob.Hidden {
			t.Fatalf("Purge of a key whose versions are kept = %+v, %v; want hidden", r, err)
		}
	})
	t.Run("versioning unreadable", func(t *testing.T) {
		f := newFakeS3(t)
		versionedBucket(f, "deny", key, false, nil)
		s := openFake(t, f, nil)
		if _, err := s.Put(ctx, key, strings.NewReader("bytes")); err != nil {
			t.Fatal(err)
		}
		r, err := s.Purge(ctx, key)
		if err != nil || len(r) != 1 || r[0].Disposition != blob.Unknown {
			t.Fatalf("Purge = %+v, %v; want unknown", r, err)
		}
		// A key that holds nothing on a bucket that may keep versions is
		// never claimed purged: unknown.
		if r, err := s.Purge(ctx, "data/s/a/b/9.dcm"); err != nil || len(r) != 1 || r[0].Disposition != blob.Unknown {
			t.Fatalf("Purge of a key that holds nothing = %+v, %v; want unknown", r, err)
		}
	})
	t.Run("unversioned, nothing there", func(t *testing.T) {
		f := newFakeS3(t)
		versionedBucket(f, "", key, false, nil)
		s := openFake(t, f, nil)
		if r, err := s.Purge(ctx, "data/s/a/b/9.dcm"); err != nil || len(r) != 0 {
			t.Fatalf("Purge of a key that holds nothing on an unversioned bucket = %+v, %v; want nothing to purge", r, err)
		}
	})
}

// TestPurgeOlderAndVersion guards the version purges a correction asks for:
// PurgeOlder deletes every version and delete marker of exactly the key but
// the current one, by version id, a locked one answered retained with its
// retention's end; PurgeVersion deletes the one version named; a bucket
// that keeps no versions has none older, and one whose versioning cannot be
// read answers unknown.
func TestPurgeOlderAndVersion(t *testing.T) {
	ctx := context.Background()
	const key = "meta/s/a/b/manifest.json"
	t.Run("versioned", func(t *testing.T) {
		f := newFakeS3(t)
		deleted := versionedBucket(f, "Enabled", key, true, map[string]bool{"v2": true})
		s := openFake(t, f, nil)
		r, err := s.PurgeOlder(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(r) != 2 || r[0].Version != "v1" || r[0].Disposition != blob.Removed || r[1].Version != "v2" ||
			r[1].Disposition != blob.Retained || r[1].RetainUntil.IsZero() {
			t.Fatalf("PurgeOlder = %+v, want v1 removed and v2 retained", r)
		}
		if got := deleted(); len(got) != 1 || got[0] != key+"@v1" {
			t.Fatalf("PurgeOlder deleted %q, want the older version alone, never the current delete marker", got)
		}
		v, err := s.PurgeVersion(ctx, key, "m1")
		if err != nil || v.Version != "m1" || v.Disposition != blob.Removed {
			t.Fatalf("PurgeVersion = %+v, %v", v, err)
		}
		if got := deleted(); len(got) != 2 || got[1] != key+"@m1" {
			t.Fatalf("PurgeVersion deleted %q, want m1 alone", got)
		}
		if _, err := s.PurgeVersion(ctx, key, ""); err == nil {
			t.Fatal("a version purge naming no version was taken")
		}
	})
	t.Run("unversioned", func(t *testing.T) {
		f := newFakeS3(t)
		versionedBucket(f, "", key, false, nil)
		s := openFake(t, f, nil)
		if r, err := s.PurgeOlder(ctx, key); err != nil || len(r) != 0 {
			t.Fatalf("PurgeOlder on an unversioned bucket = %+v, %v; want none", r, err)
		}
	})
	t.Run("versioning unreadable", func(t *testing.T) {
		f := newFakeS3(t)
		versionedBucket(f, "deny", key, false, nil)
		s := openFake(t, f, nil)
		if r, err := s.PurgeOlder(ctx, key); err != nil || len(r) != 1 || r[0].Disposition != blob.Unknown {
			t.Fatalf("PurgeOlder = %+v, %v; want unknown", r, err)
		}
	})
}
