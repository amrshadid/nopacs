package receive

import (
	"context"
	"net/netip"
	"net/url"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestStudyIsArchivedOnS3 guards the whole pipeline with an S3 storage: the
// study is archived byte for byte with its manifest and sidecar, under a
// prefix of its own in the scratch bucket.
func TestStudyIsArchivedOnS3(t *testing.T) {
	store := openTestS3(t, "receive-"+newID()[:12]+"/")
	e := newEnv(t, envOptions{storage: func(blob.Store) blob.Store { return store }})
	e.storage = store
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", rs[0])
	}
	checkArchive(t, e, rs[0], sentMap(t, studyFiles...))
}

// openTestS3 opens the scratch S3 server's bucket under prefix, closed when
// tb ends, or skips tb when there is none.
func openTestS3(tb testing.TB, prefix string) blob.Store {
	tb.Helper()
	address, ok := testkit.S3Address(tb)
	if !ok {
		tb.Skipf("%s is not set; the test against S3 is skipped", testkit.S3Env)
	}
	u, err := url.Parse(address)
	if err != nil || u.User == nil || len(u.Path) < 2 {
		tb.Fatalf("%s is not an http URL with a key, a secret and a bucket", testkit.S3Env)
	}
	secret, _ := u.User.Password()
	region := u.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	store, err := s3.Open(context.Background(), s3.Config{
		Endpoint: u.Scheme + "://" + u.Host, Region: region, Bucket: u.Path[1:], Prefix: prefix,
		PathStyle: u.Query().Get("pathstyle") != "0", AccessKeyID: s3.NewSecret(u.User.Username()),
		SecretAccessKey: s3.NewSecret(secret), AllowHTTP: u.Scheme == "http", AllowAddress: func(netip.Addr) bool { return true },
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}
