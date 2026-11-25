package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// addTenant adds a tenant on SQLite to the rig's installation, its id id
// and its name name, with a directory storage "main" at root/name, and
// returns its storage. Its database is closed again.
func (r *rig) addTenant(id, name string) blob.Store {
	r.t.Helper()
	sealed, err := r.keys.Seal(registry.AddressContext(id), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := r.m.CreateTenant(r.ctx, master.Tenant{ID: id, Name: name, Driver: master.SQLiteDriver, SealedAddress: sealed, State: master.StateReady,
		Created: now}, r.masterAudit(), master.Server()); err != nil {
		r.t.Fatal(err)
	}
	row, err := r.m.Tenant(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	st, err := OpenTenant(r.ctx, r.keys, r.dataDir, row)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.root, name), 0o700); err != nil {
		r.t.Fatal(err)
	}
	a := r.audit()
	a.TenantName = name
	if _, err := st.(tenant.Pipeline).AddStorage(r.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, now, a, nil); err != nil {
		r.t.Fatal(err)
	}
	ds, err := dir.Open(filepath.Join(r.root, name))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = ds.Close() })
	return ds
}

// putOwner writes an owner record on st naming installation and tenant at
// generation, signed with keys.
func (r *rig) putOwner(st blob.Store, installation, tenantID string, generation int64, keys *secret.Keys) {
	r.t.Helper()
	if err := writeOwner(r.ctx, st, record.Owner{Installation: installation, Tenant: tenantID, Generation: generation, Written: time.Now().UTC()},
		keys); err != nil {
		r.t.Fatal(err)
	}
}

// hasOwner reports whether st holds an owner record.
func (r *rig) hasOwner(st blob.Store) bool {
	r.t.Helper()
	_, err := st.Stat(r.ctx, layout.OwnerKey)
	if err != nil && fault.KindOf(err) != fault.NotFound {
		r.t.Fatal(err)
	}
	return err == nil
}

// suspendedKeys counts the master's keys suspended.
func (r *rig) suspendedKeys() int {
	r.t.Helper()
	keys, _, err := r.m.ListKeys(r.ctx, pageAll)
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, k := range keys {
		if !k.Suspended.IsZero() {
			n++
		}
	}
	return n
}

// TestReconcileLeavesATenantAnotherInstallationWrites guards a reconcile in
// place of an installation one of whose tenants was handed to another
// installation since the copy, which took its storage over (the owner
// record names it): every owner record is read before anything is
// written, that tenant is not reconciled but left fenced (read only) in the
// master with the reason, its owner record as the other installation wrote
// it, and the report names it and offers --take-over only for a claim known
// to be stale; the reconcile goes on for the other tenant, ends, and leaves
// no marker.
func TestReconcileLeavesATenantAnotherInstallationWrites(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	other := server.NewID()
	handed := r.addTenant("T0000000000002", "aaa")
	r.putOwner(handed, other, "T0000000000002", 3, otherKeys(t))
	_ = r.st.Close()
	_ = r.m.Close()

	rep, err := ReconcileOnly(r.ctx, r.reconcileConfig())
	if err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	if len(rep.Tenants) != 2 || rep.Tenants[0].Tenant != "aaa" || rep.Tenants[1].Tenant != "lab" {
		t.Fatalf("the report's tenants are %+v", rep.Tenants)
	}
	if f := rep.Tenants[0].Fenced; !strings.Contains(f, "names installation "+other) || !strings.Contains(f, "only if you know its claim is stale") {
		t.Fatalf("the handed-off tenant's result says %q", f)
	}
	if lab := rep.Tenants[1]; lab.Fenced != "" || len(lab.Owners) != 1 || lab.Owners[0].Generation != 1 || !lab.Rebuild.Rebuild.Activated {
		t.Fatalf("the other tenant's reconcile is %+v", lab)
	}
	var out strings.Builder
	if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "NOT reconciled, and left fenced (read only") ||
		strings.Count(out.String(), "--take-over") != 1 {
		t.Fatalf("the report reads %q (%v)", out.String(), err)
	}
	if ReconcileInterrupted(r.dataDir) {
		t.Fatal("the reconcile left its marker")
	}
	o, err := readOwner(r.ctx, handed, "main")
	if err != nil || o.record.Installation != other || o.record.Generation != 3 {
		t.Fatalf("the handed-off tenant's owner record is %+v (%v), want the other installation's at generation 3", o.record, err)
	}
	r.openMaster()
	row, err := r.m.TenantByName(r.ctx, "aaa")
	if err != nil || row.State != master.StateFenced || !strings.Contains(row.StateReason, "names installation "+other) {
		t.Fatalf("the handed-off tenant is %s, %q (%v), want fenced with the reason", row.State, row.StateReason, err)
	}
	if n := r.suspendedKeys(); n != 2 {
		t.Fatalf("%d keys suspended, want 2", n)
	}
}

