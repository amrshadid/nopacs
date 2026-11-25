package s3

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
)

// TestSuite guards that the S3 driver keeps the storage contract on the
// server the environment names, declaring what a bucket can do that a disk
// cannot: hold an object under a name that is also another's prefix. A
// bucket has no volume to report. The server checks and echoes the SHA-256
// of every request (Open refuses one that does not), so a small object's put
// is confirmed and HEAD with the checksum mode gives the object's SHA-256.
func TestSuite(t *testing.T) {
	testConfig(t) // Skips, or fails when the server is required, before Run starts.
	blobtest.Run(t, blobtest.Driver{
		Open:         func(tb testing.TB) blob.Store { return openTest(tb) },
		UsageKnown:   false,
		KeyAndPrefix: true,
		KeepsSHA256:  true,
		ConfirmsPuts: true,
	})
}
