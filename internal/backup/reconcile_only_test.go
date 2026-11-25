package backup

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// siteCopy is a copy of the rig's two databases as of one moment, the way a
// site's own recovery would bring them back: the SQLite files copied with
// the databases closed, or every row of every table of the two Postgres
// schemas, with each sequence's value, read in one repeatable-read
// transaction per schema (testkit.CopyPostgresSchema).
type siteCopy struct {
	files map[string][]byte
	pg    map[string]*testkit.PostgresSchemaCopy
}

// sqliteFiles returns the rig's SQLite database files in its data
// directory, with the files SQLite keeps beside each.
func (r *rig) sqliteFiles() []string {
	var out []string
	for _, base := range []string{filepath.Join(r.dataDir, "master.db"), r.tenantDB(r.dataDir)} {
		out = append(out, base, base+"-wal", base+"-shm")
	}
	return out
}

// takeCopy copies the rig's databases as they are now. On SQLite the stores
// are closed for the copy and opened again after it.
func (r *rig) takeCopy() siteCopy {
	r.t.Helper()
	if !r.postgres {
		_ = r.st.Close()
		_ = r.m.Close()
		c := siteCopy{files: map[string][]byte{}}
		for _, f := range r.sqliteFiles() {
			b, err := os.ReadFile(f) // #nosec G304 -- the rig's own files.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				r.t.Fatal(err)
			}
			c.files[f] = b
		}
		r.openMaster()
		r.openTenant()
		return c
	}
	c := siteCopy{pg: map[string]*testkit.PostgresSchemaCopy{}}
	for _, a := range []string{r.masterAddr, r.tenantAddr} {
		c.pg[a] = testkit.CopyPostgresSchema(r.t, a)
	}
	return c
}

// putBack replaces the rig's databases with the copy, the stores closed, as
// a site's recovery does with the server stopped.
func (r *rig) putBack(c siteCopy) {
	r.t.Helper()
	_ = r.st.Close()
	_ = r.m.Close()
	if !r.postgres {
		for _, f := range r.sqliteFiles() {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				r.t.Fatal(err)
			}
		}
		for f, b := range c.files {
			if err := os.WriteFile(f, b, 0o600); err != nil {
				r.t.Fatal(err)
			}
		}
		return
	}
	for a, s := range c.pg {
		testkit.RestorePostgresSchema(r.t, a, s)
	}
}

// reconcileConfig returns the configuration of a reconcile of the rig's
// installation in place.
func (r *rig) reconcileConfig() ReconcileConfig {
	cfg := ReconcileConfig{Keys: r.keys, DataDir: r.dataDir, MasterAddress: r.masterAddr}
	if r.postgres {
		cfg.Scratch, _ = testkit.PostgresScratch(r.t)
	}
	return cfg
}