// TestReconcileRefusesBeforeWritingAnything guards a refusal of an owner
// record that is not another installation's (one naming this installation
// at a generation past the restored database's, which a restore or a
// reconcile since wrote): it stops the reconcile before anything is
// written, so the tenant listed before it keeps its storage untaken, no
// key is suspended and no marker is left; with TakeOver it succeeds.
func TestReconcileRefusesBeforeWritingAnything(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	first := r.addTenant("T0000000000002", "aaa")
	r.putOwner(r.storage, r.id, tenantID, 5, r.keys)
	_ = r.st.Close()
	_ = r.m.Close()

	_, err := ReconcileOnly(r.ctx, r.reconcileConfig())
	if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "--take-over") ||
		!strings.Contains(fault.Message(err), "owner generation 5") {
		t.Fatalf("the reconcile answered %v, want a conflict naming the generation and --take-over", err)
	}
	if strings.Count(err.Error(), "owner generation 5") != 1 {
		t.Fatalf("the error's text repeats its message: %s", err.Error())
	}
	if ReconcileInterrupted(r.dataDir) {
		t.Fatal("a reconcile refused before writing left its marker")
	}
	if r.hasOwner(first) {
		t.Fatal("a reconcile refused before writing took the storage of the tenant before the refused one")
	}
	r.openMaster()
	if n := r.suspendedKeys(); n != 0 {
		t.Fatalf("a reconcile refused before writing suspended %d keys", n)
	}
	_ = r.m.Close()

	cfg := r.reconcileConfig()
	cfg.TakeOver = true
	rep, err := ReconcileOnly(r.ctx, cfg)
	if err != nil || rep.Tenants[1].Owners[0].Generation != 6 {
		t.Fatalf("the reconcile with TakeOver: %+v %v", rep.Tenants, err)
	}
}

// TestRestoreLeavesATenantAnotherInstallationWrites guards a restore of a
// backup made between a tenant's hand-off and its removal, whose storage
// the other installation has taken over since (the owner record names it):
// the other tenants are restored, the handed-off one is left fenced in the
// master with the reason, not reconciled, its owner record as the other
// installation wrote it, and the report names it, offering --take-over
// only for a claim known to be stale. With TakeOver, a restore into
// another directory takes every storage, that one included.
func TestRestoreLeavesATenantAnotherInstallationWrites(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	handed := r.addTenant("T0000000000002", "aaa")
	path, _ := r.backup()
	other := server.NewID()
	r.putOwner(handed, other, "T0000000000002", 1, otherKeys(t))
	rep, err := r.restoreFresh(path)
	if err != nil {
		t.Fatalf("the restore failed: %v", err)
	}
	if len(rep.Tenants) != 2 || rep.Tenants[0].Tenant != "aaa" || rep.Tenants[1].Tenant != "lab" {
		t.Fatalf("the report's tenants are %+v", rep.Tenants)
	}
	if f := rep.Tenants[0].Fenced; !strings.Contains(f, "names installation "+other) || !strings.Contains(f, "only if you know its claim is stale") {
		t.Fatalf("the handed-off tenant's result says %q", f)
	}
	if lab := rep.Tenants[1]; lab.Fenced != "" || len(lab.Owners) != 1 || lab.Owners[0].Generation != 1 || !lab.Rebuild.Rebuild.Activated {
		t.Fatalf("the other tenant's restore is %+v", lab)
	}
	var out strings.Builder
	if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "NOT reconciled, and left fenced") ||
		!strings.Contains(out.String(), "run the restore again with --take-over") {
		t.Fatalf("the report reads %q (%v)", out.String(), err)
	}
	if got := r.subjects(r.p); len(got) != 1 {
		t.Fatalf("the restored tenant's catalog holds %v", got)
	}
	o, err := readOwner(r.ctx, handed, "main")
	if err != nil || o.record.Installation != other || o.record.Generation != 1 {
		t.Fatalf("the handed-off tenant's owner record is %+v (%v), want the other installation's", o.record, err)
	}
	row, err := r.m.TenantByName(r.ctx, "aaa")
	if err != nil || row.State != master.StateFenced || !strings.Contains(row.StateReason, "names installation "+other) {
		t.Fatalf("the handed-off tenant is %s, %q (%v), want fenced with the reason", row.State, row.StateReason, err)
	}

	again := filepath.Join(t.TempDir(), "restored")
	rep, err = Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: again,
		MasterAddress: driver.SQLiteScheme + filepath.Join(again, "master.db"), TakeOver: true})
	if err != nil || rep.Tenants[0].Fenced != "" || len(rep.Tenants[0].Owners) != 1 || rep.Tenants[0].Owners[0].Generation != 2 {
		t.Fatalf("the restore with TakeOver: %+v %v, want the handed-off tenant's storage taken at generation 2", rep.Tenants, err)
	}
	if o, err := readOwner(r.ctx, handed, "main"); err != nil || o.record.Installation != r.id {
		t.Fatalf("after the restore with TakeOver the owner record is %+v (%v)", o.record, err)
	}
}
