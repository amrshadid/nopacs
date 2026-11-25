package postgres

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCommitBesideAttachQueuesTheScan guards a scan committed while a
// replica of its storage is being attached: the attach's backfill cannot
// see the scan, not yet committed, so the commit's read of the replicas
// must see the attach, which it does because the commit reads its
// storage's row FOR SHARE before anything else of the scan, which waits for
// the attach's lock of that row. The order is made so on
// every run: the attach is held, after its backfill, at its audit entry,
// whose id an open transaction of the test has written; the commit is
// started then, and the test lets the attach go once the commit waits (or
// has ended, as it would if it took no lock). The scan must then be owed
// to the replica.
func TestCommitBesideAttachQueuesTheScan(t *testing.T) {
	e := newCenv(t, 2, 4)
	e.storage("store")
	e.storage("copy")
	e.receiver("a", "store", 11112)
	r, fs := e.identified("a", cUID(1, 0), "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{cFile(r, 0, 0), cFile(r, 0, 1), cFile(r, 0, 2)}
	})
	claims := make([]tenant.InstanceClaim, len(fs))
	for i, f := range fs {
		claims[i] = claimOfFile(f)
	}
	_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
	e.ok("ClaimInstances", err)

	held, err := e.db.Begin(e.ctx)
	e.ok("begin", err)
	defer func() { _ = held.Rollback(e.ctx) }()
	attachAudit := e.audit()
	if _, err := held.Exec(e.ctx, `INSERT INTO audit (id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome,
		reason, details, request_id) VALUES ($1, now(), '', '', '', 'Test', '', '', 'done', '', '{}', '')`, attachAudit.ID); err != nil {
		t.Fatal(err)
	}
	attached := make(chan error, 1)
	go func() {
		_, _, _, err := e.stores[0].AttachReplica(e.ctx, "copy", "store", cT0, attachAudit, nil)
		attached <- err
	}()
	e.waitBlocked(1)
	committed := make(chan error, 1)
	go func() {
		_, err := e.stores[1].CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-1", "scan-1", "store", fs))
		committed <- err
	}()
	var commitErr error
	commitEnded := false
	for deadline := time.Now().Add(30 * time.Second); !commitEnded; {
		select {
		case commitErr = <-committed:
			commitEnded = true
		default:
		}
		if commitEnded || e.scalar(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'`,
			e.app) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the commit neither waits nor ends after 30s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := held.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ok("AttachReplica", <-attached)
	if !commitEnded {
		commitErr = <-committed
	}
	e.ok("CommitScan", commitErr)
	if n := e.scalar(`SELECT count(*) FROM replica_scans WHERE replica = 'copy' AND series_uid = $1`, cUID(2, 0)); n != 1 {
		t.Errorf("the scan committed beside the attach has %d rows on the replica, want 1: it is owed to no replica", n)
	}
}
