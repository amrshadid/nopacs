package backup

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// earlierBuild makes the SQLite database at path one an earlier
// development build made: its recorded baseline has another SHA-256.
func earlierBuild(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE schema_migrations SET checksum = zeroblob(32) WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
}

// TestCreateRefusesAnEarlierBuildsDatabase guards that a backup stamps
// this binary's baseline only on databases made from it: a master or a
// tenant database an earlier development build made (its recorded baseline
// another), which a server refuses to open but a backup made with the
// server stopped reads without migrating, is refused before a byte is
// written. Without the check the backup took the binary's fingerprint, and
// a restore accepted it and wrote its rows under this binary's schema.
func TestCreateRefusesAnEarlierBuildsDatabase(t *testing.T) {
	for _, which := range []string{"master", "tenant"} {
		t.Run(which, func(t *testing.T) {
			r := newRig(t, false)
			path := filepath.Join(r.dataDir, "master.db")
			if which == "tenant" {
				var err error
				if path, err = (driver.SQLiteTenants{DataDir: r.dataDir}).Path("lab"); err != nil {
					t.Fatal(err)
				}
			}
			earlierBuild(t, path)
			dbs, closeAll, err := OpenTenants(r.ctx, r.m, r.keys, r.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll()
			var out bytes.Buffer
			_, err = Create(r.ctx, CreateConfig{Installation: r.id, Keys: r.keys, Master: r.m.(snapshot.Database), Tenants: dbs, Out: &out})
			if !strings.Contains(fault.Message(err), "made by an earlier development build") || !strings.Contains(fault.Message(err), "make it again") {
				t.Fatalf("a backup of an earlier build's %s database: %v, want a refusal saying it was made by an earlier development build", which, err)
			}
			t.Logf("refused: %s", fault.Message(err))
			if out.Len() != 0 {
				t.Errorf("the refused backup wrote %d bytes", out.Len())
			}
		})
	}
}

// TestExportRefusesAnEarlierBuildsDatabase guards the same for a tenant's
// export: a tenant database an earlier development build made is refused
// before its storages' owner records move or a byte is written.
func TestExportRefusesAnEarlierBuildsDatabase(t *testing.T) {
	r := newRig(t, false)
	path, err := (driver.SQLiteTenants{DataDir: r.dataDir}).Path("lab")
	if err != nil {
		t.Fatal(err)
	}
	earlierBuild(t, path)
	generation := func() int64 {
		if _, err := r.storage.Stat(r.ctx, layout.OwnerKey); err != nil {
			return 0
		}
		return r.ownerRecord().Generation
	}
	before := generation()
	var out bytes.Buffer
	_, _, err = ExportTenant(r.ctx, ExportConfig{Installation: r.id, Keys: r.keys, Tenant: "lab", TenantID: tenantID, Driver: master.SQLiteDriver,
		DB: r.st.(snapshot.Database), Store: r.st, Owners: r.ownerCheck(), Out: &out})
	if !strings.Contains(fault.Message(err), "made by an earlier development build") {
		t.Fatalf("an export of an earlier build's tenant database: %v, want a refusal saying it was made by an earlier development build", err)
	}
	t.Logf("refused: %s", fault.Message(err))
	if out.Len() != 0 {
		t.Errorf("the refused export wrote %d bytes", out.Len())
	}
	if after := generation(); after != before {
		t.Errorf("the refused export moved the storage's owner record from generation %d to %d", before, after)
	}
}

// TestImportRefusesAnotherBaseline guards the baseline's fingerprint in a
// tenant's export: an export whose section records another baseline, or
// none (a file of an earlier development build), is refused with a plain
// message before anything is written: the tenant is not registered and the
// target holds no tenant file.
func TestImportRefusesAnotherBaseline(t *testing.T) {
	r := newRig(t, false)
	path, _, key := r.export()
	tk, err := secret.TransferKeys(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	plain := plaintext(t, raw, tk)
	for _, c := range []struct{ name, schema string }{
		{"another baseline", strings.Repeat("a", 64)},
		{"a file that records none", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			edited := editEnds(t, plain, func(e *SectionEnd) { e.Point.Schema = c.schema })
			crafted := filepath.Join(t.TempDir(), "crafted.export")
			if err := os.WriteFile(crafted, encrypt(t, reseal(t, edited), tk), 0o600); err != nil {
				t.Fatal(err)
			}
			tg := newTarget(t, r)
			_, err := ImportTenant(r.ctx, ImportConfig{File: crafted, TransferKey: key, Keys: tg.keys, Installation: tg.id, Master: tg.m,
				DataDir: tg.dataDir, RebuildTimeout: time.Minute})
			if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "made by an earlier development build") {
				t.Fatalf("the import answered %v, want Invalid saying an earlier development build made it", err)
			}
			t.Logf("refused: %s", fault.Message(err))
			if _, err := tg.m.TenantByName(r.ctx, "lab"); fault.KindOf(err) != fault.NotFound {
				t.Errorf("a refused import registered the tenant: %v", err)
			}
			if p, err := (driver.SQLiteTenants{DataDir: tg.dataDir}).Path("lab"); err != nil {
				t.Fatal(err)
			} else if _, err := os.Lstat(p); err == nil {
				t.Error("a refused import left the tenant's file")
			}
		})
	}
}
