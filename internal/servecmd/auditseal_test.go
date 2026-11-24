package servecmd_test

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/amrshadid/nopacs/internal/auditseal"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/server"
)

// TestServeSealsTheAudit guards that the real server seals its audit logs:
// a tenant created through the API, a read, then a clean stop, leaves the
// master's log and the tenant's sealed by the server's last pass, which
// runs after the drain and the audit flush, so nothing is left unsealed;
// each verifying with the public keys derived from the installation's key
// file, each holding the other's latest checkpoint as its anchor, and the
// creation's entry sealed.
func TestServeSealsTheAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stopping the real server cleanly needs SIGTERM, which Windows cannot send")
	}
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	c := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dir)
	waitServing(t, dir, c)
	hc := socketClient(dir)
	status, err := call(hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"lab","databaseType":"DATABASE_TYPE_SQLITE"}`, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("creating a tenant: %d %v\n%s", status, err, c.stderr.String())
	}
	// A change in the tenant, so its own log has an entry to seal.
	status, err = call(hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"events.retention","scope":{"tenant":"lab"},"value":"30d"}}`, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("setting the tenant's events.retention: %d %v\n%s", status, err, c.stderr.String())
	}
	// A read just before the stop: its entry is held by the audit writer
	// and written at the stop, after the drain; the last pass runs after
	// that, so it is sealed too.
	status, err = call(hc, "/nopacs.v1.TenantService/ListTenants", `{}`, "")
	hc.CloseIdleConnections()
	if err != nil || status != http.StatusOK {
		t.Fatalf("listing the tenants: %d %v\n%s", status, err, c.stderr.String())
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, c); code != 0 {
		t.Fatalf("serve exited %d\n%s", code, c.stderr.String())
	}

	ctx := context.Background()
	id, err := server.ReadInstallationID(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(filepath.Join(dir, servecmd.KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := auditseal.PublicKeys(keys, id)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sqlite.OpenMaster(ctx, filepath.Join(dir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	lab, err := m.TenantByName(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := sqlite.OpenTenant(ctx, filepath.Join(dir, "tenants", "lab.db"), lab.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ts.Close() }()

	logs := map[string]master.AuditSeals{master.MasterAuditLog: m, lab.ID: ts}
	latest := map[string]master.AuditCheckpoint{}
	for name, st := range logs {
		r, err := auditseal.Verify(ctx, st, auditseal.VerifyOptions{Installation: id, Log: name, Keys: pub})
		if err != nil {
			t.Fatalf("Verify(%s): %v", name, err)
		}
		if !r.Intact || r.Last == nil || r.EntriesChecked == 0 || r.Unsealed != 0 {
			t.Fatalf("log %s after serve: %+v", name, r)
		}
		latest[name] = *r.Last
		t.Logf("log %s: %d entries in %d checkpoints verified, %d unsealed", name, r.EntriesChecked, r.CheckpointsChecked, r.Unsealed)
	}
	for name, st := range logs {
		anchors, err := st.AuditAnchors(ctx)
		if err != nil {
			t.Fatal(err)
		}
		other := master.MasterAuditLog
		if name == master.MasterAuditLog {
			other = lab.ID
		}
		if len(anchors) != 1 || anchors[0].Log != other || anchors[0].Digest != latest[other].Digest {
			t.Errorf("log %s keeps anchors %+v, want the latest checkpoint of %s", name, anchors, other)
		}
	}
	sealed, err := m.SealedAudit(ctx, 1, master.MaxAuditSealBatch)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sealed {
		if s.Entry.Method == "/nopacs.v1.TenantService/CreateTenant" && s.Entry.Outcome == master.OutcomeDone {
			found = true
		}
	}
	if !found {
		t.Error("the tenant's creation is not among the master's sealed entries")
	}
}
