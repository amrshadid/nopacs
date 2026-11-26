package servecmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// archivedSubject archives MR_small.dcm through the rig's pipeline and
// returns the label of its subject.
func archivedSubject(t *testing.T, r *workRig) string {
	t.Helper()
	p := r.pipeline()
	r.accept(p, "MR_small.dcm")
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	if err != nil || len(scans.Items) != 1 {
		t.Fatalf("the scans archived: %+v, %v", scans.Items, err)
	}
	return scans.Items[0].SubjectLabel
}

// putLostHoldRecord puts on the storage named the active record of a hold
// of subject this database never had, as a database lost and made again
// leaves one, and returns the hold's id.
func putLostHoldRecord(t *testing.T, r *workRig, storage, subject string) string {
	t.Helper()
	lost, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := record.EncodeHold(record.Hold{Format: record.Format, Kind: record.HoldKind, ID: lost, Level: record.LevelSubject, Subject: subject,
		State: record.HoldActive, ReasonSHA256: record.ReasonDigest("audit"), Placed: time.Now().UTC().Truncate(time.Second), Seq: 3},
		"T0000000000001", r.keys)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := layout.HoldKey(lost)
	path := filepath.Join(r.root, "lab", storage, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return lost
}

// holdRecordOn reads and checks the record of the hold id on the storage
// named, straight from the disk.
func holdRecordOn(t *testing.T, r *workRig, storage, id string) record.Hold {
	t.Helper()
	key, _ := layout.HoldKey(id)
	b, err := os.ReadFile(filepath.Join(r.root, "lab", storage, filepath.FromSlash(key))) // #nosec G304 -- the test's own storage.
	if err != nil {
		t.Fatalf("the record of hold %s on %s: %v", id, storage, err)
	}
	rec, err := record.DecodeHold(bytes.NewReader(b), "T0000000000001", r.keys)
	if err != nil {
		t.Fatalf("the record of hold %s on %s does not verify: %v", id, storage, err)
	}
	return rec
}

// TestTheServersHoldWriterAndStartAdoption guards the server's wiring of
// hold records: the hold writer the API places a hold through puts the
// record on the primary and on its replica, opened by their rows, before
// the row; and a tenant's start adopts a hold whose record it finds active
// on a storage with no row, as the hold it was, found on storage.
func TestTheServersHoldWriterAndStartAdoption(t *testing.T) {
	r := newWorkRig(t)
	r.w.tenants = oneTenantStore{st: r.db}
	subject := archivedSubject(t, r)
	w, err := r.w.HoldWriter(r.ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	h, made, err := w.Place(r.ctx, holds.Placing{Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation", Audit: r.audit()})
	if err != nil || !made || len(h.StoragesPending) != 0 {
		t.Fatalf("Place through the server's writer = %+v, %v, %v", h, made, err)
	}
	key, _ := layout.HoldKey(h.ID)
	for _, storage := range []string{"main", "copy"} {
		if f := r.files(storage, key); len(f) != 1 {
			t.Fatalf("the hold's record is not on %s: %v", storage, f)
		}
	}

	lost := putLostHoldRecord(t, r, "main", subject)
	r.start()
	var got tenant.Hold
	r.waitFor("the start to adopt the hold found", time.Minute, func() bool {
		got, err = r.db.Hold(r.ctx, lost)
		return err == nil
	})
	if got.State != tenant.HoldActive || !got.FoundOnStorage || got.Target.Subject != subject || got.Seq != 3 {
		t.Fatalf("the hold adopted at the start is %+v", got)
	}
}

// TestAFencedTenantsHoldWorkerWritesNothing guards the fence of the
// hold-record worker: while the registry fences the tenant, a pass of the
// worker as the server starts it adopts no hold found on storage, writes no
// record owed and marks none; once the fence lets writes through, the same
// worker adopts the hold and writes the record.
func TestAFencedTenantsHoldWorkerWritesNothing(t *testing.T) {
	r := newWorkRig(t)
	subject := archivedSubject(t, r)
	// A hold whose record is on main only: the replica is owed it.
	h, _, err := r.db.PlaceHold(r.ctx, tenant.HoldPlace{ID: mustCorrectionID(t), Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil)
	if err != nil || !slices.Equal(h.StoragesPending, []string{"copy"}) {
		t.Fatalf("PlaceHold = %+v, %v", h, err)
	}
	lost := putLostHoldRecord(t, r, "main", subject)
	f := &refusingFence{}
	f.refuse.Store(true)
	run := r.w.holdRecords("T0000000000001", "lab", fenceDB(r.db, f.check), fencedOpen(r.w.openFor(place0(r), r.db), f.check), nil, r.w.log)
	pass := func() {
		ctx, cancel := context.WithTimeout(r.ctx, time.Second)
		defer cancel()
		run(ctx)
	}
	pass()
	if _, err := r.db.Hold(r.ctx, lost); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a fenced tenant's worker adopted a hold: %v", err)
	}
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || !slices.Equal(got.StoragesPending, []string{"copy"}) {
		t.Fatalf("a fenced tenant's worker marked a record written: %+v, %v", got, err)
	}
	f.refuse.Store(false)
	pass()
	if got, err := r.db.Hold(r.ctx, lost); err != nil || got.State != tenant.HoldActive {
		t.Fatalf("once the fence let writes through the hold found is %+v, %v", got, err)
	}
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || len(got.StoragesPending) != 0 {
		t.Fatalf("once the fence let writes through the hold owes %+v, %v", got, err)
	}
}

// TestARelabelsHoldRecordsAreWrittenAtOnce guards WriteHoldsOwed, which a
// relabel's publish calls: the records of a hold the relabel moved name the
// new label once it returns, not at the hold-record worker's next pass,
// and are marked written.
func TestARelabelsHoldRecordsAreWrittenAtOnce(t *testing.T) {
	r := newWorkRig(t)
	r.w.tenants = oneTenantStore{st: r.db}
	subject := archivedSubject(t, r)
	w, err := r.w.HoldWriter(r.ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Place(r.ctx, holds.Placing{Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	es := retentionEngines(t, r)
	e, release, err := es.Use(r.ctx, "lab", r.db)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.Relabel(r.ctx, correct.RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "sub-renamed",
		Reason: "wrong label", Audit: r.audit()})
	if err == nil {
		_, err = e.ContinueRelabel(r.ctx, c.ID)
	}
	release()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || got.Target.Subject != "sub-renamed" || len(got.StoragesPending) != 2 {
		t.Fatalf("after the relabel the hold is %+v (%v), want it moved with its records owed", got, err)
	}
	r.w.WriteHoldsOwed(r.ctx, "lab")
	for _, storage := range []string{"main", "copy"} {
		if rec := holdRecordOn(t, r, storage, h.ID); rec.Subject != "sub-renamed" || rec.Seq != 2 {
			t.Fatalf("the record on %s after the relabel names %q at count %d, want sub-renamed at 2", storage, rec.Subject, rec.Seq)
		}
	}
	if got, err := r.db.Hold(r.ctx, h.ID); err != nil || len(got.StoragesPending) != 0 {
		t.Fatalf("the records written at once are still owed: %+v (%v)", got, err)
	}
}
