package postgres

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestEnqueueHeldKeyAnswersFirst guards the order of EnqueueWork's answers:
// a unit queued under a unique key that a unit already holds is answered
// with that unit (created false) before anything about the new unit is
// looked at, its reception included, so a unit naming a reception the
// tenant does not have still gets the held unit, as one writer answers it.
func TestEnqueueHeldKeyAnswersFirst(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 0), "rc-1", cT0)
	e.ok("open", err)
	_, created, err := e.p().EnqueueWork(e.ctx, tenant.Work{ID: "u1", Kind: "reception", ReceptionID: r.ID, UniqueKey: "k", Due: cT0}, cT0)
	e.ok("enqueue", err)
	if !created {
		t.Fatal("the first unit under the key was not created")
	}
	got, created, err := e.p().EnqueueWork(e.ctx, tenant.Work{ID: "u2", Kind: "reception", ReceptionID: "rc-none", UniqueKey: "k", Due: cT0}, cT0)
	if err != nil || created || got.ID != "u1" {
		t.Fatalf("enqueue under a held key = %q, created %v, err %v (kind %v); want u1, created false, no error",
			got.ID, created, err, fault.KindOf(err))
	}
	// With the key free, the reception is checked.
	_, _, err = e.p().EnqueueWork(e.ctx, tenant.Work{ID: "u3", Kind: "reception", ReceptionID: "rc-none", UniqueKey: "k2", Due: cT0}, cT0)
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("enqueue naming a reception the tenant does not have = %v (kind %v); want a precondition failure", err, fault.KindOf(err))
	}
}
