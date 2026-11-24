package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob/blobtest"
)

// manifestEnv names a file to which TestWriteForOutsideCheck writes what it
// put, for a program that is not this one (an S3 command line, a Python
// client) to read back and compare. Without it the test is skipped.
const manifestEnv = "NOPACS_S3_OUTSIDE_MANIFEST"

// TestWriteForOutsideCheck puts objects of sizes around the single-request
// limit and the part size, in nested keys, and writes their keys, sizes and
// SHA-256 to a manifest. It leaves them in the bucket for the outside
// reader, which removes them. It guards nothing by itself: the check is the
// other program agreeing with what Put reported.
func TestWriteForOutsideCheck(t *testing.T) {
	path := os.Getenv(manifestEnv)
	if path == "" {
		t.Skipf("%s is not set; no manifest is written for an outside reader", manifestEnv)
	}
	cfg := serverConfig(t)
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type object struct {
		Key    string `json:"key"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	var objects []object
	for i, n := range []int{0, 1, 1000, DefaultPartSize - 1, DefaultPartSize, DefaultPartSize + 1, 2 * DefaultPartSize, 3*DefaultPartSize + 12345, 40 << 20} {
		key := "data/sub-" + hex.EncodeToString([]byte{byte(i)}) + "/ses-1/scan.2/obj-" + hex.EncodeToString([]byte{byte(i)}) + ".dcm"
		data := blobtest.PatternBytes(uint64(i), n)
		w, err := s.Put(context.Background(), key, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object{key, int(w.Size), hex.EncodeToString(w.SHA256[:])})
		if w.SHA256 != sha256.Sum256(data) {
			t.Fatalf("Put returned the checksum of other bytes for %d bytes", n)
		}
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"endpoint": cfg.Endpoint, "region": cfg.Region, "bucket": cfg.Bucket, "prefix": s.set.prefix,
		"access_key": cfg.AccessKeyID.Reveal(), "secret_key": cfg.SecretAccessKey.Reveal(), "objects": objects,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- the path is the developer's own environment variable, in a test.
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
}
