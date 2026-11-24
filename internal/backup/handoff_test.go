package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// target is a second installation, with its own key file and master, that
// lists the rig's storage root under the same name.
type target struct {
	dataDir string
	keys    *secret.Keys
	id      string
	m       master.Store
}

// newTarget makes the second installation over r's storage root.
func newTarget(t *testing.T, r *rig) *target {
	t.Helper()
	tg := &target{dataDir: filepath.Join(t.TempDir(), "data")}
	if err := server.EnsurePrivateDir(tg.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := secret.CreateKeyFile(filepath.Join(tg.dataDir, "key")); err != nil {
		t.Fatal(err)
	}
	var err error
	if tg.keys, err = secret.LoadKeyFile(filepath.Join(tg.dataDir, "key")); err != nil {
		t.Fatal(err)
	}
	m, err := driver.OpenMaster(r.ctx, driver.SQLiteScheme+filepath.Join(tg.dataDir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	tg.m, tg.id = m, server.NewID()
	if err := m.SetSetting(r.ctx, server.InstallationIDSetting, tg.id, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSetting(r.ctx, settings.StorageDirectoryRoots, "archive="+r.root, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	return tg
}

// export writes the rig's tenant's export to a file and returns its path,
// summary and transfer key.
func (r *rig) export() (string, Summary, []byte) {
	r.t.Helper()
	path := filepath.Join(filepath.Dir(r.dataDir), "lab.export")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- the test's own directory.
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sum, key, err := ExportTenant(r.ctx, ExportConfig{Installation: r.id, Keys: r.keys, Tenant: "lab", TenantID: tenantID, Driver: master.SQLiteDriver,
		DB: r.st.(snapshot.Database), Store: r.st, Owners: r.ownerCheck(), Out: f})
	if err != nil {
		r.t.Fatal(err)
	}
	return path, sum, key
}

// importInto imports the export at path into tg.
func (tg *target) importInto(r *rig, path string, key []byte, takeOver bool) (ImportReport, error) {
	return ImportTenant(r.ctx, ImportConfig{File: path, TransferKey: key, Keys: tg.keys, Installation: tg.id, Master: tg.m, DataDir: tg.dataDir,
		TakeOver: takeOver, RebuildTimeout: time.Minute})
}

// ownerCheck returns the owner check of the rig's installation.
func (r *rig) ownerCheck() OwnerCheck {
	return OwnerCheck{Installation: r.id, Keys: r.keys, Opener: place.Opener{Values: settingvalue.Reader{Master: r.m}, Keys: r.keys, StaleUploadAge: -1}}
}

// TestExportImportHandsTheTenantOver guards a tenant's hand-off from one
// installation to another over the same storage: the export opens with its
// transfer key alone and holds no plaintext of a sealed value; the export
// takes the storage's owner record to generation 1 before its snapshot and
// holds that generation; the import
// loads the tenant under the target's key file (its naming secret opens to
// the same bytes there and not under the source's), takes the storage over
// with an owner record naming the target at generation 2 that verifies
// under the target's key file, writes the correction record again under
// that key, rebuilds the catalog with the deleted subject still hidden, and
// registers the tenant ready with its audit entry; then the source's own
// owner check fences its tenant and refuses to take it back, naming the
// target.
func TestExportImportHandsTheTenantOver(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	secretPlain := bytes.Repeat([]byte{7}, 32)
	sealed, err := r.keys.Seal(registry.SecretContext(tenantID, tenant.SecretNaming), secretPlain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.(tenant.Secrets).KeepTenantSecret(r.ctx, tenant.SecretNaming, sealed, time.Now()); err != nil {
		t.Fatal(err)
	}
	all := r.subjects(r.p)
	if len(all) < 2 {
		t.Fatalf("the corpus gives %d subjects", len(all))
	}
	del := r.deleteSubject(r.engine(), all[0])
	if found, err := r.ownerCheck().Fence(r.ctx, tenantID, "lab", r.st); err != nil || found != "" {
		t.Fatalf("the source's tenant before the hand-off: fence %q, %v", found, err)
	}
	if err := r.ownerCheck().Reclaimable(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Fatalf("a hand-off no import followed is not reclaimable: %v", err)
	}

	path, sum, key := r.export()
	if len(key) != secret.TransferKeySize || sum.Size == 0 || len(sum.Sections) != 1 || sum.Sections[0].Tenant != "lab" ||
		!slices.Equal(sum.Sections[0].Point.Owners, []Owner{{Storage: "main", Generation: 1}}) {
		t.Fatalf("the export's summary: %+v, key of %d bytes", sum, len(key))
	}
	if o := r.ownerRecord(); o.Installation != r.id || o.Tenant != tenantID || o.Generation != 1 || record.VerifyOwner(o, r.keys) != nil {
		t.Fatalf("the owner record after the export: %+v", o)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, secretPlain) {
		t.Fatal("the export holds a sealed value's plaintext")
	}
	if _, err := Verify(bytes.NewReader(raw), r.keys); err == nil {
		t.Fatal("the export opened with the source's key file")
	}

	tg := newTarget(t, r)
	rep, err := tg.importInto(r, path, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.From != r.id || rep.Tenant != "lab" || len(rep.Result.Owners) != 1 || rep.Result.Owners[0] != (Owner{Storage: "main", Generation: 2}) ||
		rep.Rewritten != 1 || rep.Result.Rebuild.Rebuild == nil || !rep.Result.Rebuild.Rebuild.Activated {
		t.Fatalf("the import's report: %+v", rep)
	}
	row, err := tg.m.TenantByName(r.ctx, "lab")
	if err != nil || row.ID != tenantID || row.State != master.StateReady {
		t.Fatalf("the target's row: %+v %v", row, err)
	}
	entries, _, err := tg.m.ListAudit(r.ctx, master.AuditFilter{TenantName: "lab"}, master.Page{Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].Method != ImportMethod || entries[0].Details["from_installation"] != r.id ||
		entries[0].Details["owner_generations"] != "main=2" {
		t.Fatalf("the import's audit entry: %+v %v", entries, err)
	}
	st, err := OpenTenant(r.ctx, tg.keys, tg.dataDir, row)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := st.(tenant.Secrets).TenantSecret(r.ctx, tenant.SecretNaming)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := tg.keys.Open(registry.SecretContext(tenantID, tenant.SecretNaming), got); err != nil || !bytes.Equal(plain, secretPlain) {
		t.Fatalf("the naming secret under the target's key file: %v", err)
	}
	if _, err := r.keys.Open(registry.SecretContext(tenantID, tenant.SecretNaming), got); err == nil {
		t.Fatal("the imported secret still opens with the source's key file")
	}
	if subs := r.subjects(st.(tenant.Pipeline)); !slices.Equal(subs, all[1:]) {
		t.Fatalf("the target's catalog holds %v, want %v (the deleted subject hidden)", subs, all[1:])
	}
	o := r.ownerRecord()
	if o.Installation != tg.id || o.Tenant != tenantID || o.Generation != 2 || record.VerifyOwner(o, tg.keys) != nil {
		t.Fatalf("the owner record after the import: %+v", o)
	}
	key2, err := layout.CorrectionKey(del.ID)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.storage.Read(r.ctx, key2, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	_, derr := record.DecodeCorrection(rc, tenantID, tg.keys)
	_ = rc.Close()
	if derr != nil {
		t.Fatalf("the correction record does not verify under the target's key file: %v", derr)
	}

	reason, err := r.ownerCheck().Fence(r.ctx, tenantID, "lab", r.st)
	if err != nil || !strings.Contains(reason, "names installation "+tg.id) {
		t.Fatalf("the source's fence after the import: %q, %v", reason, err)
	}
	if err := r.ownerCheck().Reclaimable(r.ctx, tenantID, "lab", r.st); fault.KindOf(err) != fault.Precondition ||
		!strings.Contains(fault.Message(err), tg.id) {
		t.Fatalf("the source's reclaim after the import: %v", err)
	}
	if _, err := tg.importInto(r, path, key, false); fault.KindOf(err) != fault.Conflict {
		t.Fatalf("a second import of the same tenant: %v", err)
	}
}

// TestImportRefusesAnExportOlderThanAReclaim guards a hand-off undone: the
// source reclaims its tenant after an export no import followed and goes on
// writing (a subject deleted), so the export no longer says what the tenant
// holds. The reclaim takes the owner record one generation past the one the
// export holds, and an import of that export is then refused, naming
// --take-over, and registers nothing, rather than bring the deleted subject
// back on the other installation.
func TestImportRefusesAnExportOlderThanAReclaim(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	all := r.subjects(r.p)
	if len(all) < 2 {
		t.Fatalf("the corpus gives %d subjects", len(all))
	}
	path, sum, key := r.export()
	if !slices.Equal(sum.Sections[0].Point.Owners, []Owner{{Storage: "main", Generation: 1}}) {
		t.Fatalf("the export holds owner generations %+v", sum.Sections[0].Point.Owners)
	}
	owners, err := r.ownerCheck().Reclaim(r.ctx, tenantID, "lab", r.st)
	if err != nil || !slices.Equal(owners, []Owner{{Storage: "main", Generation: 2}}) {
		t.Fatalf("the reclaim: %+v %v", owners, err)
	}
	if o := r.ownerRecord(); o.Installation != r.id || o.Generation != 2 || record.VerifyOwner(o, r.keys) != nil {
		t.Fatalf("the owner record after the reclaim: %+v", o)
	}
	if reason, err := r.ownerCheck().Fence(r.ctx, tenantID, "lab", r.st); err != nil || reason != "" {
		t.Fatalf("the source's fence after its reclaim: %q, %v", reason, err)
	}
	r.deleteSubject(r.engine(), all[0])
	tg := newTarget(t, r)
	rep, err := tg.importInto(r, path, key, false)
	if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "--take-over") || !strings.Contains(fault.Message(err), "generation 2") {
		t.Fatalf("an export older than a reclaim: owners %+v, %v", rep.Result.Owners, err)
	}
	if _, err := tg.m.TenantByName(r.ctx, "lab"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a refused import registered the tenant: %v", err)
	}
	if o := r.ownerRecord(); o.Installation != r.id || o.Generation != 2 {
		t.Fatalf("a refused import wrote the owner record: %+v", o)
	}
}

// TestImportRefuses guards what an import refuses before it writes
// anything: another transfer key, a backup given for an export, an export
// given back to the installation that made it; and a storage whose owner
// record names a third installation, refused naming --take-over and taken
// with it.
func TestImportRefuses(t *testing.T) {
	r := newRig(t, false)
	path, _, key := r.export()
	tg := newTarget(t, r)
	other, _, err := secret.NewTransferKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.importInto(r, path, other, false); fault.KindOf(err) != fault.Invalid {
		t.Errorf("another transfer key: %v", err)
	}
	backupPath, _ := r.backup()
	if _, err := tg.importInto(r, backupPath, key, false); err == nil {
		t.Error("a backup was imported")
	}
	if _, err := ImportTenant(r.ctx, ImportConfig{File: path, TransferKey: key, Keys: r.keys, Installation: r.id, Master: r.m,
		DataDir: t.TempDir()}); fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "reclaim") {
		t.Errorf("an export given back to its installation: %v", err)
	}
	if _, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: filepath.Join(t.TempDir(), "x"),
		MasterAddress: "sqlite:" + filepath.Join(t.TempDir(), "m.db")}); err == nil {
		t.Error("an export was restored as a backup")
	}
	if p, err := (driver.SQLiteTenants{DataDir: tg.dataDir}).Path("lab"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Lstat(p); err == nil {
		t.Fatal("a refused import left the tenant's file")
	}

	third, err := record.EncodeOwner(record.Owner{Format: record.Format, Installation: "someone-else", Tenant: tenantID, Generation: 1, Written: time.Now()}, tg.keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.storage.Put(r.ctx, layout.OwnerKey, bytes.NewReader(third)); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.importInto(r, path, key, false); fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "--take-over") {
		t.Fatalf("a storage a third installation owns: %v", err)
	}
	if _, err := tg.m.TenantByName(r.ctx, "lab"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a refused import registered the tenant: %v", err)
	}
	p, _ := (driver.SQLiteTenants{DataDir: tg.dataDir}).Path("lab")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(p + suffix)
	}
	rep, err := tg.importInto(r, path, key, true)
	if err != nil || rep.Result.Owners[0].Generation != 2 {
		t.Fatalf("the import with --take-over: %+v %v", rep.Result.Owners, err)
	}
}

// TestOwnerCheckJudges guards each finding of the owner check against the
// generation the tenant's database holds: no record lets it write; a
// record of its own at the generation held, verifying, lets it write; one
// without a MAC this key file makes, one naming another tenant, one at a
// later generation, and a record that is not one, each fence it with a
// reason naming the storage; reclaim then refuses with the same reason. A
// record one generation behind the database is reclaimable, two behind is
// not, and a record this tenant wrote that is gone is not.
func TestOwnerCheckJudges(t *testing.T) {
	r := newRig(t, false)
	c := r.ownerCheck()
	put := func(b []byte) {
		t.Helper()
		if _, err := r.storage.Put(r.ctx, layout.OwnerKey, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	signed := func(o record.Owner) []byte {
		t.Helper()
		o.Format, o.Written = record.Format, time.Now()
		b, err := record.EncodeOwner(o, r.keys)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if reason, err := c.Fence(r.ctx, tenantID, "lab", r.st); err != nil || reason != "" {
		t.Fatalf("no record: %q %v", reason, err)
	}
	if err := r.st.(tenant.Restoring).SetStorageOwner(r.ctx, "main", 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	put(signed(record.Owner{Installation: r.id, Tenant: tenantID, Generation: 2}))
	if reason, err := c.Fence(r.ctx, tenantID, "lab", r.st); err != nil || reason != "" {
		t.Fatalf("its own record: %q %v", reason, err)
	}
	if err := c.Reclaimable(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Fatalf("its own record, reclaim: %v", err)
	}
	forged := []byte(`{"format":1,"installation":"` + r.id + `","tenant":"` + tenantID + `","generation":2,"written":"2026-01-02T03:04:05Z"}` + "\n")
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"no mac":       {forged, "does not verify"},
		"other tenant": {signed(record.Owner{Installation: r.id, Tenant: "T9", Generation: 2}), "another tenant"},
		"later":        {signed(record.Owner{Installation: r.id, Tenant: tenantID, Generation: 3}), "generation 3"},
		"another inst": {signed(record.Owner{Installation: "inst-B", Tenant: tenantID, Generation: 1}), "names installation inst-B"},
		"not a record": {[]byte("{}\n"), "not one this installation writes"},
		"older":        {signed(record.Owner{Installation: r.id, Tenant: tenantID, Generation: 1}), ""},
	} {
		put(tc.b)
		reason, err := c.Fence(r.ctx, tenantID, "lab", r.st)
		if err != nil || !strings.Contains(reason, tc.want) || (tc.want == "") != (reason == "") || (reason != "" && !strings.Contains(reason, "main")) {
			t.Errorf("%s: fence %q, %v; want %q", name, reason, err, tc.want)
		}
		if tc.want != "" {
			if err := c.Reclaimable(r.ctx, tenantID, "lab", r.st); fault.KindOf(err) != fault.Precondition {
				t.Errorf("%s: reclaim %v", name, err)
			}
		}
	}
	// One generation behind the database is what an advance that wrote the
	// database and not the record leaves; two behind is not.
	put(signed(record.Owner{Installation: r.id, Tenant: tenantID, Generation: 1}))
	if err := c.Reclaimable(r.ctx, tenantID, "lab", r.st); err != nil {
		t.Errorf("a record one generation behind, reclaim: %v", err)
	}
	if err := r.st.(tenant.Restoring).SetStorageOwner(r.ctx, "main", 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.Reclaimable(r.ctx, tenantID, "lab", r.st); fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "generation 1") {
		t.Errorf("a record two generations behind, reclaim: %v", err)
	}
	if err := r.storage.Delete(r.ctx, layout.OwnerKey); err != nil {
		t.Fatal(err)
	}
	if err := c.Reclaimable(r.ctx, tenantID, "lab", r.st); fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "removed") {
		t.Errorf("a record this tenant wrote, gone: %v", err)
	}
}

// TestAdvanceTakesEveryRecordFurther guards the advance an export and a
// reclaim make: a storage with no record gets one at generation 1, signed
// for this installation, and the database holds 1; again, both are at 2; a
// record naming another installation is refused as a precondition, and
// neither the record nor the database changes.
func TestAdvanceTakesEveryRecordFurther(t *testing.T) {
	r := newRig(t, false)
	c := r.ownerCheck()
	held := func() []tenant.StorageOwner {
		t.Helper()
		got, err := r.st.(tenant.Restoring).StorageOwners(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for want := int64(1); want <= 2; want++ {
		owners, err := c.Advance(r.ctx, tenantID, "lab", r.st)
		if err != nil || !slices.Equal(owners, []Owner{{Storage: "main", Generation: want}}) {
			t.Fatalf("advance to %d: %+v %v", want, owners, err)
		}
		if o := r.ownerRecord(); o.Installation != r.id || o.Tenant != tenantID || o.Generation != want || record.VerifyOwner(o, r.keys) != nil {
			t.Fatalf("the owner record at %d: %+v", want, o)
		}
		if h := held(); len(h) != 1 || h[0].Generation != want {
			t.Fatalf("the database holds %+v, want %d", h, want)
		}
	}
	foreign, err := record.EncodeOwner(record.Owner{Format: record.Format, Installation: "inst-B", Tenant: tenantID, Generation: 3, Written: time.Now()}, r.keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.storage.Put(r.ctx, layout.OwnerKey, bytes.NewReader(foreign)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Advance(r.ctx, tenantID, "lab", r.st); fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "inst-B") {
		t.Fatalf("advance over another installation's record: %v", err)
	}
	if o := r.ownerRecord(); o.Installation != "inst-B" || o.Generation != 3 {
		t.Fatalf("a refused advance wrote the record: %+v", o)
	}
	if h := held(); len(h) != 1 || h[0].Generation != 2 {
		t.Fatalf("a refused advance changed the database: %+v", h)
	}
}

// TestExportRefusesAValueThatDoesNotOpen guards that an export whose
// tenant holds a sealed value its key file does not open fails, rather than
// carrying a value nobody can open on the other side.
func TestExportRefusesAValueThatDoesNotOpen(t *testing.T) {
	r := newRig(t, false)
	other, _, err := secret.NewTransferKey()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := secret.TransferKeys(other)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := foreign.Seal(registry.SecretContext(tenantID, tenant.SecretUID), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.(tenant.Secrets).KeepTenantSecret(r.ctx, tenant.SecretUID, sealed, time.Now()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, key, err := ExportTenant(r.ctx, ExportConfig{Installation: r.id, Keys: r.keys, Tenant: "lab", TenantID: tenantID, DB: r.st.(snapshot.Database), Store: r.st,
		Owners: r.ownerCheck(), Out: &out})
	if err == nil || key != nil || !strings.Contains(fault.Message(err), "does not open") {
		t.Fatalf("an export over a value that does not open: key %v, %v", key != nil, err)
	}
}
