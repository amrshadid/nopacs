package tenanttest

import (
	"errors"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of holds.

// holdID returns a new hold id made at second s.
func (e *tenv) holdID(s int) string { return e.newID(s) }

// place places a hold of target with reason at second s, its record
// written on every storage HoldStorages answers but the replicas,
// failing the test on an error.
func (e *tenv) place(target tenant.HoldTarget, reason string, s int) tenant.Hold {
	e.Helper()
	storages, err := e.p.HoldStorages(e.ctx, target)
	e.ok("HoldStorages", err)
	var written []string
	for _, st := range storages {
		if !st.Replica {
			written = append(written, st.Name)
		}
	}
	h, made, err := e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.holdID(s), Target: target, Reason: reason, KeyID: "k-admin",
		Written: written, Now: at(s)}, e.audit(), nil)
	e.ok("PlaceHold", err)
	if !made || h.State != tenant.HoldActive {
		e.Fatalf("a new hold is %+v (made %v)", h, made)
	}
	return h
}

// wantHeld fails unless err is a delete's HELD refusal naming hold.
func (e *tenv) wantHeld(what string, err error, hold string) {
	e.Helper()
	var h *tenant.HeldRefusal
	if !errors.As(err, &h) || !errors.Is(err, tenant.ErrHeld) || fault.KindOf(err) != fault.Precondition || h.Hold != hold {
		e.Fatalf("%s answers %v, want HELD naming %s", what, err, hold)
	}
}

// testHolds guards placing, reading, listing and releasing holds: the
// record must be on every primary before the row commits; a hold placed
// again with the same target and reason answers the first; a target the
// tenant holds nothing of is NotFound; a release answers the hold
// released, and again as it stands; the list is newest first and keeps
// the whole tenant's holds with a subject's.
func testHolds(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	target := tenant.HoldTarget{Subject: "sub-01", Session: "ses-1"}
	_, _, err := e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.holdID(10), Target: target, Reason: "audit 2026-17", Now: at(10)}, e.audit(), nil)
	e.wantKind("a hold whose record is on no primary", err, fault.Precondition)
	_, err = e.p.HoldStorages(e.ctx, tenant.HoldTarget{Subject: "sub-09"})
	e.wantKind("a hold of a subject not held", err, fault.NotFound)
	_, err = e.p.HoldStorages(e.ctx, tenant.HoldTarget{Subject: "sub-01", WholeTenant: true})
	e.wantKind("a whole tenant's hold with a label", err, fault.Invalid)
	h := e.place(target, "audit 2026-17", 11)
	if h.Target != target || h.Reason != "audit 2026-17" || h.PlacedBy != "k-admin" || !h.Placed.Equal(at(11)) || h.Seq != 1 ||
		!slices.Equal(h.Storages, []string{"store"}) || len(h.StoragesPending) != 0 {
		e.Fatalf("a placed hold is %+v", h)
	}
	again, made, err := e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.holdID(12), Target: tenant.HoldTarget{Subject: "SUB-01", Session: "ses-1"},
		Reason: "audit 2026-17", Written: []string{"store"}, Now: at(12)}, e.audit(), nil)
	e.ok("PlaceHold again", err)
	if made || again.ID != h.ID {
		e.Fatalf("the same hold placed again answers %s (made %v), want %s", again.ID, made, h.ID)
	}
	if m, found, err := e.p.MatchingHold(e.ctx, target, "audit 2026-17"); err != nil || !found || m.ID != h.ID {
		e.Fatalf("MatchingHold answers %+v, %v, %v", m, found, err)
	}
	whole := e.place(tenant.HoldTarget{WholeTenant: true}, "litigation", 13)
	list := allPages(e.penv, 1, func(pg master.Page) ([]tenant.Hold, string, error) {
		return e.p.ListHolds(e.ctx, tenant.HoldFilter{Subject: "sub-01"}, pg)
	})
	if len(list) != 2 || list[0].ID != whole.ID || list[1].ID != h.ID {
		e.Fatalf("a subject's holds are %+v, want the whole tenant's then the session's", list)
	}
	r, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "audit closed", KeyID: "k-admin", Now: at(20)}, e.audit(), nil)
	e.ok("ReleaseHold", err)
	if r.State != tenant.HoldReleased || r.ReleaseReason != "audit closed" || !r.Released.Equal(at(20)) || r.Seq != 2 ||
		!slices.Equal(r.StoragesPending, []string{"store"}) {
		e.Fatalf("a released hold is %+v, want released with its record owed again", r)
	}
	if again, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "again", Now: at(21)}, e.audit(), nil); err != nil ||
		again.ReleaseReason != "audit closed" {
		e.Fatalf("a release asked again answers %+v, %v", again, err)
	}
	active := allPages(e.penv, 10, func(pg master.Page) ([]tenant.Hold, string, error) {
		return e.p.ListHolds(e.ctx, tenant.HoldFilter{States: []tenant.HoldState{tenant.HoldActive}}, pg)
	})
	if len(active) != 1 || active[0].ID != whole.ID {
		e.Fatalf("the active holds are %+v", active)
	}
	_, err = e.p.Hold(e.ctx, e.holdID(30))
	e.wantKind("a hold not held", err, fault.NotFound)
}

