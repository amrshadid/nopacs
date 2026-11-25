package scrub

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// unanswering is a store that opens but answers no read, listing or stat:
// each fails as unavailable, as a bucket out of reach does.
type unanswering struct{ blob.Store }

// errUnanswered is what each call of an unanswering store fails with.
var errUnanswered = fault.New(fault.Unavailable, "the storage does not answer")

// Read fails as unavailable.
func (unanswering) Read(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errUnanswered
}

// List fails as unavailable.
func (unanswering) List(context.Context, blob.ListOptions) (blob.Page, error) {
	return blob.Page{}, errUnanswered
}

// Stat fails as unavailable.
func (unanswering) Stat(context.Context, string) (blob.Info, error) {
	return blob.Info{}, errUnanswered
}

// TestReplicaThatFailsReadsIsPassedOver guards that a replica that opens
// but fails its reads does not hold back the scrub of its primary's scans:
// the job ends done, every instance read on the primary and the primary's
// copy recorded verified, the replica tallied unreadable, nothing recorded
// of it (no damage, no verification), and its row as it was.
func TestReplicaThatFailsReadsIsPassedOver(t *testing.T) {
	r := newReplicaRig(t)
	_, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.513")
	copy1 := r.addReplica("copy")
	r.copyAll()
	mem := &memVerifications{got: map[string]bool{}}
	cfg := r.config()
	cfg.Verifications = func(tenant.Store) VerificationRecorder { return mem }
	stores := named{"main": readOnly{Store: r.store, writes: &r.writes}, "copy": readOnly{Store: unanswering{copy1}, writes: &r.writes}}
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages { return stores }

	j := r.createScrub("")
	s := r.start(cfg)
	got := r.waitEndedWithin(s, j.ID, 20*time.Second)
	if got.State != tenant.JobDone || got.Counts.InstancesSent != 2 || got.Counts.InstancesRefused != 0 {
		t.Errorf("the scrub ended as %s with %d sent, %d refused; want done with both instances read good on the primary", got.State,
			got.Counts.InstancesSent, got.Counts.InstancesRefused)
	}
	if n := r.tallies(j.ID)[TallyReplicaUnreadable]; n != 1 {
		t.Errorf("the scrub tallied %d replicas unreadable; want 1", n)
	}
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("the unreadable replica's row is %s with %d damaged; want in step as it was", row.State, row.Damaged)
	}
	if got := mem.list(); len(got) != 1 || got[0] != "main s1 presence=false" {
		t.Errorf("the verifications recorded are %v; want only the primary's", got)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("the scrub recorded findings %v on a replica it could not read", got)
	}
}

// notOnlineStore answers one object as held in a tier it cannot read now.
type notOnlineStore struct {
	blob.Store
	key string
}

// Read fails the one object as not online.
func (n notOnlineStore) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == n.key {
		return nil, errors.Join(blob.ErrNotOnline, errors.New("held cold"))
	}
	return n.Store.Read(ctx, key, offset, length)
}

// TestScanWithAnObjectNotOnlineIsNotVerified guards that coverage counts a
// scan verified only when every one of its objects was read: a scan with
// an instance not online now is not recorded, a scan read whole beside it
// is.
func TestScanWithAnObjectNotOnlineIsNotVerified(t *testing.T) {
	f := newFixture(t)
	files := synthetic("1.2.826.0.1.3680043.10.514", 2)
	f.archiveEntries("s1", "1.2.826.0.1.3680043.10.514.90", "1.2.826.0.1.3680043.10.514.80", files)
	f.archiveEntries("s2", "1.2.826.0.1.3680043.10.515.90", "1.2.826.0.1.3680043.10.515.80", synthetic("1.2.826.0.1.3680043.10.515", 2))
	mem := &memVerifications{got: map[string]bool{}}
	cfg := f.config()
	cfg.Verifications = func(tenant.Store) VerificationRecorder { return mem }
	cold := notOnlineStore{Store: f.store, key: f.keys[files[1].uid]}
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages { return storages{s: readOnly{Store: cold, writes: &f.writes}} }
	j := f.scrub(cfg)
	if n := f.tallies(j.ID)[tenant.TallyNotOnline]; n != 1 {
		t.Errorf("the scrub tallied %d objects not online; want 1", n)
	}
	if got := mem.list(); len(got) != 1 || got[0] != "main s2 presence=false" {
		t.Errorf("the verifications recorded are %v; want only the scan read whole", got)
	}
}
