package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestRestoreBringsReplicaState guards that what the tenant's database
// keeps of a replica comes through a backup and its restore, on each
// database type: the replica, under its primary, as stored, and a row per
// scan of its primary; and that the restore's rebuild, which makes the
// catalog from the primary alone, leaves the catalog's scans on the primary
// and every one of them with a row on the replica, pending, since nothing
// was copied to it.
func TestRestoreBringsReplicaState(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		spare := filepath.Join(filepath.Dir(r.root), "spare")
		if err := os.MkdirAll(filepath.Join(spare, "lab"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := r.m.SetSetting(ctx, settings.StorageDirectoryRoots, "archive="+r.root+"\nspare="+spare, r.masterAudit(),
			master.Server()); err != nil {
			t.Fatal(err)
		}
		r.archive(studyFiles...)
		if _, err := r.p.AddStorage(ctx, tenant.Storage{Name: "copy", Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "spare"}, ReplicaOf: "main"}, r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		totals, err := r.p.CatalogTotals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		storages, _, err := r.p.ListStorages(ctx, master.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		path, _ := r.backup()
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
		if len(rep.Tenants) != 1 || len(rep.Tenants[0].Owners) != 2 || !rep.Tenants[0].Rebuild.Rebuild.Activated {
			t.Fatalf("the restore's report is %+v", rep)
		}
		r.dataDir, r.masterAddr = target, cfg.MasterAddress
		r.openMaster()
		r.openTenant()
		restored, _, err := r.p.ListStorages(ctx, master.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(restored, storages) {
			t.Fatalf("the restored storages are %+v, want %+v", restored, storages)
		}
		after, err := r.p.CatalogTotals(ctx)
		if err != nil || after != totals {
			t.Fatalf("the restored catalog's totals are %+v (%v), want %+v", after, err, totals)
		}
		list, _, err := r.p.ListReplicas(ctx, "", master.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Replica != "copy" || list[0].Primary != "main" || list[0].Pending != totals.Scans ||
			list[0].InStep != 0 || list[0].BackfillPending != totals.Scans {
			t.Fatalf("the restored replicas are %+v; want copy with the primary's %d scans pending", list, totals.Scans)
		}
		page, err := r.p.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Items {
			if s.Storage != "main" {
				t.Errorf("the restored catalog holds the scan %s on %s, want the primary", s.Label, s.Storage)
			}
			row, err := r.p.ReplicaScan(ctx, "copy", s.SeriesUID)
			if err != nil || row.State != tenant.ReplicaPending {
				t.Errorf("the replica's row of the scan %s after the restore is %+v (%v), want pending", s.Label, row, err)
			}
		}
	})
}
