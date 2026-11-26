package holds

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// unanswered is the rig's database whose ReleaseHold, and AbortHoldRelease
// when abortToo is set, answer unavailable without changing anything: a
// database that did not answer. before runs first in ReleaseHold.
type unanswered struct {
	db
	abortToo bool
	before   func()
}

// ReleaseHold answers unavailable.
func (u unanswered) ReleaseHold(context.Context, tenant.HoldRelease, master.AuditEntry, tenant.Guard) (tenant.Hold, error) {
	if u.before != nil {
		u.before()
	}
	return tenant.Hold{}, fault.New(fault.Unavailable, "the database does not answer")
}

// AbortHoldRelease answers unavailable when abortToo is set.
func (u unanswered) AbortHoldRelease(ctx context.Context, id string, seq int64) (tenant.Hold, error) {
	if u.abortToo {
		return tenant.Hold{}, fault.New(fault.Unavailable, "the database does not answer")
	}
	return u.db.AbortHoldRelease(ctx, id, seq)
}

// releaseUnanswered places a hold of the rig's first subject, then
// releases it through a database whose ReleaseHold does not answer, and
// returns the hold and the bytes of the released record its primary held
// when the release was sent, as someone with access to the storage could
// keep them.
func releaseUnanswered(t *testing.T, r *rig) (tenant.Hold, []byte) {
	t.Helper()
	h := r.place(r.writer(), r.subjects[0])
	var kept []byte
	w, err := New(r.config(unanswered{db: r.db, before: func() { kept = r.rawRecord(testkit.GenStorage, h.ID) }}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Release(r.ctx, Releasing{ID: h.ID, Reason: "settled", Audit: r.audit()}); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a release the database does not answer = %v", err)
	}
	return h, kept
}

// adoptAfterLoss reads the rig's storages into a tenant database made again
// with the same catalog and no holds, and returns it and what was adopted.
func adoptAfterLoss(t *testing.T, r *rig) (db, Adoption) {
	t.Helper()
	fresh := openTenant(t, 1)
	w, err := New(r.config(fresh))
	if err != nil {
		t.Fatal(err)
	}
	a, err := w.AdoptFound(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return fresh, a
}

// TestAReleaseThatDoesNotCommitKeepsTheHold guards a release whose commit
// the database does not answer: the released record was written at the
// count the release reserved, and the release is ended, so the hold is
// active at the count after with its active record on every storage; a
// database lost after then finds the hold active, never released.
func TestAReleaseThatDoesNotCommitKeepsTheHold(t *testing.T) {
	r := newRig(t)
	h, kept := releaseUnanswered(t, r)
	if rec, err := record.DecodeHold(bytes.NewReader(kept), rigTenantID, r.keys); err != nil || rec.Active() || rec.Seq != 2 {
		t.Fatalf("the record written before the commit is %+v, %v, want released at 2", rec, err)
	}
	got, err := r.db.Hold(r.ctx, h.ID)
	if err != nil || got.State != tenant.HoldActive || got.Releasing || got.Seq != 3 || len(got.StoragesPending) != 0 {
		t.Fatalf("after a release that did not commit the hold is %+v, %v, want active at 3 with nothing owed", got, err)
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		if rec := r.record(storage, h.ID); !rec.Active() || rec.Seq != 3 {
			t.Fatalf("the record on %s is %+v, want active at 3", storage, rec)
		}
	}
	fresh, a := adoptAfterLoss(t, r)
	if !slices.Equal(a.Adopted, []string{h.ID}) {
		t.Fatalf("after a database loss %v was adopted, want %s", a.Adopted, h.ID)
	}
	if got, err := fresh.Hold(r.ctx, h.ID); err != nil || got.State != tenant.HoldActive || got.Seq != 3 {
		t.Fatalf("the hold adopted is %+v, %v", got, err)
	}
}

// TestAKeptReleasedRecordDoesNotSuppressTheHold guards a released record
// written for a release that did not commit, kept, and put back on a
// storage after the hold's rows went on to the count after it: the active
// record of that count on another storage outranks it, so a database lost
// after still finds the hold active.
func TestAKeptReleasedRecordDoesNotSuppressTheHold(t *testing.T) {
	r := newRig(t)
	h, kept := releaseUnanswered(t, r)
	r.putRaw(testkit.GenStorage, h.ID, kept)
	if rec := r.record(testkit.GenStorage, h.ID); rec.Active() || rec.Seq != 2 {
		t.Fatalf("the record put back is %+v", rec)
	}
	_, a := adoptAfterLoss(t, r)
	if !slices.Equal(a.Adopted, []string{h.ID}) {
		t.Fatalf("with a kept released record put back, %v was adopted, want %s", a.Adopted, h.ID)
	}
}

// TestAReleaseLeftUnderWayIsEndedOnceStale guards a release whose writer
// could not end it, the database answering neither the commit nor the end:
// the storages written released are given the active record of the count
// after at once, so a database lost then finds the hold; the hold, still
// holding, owes the background writer nothing while the release is under
// way; and once it is stale the worker ends it and writes the hold's active
// records, so nothing is owed after.
func TestAReleaseLeftUnderWayIsEndedOnceStale(t *testing.T) {
	r := newRig(t)
	h := r.place(r.writer(), r.subjects[0])
	w, err := New(r.config(unanswered{db: r.db, abortToo: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Release(r.ctx, Releasing{ID: h.ID, Reason: "settled", Audit: r.audit()}); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a release the database does not answer = %v", err)
	}
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || !got.Releasing || got.State != tenant.HoldActive || got.Seq != 2 {
		t.Fatalf("a release left under way leaves the hold %+v, %v", got, err)
	}
	if rec := r.record(testkit.GenStorage, h.ID); !rec.Active() || rec.Seq != 3 {
		t.Fatalf("the primary's record after a release left under way is %+v, want active at 3", rec)
	}
	if n, err := r.writer().WriteOwed(r.ctx); err != nil || n != 0 {
		t.Fatalf("a pass while the release is under way wrote %d, %v", n, err)
	}
	cfg := r.config(r.db)
	cfg.Now = func() time.Time { return time.Now().Add(StaleRelease + time.Minute) }
	later, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := later.WriteOwed(r.ctx); err != nil || n != 2 {
		t.Fatalf("the pass once the release is stale wrote %d, %v, want 2", n, err)
	}
	got, err := r.db.Hold(r.ctx, h.ID)
	if err != nil || got.Releasing || got.State != tenant.HoldActive || got.Seq != 3 || len(got.StoragesPending) != 0 {
		t.Fatalf("after the stale release was ended the hold is %+v, %v", got, err)
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		if rec := r.record(storage, h.ID); !rec.Active() || rec.Seq != 3 {
			t.Fatalf("the record on %s is %+v, want active at 3", storage, rec)
		}
	}
}
