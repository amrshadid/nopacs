package servecmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// adminMaster makes a master database on SQLite holding the tenant lab and
// the key id with the admin role on it, and returns it.
func adminMaster(t *testing.T, id string) master.Store {
	t.Helper()
	ctx := context.Background()
	m, err := sqlite.OpenMaster(ctx, filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	n := 0
	entry := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("m%08d", n), Time: now, LocalUser: "501", Method: "Test", Outcome: master.OutcomeDone}
	}
	if err := m.CreateTenant(ctx, master.Tenant{ID: "T0000000000001", Name: "lab", Driver: master.SQLiteDriver, SealedAddress: []byte{1},
		State: master.StateReady, Created: now}, entry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateKey(ctx, master.Key{ID: id, Hash: make([]byte, master.KeyHashLength), Holder: "an admin", Created: now}, entry(),
		master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetGrant(ctx, master.Grant{KeyID: id, Scope: master.TenantScope("T0000000000001"), Role: master.RoleAdmin}, entry(),
		master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAPersonsDisposalStopsWhenItsKeyIsDemoted guards a person's dispose
// job against the key that confirmed it: an admin queues a disposal, then
// loses the admin role on the tenant before the runner reaches it; the
// session's delete is refused in its own transaction, nothing is deleted,
// and the job ends failed with the reason, its session left without an
// outcome, rather than deleting in the name of a key that may no longer.
func TestAPersonsDisposalStopsWhenItsKeyIsDemoted(t *testing.T) {
	r, _ := disposalRig(t, "MR_small.dcm")
	const key = "K0000000000ad"
	m := adminMaster(t, key)
	r.w.d.Master = m
	later := time.Now().Add(72 * time.Hour)
	r.w.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	p, err := r.db.PreviewDisposal(r.ctx, "trial7", 100, later)
	if err != nil || len(p.Sessions) != 1 {
		t.Fatalf("the preview lists %+v (%v), want the one session", p.Sessions, err)
	}
	if _, _, _, err := r.db.QueueDispose(r.ctx, tenant.DisposeQueue{ID: "jdispose00001", Rule: "trial7", Reason: "the trial ended",
		Confirm: p.Confirm, KeyID: key, Now: later}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetGrant(r.ctx, master.Grant{KeyID: key, Scope: master.TenantScope("T0000000000001"), Role: master.RoleReader},
		master.AuditEntry{ID: "mdemote00001", Time: time.Now(), LocalUser: "501", Method: "Test", Outcome: master.OutcomeDone},
		master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	ret := &tenantRetention{w: r.w, name: "lab", st: r.db, base: r.db, log: r.w.log}
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the check: %v", err)
	}
	dj, err := r.db.DisposeJob(r.ctx, "jdispose00001")
	if err != nil {
		t.Fatal(err)
	}
	if dj.Job.State != tenant.JobFailed || !strings.Contains(dj.Job.Reason, master.RefusedNoRole) {
		t.Fatalf("the disposal of a key demoted is %s (%q), want failed for the key's lost role", dj.Job.State, dj.Job.Reason)
	}
	if s := dj.Sessions[0]; s.Outcome != "" || s.Correction != "" {
		t.Fatalf("the session of a disposal stopped is %+v, want no outcome and no delete", s)
	}
	cs, _, err := r.db.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 10})
	if err != nil || len(cs) != 0 {
		t.Fatalf("the corrections after the stopped disposal are %+v (%v), want none", cs, err)
	}
}
