package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// testHoldRecordsOfAStorageGone guards that a storage removed, or a replica
// detached, keeps no record row of any hold: nothing more is written there,
// so a row left would be owed for ever and name a storage the hold is no
// longer on.
func testHoldRecordsOfAStorageGone(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	e.addStorage("copy")
	if _, _, _, err := e.p.AttachReplica(e.ctx, "copy", "store", at(1), e.audit(), nil); err != nil {
		e.Fatalf("AttachReplica: %v", err)
	}
	h := e.place(tenant.HoldTarget{Subject: "sub-01"}, "audit", 10)
	if !slices.Equal(h.Storages, []string{"copy", "store"}) || !slices.Equal(h.StoragesPending, []string{"copy"}) {
		e.Fatalf("a hold with its replica not written is on %v owing %v", h.Storages, h.StoragesPending)
	}
	if _, _, err := e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(20)}, e.audit(), nil); err != nil {
		e.Fatalf("DetachReplica: %v", err)
	}
	got, err := e.p.Hold(e.ctx, h.ID)
	e.ok("Hold", err)
	if !slices.Equal(got.Storages, []string{"store"}) || len(got.StoragesPending) != 0 {
		e.Fatalf("after its replica was detached the hold is on %v owing %v, want the primary alone", got.Storages, got.StoragesPending)
	}
	if owed := e.owedPairs(tenant.HoldOwedPage{Limit: 10}); len(owed) != 0 {
		e.Fatalf("after the detach the records owed are %v", owed)
	}

	// A record found on a storage that holds no instance makes a row there;
	// the storage may still be removed.
	e.addStorage("spare")
	found := tenant.HoldFound{ID: e.holdID(30), Target: tenant.HoldTarget{Subject: "sub-07"}, Reason: "kept by the record",
		Placed: at(5), Storage: "spare", Now: at(30)}
	a, adopted, err := e.p.AdoptHold(e.ctx, found, e.audit())
	e.ok("AdoptHold", err)
	if !adopted || !slices.Equal(a.Storages, []string{"spare"}) {
		e.Fatalf("a hold adopted from a record on spare is %+v (adopted %v)", a, adopted)
	}
	_, err = e.p.RemoveStorage(e.ctx, "spare", e.audit(), nil)
	e.ok("RemoveStorage", err)
	got, err = e.p.Hold(e.ctx, a.ID)
	e.ok("Hold", err)
	if len(got.Storages) != 0 || len(got.StoragesPending) != 0 || got.State != tenant.HoldActive {
		e.Fatalf("after its storage was removed the hold is %+v, want it active with no record row", got)
	}
}
