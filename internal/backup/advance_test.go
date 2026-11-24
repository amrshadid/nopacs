package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ownersStore is a tenant's database the owner check reads through; it
// runs afterOwners once, right after its owner generations are read.
type ownersStore struct {
	ownersDB
	afterOwners func()
}

// ownersDB is what the owner check asks of a tenant's database.
type ownersDB interface {
	tenant.Store
	tenant.Pipeline
	tenant.Restoring
}

// StorageOwners implements tenant.Restoring, running afterOwners once.
func (s *ownersStore) StorageOwners(ctx context.Context) ([]tenant.StorageOwner, error) {
	out, err := s.ownersDB.StorageOwners(ctx)
	if h := s.afterOwners; h != nil {
		s.afterOwners = nil
		h()
	}
	return out, err
}

// TestAFenceCheckBesideAnAdvanceDoesNotFence guards the registry's fence
// check running while an export or a reclaim of the same tenant takes its
// owner records one generation further: the advance lands right after the
// check read the database's generations, and the check still finds
// nothing, instead of reading a record past the generation it holds and
// fencing a tenant this installation is handing off.
func TestAFenceCheckBesideAnAdvanceDoesNotFence(t *testing.T) {
	r := newRig(t, false)
	if _, err := r.ownerCheck().Advance(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Fatal(err)
	}
	beside := &ownersStore{ownersDB: r.st.(ownersDB)}
	beside.afterOwners = func() {
		if _, err := r.ownerCheck().Advance(r.ctx, tenantID, "lab", r.st); err != nil {
			t.Errorf("the advance beside the check: %v", err)
		}
	}
	reason, err := r.ownerCheck().Fence(r.ctx, tenantID, "lab", beside)
	if err != nil || reason != "" {
		t.Fatalf("a fence check beside this installation's own advance: %q, %v", reason, err)
	}
	if beside.afterOwners != nil {
		t.Fatal("the advance did not run beside the check")
	}
	if o := r.ownerRecord(); o.Generation != 2 {
		t.Fatalf("the owner record after the advance is at %d, want 2", o.Generation)
	}
}

// TestAnAdvanceLeavesAPutInProgress guards the open an advance makes to
// write an owner record: a file in the storage's temporary directory, a
// Put of a store the server opened earlier still in flight, is left where
// it is.
func TestAnAdvanceLeavesAPutInProgress(t *testing.T) {
	r := newRig(t, false)
	tmp := filepath.Join(r.root, "lab", blob.ReservedPrefix+"tmp", "put-in-progress")
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ownerCheck().Advance(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "x" { // #nosec G304 -- the test's own file.
		t.Fatalf("the advance removed a Put in progress: %q, %v", b, err)
	}
	if o := r.ownerRecord(); o.Generation != 1 {
		t.Fatalf("the owner record after the advance is at %d, want 1", o.Generation)
	}
}

// TestAReclaimAfterAHalfAdvanceStillRefusesAnOlderExport guards a reclaim
// tried again after one whose advance wrote the database's generation and
// not the record: the retry is accepted, takes the record past the
// generation the export holds, and an import of that export is refused.
func TestAReclaimAfterAHalfAdvanceStillRefusesAnOlderExport(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	path, _, key := r.export()
	held, err := r.st.(tenant.Restoring).StorageOwners(r.ctx)
	if err != nil || len(held) != 1 {
		t.Fatalf("the generations after the export: %+v %v", held, err)
	}
	if err := r.st.(tenant.Restoring).SetStorageOwner(r.ctx, "main", held[0].Generation+1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ownerCheck().Reclaim(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Fatalf("the reclaim tried again: %v", err)
	}
	tg := newTarget(t, r)
	if _, err := tg.importInto(r, path, key, false); fault.KindOf(err) != fault.Conflict {
		t.Fatalf("the export older than the reclaim: %v", err)
	}
}