// testHoldRefusesDelete guards that a hold refuses a new delete of any of
// what it covers, naming itself, and that its preview names it; a delete
// of what it does not cover goes on; once released the delete is made.
func testHoldRefusesDelete(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100, 101}, 0, at(0), "ses-1", nil)
	e.archiveScans("recv", 2, []int{200}, 100, at(0), "ses-2", nil)
	h := e.place(tenant.HoldTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 101)}, "audit", 10)
	target := tenant.CorrectionTarget{Subject: "sub-01"}
	p, err := e.p.PreviewDelete(e.ctx, target, at(20))
	e.ok("PreviewDelete", err)
	if !slices.Equal(p.Holds, []string{h.ID}) {
		e.Fatalf("the preview names the holds %v, want %s", p.Holds, h.ID)
	}
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(21), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(21)}, e.audit(), nil)
	e.wantHeld("a subject's delete with a scan held", err, h.ID)
	e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 30)
	if _, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(40)}, e.audit(), nil); err != nil {
		e.Fatalf("ReleaseHold: %v", err)
	}
	e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 50)
}

// testHoldStopsPurge guards that a hold placed on data a delete committed
// keeps the delete committed and undoable, and stops its purge before any
// object: the purge starts, but its objects are refused, naming the hold,
// until the hold is released.
func testHoldStopsPurge(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 10)
	e.commit(c.ID, 20, nil)
	h := e.place(tenant.HoldTarget{Subject: "sub-01"}, "litigation", 30)
	if !slices.Equal(h.Storages, []string{"store"}) {
		e.Fatalf("a hold of a deleted subject is on %v, want the storage its data is on", h.Storages)
	}
	if id, err := e.p.CorrectionHeld(e.ctx, c.ID); err != nil || id != h.ID {
		e.Fatalf("CorrectionHeld answers %q, %v, want %s", id, err, h.ID)
	}
	if _, err := e.p.StartPurge(e.ctx, c.ID, at(40), &master.AuditEntry{ID: e.id("a"), Time: at(40), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil); err != nil {
		e.Fatalf("StartPurge of a held delete: %v", err)
	}
	_, err := e.p.PurgeTargets(e.ctx, c.ID, 10)
	e.wantHeld("a held delete's purge", err, h.ID)
	if _, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "settled", Now: at(50)}, e.audit(), nil); err != nil {
		e.Fatalf("ReleaseHold: %v", err)
	}
	targets, err := e.p.PurgeTargets(e.ctx, c.ID, 10)
	e.ok("PurgeTargets after the release", err)
	if len(targets) == 0 {
		e.Fatal("a released delete's purge has nothing to remove")
	}
}

