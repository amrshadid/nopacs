package backup

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestReconcileChecksWhoseMasterBeforeMigrating guards a reconcile given
// the address of another installation's master by mistake, one this
// binary's migration would refuse or write (made from another baseline
// schema here): it is refused as another installation's, by its key check,
// before anything is migrated, and the file is left byte for byte as it
// was.
func TestReconcileChecksWhoseMasterBeforeMigrating(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	theirs := newRig(t, false)
	_ = theirs.st.Close()
	_ = theirs.m.Close()
	path := filepath.Join(theirs.dataDir, "master.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET name = '0001_another_baseline.sql' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	_ = r.st.Close()
	_ = r.m.Close()
	cfg := r.reconcileConfig()
	cfg.MasterAddress = driver.SQLiteScheme + path
	_, err = ReconcileOnly(r.ctx, cfg)
	if err == nil || strings.Contains(err.Error(), "schema") {
		t.Fatalf("a reconcile of another installation's master answered %v, want its identity refused before its schema is read", err)
	}
	after, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	if a, b := sha256.Sum256(before), sha256.Sum256(after); !bytes.Equal(a[:], b[:]) {
		t.Fatal("the refused reconcile wrote the other installation's master")
	}
}

// TestImportRefusesAnInterruptedDataDirectory guards an import into an
// installation whose data directory a reconcile, or a restore, was
// interrupted in (its marker there, the import holding the lock): it is
// refused with that marker's advice before anything is loaded, and no
// tenant is registered.
func TestImportRefusesAnInterruptedDataDirectory(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	tg := newTarget(t, r)
	path, _, key := r.export()
	if err := os.MkdirAll(server.RunDir(tg.dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ marker, advice string }{
		{ReconcileMarkerPath(tg.dataDir), "--reconcile-only again"},
		{MarkerPath(tg.dataDir), "a restore was interrupted"},
	} {
		if err := os.WriteFile(m.marker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := tg.importInto(r, path, key, false); err == nil || !strings.Contains(fault.Message(err), m.advice) {
			t.Fatalf("an import beside %s answered %v", filepath.Base(m.marker), err)
		}
		if _, err := tg.m.TenantByName(r.ctx, "lab"); fault.KindOf(err) != fault.NotFound {
			t.Fatalf("an import refused registered the tenant (%v)", err)
		}
		if err := os.Remove(m.marker); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReconcileKeepsQuarantinedFilesWhoseBytesAreThere guards the
// quarantine of a reconcile in place, whose incoming area is the one the
// restored databases' rows name: a held file whose bytes are there whole
// stays held with its bytes (it can still be released); one whose bytes
// are gone, and one whose bytes changed, are marked incoming lost; the
// report counts two. On each database type.
func TestReconcileKeepsQuarantinedFilesWhoseBytesAreThere(t *testing.T) {
	onBoth(t, testReconcileKeepsQuarantinedFiles)
}

// testReconcileKeepsQuarantinedFiles is
// TestReconcileKeepsQuarantinedFilesWhoseBytesAreThere on one database.
func testReconcileKeepsQuarantinedFiles(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:1]...)
	r.arrive([]byte("the first file that is not DICOM at all, only some text"))
	r.arrive([]byte("the second file that is not DICOM at all, only some text"))
	r.arrive([]byte("the third file that is not DICOM at all, only some text"))
	held, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{}, pageAll)
	if err != nil || len(held) != 3 {
		t.Fatalf("the quarantine is %+v (%v), want three files kept", held, err)
	}
	area := filepath.Join(r.dataDir, "incoming", "lab")
	for _, q := range held {
		if !q.ContentKept || q.Incoming == "" {
			t.Fatalf("a quarantined file is not kept: %+v", q)
		}
	}
	gone, changed, whole := held[0], held[1], held[2]
	if err := os.Remove(filepath.Join(area, filepath.FromSlash(gone.Incoming))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(area, filepath.FromSlash(changed.Incoming)), []byte("the second file that is not DICOM at all, only some TEXT"),
		0o600); err != nil {
		t.Fatal(err)
	}
	_ = r.st.Close()
	_ = r.m.Close()
	rep, err := ReconcileOnly(r.ctx, r.reconcileConfig())
	if err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	if n := rep.Tenants[0].QuarantineMarked; n != 2 {
		t.Fatalf("the reconcile marked %d files incoming lost, want 2", n)
	}
	r.openMaster()
	r.openTenant()
	got, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{}, pageAll)
	if err != nil || len(got) != 3 {
		t.Fatalf("the reconciled quarantine is %+v (%v)", got, err)
	}
	for _, q := range got {
		lost := q.Reason == tenant.QuarantineIncomingLost && !q.ContentKept && q.Incoming == ""
		switch q.ID {
		case whole.ID:
			if lost || !q.ContentKept || q.Incoming != whole.Incoming || q.Reason != whole.Reason {
				t.Errorf("the file whose bytes are there whole became %+v", q)
			}
		default:
			if !lost {
				t.Errorf("the file %s whose bytes are gone or changed is %+v, want incoming lost", q.ID, q)
			}
		}
	}
}