// TestReconcileOnlyAppliesWhatFollowedTheCopy guards the reconcile of
// databases a site restored by its own point-in-time recovery, on each
// database type: an installation archives the corpus, its databases are
// copied (SQLite files with the databases closed; every row of the two
// Postgres schemas in one transaction each), then a subject is deleted
// (committed in its grace period, its record written) and a key revoked.
// The copy is put back, and before the reconcile it still holds the
// subject and the key unrevoked. The reconcile applies the delete from its
// record (committed, unresolved for an admin, naming its subject so a
// re-send is held), rebuilds the catalog from the storage (the subjects
// those the storage holds, the catalog_dump.py dump equal to the one taken
// before the copy was put back), suspends every key, the revoked one
// included, takes the storage's owner record at generation 1, begins a new
// event history, says it reconciled a site's copy, writes its audit entry
// and leaves no marker; its rebuild job and its report are named for a
// reconcile, not a restore. Run again it succeeds, at generation 2, with
// the same catalog.
func TestReconcileOnlyAppliesWhatFollowedTheCopy(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		if len(all) < 2 {
			t.Fatalf("the corpus gives fewer than two subjects: %v", all)
		}
		history, err := r.p.EventHistory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cp := r.takeCopy()

		later := r.deleteSubject(r.engine(), all[0])
		if _, err := r.m.RevokeKey(ctx, "K00000000001", time.Now().UTC(), r.masterAudit(), master.Server()); err != nil {
			t.Fatal(err)
		}
		want := all[1:]
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("after the delete the catalog holds %v, want %v", got, want)
		}
		tool, outside := newOutside(t)
		var liveDump string
		if outside {
			liveDump = tool.dump(r.tenantDB(r.dataDir), r.tenantAddr)
		}

		r.putBack(cp)
		r.openMaster()
		r.openTenant()
		if got := r.subjects(r.p); !slices.Equal(got, all) {
			t.Fatalf("the copy put back holds %v, want the catalog of before the delete %v", got, all)
		}
		if k, err := r.m.Key(ctx, "K00000000001"); err != nil || !k.Revoked.IsZero() || !k.Suspended.IsZero() {
			t.Fatalf("the copy put back holds the key as %+v (%v), want it neither revoked nor suspended", k, err)
		}
		_ = r.st.Close()
		_ = r.m.Close()

		rep, err := ReconcileOnly(ctx, r.reconcileConfig())
		if err != nil {
			t.Fatalf("the reconcile failed: %v", err)
		}
		if !rep.ReconcileOnly || rep.Installation != r.id || rep.KeysSuspended != 2 || len(rep.Tenants) != 1 || len(rep.Sections) != 2 {
			t.Fatalf("the reconcile's report is %+v", rep)
		}
		res := rep.Tenants[0]
		if res.Records != 1 || res.Added != 1 || len(res.Owners) != 1 || res.Owners[0] != (Owner{Storage: "main", Generation: 1}) ||
			!res.Rebuild.Rebuild.Activated {
			t.Fatalf("the tenant's reconcile is %+v", res)
		}
		if p := rep.Sections[1].Point; p.EventHistory != history || p.LastReception == "" || p.LastCorrection != "" {
			t.Fatalf("the tenant's recovery point is %+v, want the copy's history %q, a reception and no correction", p, history)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Rebuild.ID, reconcileUser) {
			t.Fatalf("the reconcile's rebuild job is %q, want it named for a reconcile", res.Rebuild.ID)
		}
		for _, s := range []string{"Reconciled installation " + r.id, "No backup was read", "Keys: all 2 suspended",
			"after the recovery point", "1 with no row in the restored database", "the reconcile's own entry"} {
			if !strings.Contains(out.String(), s) {
				t.Fatalf("the report does not say %q:\n%s", s, out.String())
			}
		}
		if strings.Contains(out.String(), "Restored installation") || strings.Contains(out.String(), "listener's certificate") ||
			strings.Contains(out.String(), "restore's own entry") {
			t.Fatalf("the report of a reconcile reads as a restore's:\n%s", out.String())
		}
		if ReconcileInterrupted(r.dataDir) {
			t.Fatal("the reconcile left its marker")
		}

		r.openMaster()
		r.openTenant()
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("the reconciled catalog holds %v, want %v", got, want)
		}
		c, err := r.p.Correction(ctx, later.ID)
		if err != nil {
			t.Fatalf("the delete made after the copy is not in the reconciled database: %v", err)
		}
		if c.State != record.StateCommitted || !c.Unresolved || c.StateReason != tenant.StateReasonUnresolved {
			t.Fatalf("the delete adopted from its record is %+v", c)
		}
		if match, err := r.p.DeletedMatches(ctx, tenant.DeletedProbe{Subject: all[0], StudyUID: "1.2.3"}); err != nil || match.Subject != later.ID {
			t.Fatalf("the adopted delete does not name its subject: %+v %v", match, err)
		}
		keys, _, err := r.m.ListKeys(ctx, master.Page{Limit: 100})
		if err != nil || len(keys) != 2 {
			t.Fatalf("the reconciled master lists keys %+v (%v)", keys, err)
		}
		for _, k := range keys {
			if k.Suspended.IsZero() {
				t.Errorf("the key %s is not suspended after the reconcile", k.ID)
			}
		}
		if h, err := r.p.EventHistory(ctx); err != nil || h == history {
			t.Fatalf("the feed's history is %q (%v), the copy's was %q: a new one was not begun", h, err, history)
		}
		if o := r.ownerRecord(); o.Installation != r.id || o.Tenant != tenantID || o.Generation != 1 {
			t.Fatalf("the storage's owner record is %+v", o)
		}
		entries, _, err := r.m.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			found = found || e.Method == reconcileMethod && e.Object == r.id
		}
		if !found {
			t.Fatalf("the master's log has no entry of the reconcile: %+v", entries)
		}
		if outside {
			if got := tool.dump(r.tenantDB(r.dataDir), r.tenantAddr); got != liveDump {
				t.Fatalf("catalog_dump.py reads another catalog after the reconcile:\nbefore %s\nafter  %s", liveDump, got)
			}
		}

		// Run again, as after an interruption: each step reads what the last
		// left, and the catalog is rebuilt whole again.
		_ = r.st.Close()
		_ = r.m.Close()
		again, err := ReconcileOnly(ctx, r.reconcileConfig())
		if err != nil {
			t.Fatalf("a second reconcile failed: %v", err)
		}
		if g := again.Tenants[0].Owners; len(g) != 1 || g[0].Generation != 2 {
			t.Fatalf("the second reconcile took the storage at %+v, want generation 2", g)
		}
		r.openMaster()
		r.openTenant()
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("after a second reconcile the catalog holds %v, want %v", got, want)
		}
	})
}

