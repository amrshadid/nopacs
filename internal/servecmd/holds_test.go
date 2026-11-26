package servecmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestTheServersHoldWriterAndStartAdoption guards the server's wiring of
// hold records: the hold writer the API places a hold through puts the
// record on the primary and on its replica, opened by their rows, before
// the row; and a tenant's start adopts a hold whose record it finds active
// on a storage with no row, as the hold it was, found on storage.
func TestTheServersHoldWriterAndStartAdoption(t *testing.T) {
	r := newWorkRig(t)
	r.w.tenants = oneTenantStore{st: r.db}
	p := r.pipeline()
	r.accept(p, "MR_small.dcm")
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	if err != nil || len(scans.Items) != 1 {
		t.Fatalf("the scans archived: %+v, %v", scans.Items, err)
	}
	subject := scans.Items[0].SubjectLabel
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

	// A record of a hold this database never had, as a database lost and
	// made again leaves one, put on main before the tenant's work starts.
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
	lostKey, _ := layout.HoldKey(lost)
	if err := os.WriteFile(filepath.Join(r.root, "lab", "main", filepath.FromSlash(lostKey)), b, 0o600); err != nil {
		t.Fatal(err)
	}
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
