package scrub

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errDown is the answer of a storage that does not answer.
var errDown = errors.New("the storage does not answer")

// downStore is a storage that answers nothing: a replica out of reach,
// which holds a purge in state purging.
type downStore struct{ blob.Store }

// Put fails.
func (downStore) Put(context.Context, string, io.Reader) (blob.Written, error) {
	return blob.Written{}, errDown
}

// Read fails.
func (downStore) Read(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errDown
}

// Stat fails.
func (downStore) Stat(context.Context, string) (blob.Info, error) { return blob.Info{}, errDown }

// Delete fails.
func (downStore) Delete(context.Context, string) error { return errDown }

// List fails.
func (downStore) List(context.Context, blob.ListOptions) (blob.Page, error) {
	return blob.Page{}, errDown
}

// TestPurgingInstanceIsNotFlaggedOnReplica guards that a replica is not
// called damaged while an instance delete on its scan is purging: the scan
// is not copied until the purge ends (a copy could put back a manifest that
// names the deleted instance), so the replica's row waits and a scrub passes
// it over, recording no damage and no finding.
func TestPurgingInstanceIsNotFlaggedOnReplica(t *testing.T) {
	r := newReplicaRig(t)
	p, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.505")
	copy1 := r.addReplica("copy")
	r.copyAll()
	victim := "1.2.826.0.1.3680043.10.505.2"

	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: fixtureTenantID, Keys: r.installationKeys, Store: r.p, Now: time.Now,
		OpenStorage: func(_ context.Context, name string) (blob.Store, error) {
			if name == "main" {
				return r.store, nil
			}
			return downStore{Store: copy1}, nil
		},
		Grace: func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }})
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan, Instance: victim}
	preview, err := r.p.PreviewDelete(r.ctx, target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: preview.Confirm, Audit: r.audit()})
	if err != nil {
		t.Fatalf("Delete = %+v, %v", c, err)
	}
	_, _ = e.PurgeNow(r.ctx, c.ID, r.audit(), nil) // the replica does not answer: the purge stays unfinished
	if got, err := r.p.Correction(r.ctx, c.ID); err != nil || got.State != record.StatePurging {
		t.Fatalf("the purge with its replica out of reach is %+v, %v; want purging", got, err)
	}
	// The instance's object is gone from the replica, as a purge that reached
	// it would leave it; no copy runs while the purge does.
	if err := copy1.Delete(r.ctx, keys[1]); err != nil {
		t.Fatal(err)
	}
	r.copyAll()
	if _, err := copy1.Stat(r.ctx, keys[1]); err == nil {
		t.Fatal("a copy made during the purge put the purging instance's object back")
	}
	if row := r.row("copy", series); row.State == tenant.ReplicaInStep {
		t.Fatalf("the replica's row before the scrub is %+v; want it waiting for the purge", row)
	}

	j := r.scrubAll(r.config())
	if row := r.row("copy", series); row.Damaged != 0 || row.State == tenant.ReplicaDamaged {
		t.Errorf("after the scrub the replica's row is %s with %d damaged; want no damage", row.State, row.Damaged)
	}
	if got := r.tallies(j.ID); got[TallyReplicaMissing] != 0 {
		t.Errorf("the scrub tallied %v; want nothing missing on the replica", got)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("the findings on the replica are %v; want none", got)
	}
}