// TestReconcileOnlyRefuses guards what a reconcile of a site's own copy
// refuses before it changes anything: a data directory a server holds (the
// lock), one a restore was interrupted in, an empty master (never filled), a
// master with no installation id, one made from another baseline schema
// (its first migration's name changed), and a key file that does not open
// the master's key check. After each refusal the catalog and the keys are as
// they were.
func TestReconcileOnlyRefuses(t *testing.T) {
	r := newRig(t, false)
	ctx := r.ctx
	r.archive(studyFiles[:1]...)
	_ = r.st.Close()
	_ = r.m.Close()
	cfg := r.reconcileConfig()

	lock, err := server.TakeLock(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileOnly(ctx, cfg); fault.KindOf(err) != fault.Conflict || !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("a reconcile beside a server answered %v", err)
	}
	_ = lock.Release()

	if err := os.WriteFile(MarkerPath(r.dataDir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileOnly(ctx, cfg); err == nil || !strings.Contains(err.Error(), "a restore was interrupted") {
		t.Fatalf("a reconcile where a restore was interrupted answered %v", err)
	}
	if err := os.Remove(MarkerPath(r.dataDir)); err != nil {
		t.Fatal(err)
	}

	empty := cfg
	empty.MasterAddress = driver.SQLiteScheme + filepath.Join(t.TempDir(), "empty.db")
	if _, err := ReconcileOnly(ctx, empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("a reconcile of an empty master answered %v", err)
	}

	noID := cfg
	noIDPath := filepath.Join(t.TempDir(), "noid.db")
	noID.MasterAddress = driver.SQLiteScheme + noIDPath
	m, err := driver.OpenMaster(ctx, noID.MasterAddress)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	if _, err := ReconcileOnly(ctx, noID); err == nil || !strings.Contains(err.Error(), "holds no installation id") {
		t.Fatalf("a reconcile of a master with no installation id answered %v", err)
	}

	other := cfg
	otherPath := filepath.Join(t.TempDir(), "other.db")
	b, err := os.ReadFile(filepath.Join(r.dataDir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, b, 0o600); err != nil { // #nosec G703 -- the test's own file in its own directory.
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET name = '0001_another_baseline.sql' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	other.MasterAddress = driver.SQLiteScheme + otherPath
	if _, err := ReconcileOnly(ctx, other); err == nil || !strings.Contains(err.Error(), "cannot be brought to this version's schema") {
		t.Fatalf("a reconcile of a master from another baseline answered %v", err)
	}

	wrongKeyPath := filepath.Join(t.TempDir(), "key")
	if err := secret.CreateKeyFile(wrongKeyPath); err != nil {
		t.Fatal(err)
	}
	wrong := cfg
	if wrong.Keys, err = secret.LoadKeyFile(wrongKeyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileOnly(ctx, wrong); err == nil {
		t.Fatal("a reconcile with another installation's key file was not refused")
	}

	r.openMaster()
	r.openTenant()
	if got := r.subjects(r.p); len(got) != 1 {
		t.Fatalf("after the refusals the catalog holds %v", got)
	}
	keys, _, err := r.m.ListKeys(ctx, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !k.Suspended.IsZero() {
			t.Fatalf("a refused reconcile suspended the key %s", k.ID)
		}
	}
	if ReconcileInterrupted(r.dataDir) {
		t.Fatal("a refused reconcile left its marker")
	}
}
