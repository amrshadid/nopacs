package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// onBoth runs fn with the installation on SQLite, then on Postgres
// (skipped when there is none).
func onBoth(t *testing.T, fn func(t *testing.T, pg bool)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, false) })
	t.Run("postgres", func(t *testing.T) { fn(t, true) })
}

// state is what a restore must bring back of the rig's databases, read
// through their ports.
type state struct {
	keys           []master.Key
	grants         []master.Grant
	masterSettings []master.Setting
	tenantSettings []master.Setting
	storages       []tenant.Storage
	receivers      []tenant.Receiver
	profiles       []tenant.ProfileVersion
}

// readState reads the state of the master m and the tenant's store st.
func readState(t *testing.T, ctx context.Context, m master.Store, st tenant.Store) state {
	t.Helper()
	var s state
	var err error
	page := master.Page{Limit: 1000}
	if s.keys, _, err = m.ListKeys(ctx, page); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []master.Scope{{}, master.TenantScope(tenantID)} {
		g, _, err := m.GrantsByScope(ctx, scope, page)
		if err != nil {
			t.Fatal(err)
		}
		s.grants = append(s.grants, g...)
	}
	if s.masterSettings, _, err = m.ListSettings(ctx, page); err != nil {
		t.Fatal(err)
	}
	if s.tenantSettings, _, err = st.ListSettings(ctx, page); err != nil {
		t.Fatal(err)
	}
	p := st.(tenant.Pipeline)
	if s.storages, _, err = p.ListStorages(ctx, page); err != nil {
		t.Fatal(err)
	}
	if s.receivers, _, err = p.ListReceivers(ctx, page); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"export", "research"} {
		v, _, err := st.(tenant.Profiles).ListProfileVersions(ctx, name, page)
		if err != nil {
			t.Fatal(err)
		}
		s.profiles = append(s.profiles, v...)
	}
	return s
}

