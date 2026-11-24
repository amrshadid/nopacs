package postgres

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestClaimBesideACommitIsDropped guards that a UID is never claimed and
// held at once. Nothing in the schema ties the claims table to the
// instances table, so the rule rests on a probe after a claim is written:
// when one reception's commit of its claim is still open, a second
// reception's claim of the same UID waits for it, is then written, and must
// find the UID held and be dropped, leaving the instance as the only row for
// the UID.
func TestClaimBesideACommitIsDropped(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	e.receiver("b", "store", 11113)
	study := cUID(1, 0)
	ra, fa := e.identified("a", study, "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{cFile(r, 0, 1), cFile(r, 0, 2)}
	})
	rb, _ := e.identified("b", study, "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{cFile(r, 0, 2)}
	})
	_, err := e.p().ClaimInstances(e.ctx, ra.ID, []tenant.InstanceClaim{claimOfFile(fa[0]), claimOfFile(fa[1])}, cT0, nil)
	e.ok("claim A", err)
	// A's first commit makes the scan the second commit adds to.
	_, err = e.p().CommitScan(e.ctx, scanCommit(ra, "sub-01", "ses-1", "scan-1", "store", fa[:1]))
	e.ok("commit A's first", err)
	scanID := e.scalar(`SELECT id FROM scans`)
	uid := fa[1].SOPInstanceUID

	// A's commit of the second file, left open: it has moved the claim into
	// instances and committed neither.
	txA, err := e.p().pool.Begin(e.ctx)
	e.ok("begin A", err)
	defer func() { _ = txA.Rollback(e.ctx) }()
	_, err = txA.Exec(e.ctx, confirmSQL, scanID, micros(cT0), []string{uid}, []int64{0}, []int64{fa[1].Size},
		[]int64{fa[1].Size}, [][]byte{sumBytes(fa[1].SHA256)}, ra.ID, study, fa[1].SeriesUID)
	e.ok("A's confirmation", err)

	type result struct {
		lost []string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		txB, err := e.p().pool.Begin(e.ctx)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = txB.Rollback(e.ctx) }()
		lost, err := writeClaims(e.ctx, txB, rb.ID, cT0, []string{uid}, []string{study}, []string{fa[1].SeriesUID},
			[][]byte{sumBytes(fa[1].SHA256)})
		if err == nil {
			err = txB.Commit(e.ctx)
		}
		done <- result{lost, err}
	}()
	e.waitBlocked(1)
	e.ok("commit A", txA.Commit(e.ctx))
	got := <-done
	e.ok("B's claim", got.err)
	if len(got.lost) != 1 || got.lost[0] != uid {
		t.Errorf("B's claim lost %v, want [%s]: the UID was already held", got.lost, uid)
	}
	if n := e.scalar(`SELECT count(*) FROM instance_claims WHERE sop_instance_uid = $1`, uid); n != 0 {
		t.Errorf("%d claims remain on a UID that is held", n)
	}
	if n := e.scalar(`SELECT count(*) FROM instances WHERE sop_instance_uid = $1`, uid); n != 1 {
		t.Errorf("the UID is held %d times, want once", n)
	}
}
