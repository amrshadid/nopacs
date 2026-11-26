package holds

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// adopterFunc is an Adopter that hands each hold found to a function and
// adopts it.
type adopterFunc func(f tenant.HoldFound)

// AdoptHold implements Adopter.
func (a adopterFunc) AdoptHold(_ context.Context, f tenant.HoldFound, _ master.AuditEntry) (tenant.Hold, bool, error) {
	a(f)
	return tenant.Hold{ID: f.ID}, true, nil
}

// errSwitching is the refusal of refusingGate.
var errSwitching = errors.New("the storage is switching")

// testGate holds every write until open is closed, counting the writes
// that entered.
type testGate struct {
	open    chan struct{}
	entered atomic.Int64
}

// Enter waits for the gate to open.
func (g *testGate) Enter(ctx context.Context, _ string) (func(), error) {
	g.entered.Add(1)
	select {
	case <-g.open:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refusingGate refuses every write, as a switch's gate does once its wait
// runs out.
type refusingGate struct{}

// Enter refuses.
func (refusingGate) Enter(context.Context, string) (func(), error) { return nil, errSwitching }

// TestPlaceWritesTheRecordBeforeTheRow guards placing a hold: its record,
// sealed for the tenant, is on the primary its target touches and on the
// primary's replica, each written while the database has no row of it yet;
// the record names the target, the placing key, the reason's digest (never
// its text) and count 1; the row then owes no storage its record; and the
// same target and reason placed again answers the first hold, writing
// nothing more.
func TestPlaceWritesTheRecordBeforeTheRow(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	for name, s := range r.stores {
		s.beforeP = func(key string) {
			if hs := holdsOf(t, r.db); len(hs) != 0 {
				t.Errorf("the record %s was written on %s after the row %+v", key, name, hs)
			}
		}
	}
	h := r.place(w, r.subjects[0])
	for _, s := range r.stores {
		s.beforeP = nil
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		rec := r.record(storage, h.ID)
		if !rec.Active() || rec.Level != record.LevelSubject || rec.Subject != r.subjects[0] || rec.PlacedBy != "K1234567890a" ||
			rec.ReasonSHA256 != record.ReasonDigest("litigation 2026-17") || rec.Seq != 1 || !rec.Placed.Equal(h.Placed) {
			t.Fatalf("the record on %s is %+v, the hold %+v", storage, rec, h)
		}
		if bytes.Contains(r.rawRecord(storage, h.ID), []byte("litigation")) {
			t.Fatalf("the record on %s carries the reason's text", storage)
		}
	}
	if !slices.Equal(h.Storages, []string{replica, testkit.GenStorage}) || len(h.StoragesPending) != 0 {
		t.Fatalf("the hold's records stand %v, pending %v", h.Storages, h.StoragesPending)
	}
	again, made, err := w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: r.subjects[0]}, Reason: "litigation 2026-17", Audit: r.audit()})
	if err != nil || made || again.ID != h.ID {
		t.Fatalf("the same hold placed again answers %+v, %v, %v", again, made, err)
	}
	if n := len(holdsOf(t, r.db)); n != 1 {
		t.Fatalf("%d holds after placing the same one twice", n)
	}
}

// TestAPrimaryThatFailsRefusesTheHold guards that a hold is placed only
// once its record is on every primary: a primary that does not take the
// record refuses the hold with its error, no row is made, and no record is
// left anywhere that a later reading would adopt.
func TestAPrimaryThatFailsRefusesTheHold(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	r.stores[testkit.GenStorage].fail.Store(true)
	_, _, err := w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: r.subjects[0]}, Reason: "litigation", Audit: r.audit()})
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Place with its primary down = %v, want the storage's unavailable", err)
	}
	if hs := holdsOf(t, r.db); len(hs) != 0 {
		t.Fatalf("a hold was placed with its primary down: %+v", hs)
	}
	r.stores[testkit.GenStorage].fail.Store(false)
	a, err := w.AdoptFound(r.ctx)
	if err != nil || len(a.Adopted) != 0 || len(holdsOf(t, r.db)) != 0 {
		t.Fatalf("a refused hold's records were adopted: %+v, %v", a, err)
	}
}

