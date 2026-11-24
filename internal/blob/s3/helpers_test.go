package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amrshadid/nopacs/internal/blob"
)

// addressEnv names the variable that holds how to reach a scratch S3
// server the tests may write to: an http URL whose user and password are the
// access key id and secret, whose path is the bucket (made if it is
// missing), and whose query may say region=NAME and pathstyle=0 (the
// default is path style). requireEnv, when set to anything, turns a missing
// address from a skip into a failure: CI sets it, so a server that failed to
// start cannot make the tests pass by not running.
const (
	addressEnv = "NOPACS_TEST_S3"
	requireEnv = "NOPACS_REQUIRE_S3"
)

// allowAll is the address check of tests, which talk to servers on
// loopback.
func allowAll(netip.Addr) bool { return true }

// testConfig returns the configuration for the test server. Without one it
// skips the test and says why, or fails it when requireEnv is set.
func testConfig(tb testing.TB) Config {
	tb.Helper()
	address := os.Getenv(addressEnv)
	if address == "" {
		if os.Getenv(requireEnv) != "" {
			tb.Fatalf("%s is not set, and %s is: the S3 tests must run here, not be skipped", addressEnv, requireEnv)
		}
		tb.Skipf("%s is not set; the tests against a server are skipped (point it at http://KEY:SECRET@host:port/bucket of a scratch S3 server)", addressEnv)
	}
	u, err := url.Parse(address)
	if err != nil || u.User == nil || len(u.Path) < 2 {
		tb.Fatalf("%s is not an http URL with a key, a secret and a bucket", addressEnv)
	}
	secret, _ := u.User.Password()
	region := u.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	return Config{
		Endpoint:        u.Scheme + "://" + u.Host,
		Region:          region,
		Bucket:          u.Path[1:],
		PathStyle:       u.Query().Get("pathstyle") != "0",
		AccessKeyID:     NewSecret(u.User.Username()),
		SecretAccessKey: NewSecret(secret),
		AllowHTTP:       u.Scheme == "http",
		AllowAddress:    allowAll,
	}
}

var bucketOnce sync.Once

// ensureBucket makes the test bucket if the server does not have it.
func ensureBucket(tb testing.TB, cfg Config) {
	tb.Helper()
	bucketOnce.Do(func() {
		set, err := cfg.check()
		if err != nil {
			tb.Fatal(err)
		}
		c := awss3.New(clientOptions(set, newTransport(set)))
		_, err = c.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)})
		if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") && !strings.Contains(err.Error(), "BucketAlreadyExists") {
			tb.Logf("creating the bucket: %v", err) // Open says next if the bucket is unusable.
		}
	})
}

// uniquePrefix returns a prefix no other test in the bucket uses.
func uniquePrefix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "t-" + hex.EncodeToString(b[:]) + "/"
}

// openAt opens a store with cfg, closed when tb ends, whose objects and
// unfinished uploads are removed too.
func openAt(tb testing.TB, cfg Config) *Store {
	tb.Helper()
	s, err := Open(context.Background(), cfg)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(func() {
		// A test may have closed s; the cleanup works through a store of its own.
		c := newStore(s.set)
		sweepAll(tb, c)
		c.tr.CloseIdleConnections()
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	return s
}

// openTest opens a store in the test bucket under a prefix of its own.
func openTest(tb testing.TB) *Store {
	tb.Helper()
	cfg := testConfig(tb)
	ensureBucket(tb, cfg)
	cfg.Prefix = uniquePrefix()
	// Opening is not what these tests are about, and a cleanup of old uploads
	// on every one of dozens of stores in a few seconds is load for the server
	// and not a check of anything; the cleanup has tests of its own.
	cfg.StaleUploadAge = -1
	return openAt(tb, cfg)
}

// sweepAll fails the test if s left an unfinished upload, then deletes every
// object under its prefix, so the bucket does not fill up over many runs.
func sweepAll(tb testing.TB, s *Store) {
	tb.Helper()
	ctx := context.Background()
	if left := unfinished(tb, s); len(left) != 0 {
		tb.Errorf("%d unfinished multipart uploads are left under the prefix: %v", len(left), left)
		for _, u := range uploadsUnder(tb, s) {
			if err := s.abort(ctx, u.Key, u.UploadId); err != nil {
				tb.Errorf("cleanup: aborting an upload: %v", err)
			}
		}
	}
	var keys []string
	opts := blob.ListOptions{}
	for {
		p, err := s.List(ctx, opts)
		if err != nil {
			tb.Errorf("cleanup: List: %v", err)
			return
		}
		keys = append(keys, p.Keys...)
		if p.Next == "" {
			break
		}
		opts.Token = p.Next
	}
	var wg sync.WaitGroup
	work := make(chan string)
	for range 16 {
		wg.Go(func() {
			for k := range work {
				if err := s.Delete(ctx, k); err != nil {
					tb.Errorf("cleanup: Delete(%q): %v", k, err)
				}
			}
		})
	}
	for _, k := range slices.Clone(keys) {
		work <- k
	}
	close(work)
	wg.Wait()
}

// uploadsUnder returns the unfinished multipart uploads whose keys begin
// with the store's prefix, asking the server directly. It lists the whole
// bucket's uploads and filters here, since MinIO's listing matches a prefix
// against a whole key and would show nothing.
func uploadsUnder(tb testing.TB, s *Store) []types.MultipartUpload {
	tb.Helper()
	var ups []types.MultipartUpload
	in := &awss3.ListMultipartUploadsInput{Bucket: aws.String(s.set.bucket)}
	for {
		out, err := s.client.ListMultipartUploads(context.Background(), in)
		if err != nil {
			tb.Fatalf("listing unfinished uploads: %v", err)
		}
		for _, u := range out.Uploads {
			if strings.HasPrefix(aws.ToString(u.Key), s.set.prefix) {
				ups = append(ups, u)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return ups
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// unfinished returns the keys of the unfinished uploads under the store's
// prefix, without the prefix.
func unfinished(tb testing.TB, s *Store) []string {
	tb.Helper()
	var keys []string
	for _, u := range uploadsUnder(tb, s) {
		keys = append(keys, strings.TrimPrefix(aws.ToString(u.Key), s.set.prefix))
	}
	return keys
}

// keyOfLength returns a key the port accepts of exactly n bytes, made of
// segments of 200 bytes of c; n must be at least 1 and not above the port's
// longest key.
func keyOfLength(n int, c byte) string {
	var parts []string
	for n > 201 {
		parts = append(parts, strings.Repeat(string(c), 200))
		n -= 201
	}
	parts = append(parts, strings.Repeat(string(c), n))
	return strings.Join(parts, "/")
}

// longestPrefix is the longest storage prefix there may be, with its "/".
func longestPrefix() string { return strings.Repeat("p", MaxPrefixLength-1) + "/" }
