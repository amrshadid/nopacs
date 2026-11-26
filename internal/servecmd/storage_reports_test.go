package servecmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// reportingStore is an S3 store as far as a status asks it: what it says
// of its encryption, or its failure.
type reportingStore struct {
	blob.Store
	enc    blob.Encryption
	err    error
	closed *int
}

// Encryption answers what the store was given.
func (s reportingStore) Encryption(context.Context) (blob.Encryption, error) { return s.enc, s.err }

// Close counts the close.
func (s reportingStore) Close() error {
	*s.closed++
	return nil
}

// TestStorageEncryptionIsAskedAtOpenAndKept guards the encryption a status
// shows of a storage: an S3 storage is opened and asked, closed after, and
// its answer kept for the row it was asked under, so a second status does
// not open it again; a changed row and an answer an hour old are asked
// again; a storage that cannot be asked is answered as not asked, so the
// status says unknown; a directory is never opened, its record being its
// admin's statement.
func TestStorageEncryptionIsAskedAtOpenAndKept(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	opens, closes := 0, 0
	var answer blob.Encryption
	var fail error
	r := &storageReports{
		open: func(context.Context, string, tenant.Storage) (blob.Store, error) {
			opens++
			return reportingStore{enc: answer, err: fail, closed: &closes}, nil
		},
		now: func() time.Time { return now },
	}
	row := tenant.Storage{Name: "cold", Type: tenant.StorageS3, Updated: now.Add(-time.Hour)}
	answer = blob.Encryption{State: blob.EncryptionOn, Algorithm: "aws:kms"}
	for range 2 {
		enc, asked := r.Encryption(ctx, "lab", row)
		if !asked || enc != answer {
			t.Fatalf("Encryption = %+v, %v; want %+v, asked", enc, asked, answer)
		}
	}
	if opens != 1 || closes != 1 {
		t.Fatalf("two statuses opened the storage %d times and closed it %d; want once each", opens, closes)
	}
	answer = blob.Encryption{State: blob.EncryptionNone}
	row.Updated = now
	if enc, _ := r.Encryption(ctx, "lab", row); enc.State != blob.EncryptionNone || opens != 2 {
		t.Fatalf("after a change of the row: %+v, %d opens; want none, asked again", enc, opens)
	}
	now = now.Add(encryptionKept + time.Second)
	fail = errors.New("the bucket does not answer")
	if enc, asked := r.Encryption(ctx, "lab", row); asked || enc != (blob.Encryption{}) || opens != 3 {
		t.Fatalf("a storage that does not answer: %+v, %v, %d opens; want not asked, asked again", enc, asked, opens)
	}
	if _, asked := r.Encryption(ctx, "tenant", tenant.Storage{Name: "disk", Type: tenant.StorageDirectory}); asked || opens != 3 {
		t.Fatalf("a directory: asked %v, %d opens; want never opened", asked, opens)
	}
}