// testHoldFollowsRelabel guards that a relabel's publish rewrites the
// labels of the holds it moves, owing their records again, so the hold
// still refuses a delete under the new label; and that a relabel's purge,
// which removes the objects under the old labels, waits for a hold of its
// new labels, whether the publish moved it there or it was placed after.
func testHoldFollowsRelabel(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	h := e.place(tenant.HoldTarget{Subject: "sub-01", Session: "ses-1"}, "audit", 10)
	c, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-02", 20)
	e.ok("PrepareCorrection of a relabel under a hold", err)
	e.commit(c.ID, 30, nil)
	got, err := e.p.Hold(e.ctx, h.ID)
	e.ok("Hold", err)
	if got.Target != (tenant.HoldTarget{Subject: "sub-02", Session: "ses-1"}) || got.Seq != 2 || !slices.Equal(got.StoragesPending, []string{"store"}) {
		e.Fatalf("after a relabel the hold is %+v, want it on sub-02 with its record owed", got)
	}
	p, err := e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-02"}, at(40))
	e.ok("PreviewDelete", err)
	if !slices.Equal(p.Holds, []string{h.ID}) {
		e.Fatalf("a delete's preview under the new label names the holds %v, want %s", p.Holds, h.ID)
	}
	if p, err := e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-01"}, at(41)); err == nil {
		e.Fatalf("the old label still resolves: %+v", p)
	}
	// The relabel's purge removes the objects under the old labels, which
	// the moved hold keeps: it waits for the hold.
	if id, err := e.p.CorrectionHeld(e.ctx, c.ID); err != nil || id != h.ID {
		e.Fatalf("a relabel whose hold moved to the new labels answers CorrectionHeld %q, %v, want %s", id, err, h.ID)
	}
	// A hold placed on the new labels after a relabel's publish holds that
	// relabel's purge too.
	e.archiveScansOf("sub-05", "store", "recv", 2, []int{200}, 100, at(50), "ses-2", nil)
	c2, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-05", Session: "ses-2"}, "ses-9", 60)
	e.ok("PrepareCorrection of a relabel", err)
	e.commit(c2.ID, 70, nil)
	if id, err := e.p.CorrectionHeld(e.ctx, c2.ID); err != nil || id != "" {
		e.Fatalf("a relabel of a session no hold covers answers CorrectionHeld %q, %v", id, err)
	}
	h2 := e.place(tenant.HoldTarget{Subject: "sub-05", Session: "ses-9"}, "audit of the relabeled session", 80)
	if id, err := e.p.CorrectionHeld(e.ctx, c2.ID); err != nil || id != h2.ID {
		e.Fatalf("a relabel whose new labels a later hold names answers CorrectionHeld %q, %v, want %s", id, err, h2.ID)
	}
}

// testHoldRecordsFollowTarget guards that a hold's target spreading after
// the hold is placed owes its record where it spread: a replica attached
// to the primary its data is on, and another primary a held subject's new
// scan lands on; a hold that covers neither is owed nothing more.
func testHoldRecordsFollowTarget(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	e.archiveScansOf("sub-03", "store", "recv", 3, []int{300}, 300, at(1), "ses-3", nil)
	h := e.place(tenant.HoldTarget{Subject: "sub-01"}, "audit", 10)
	other := e.place(tenant.HoldTarget{Subject: "sub-03", Session: "ses-3"}, "another audit", 11)
	e.addStorage("copy")
	if _, _, _, err := e.p.AttachReplica(e.ctx, "copy", "store", at(20), e.audit(), nil); err != nil {
		e.Fatalf("AttachReplica: %v", err)
	}
	got, err := e.p.Hold(e.ctx, h.ID)
	e.ok("Hold", err)
	if !slices.Equal(got.Storages, []string{"copy", "store"}) || !slices.Equal(got.StoragesPending, []string{"copy"}) {
		e.Fatalf("after a replica of its primary was attached the hold is on %v owing %v, want the replica owed", got.Storages,
			got.StoragesPending)
	}
	e.addReceiver("recv2", "elsewhere", 11113)
	e.archiveScansOf("sub-01", "elsewhere", "recv2", 2, []int{200}, 100, at(30), "ses-2", nil)
	got, err = e.p.Hold(e.ctx, h.ID)
	e.ok("Hold", err)
	if !slices.Equal(got.Storages, []string{"copy", "elsewhere", "store"}) || !slices.Equal(got.StoragesPending, []string{"copy", "elsewhere"}) {
		e.Fatalf("after a new scan of the held subject landed on another primary the hold is on %v owing %v, want that primary owed",
			got.Storages, got.StoragesPending)
	}
	if o, err := e.p.Hold(e.ctx, other.ID); err != nil || !slices.Equal(o.Storages, []string{"copy", "store"}) {
		e.Fatalf("a hold the new scan is not under is on %v (%v), want its primary and the replica attached", o.Storages, err)
	}
	owed, err := e.p.HoldRecordsOwed(e.ctx, tenant.HoldOwedPage{Limit: 10})
	e.ok("HoldRecordsOwed", err)
	var pairs []string
	for _, o := range owed {
		pairs = append(pairs, o.Hold.ID+"@"+o.Storage)
	}
	want := []string{h.ID + "@copy", h.ID + "@elsewhere", other.ID + "@copy"}
	if !slices.Equal(pairs, want) {
		e.Fatalf("the records owed are %v, want %v", pairs, want)
	}
}

