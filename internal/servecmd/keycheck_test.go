package servecmd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/server"
)

// snapshot returns every file and directory under dir with the SHA-256 of
// each file's content, so two snapshots are equal when the directory is
// byte-identical.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own data directory.
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameSnapshot fails t when two snapshots differ, naming what changed.
func sameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed (%q, then %q)", k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s appeared", k)
		}
	}
}

// swapKeyFile puts the key file of the installation in from into the
// installation in to, keeping the file's permissions.
func swapKeyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(from, servecmd.KeyFileName)) //nolint:gosec // G304: the test's own key file.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, servecmd.KeyFileName), b, 0o600); err != nil { //nolint:gosec // G703: the test's own temporary directory.
		t.Fatal(err)
	}
}

// serveRefused runs serve in this process with the real handlers and tenant
// registry, expecting a refusal, which it returns. A serve that starts does
// not return, so a watchdog fails the test then.
func serveRefused(t *testing.T, dir string) error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		root := newRoot(servecmd.Options{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(string) string { return "" }, Services: servecmd.APIServices})
		root.SetArgs([]string{"serve", "--data-dir", dir})
		result <- root.ExecuteContext(context.Background())
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("serve returned without an error")
		}
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("serve started, where it should have been refused")
	}
	return nil
}

// childExit waits for a child to exit and returns its status.
func childExit(t *testing.T, c *child) int {
	t.Helper()
	select {
	case err := <-c.done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(15 * time.Second):
		t.Fatalf("the child did not exit:\n%s", c.stderr.String())
	}
	return -1
}

// TestWrongKeyFileRefused guards the key check: serve given a key file that
// is not the installation's (here, another installation's) refuses to
// start before it opens any tenant or listens: as a process it exits 1 with
// the line that says so; and the data directory is left byte-identical.
func TestWrongKeyFileRefused(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(dir), "other")
	if _, err := run(t, nil, "init", "--data-dir", other); err != nil {
		t.Fatal(err)
	}
	swapKeyFile(t, other, dir)
	before := snapshot(t, dir)
	err := serveRefused(t, dir)
	if !strings.Contains(err.Error(), server.MessageWrongKeyFile) {
		t.Fatalf("serve with another key file: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, dir))

	// Again as a process of its own, whose process id differs from this
	// test's: what it writes in the lock file while it holds the lock must
	// be gone when it has exited.
	c := startChild(t, "serve", "--data-dir", dir)
	if code := childExit(t, c); code != 1 || !strings.Contains(c.stderr.String(), server.MessageWrongKeyFile) {
		t.Fatalf("serve as a process with another key file: exit %d, %s", code, c.stderr.String())
	}
	sameSnapshot(t, before, snapshot(t, dir))
	if _, err := os.Stat(server.SocketPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refused serve made its socket")
	}
	t.Logf("refused: %s", strings.TrimSpace(c.stderr.String()))
}

// TestKeyFileWithNewerVersionStarts guards the key check across rotation:
// a key file with a newer key version added (the right key file, one line
// longer) still opens the check, and serve starts.
func TestKeyFileWithNewerVersionStarts(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, servecmd.KeyFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: the test's own key file.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("nopacs-key-v1 2 " + base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	c := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, c)
}

// TestNoKeyCheckRefused guards an installation made before the key check:
// a master that holds none is refused with its own line, and changed in no
// way.
func TestNoKeyCheckRefused(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := sqlite.OpenMaster(ctx, filepath.Join(dir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UnsetSetting(ctx, server.KeyCheckSetting, master.AuditEntry{
		ID: server.NewID(), Time: time.Now().UTC(), Method: "test", Outcome: master.OutcomeDone,
	}, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	err = serveRefused(t, dir)
	if !strings.Contains(err.Error(), server.MessageNoKeyCheck) {
		t.Fatalf("serve with no key check: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, dir))
}

// TestWrongKeyFileLeavesTenants guards that a refused start changes no
// tenant: a SQLite tenant created by the real server is ready; started
// again with another installation's key file, serve is refused and the
// tenant's row is still ready (without the check, every tenant became
// failed);
// with the right key file back, serve starts and serves it.
func TestWrongKeyFileLeavesTenants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stopping the real server cleanly needs SIGTERM, which Windows cannot send")
	}
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(dir), "other")
	if _, err := run(t, nil, "init", "--data-dir", other); err != nil {
		t.Fatal(err)
	}
	keep, err := os.ReadFile(filepath.Join(dir, servecmd.KeyFileName)) //nolint:gosec // G304: the test's own key file.
	if err != nil {
		t.Fatal(err)
	}

	real := []string{childRealEnv + "=1"}
	c := startChildEnv(t, real, "serve", "--data-dir", dir)
	waitServing(t, dir, c)
	hc := socketClient(dir)
	status, err := call(hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"lab","databaseType":"DATABASE_TYPE_SQLITE"}`, "")
	hc.CloseIdleConnections()
	if err != nil || status != http.StatusOK {
		t.Fatalf("creating a tenant: %d %v\n%s", status, err, c.stderr.String())
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, c); code != 0 {
		t.Fatalf("the first serve exited %d", code)
	}

	swapKeyFile(t, other, dir)
	before := snapshot(t, dir)
	if err := serveRefused(t, dir); !strings.Contains(err.Error(), server.MessageWrongKeyFile) {
		t.Fatalf("serve with another key file: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, dir))
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(dir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	tn, err := m.TenantByName(context.Background(), "lab")
	_ = m.Close()
	if err != nil || tn.State != master.StateReady {
		t.Fatalf("the tenant after the refused start: %+v %v", tn, err)
	}

	if err := os.WriteFile(filepath.Join(dir, servecmd.KeyFileName), keep, 0o600); err != nil { //nolint:gosec // G703: the test's own temporary directory.
		t.Fatal(err)
	}
	c = startChildEnv(t, real, "serve", "--data-dir", dir)
	waitServing(t, dir, c)
	hc = socketClient(dir)
	defer hc.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodPost, "http://local/nopacs.v1.TenantService/GetTenant", bytes.NewReader([]byte(`{"name":"lab"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "TENANT_STATE_READY") {
		t.Fatalf("the tenant with the right key file back: %d %s", resp.StatusCode, body)
	}
}
