package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReports answers a storage's encryption and coverage as a test sets
// them: nothing asked, and no coverage, until then.
type testReports struct {
	mu       sync.Mutex
	enc      map[string]blob.Encryption
	coverage map[string]api.StorageCoverage
}

// Encryption answers what the test set for the storage.
func (r *testReports) Encryption(_ context.Context, _ string, s tenant.Storage) (blob.Encryption, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.enc[s.Name]
	return e, ok
}

// Coverage answers what the test set for the storage, or unavailable.
func (r *testReports) Coverage(_ context.Context, _ string, _ tenant.Store, storage string) (api.StorageCoverage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cv, ok := r.coverage[storage]; ok {
		return cv, nil
	}
	return api.StorageCoverage{}, fault.New(fault.Unavailable, "no coverage in this test")
}

// set sets what a storage answers.
func (r *testReports) set(storage string, e *blob.Encryption, cv *api.StorageCoverage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.enc == nil {
		r.enc, r.coverage = map[string]blob.Encryption{}, map[string]api.StorageCoverage{}
	}
	if e != nil {
		r.enc[storage] = *e
	}
	if cv != nil {
		r.coverage[storage] = *cv
	}
}

// storageStatusOf returns the status of one storage of lab1.
func storageStatusOf(t *testing.T, e *env, c caller, name string) (*v1.StorageStatus, *v1.TenantStatus) {
	t.Helper()
	o := call(e.as(c).monitor.GetStatus, &v1.GetStatusRequest{Scope: tenantScope("lab1")})
	if o.err != nil {
		t.Fatal(o.err)
	}
	ts := o.res.(*v1.GetStatusResponse).GetTenants()[0]
	for _, s := range ts.GetStorages() {
		if s.GetStorage() == name {
			return s, ts
		}
	}
	t.Fatalf("no status of %s in %v", name, ts.GetStorages())
	return nil, nil
}

// TestStorageStatusCopies guards what a status says of a storage's copies
// and keeping: a directory's encryption is its admin's statement, declared
// or not, which SetStorage changes; what the storage said, and its scrub
// coverage, when the server has them; a replica's primary; the tenant's
// replicas with their counts; and where its retention stands.
func TestStorageStatusCopies(t *testing.T) {
	f := newCopiesFixture(t)
	f.attach(t, "copy", "main")
	s, ts := storageStatusOf(t, f.env, f.lab1Reader, "main")
	if s.GetEncryptionAtRest() != v1.EncryptionAtRest_ENCRYPTION_AT_REST_NOT_DECLARED || s.GetTier() != v1.StorageTier_STORAGE_TIER_ONLINE ||
		s.GetCoverage() != nil || s.GetSwitching() {
		t.Errorf("main %v", s)
	}
	if len(ts.GetReplicas()) != 1 || ts.GetReplicas()[0].GetReplica() != "copy" || ts.GetRetention() == nil || ts.GetRetention().GetPaused() {
		t.Errorf("tenant replicas %v retention %v", ts.GetReplicas(), ts.GetRetention())
	}
	if c, _ := storageStatusOf(t, f.env, f.lab1Reader, "copy"); c.GetReplicaOf() != "main" {
		t.Errorf("copy %v", c)
	}

	o := call(f.as(f.lab1Admin).storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1",
		Storage:    &v1.Storage{Name: "main", Type: &v1.Storage_Directory{Directory: &v1.DirectoryStorage{EncryptedAtRestDeclared: true}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"directory.encrypted_at_rest_declared"}}})
	if o.err != nil {
		t.Fatal(o.err)
	}
	if !o.res.(*v1.SetStorageResponse).GetStorage().GetDirectory().GetEncryptedAtRestDeclared() {
		t.Errorf("the statement was not kept: %v", o.res)
	}
	// What a directory's admin declared stands, whatever a report says.
	f.reports.set("main", &blob.Encryption{State: blob.EncryptionNone}, &api.StorageCoverage{Objects: 10, Verified: 4, Share: 0.4,
		OldestUnverified: 3 * time.Hour, Overdue: 6})
	s, _ = storageStatusOf(t, f.env, f.lab1Reader, "main")
	if s.GetEncryptionAtRest() != v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED {
		t.Errorf("main's encryption %v, want declared", s.GetEncryptionAtRest())
	}
	if cv := s.GetCoverage(); cv.GetObjects() != 10 || cv.GetVerified() != 4 || cv.GetShare() != 0.4 || cv.GetOverdue() != 6 ||
		cv.GetOldestUnverified().AsDuration() != 3*time.Hour {
		t.Errorf("coverage %v", cv)
	}
}

// TestEncryptionOfS3 guards how a status reads what an S3 storage said of
// its encryption: encrypted with its algorithm, none, or unknown when it
// said nothing or the server has not asked it.
func TestEncryptionOfS3(t *testing.T) {
	s3 := tenant.Storage{Name: "b", Type: tenant.StorageS3}
	cases := []struct {
		name  string
		enc   blob.Encryption
		asked bool
		want  v1.EncryptionAtRest
		alg   string
	}{
		{"encrypted", blob.Encryption{State: blob.EncryptionOn, Algorithm: "aws:kms"}, true, v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED, "aws:kms"},
		{"none", blob.Encryption{State: blob.EncryptionNone}, true, v1.EncryptionAtRest_ENCRYPTION_AT_REST_NONE, ""},
		{"would not say", blob.Encryption{}, true, v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNKNOWN, ""},
		{"not asked", blob.Encryption{State: blob.EncryptionOn, Algorithm: "AES256"}, false, v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNKNOWN, ""},
	}
	for _, c := range cases {
		got, alg := api.EncryptionMessage(s3, c.enc, c.asked)
		if got != c.want || alg != c.alg {
			t.Errorf("%s: %v %q, want %v %q", c.name, got, alg, c.want, c.alg)
		}
	}
}