// TestAHoldRefusedByTheStoreLeavesNoActiveRecord guards a record written
// for a hold whose row the store then refused for certain (here the
// caller's guard): it is rewritten released, so a start reading the
// storages does not adopt a hold no one placed.
func TestAHoldRefusedByTheStoreLeavesNoActiveRecord(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	denied := func(context.Context) error { return fault.New(fault.Denied, "the caller may not place a hold") }
	_, _, err := w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: r.subjects[0]}, Reason: "litigation", Audit: r.audit(), Guard: denied})
	if fault.KindOf(err) != fault.Denied {
		t.Fatalf("Place under a guard that refuses = %v", err)
	}
	a, err := w.AdoptFound(r.ctx)
	if err != nil || len(a.Adopted) != 0 || len(holdsOf(t, r.db)) != 0 {
		t.Fatalf("the records of a hold the store refused were adopted: %+v, %v", a, err)
	}
}

// TestTheWorkerWritesWhatAReplicaIsOwed guards the records owed: a
// replica that did not take a hold's record when it was placed leaves the
// hold placed, with the replica owed; the worker's pass writes the record
// there once the replica answers, and marks it, so nothing is owed after.
func TestTheWorkerWritesWhatAReplicaIsOwed(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	r.stores[replica].fail.Store(true)
	h := r.place(w, r.subjects[0])
	if !slices.Equal(h.StoragesPending, []string{replica}) || r.hasRecord(replica, h.ID) {
		t.Fatalf("a hold whose replica did not answer: pending %v", h.StoragesPending)
	}
	if n, err := w.WriteOwed(r.ctx); err == nil || n != 0 {
		t.Fatalf("a pass with the replica down wrote %d, %v", n, err)
	}
	r.stores[replica].fail.Store(false)
	if n, err := w.WriteOwed(r.ctx); err != nil || n != 1 {
		t.Fatalf("the pass wrote %d, %v, want 1", n, err)
	}
	if rec := r.record(replica, h.ID); !rec.Active() || rec.Seq != 1 || rec.Subject != r.subjects[0] {
		t.Fatalf("the record written on the replica is %+v", rec)
	}
	if owed, err := r.db.HoldRecordsOwed(r.ctx, tenant.HoldOwedPage{Limit: 10}); err != nil || len(owed) != 0 {
		t.Fatalf("records still owed: %+v, %v", owed, err)
	}
}

// TestReleaseRewritesTheRecordWithItsTime guards a release: the record on
// the primary and on the replica says released, at the hold's released
// time and the count the release reserved, and nothing is owed after; a
// primary that does not take the rewrite refuses the release and leaves
// the hold active at the count after the one reserved, the replica's
// record active at it.
func TestReleaseRewritesTheRecordWithItsTime(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	h := r.place(w, r.subjects[0])
	r.stores[testkit.GenStorage].fail.Store(true)
	if _, err := w.Release(r.ctx, Releasing{ID: h.ID, Reason: "settled", Audit: r.audit()}); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a release with its primary down = %v", err)
	}
	r.stores[testkit.GenStorage].fail.Store(false)
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || got.State != tenant.HoldActive || got.Releasing || got.Seq != 3 {
		t.Fatalf("after a refused release the hold is %+v, %v", got, err)
	}
	if rec := r.record(replica, h.ID); !rec.Active() || rec.Seq != 3 {
		t.Fatalf("after a refused release the replica's record is %+v", rec)
	}
	out, err := w.Release(r.ctx, Releasing{ID: h.ID, Reason: "settled", KeyID: "K1234567890a", Audit: r.audit()})
	if err != nil || out.State != tenant.HoldReleased || out.Seq != 4 {
		t.Fatalf("Release = %+v, %v", out, err)
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		rec := r.record(storage, h.ID)
		if rec.Active() || !rec.Released.Equal(out.Released) || rec.Seq != 4 || rec.Subject != r.subjects[0] {
			t.Fatalf("the record on %s after the release is %+v, the hold %+v", storage, rec, out)
		}
	}
	if owed, err := r.db.HoldRecordsOwed(r.ctx, tenant.HoldOwedPage{Limit: 10}); err != nil || len(owed) != 0 {
		t.Fatalf("records still owed after the release: %+v, %v", owed, err)
	}
	if again, err := w.Release(r.ctx, Releasing{ID: h.ID, Reason: "settled", Audit: r.audit()}); err != nil || again.Seq != 4 {
		t.Fatalf("a release asked again = %+v, %v", again, err)
	}
}