// TestRestoreRoundTrip guards a backup and its restore whole, on each
// database type: an installation with a tenant (a storage, a receiver,
// profiles, a setting), two keys with grants, an archive made by the real
// pipeline, a subject deleted and an instance deleted, both in their grace
// period, is backed up; a second subject is deleted after the backup; the
// restore into an empty
// data directory brings back the settings, keys (each suspended), grants,
// receivers, storages and profiles as they were, takes the storage's
// owner record at generation 1, adopts the later delete from its record
// (unresolved, and naming its subject so an arrival of it is held), and
// rebuilds the catalog from the storage, with both deleted subjects hidden
// and every other subject there, under a new event history; the
// installation's id file is written; and the instance's delete, undone
// after the restore, puts the instance back into its scan, which the
// rebuilt catalog holds under a new id.
func TestRestoreRoundTrip(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		r.archive(studyFiles...)
		// One instance of a scan of two or more is deleted, and two subjects
		// apart from that scan's.
		var two tenant.CatalogScan
		page, err := r.p.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Items {
			if s.Instances >= 2 {
				two = s
				break
			}
		}
		all := r.subjects(r.p)
		var subs []string
		for _, s := range all {
			if s != two.SubjectLabel {
				subs = append(subs, s)
			}
		}
		if two.Label == "" || len(subs) < 2 {
			t.Fatalf("the corpus gives no scan of two instances (%+v) or fewer than three subjects (%v)", two, all)
		}
		twoPath := label.Path{Subject: two.SubjectLabel, Session: two.SessionLabel, Scan: two.Label}
		held, _, err := r.p.ListHeldInstances(ctx, twoPath, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		e := r.engine()
		r.deleteSubject(e, subs[0])
		gone := r.deleteNow(e, tenant.CorrectionTarget{Subject: two.SubjectLabel, Session: two.SessionLabel, Scan: two.Label,
			Instance: held[0].SOPInstanceUID})
		history, err := r.p.EventHistory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// A sender whose TLS files are in the data directory, which a backup
		// does not hold.
		if _, err := r.st.(tenant.Sending).AddSender(ctx, tlsSender("peer"), time.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		tlsDir := server.TLSDir(r.dataDir, "lab")
		if err := os.MkdirAll(tlsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ca.pem", "client.pem", "client.key"} {
			if err := os.WriteFile(filepath.Join(tlsDir, name), []byte("a site's file"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		path, sum := r.backup()
		if len(sum.Sections) != 2 || sum.Sections[1].Tenant != "lab" || sum.Sections[1].Point.EventHistory != history ||
			sum.Sections[1].Point.LastCorrection == "" || sum.Sections[1].Point.LastReception == "" {
			t.Fatalf("the backup's sections are %+v", sum.Sections)
		}
		later := r.deleteSubject(e, subs[1])
		before := readState(t, ctx, r.m, r.st)
		var want []string
		for _, s := range all {
			if s != subs[0] && s != subs[1] {
				want = append(want, s)
			}
		}
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("before the restore the catalog holds %v, want %v", got, want)
		}
		tool, outside := newOutside(t)
		var liveDump string
		if outside {
			liveDump = tool.dump(r.tenantDB(r.dataDir), r.tenantAddr)
			if (!pg || tool.psql != "") && !strings.Contains(liveDump, `"sop_instance_uid"`) {
				t.Fatalf("catalog_dump.py reads no instance in the catalog before the restore:\n%s", liveDump)
			}
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
		if id, err := server.ReadInstallationID(target); err != nil || id != r.id {
			t.Fatalf("the restored data directory's id is %q (%v), want %q", id, err, r.id)
		}
		if Interrupted(target) {
			t.Fatal("the restore left its marker once it wrote the installation's id")
		}
		if rep.KeysSuspended != 2 || len(rep.Tenants) != 1 {
			t.Fatalf("the restore's report is %+v", rep)
		}
		res := rep.Tenants[0]
		if res.Added != 1 || res.Records != 3 || len(res.Owners) != 1 || res.Owners[0] != (Owner{Storage: "main", Generation: 1}) ||
			!res.Rebuild.Rebuild.Activated {
			t.Fatalf("the tenant's reconcile is %+v", res)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "Keys: all 2 suspended") {
			t.Fatalf("the report reads %q (%v)", out.String(), err)
		}
		if !strings.Contains(out.String(), "sender peer: TLS certificate authorities ca.pem, TLS client certificate client.pem, TLS private key client.key") ||
			strings.Contains(out.String(), "a site's file") {
			t.Fatalf("the report does not name the sender's TLS files the restored data directory lacks, or prints one:\n%s", out.String())
		}

		r.dataDir, r.masterAddr = target, cfg.MasterAddress
		r.openMaster()
		r.openTenant()
		after := readState(t, ctx, r.m, r.st)
		for i, k := range after.keys {
			if k.Suspended.IsZero() {
				t.Errorf("the restored key %s is not suspended", k.ID)
			}
			after.keys[i].Suspended = before.keys[i].Suspended
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the restored configuration differs:\nbefore %+v\nafter  %+v", before, after)
		}
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("the restored catalog holds %v, want %v", got, want)
		}
		c, err := r.p.Correction(ctx, later.ID)
		if err != nil {
			t.Fatalf("the delete made after the backup is not in the restored database: %v", err)
		}
		if c.State != record.StateCommitted || !c.Unresolved || c.StateReason != tenant.StateReasonUnresolved {
			t.Fatalf("the adopted delete is %+v", c)
		}
		match, err := r.p.DeletedMatches(ctx, tenant.DeletedProbe{Subject: subs[1], StudyUID: "1.2.3"})
		if err != nil || match.Subject != later.ID {
			t.Fatalf("the adopted delete does not name its subject: %+v %v", match, err)
		}
		if h, err := r.p.EventHistory(ctx); err != nil || h == history {
			t.Fatalf("the restored feed's history is %q (%v), the backup's was %q: a new one was not begun", h, err, history)
		}
		owner := r.ownerRecord()
		if owner.Installation != r.id || owner.Tenant != tenantID || owner.Generation != 1 {
			t.Fatalf("the storage's owner record is %+v", owner)
		}
		if outside {
			if got := tool.dump(r.tenantDB(target), r.tenantAddr); got != liveDump {
				t.Fatalf("catalog_dump.py reads another catalog after the restore:\nbefore %s\nafter  %s", liveDump, got)
			}
		}
		// The instance's delete, in its grace period at the backup, is undone
		// after the restore: the instance comes back into its scan, which the
		// rebuilt catalog holds under a new id.
		if res.Relinked == 0 {
			t.Fatalf("the restore pointed no hidden row at the rebuilt catalog: %+v", res)
		}
		if _, err := r.p.UndoCorrection(ctx, gone.ID, "deleted by mistake", time.Now(), r.audit(), nil); err != nil {
			t.Fatalf("the instance's delete cannot be undone after the restore: %v", err)
		}
		back, _, err := r.p.ListHeldInstances(ctx, twoPath, master.Page{Limit: 1000})
		if err != nil || len(back) != len(held) {
			t.Fatalf("after the undo the scan holds %d instances (%v), want %d", len(back), err, len(held))
		}
	})
}

// deleteNow previews and deletes target, in its grace period.
func (r *rig) deleteNow(e *correct.Engine, target tenant.CorrectionTarget) tenant.Correction {
	r.t.Helper()
	p, err := r.p.PreviewDelete(r.ctx, target, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// tenantDB returns the tenant's SQLite file in dataDir; "" on Postgres.
func (r *rig) tenantDB(dataDir string) string {
	if r.postgres {
		return ""
	}
	p, err := (driver.SQLiteTenants{DataDir: dataDir}).Path("lab")
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// ownerRecord reads the storage's owner record.
func (r *rig) ownerRecord() record.Owner {
	r.t.Helper()
	rc, err := r.storage.Read(context.Background(), layout.OwnerKey, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	o, err := record.DecodeOwner(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return o
}