// testHoldRecords guards where a hold's records go: every storage the
// target touches and its replicas; a replica not written is owed the
// record until marked; an older count marks nothing; a record found with
// no row is adopted as an active hold, once.
func testHoldRecords(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	e.addStorage("copy")
	if _, _, _, err := e.p.AttachReplica(e.ctx, "copy", "store", at(1), e.audit(), nil); err != nil {
		e.Fatalf("AttachReplica: %v", err)
	}
	storages, err := e.p.HoldStorages(e.ctx, tenant.HoldTarget{Subject: "sub-01"})
	e.ok("HoldStorages", err)
	if !slices.Equal(storages, []tenant.HoldStorage{{Name: "copy", Replica: true}, {Name: "store"}}) {
		e.Fatalf("a hold's storages are %+v", storages)
	}
	h := e.place(tenant.HoldTarget{Subject: "sub-01"}, "audit", 10)
	if !slices.Equal(h.StoragesPending, []string{"copy"}) {
		e.Fatalf("a hold not written on its replica owes %v", h.StoragesPending)
	}
	owed, err := e.p.HoldRecordsOwed(e.ctx, tenant.HoldOwedPage{Limit: 10})
	e.ok("HoldRecordsOwed", err)
	if len(owed) != 1 || owed[0].Hold.ID != h.ID || owed[0].Storage != "copy" {
		e.Fatalf("the records owed are %+v", owed)
	}
	e.wantKind("a count the hold has not reached", e.p.MarkHoldRecord(e.ctx, h.ID, "copy", 2, 1), fault.Precondition)
	e.wantKind("a storage the hold is not on", e.p.MarkHoldRecord(e.ctx, h.ID, "other", 1, 1), fault.NotFound)
	e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, h.ID, "copy", 1, 1))
	if owed, _ := e.p.HoldRecordsOwed(e.ctx, tenant.HoldOwedPage{Limit: 10}); len(owed) != 0 {
		e.Fatalf("after the mark the records owed are %+v", owed)
	}
	found := tenant.HoldFound{ID: e.holdID(20), Target: tenant.HoldTarget{Subject: "sub-07"}, Reason: "kept by the record",
		PlacedBy: "k-old", Placed: at(5), Storage: "store", Now: at(20)}
	a, adopted, err := e.p.AdoptHold(e.ctx, found, e.audit())
	e.ok("AdoptHold", err)
	if !adopted || a.State != tenant.HoldActive || !a.FoundOnStorage || a.Reason != "kept by the record" || a.PlacedBy != "k-old" ||
		!slices.Equal(a.Storages, []string{"store"}) || len(a.StoragesPending) != 0 {
		e.Fatalf("an adopted hold is %+v (adopted %v)", a, adopted)
	}
	if _, adopted, err := e.p.AdoptHold(e.ctx, found, e.audit()); err != nil || adopted {
		e.Fatalf("a record adopted again answers adopted %v, %v", adopted, err)
	}
	holdRecordsPagedAndResealed(e, h, a)
	holdReleaseReserved(e, h, a)
}