// TestARelabeledHoldsRecordIsRewritten guards a relabel under a hold: the
// relabel's publish moves the hold to the new label and owes its records;
// the worker rewrites them with the new label at the hold's next count, so
// a database lost after the relabel finds the hold under the label the
// catalog has.
func TestARelabeledHoldsRecordIsRewritten(t *testing.T) {
	r := newRig(t)
	w := r.writer()
	h := r.place(w, r.subjects[0])
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := r.db.PrepareCorrection(r.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel,
		Target: tenant.CorrectionTarget{Subject: r.subjects[0]}, ToLabel: "sub-moved", Reason: "wrong subject id", Now: time.Now()}, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.CommitCorrection(r.ctx, tenant.CorrectionCommit{ID: c.ID, Now: time.Now(), Grace: tenant.DefaultGrace}); err != nil {
		t.Fatal(err)
	}
	if n, err := w.WriteOwed(r.ctx); err != nil || n != 2 {
		t.Fatalf("the pass after the relabel wrote %d, %v, want the primary's and the replica's", n, err)
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		if rec := r.record(storage, h.ID); rec.Subject != "sub-moved" || rec.Seq != 2 || !rec.Active() {
			t.Fatalf("the record on %s after the relabel is %+v", storage, rec)
		}
	}
}

// TestALostHoldIsAdoptedFromItsRecord guards that a hold outlives its
// database: a tenant's database made again with the same catalog and no
// holds, its storages read, has the hold back, active, under its id,
// target, placing key and count, found on storage; a delete of what it
// covers is then refused as held.
func TestALostHoldIsAdoptedFromItsRecord(t *testing.T) {
	r := newRig(t)
	h := r.place(r.writer(), r.subjects[0])
	fresh := openTenant(t, 1)
	w, err := New(r.config(fresh))
	if err != nil {
		t.Fatal(err)
	}
	a, err := w.AdoptFound(r.ctx)
	if err != nil || !slices.Equal(a.Adopted, []string{h.ID}) || len(a.Refused) != 0 {
		t.Fatalf("AdoptFound = %+v, %v", a, err)
	}
	got, err := fresh.Hold(r.ctx, h.ID)
	if err != nil || got.State != tenant.HoldActive || !got.FoundOnStorage || got.Target != h.Target || got.PlacedBy != h.PlacedBy ||
		got.Seq != 1 || !got.Placed.Equal(h.Placed) {
		t.Fatalf("the hold adopted is %+v (%v), placed %+v", got, err, h)
	}
	p, err := fresh.PreviewDelete(r.ctx, tenant.CorrectionTarget{Subject: r.subjects[0]}, time.Now())
	if err != nil || !slices.Equal(p.Holds, []string{h.ID}) {
		t.Fatalf("a delete's preview after the adoption names the holds %v (%v)", p.Holds, err)
	}
	// Its records are written again from the row, keeping the reason's
	// digest the record carried.
	if _, err := w.WriteOwed(r.ctx); err != nil {
		t.Fatal(err)
	}
	if rec := r.record(testkit.GenStorage, h.ID); rec.ReasonSHA256 != record.ReasonDigest("litigation 2026-17") {
		t.Fatalf("the adopted hold's record names another reason digest: %+v", rec)
	}
	if again, err := w.AdoptFound(r.ctx); err != nil || len(again.Adopted) != 0 {
		t.Fatalf("a second reading adopted %+v, %v", again, err)
	}
}

