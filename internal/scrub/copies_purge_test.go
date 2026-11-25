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

// TestPurgingInstanceIsNotFlaggedOnReplica guards that an instance a
// delete in state purging hides is not called missing on a replica whose
// manifest still names it: the replicator skips the object of an instance
// whose purge began, and the purge removes it from every copy, so the
// replica's row stays in step, nothing is recorded, and the instance is
// counted as passed over.
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
	// The delete's publish queued the scan for the replica; the copy made
	// once the purge began skips the instance's object, which a first copy
	// had put there before the delete.
	if err := copy1.Delete(r.ctx, keys[1]); err != nil {
		t.Fatal(err)
	}
	r.copyAll()
	if _, err := copy1.Stat(r.ctx, keys[1]); err == nil {
		t.Fatal("the copy made during the purge put the purging instance's object")
	}
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep {
		t.Fatalf("the replica's row before the scrub is %+v; want in step", row)
	}

	j := r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after the scrub the replica's row is %s with %d damaged; want in step with none", row.State, row.Damaged)
	}
	if got := r.tallies(j.ID); got[TallyReplicaPurging] != 1 || got[TallyReplicaMissing] != 0 {
		t.Errorf("the scrub tallied %v; want the purging instance passed over once and nothing missing", got)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("the findings on the replica are %v; want none", got)
	}
}
