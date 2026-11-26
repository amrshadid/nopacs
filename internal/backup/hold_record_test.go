package backup

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// keptOpen is the rig's storage as a hold writer opens it: the writer
// closes what it opens, and the rig closes the storage at the test's end.
type keptOpen struct{ blob.Store }

// Close keeps the storage open.
func (keptOpen) Close() error { return nil }

// TestAStorageNotReadCostsNoHoldOfTheOthers guards the reconcile's
// adoption of hold records when one storage cannot be read: the holds
// found active with no row on every storage that was read are adopted, and
// the reconcile still fails, naming the storage not read.
func TestAStorageNotReadCostsNoHoldOfTheOthers(t *testing.T) {
	r := newRig(t, false)
	ctx := r.ctx
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := record.EncodeHold(record.Hold{Format: record.Format, Kind: record.HoldKind, ID: id, Level: record.LevelSubject, Subject: "sub-01",
		State: record.HoldActive, ReasonSHA256: record.ReasonDigest("litigation"), Placed: time.Now().UTC().Truncate(time.Second), Seq: 1},
		tenantID, r.keys)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := layout.HoldKey(id)
	if _, err := r.storage.Put(ctx, key, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	main, err := r.p.Storage(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	// Listed first, under a root the installation does not have: it cannot
	// be opened.
	gone := tenant.Storage{Name: "gone", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "unknown-root"}}
	row, err := r.m.TenantByName(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	opener := place.Opener{Values: settingvalue.Reader{Master: r.m}, Keys: r.keys, StaleUploadAge: -1}
	var res TenantResult
	err = adoptHolds(ctx, RestoreConfig{Keys: r.keys}, openedTenant{row: row, store: r.st}, opener, []tenant.Storage{gone, main}, time.Now(), &res)
	if err == nil || !strings.Contains(fault.Message(err), "gone") {
		t.Fatalf("the adoption with a storage not read answers %v, want an error naming it", err)
	}
	if !slices.Equal(res.HoldsAdopted, []string{id}) {
		t.Fatalf("the holds adopted from the storage read: %v, want %s", res.HoldsAdopted, id)
	}
	if h, err := r.p.(tenant.Holds).Hold(ctx, id); err != nil || h.State != tenant.HoldActive || !h.FoundOnStorage {
		t.Fatalf("the hold adopted is %+v (%v)", h, err)
	}
}

// TestAHoldPlacedAfterTheBackupSurvivesItsRestore guards that a hold's
// record outlives the database the backup brings back, on each database
// type: a subject deleted in its grace period, backed up, then held through
// the hold writer (its record on the storage); the restore's database has
// no row of the hold, and the restore makes it active again from its
// record and says so, so the delete's purge is refused as held; a hold
// record that does not verify makes no hold and is named in the report.
func TestAHoldPlacedAfterTheBackupSurvivesItsRestore(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		r.archive(studyFiles...)
		subject := r.subjects(r.p)[0]
		c := r.deleteSubject(r.engine(), subject)
		path, _ := r.backup()
		st, ok := r.p.(holds.Store)
		if !ok {
			t.Fatal("the tenant's database keeps no holds")
		}
		w, err := holds.New(holds.Config{Tenant: "lab", TenantID: tenantID, Keys: r.keys, Store: st,
			Open: func(context.Context, string) (blob.Store, error) { return keptOpen{r.storage}, nil }})
		if err != nil {
			t.Fatal(err)
		}
		h, made, err := w.Place(ctx, holds.Placing{Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation", Audit: r.audit()})
		if err != nil || !made {
			t.Fatalf("Place = %+v, %v, %v", h, made, err)
		}
		forged, _ := layout.HoldKey("01jd5x7k2m3n4p5q6r7s8t9v0w")
		if _, err := r.storage.Put(ctx, forged, bytes.NewReader([]byte(`{"format":1,"kind":"hold"}`))); err != nil {
			t.Fatal(err)
		}
		_ = r.st.Close()
		_ = r.m.Close()
		target := filepath.Join(filepath.Dir(r.dataDir), "restored")
		cfg := RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
			MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
		if pg {
			r.emptyPostgres()
			cfg.MasterAddress = r.masterAddr
			cfg.Scratch, _ = testkit.PostgresScratch(t)
		}
		rep, err := Restore(ctx, cfg)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if len(rep.Tenants) != 1 {
			t.Fatalf("the restore's report is %+v", rep)
		}
		res := rep.Tenants[0]
		if !slices.Equal(res.HoldsAdopted, []string{h.ID}) || res.HoldsRefused != 1 || len(res.HoldsRefusedRecords) != 1 ||
			!strings.Contains(res.HoldsRefusedRecords[0], forged) {
			t.Fatalf("the restore's holds: adopted %v, refused %d %v", res.HoldsAdopted, res.HoldsRefused, res.HoldsRefusedRecords)
		}
		var printed strings.Builder
		if err := rep.Print(&printed); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(printed.String(), "1 holds found active on the storages") || !strings.Contains(printed.String(), h.ID) ||
			!strings.Contains(printed.String(), "1 hold records do not verify") {
			t.Fatalf("the report does not say what became of the hold records:\n%s", printed.String())
		}
		r.dataDir, r.masterAddr = target, cfg.MasterAddress
		r.openMaster()
		r.openTenant()
		got, err := r.p.(tenant.Holds).Hold(ctx, h.ID)
		if err != nil || got.State != tenant.HoldActive || !got.FoundOnStorage {
			t.Fatalf("the restored hold is %+v (%v)", got, err)
		}
		_, err = r.p.PurgeTargets(ctx, c.ID, 10)
		var held *tenant.HeldRefusal
		if !errors.As(err, &held) || held.Hold != h.ID {
			t.Fatalf("the delete's purge after the restore = %v, want HELD by %s", err, h.ID)
		}
	})
}