// TestATamperedRecordIsRefusedNotAdopted guards adoption's fail-closed
// side against a record changed on the storage: its MAC no longer
// verifies, so it makes no hold, and it is named among the refused rather
// than passed over; a released record that verifies makes no hold either.
func TestATamperedRecordIsRefusedNotAdopted(t *testing.T) {
	r := newRig(t)
	h := r.place(r.writer(), r.subjects[0])
	for _, storage := range []string{testkit.GenStorage, replica} {
		b := bytes.Replace(r.rawRecord(storage, h.ID), []byte(r.subjects[0]), []byte(r.subjects[1]), 1)
		r.putRaw(storage, h.ID, b)
	}
	fresh := openTenant(t, 1)
	w, err := New(r.config(fresh))
	if err != nil {
		t.Fatal(err)
	}
	a, err := w.AdoptFound(r.ctx)
	if err != nil || len(a.Adopted) != 0 || len(a.Refused) != 2 {
		t.Fatalf("AdoptFound over tampered records = %+v, %v", a, err)
	}
	if hs := holdsOf(t, fresh); len(hs) != 0 {
		t.Fatalf("a tampered record made the hold %+v", hs)
	}
}

// TestTheLatestRecordDecides guards which record of a hold counts when its
// storages disagree: a released record of a later count than an active one
// left on a storage that missed the release makes no hold; an active
// record of the same count as a released one does, the lesser error.
func TestTheLatestRecordDecides(t *testing.T) {
	active := record.Hold{Format: record.Format, Kind: record.HoldKind, ID: "01jd5x7k2m3n4p5q6r7s8t9v0w", Tenant: rigTenantID,
		Level: record.LevelSubject, Subject: "sub-1", State: record.HoldActive, ReasonSHA256: record.ReasonDigest("x"),
		Placed: time.Now().UTC(), Seq: 1}
	released := active
	released.State, released.Released, released.Seq = record.HoldReleased, active.Placed.Add(time.Hour), 2
	var got []tenant.HoldFound
	db := adopterFunc(func(f tenant.HoldFound) { got = append(got, f) })
	audit := func(string, string) master.AuditEntry { return master.AuditEntry{} }
	if _, err := Adopt(context.Background(), db, []Found{{Storage: "a", Records: []record.Hold{active}}, {Storage: "b",
		Records: []record.Hold{released}}}, time.Now(), audit); err != nil || len(got) != 0 {
		t.Fatalf("a released record of a later count adopted %+v, %v", got, err)
	}
	released.Seq = 1
	if _, err := Adopt(context.Background(), db, []Found{{Storage: "b", Records: []record.Hold{released}}, {Storage: "a",
		Records: []record.Hold{active}}}, time.Now(), audit); err != nil || len(got) != 1 || got[0].Storage != "a" {
		t.Fatalf("an active record of the same count as a released one: %+v, %v", got, err)
	}
}

// TestWritesWaitAtTheSwitchGate guards that a hold's record is written
// only once the switch gate admits it: while the gate holds the primary,
// neither a record nor a row is made; once it opens, the hold is placed;
// and a gate that refuses refuses the hold.
func TestWritesWaitAtTheSwitchGate(t *testing.T) {
	r := newRig(t)
	g := &testGate{open: make(chan struct{})}
	cfg := r.config(r.db)
	cfg.Pause = g
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var h tenant.Hold
	go func() {
		var err error
		h, _, err = w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: r.subjects[0]}, Reason: "litigation", Audit: r.audit()})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Place returned while the gate held the primary: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if hs := holdsOf(t, r.db); len(hs) != 0 || g.entered.Load() == 0 {
		t.Fatalf("while the gate held: holds %+v, entries %d", hs, g.entered.Load())
	}
	close(g.open)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !r.hasRecord(testkit.GenStorage, h.ID) {
		t.Fatal("no record once the gate opened")
	}
	cfg.Pause = refusingGate{}
	w, _ = New(cfg)
	_, _, err = w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: r.subjects[1]}, Reason: "litigation", Audit: r.audit()})
	if !errors.Is(err, errSwitching) {
		t.Fatalf("Place through a gate that refuses = %v", err)
	}
}
