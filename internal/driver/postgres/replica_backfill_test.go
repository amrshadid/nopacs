package postgres

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestBackfillWhileACopyEnds guards a backfill that waits for a copy of one
// of the replica's scans to end in step, at read committed: the copy's end
// is written here as EndReplicaScan writes it and held open until the
// backfill waits for the row, then committed. The row must end pending and
// owed to the backfill (the copy runs again), never in step yet owed, which
// nothing would claim; and once copied again the backfill's job ends done.
func TestBackfillWhileACopyEnds(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.storage("copy")
	e.receiver("a", "store", 11112)
	_, _, _, err := e.p().AttachReplica(e.ctx, "copy", "store", cT0, e.audit(), nil)
	e.ok("AttachReplica", err)
	r, fs := e.identified("a", cUID(1, 0), "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{cFile(r, 0, 0), cFile(r, 0, 1)}
	})
	claims := make([]tenant.InstanceClaim, len(fs))
	for i, f := range fs {
		claims[i] = claimOfFile(f)
	}
	_, err = e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
	e.ok("ClaimInstances", err)
	_, err = e.p().CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-1", "scan-1", "store", fs))
	e.ok("CommitScan", err)
	series := fs[0].SeriesUID
	leased, err := e.p().ClaimReplicaScans(e.ctx, "copier", cT0.Add(4*time.Minute), time.Hour, 10)
	e.ok("ClaimReplicaScans", err)
	if len(leased) != 1 {
		t.Fatalf("a claim leased %d rows, want the scan's one", len(leased))
	}

	end, err := e.db.Begin(e.ctx)
	e.ok("begin", err)
	defer func() { _ = end.Rollback(e.ctx) }()
	if _, err := end.Exec(e.ctx, `UPDATE replica_scans SET state = 'in_step', queued_at = NULL, lease_until = NULL, owner = '',
		verified_at = now(), backfill = false, last_error = '' WHERE replica = 'copy' AND series_uid = $1`, series); err != nil {
		t.Fatal(err)
	}
	if _, err := end.Exec(e.ctx, `UPDATE replica_totals SET copying = copying - 1, in_step = in_step + 1 WHERE replica = 'copy'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var job tenant.Job
	go func() {
		var err error
		job, _, err = e.p().BackfillReplica(e.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: e.id("J"), Now: cT0.Add(5 * time.Minute)},
			e.audit(), nil)
		done <- err
	}()
	e.waitBlocked(1)
	if err := end.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ok("BackfillReplica", <-done)
	if n := e.scalar(`SELECT count(*) FROM replica_scans WHERE replica = 'copy' AND state = 'in_step' AND backfill`); n != 0 {
		t.Fatalf("%d rows are in step and owed to the backfill, which nothing copies again", n)
	}
	if n := e.scalar(`SELECT count(*) FROM replica_scans WHERE replica = 'copy' AND state = 'pending' AND backfill`); n != 1 {
		t.Fatalf("%d rows are pending and owed, want the scan's one", n)
	}
	if job.Counts.Scans != 1 {
		t.Errorf("the backfill's job counts %d scans, want 1", job.Counts.Scans)
	}
	again, err := e.p().ClaimReplicaScans(e.ctx, "copier", cT0.Add(6*time.Minute), time.Hour, 10)
	e.ok("ClaimReplicaScans again", err)
	if len(again) != 1 {
		t.Fatalf("a claim after the backfill leased %d rows, want 1", len(again))
	}
	if _, err := e.p().EndReplicaScan(e.ctx, again[0].Fence, again[0].ManifestSHA256, cT0.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ended, err := e.p().Job(e.ctx, job.ID)
	e.ok("Job", err)
	if ended.State != tenant.JobDone || ended.Counts.ScansDone != 1 {
		t.Errorf("the backfill's job after the copy is %s with %d of %d scans done, want done", ended.State, ended.Counts.ScansDone, ended.Counts.Scans)
	}
}
