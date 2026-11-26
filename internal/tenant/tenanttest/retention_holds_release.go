package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// owedPairs answers the records page owes, each as "<hold>@<storage>".
func (e *tenv) owedPairs(page tenant.HoldOwedPage) []string {
	e.Helper()
	owed, err := e.p.HoldRecordsOwed(e.ctx, page)
	e.ok("HoldRecordsOwed", err)
	var out []string
	for _, o := range owed {
		out = append(out, o.Hold.ID+"@"+o.Storage)
	}
	return out
}

// holdRecordsPagedAndResealed guards, over testHoldRecords' holds (h on
// "store", marked under key version 1 on "copy" only; a adopted from a
// record found on "store"), the records sealed under another key version
// owed again for active holds alone, and the records owed paged by a
// cursor that leaves out the storages named: a storage that does not
// answer must not hold back the records every other storage is owed.
func holdRecordsPagedAndResealed(e *tenv, h, a tenant.Hold) {
	e.Helper()
	n, err := e.p.OweHoldRecordsNotSealedUnder(e.ctx, 1)
	e.ok("OweHoldRecordsNotSealedUnder", err)
	want := []string{h.ID + "@store", a.ID + "@store"}
	if got := e.owedPairs(tenant.HoldOwedPage{Limit: 10}); n != 2 || !slices.Equal(got, want) {
		e.Fatalf("records not sealed under version 1 owed %d: %v, want %v", n, got, want)
	}
	if got := e.owedPairs(tenant.HoldOwedPage{Limit: 1}); !slices.Equal(got, want[:1]) {
		e.Fatalf("the first page of one is %v, want %v", got, want[:1])
	}
	if got := e.owedPairs(tenant.HoldOwedPage{AfterHold: h.ID, AfterStorage: "store", Limit: 1}); !slices.Equal(got, want[1:]) {
		e.Fatalf("the page after %s@store is %v, want %v", h.ID, got, want[1:])
	}
	if got := e.owedPairs(tenant.HoldOwedPage{Skip: []string{"store"}, Limit: 10}); len(got) != 0 {
		e.Fatalf("with store left out the records owed are %v", got)
	}
	_, err = e.p.HoldRecordsOwed(e.ctx, tenant.HoldOwedPage{AfterStorage: "store", Limit: 1})
	e.wantKind("a cursor that names a storage and no hold", err, fault.Invalid)
	e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, h.ID, "store", 1, 1))
	e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, a.ID, "store", 1, 1))
	if n, err := e.p.OweHoldRecordsNotSealedUnder(e.ctx, 1); err != nil || n != 0 {
		e.Fatalf("with every record sealed under version 1, %d owed again (%v)", n, err)
	}
	if n, err := e.p.OweHoldRecordsNotSealedUnder(e.ctx, 2); err != nil || n != 3 {
		e.Fatalf("under version 2, %d records owed again (%v), want 3", n, err)
	}
	for _, p := range []struct{ id, storage string }{{h.ID, "store"}, {h.ID, "copy"}, {a.ID, "store"}} {
		e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, p.id, p.storage, 1, 2))
	}
	if got := e.owedPairs(tenant.HoldOwedPage{Limit: 10}); len(got) != 0 {
		e.Fatalf("records rewritten under version 2 still owed: %v", got)
	}
}

// holdReleaseReserved guards a release's count reserved before any record
// says released: begun, the hold still holds at its next count and owes no
// record to the background writer; a release at another count, or with no
// count, is refused; a release that did not commit, ended, leaves the hold
// active at the count after, every record owed, so its active records
// outrank a released one written at the count reserved; a release begun
// again commits at its count; and a release left under way past a time is
// ended by the sweep.
func holdReleaseReserved(e *tenv, h, a tenant.Hold) {
	e.Helper()
	b, err := e.p.BeginHoldRelease(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(30)})
	e.ok("BeginHoldRelease", err)
	if !b.Releasing || b.State != tenant.HoldActive || b.Seq != h.Seq+1 || !slices.Equal(b.StoragesPending, []string{"copy", "store"}) {
		e.Fatalf("a release begun leaves the hold %+v", b)
	}
	if got := e.owedPairs(tenant.HoldOwedPage{Limit: 10}); len(got) != 0 {
		e.Fatalf("a hold whose release is under way owes the background writer %v", got)
	}
	_, err = e.p.BeginHoldRelease(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "again", Now: at(31)})
	e.wantKind("a release begun twice", err, fault.Precondition)
	_, err = e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(31)}, e.audit(), nil)
	e.wantKind("a release with no count while one is under way", err, fault.Precondition)
	_, err = e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Seq: b.Seq + 1, Now: at(31)}, e.audit(), nil)
	e.wantKind("a release at another count", err, fault.Precondition)
	e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, h.ID, "store", b.Seq, 2))
	ended, err := e.p.AbortHoldRelease(e.ctx, h.ID, b.Seq)
	e.ok("AbortHoldRelease", err)
	if ended.Releasing || ended.State != tenant.HoldActive || ended.Seq != b.Seq+1 || !slices.Equal(ended.StoragesPending, []string{"copy", "store"}) {
		e.Fatalf("a release ended leaves the hold %+v, want active at %d with every record owed", ended, b.Seq+1)
	}
	if again, err := e.p.AbortHoldRelease(e.ctx, h.ID, b.Seq); err != nil || again.Seq != ended.Seq {
		e.Fatalf("a release ended twice answers %+v, %v", again, err)
	}
	b, err = e.p.BeginHoldRelease(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(32)})
	e.ok("BeginHoldRelease", err)
	e.ok("MarkHoldRecord", e.p.MarkHoldRecord(e.ctx, h.ID, "store", b.Seq, 2))
	out, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Seq: b.Seq, Now: at(32)}, e.audit(), nil)
	e.ok("ReleaseHold", err)
	if out.State != tenant.HoldReleased || out.Releasing || out.Seq != b.Seq || !slices.Equal(out.StoragesPending, []string{"copy"}) {
		e.Fatalf("a release committed at its count is %+v, want released at %d owing copy", out, b.Seq)
	}
	if ended, err := e.p.AbortHoldRelease(e.ctx, h.ID, b.Seq); err != nil || ended.State != tenant.HoldReleased {
		e.Fatalf("a committed release ended after is %+v, %v, want released still", ended, err)
	}
	if _, err := e.p.BeginHoldRelease(e.ctx, tenant.HoldRelease{ID: a.ID, Reason: "left", Now: at(40)}); err != nil {
		e.Fatalf("BeginHoldRelease: %v", err)
	}
	if ids, err := e.p.AbortStaleHoldReleases(e.ctx, at(40)); err != nil || len(ids) != 0 {
		e.Fatalf("a release begun at the bound is ended as stale: %v, %v", ids, err)
	}
	if ids, err := e.p.AbortStaleHoldReleases(e.ctx, at(41)); err != nil || !slices.Equal(ids, []string{a.ID}) {
		e.Fatalf("the stale releases ended are %v, %v, want %s", ids, err, a.ID)
	}
	if got, err := e.p.Hold(e.ctx, a.ID); err != nil || got.Releasing || got.State != tenant.HoldActive || got.Seq != a.Seq+2 {
		e.Fatalf("a hold whose release was ended as stale is %+v, %v, want active at %d", got, err, a.Seq+2)
	}
}
