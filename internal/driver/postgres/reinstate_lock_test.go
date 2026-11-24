package postgres

import (
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// TestReinstateAllAndByIDDoNotDeadlock guards the order in which
// ReinstateKeys locks rows: a reinstatement of all keys and one by ids,
// running at once, must each wait for the other, never both hold a row the
// other needs. The keys are written in the reverse of their ids' order, so
// an UPDATE that locked rows as it met them on disk would take kc before
// kb; a third connection holds kb, the reinstatement of all is queued on
// it, then the one of {ka, kc}. Were all to hold kc while waiting on kb,
// and the ids call to hold ka while waiting on kc, letting kb go would
// close the cycle and Postgres would fail one as a deadlock (SQLSTATE
// 40P01). Locked in the order of their ids, both succeed and every key is
// reinstated once.
func TestReinstateAllAndByIDDoNotDeadlock(t *testing.T) {
	r := newLockRig(t)
	for _, id := range []string{"kc", "kb", "ka"} {
		k := master.Key{ID: id, Hash: make([]byte, master.KeyHashLength), Holder: "h", Created: lockT0, Expires: lockT0.Add(time.Hour)}
		r.ok("CreateKey "+id, r.m.CreateKey(r.ctx, k, r.audit(), master.LocalOperator()))
	}
	_, err := r.m.SuspendKeys(r.ctx, lockT0, r.audit())
	r.ok("SuspendKeys", err)

	pid, release := r.hold(`SELECT id FROM api_keys WHERE id = 'kb' FOR UPDATE`)
	var wg sync.WaitGroup
	var nAll, nIDs int64
	var errAll, errIDs error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, nAll, errAll = r.m.ReinstateKeys(r.ctx, nil, true, r.audit(), master.LocalOperator())
	}()
	r.waitBlocked(pid, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, nIDs, errIDs = r.m.ReinstateKeys(r.ctx, []string{"ka", "kc"}, false, r.audit(), master.LocalOperator())
	}()
	r.waitBlocked(pid, 2)
	release()
	wg.Wait()

	if errAll != nil || errIDs != nil {
		t.Fatalf("reinstating all: %v; reinstating ka and kc: %v; want both to succeed, one after the other", errAll, errIDs)
	}
	if nAll+nIDs != 3 {
		t.Errorf("all reinstated %d and the ids %d; want the three keys reinstated once in all", nAll, nIDs)
	}
	for _, id := range []string{"ka", "kb", "kc"} {
		k, err := r.m.Key(r.ctx, id)
		r.ok("Key "+id, err)
		if !k.Suspended.IsZero() {
			t.Errorf("key %s is still suspended", id)
		}
	}
}
