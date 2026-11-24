package servecmd_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// childArgsEnv, when set, makes this test binary run the commands with the
// JSON list of arguments it holds, as a separate process, so a test can
// signal and kill a real server.
const childArgsEnv = "NOPACS_SERVECMD_CHILD_ARGS"

// childRealEnv, when set, makes the child serve the real handlers and tenant
// registry instead of the stand-ins.
const childRealEnv = "NOPACS_SERVECMD_CHILD_REAL"

// TestMain runs the tests, or the commands as a child process.
func TestMain(m *testing.M) {
	if raw := os.Getenv(childArgsEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			fmt.Fprintln(os.Stderr, "bad child args:", err)
			os.Exit(2)
		}
		os.Exit(runChild(args))
	}
	os.Exit(m.Run())
}

// runChild runs the commands with stand-in handlers, as the stand-in
// program does, and returns the exit status.
func runChild(args []string) int {
	delay := 300 * time.Millisecond
	opts := servecmd.Options{
		Stdout: os.Stdout, Stderr: os.Stderr,
		Services: func(d servecmd.Deps) (servecmd.Built, error) {
			return servecmd.Built{Register: (&servertest.Standins{Access: d.Access, Delay: delay}).Register, Methods: servertest.Contract()}, nil
		},
	}
	if os.Getenv(childRealEnv) != "" {
		// The real handlers and tenant registry, as the binary serves them.
		opts.Services = servecmd.APIServices
	}
	root := newRoot(opts)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "nopacs:", err)
		return 1
	}
	return 0
}

// newRoot is a minimal command tree with the two commands attached by the
// one line the real tree will use.
func newRoot(opts servecmd.Options) *cobra.Command {
	root := &cobra.Command{Use: "nopacs", SilenceUsage: true, SilenceErrors: true}
	// As internal/cli does: --data-dir belongs to the root, for every command.
	root.PersistentFlags().String(servecmd.DataDirFlag, "", "the data directory")
	tenant := &cobra.Command{Use: "tenant"}
	tenant.AddCommand(servecmd.TenantImport(opts))
	root.AddCommand(servecmd.Init(opts), servecmd.Serve(opts), servecmd.Backup(opts), tenant)
	return root
}

// run runs the commands in this process and returns standard output and
// the error.
func run(t *testing.T, env map[string]string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	opts := servecmd.Options{Stdout: &out, Stderr: io.Discard, Getenv: func(k string) string { return env[k] }}
	root := newRoot(opts)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// shortDir returns a new directory path, not yet made, under a short
// private parent, so its socket path stays under the system's limit.
func shortDir(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "mgc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	return filepath.Join(parent, "d")
}

// TestInit guards "nopacs-server init": it makes the directory private with the
// key file, the migrated master database, tenants/ and run/; it says where
// the key file is and that losing it loses every secret; it prints nothing
// of the key; and a second init on the same directory is refused.
func TestInit(t *testing.T) {
	dir := shortDir(t)
	out, err := run(t, nil, "init", "--data-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, servecmd.KeyFileName)
	for _, want := range []string{keyPath, "Losing it loses every secret", "nopacs-server serve", filepath.Join(dir, servecmd.MasterFileName)} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	keyText, err := os.ReadFile(keyPath) //nolint:gosec // G304: the test's own key file.
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(keyText))
	key, err := base64.StdEncoding.DecodeString(fields[len(fields)-1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, fields[len(fields)-1]) || bytes.Contains([]byte(out), key) {
		t.Fatal("init printed the key")
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{dir, filepath.Join(dir, "tenants"), filepath.Join(dir, "run")} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("%s: %v %v", p, info.Mode(), err)
			}
		}
	}
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(dir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := m.Version(context.Background()); err != nil || v < 1 {
		t.Errorf("the master database is not migrated: version %d, %v", v, err)
	}
	_ = m.Close()

	if _, err := run(t, nil, "init", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "already holds an installation") {
		t.Fatalf("a second init: %v", err)
	}
}

// TestInitMasterEnv guards --master-env: the address is read from the
// variable it names and never printed; an empty variable is refused and
// leaves nothing behind.
func TestInitMasterEnv(t *testing.T) {
	dir := shortDir(t)
	other := filepath.Join(filepath.Dir(dir), "elsewhere.db")
	address := "sqlite:" + other
	if _, err := run(t, map[string]string{"MASTER": ""}, "init", "--data-dir", dir, "--master-env", "MASTER"); err == nil {
		t.Fatal("an empty --master-env variable was accepted")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused init left %s behind: %v", dir, err)
	}
	out, err := run(t, map[string]string{"MASTER": address}, "init", "--data-dir", dir, "--master-env", "MASTER")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, other) {
		t.Fatalf("init printed the master's address:\n%s", out)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("the master was not made at the address: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, servecmd.MasterFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a SQLite master was made in the data directory as well")
	}
	// serve without the flag refuses rather than make an empty master.
	if _, err := run(t, nil, "serve", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "--master-env") {
		t.Fatalf("serve without --master-env: %v", err)
	}
}

// TestServeNeedsInit guards that serve never makes an installation.
func TestServeNeedsInit(t *testing.T) {
	dir := shortDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, nil, "serve", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "nopacs-server init") {
		t.Fatalf("serve on an empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, servecmd.KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("serve made a key file")
	}
}

// child is a server process started from this test binary.
type child struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	done   chan error
}

// startChild starts this binary as "nopacs <args>".
func startChild(t *testing.T, args ...string) *child {
	t.Helper()
	return startChildEnv(t, nil, args...)
}

// startChildEnv is startChild with more environment for the child.
func startChildEnv(t *testing.T, env []string, args ...string) *child {
	t.Helper()
	raw, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: this test's own binary.
	cmd.Env = append(append(os.Environ(), env...), childArgsEnv+"="+string(raw))
	var stderr bytes.Buffer
	cmd.Stderr = &lockedWriter{w: &stderr}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, stderr: &stderr, done: make(chan error, 1)}
	exited := make(chan struct{})
	go func() {
		c.done <- cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		// The test may already have read done; exited is closed either way.
		_ = cmd.Process.Kill()
		<-exited
	})
	return c
}

// lockedWriter serializes writes from the process's pipe.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write writes p.
func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// socketClient returns an HTTP/1.1 client over a data directory's socket.
func socketClient(dir string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", server.SocketPath(dir))
		},
	}}
}

// call posts a Connect JSON request and returns the status.
func call(hc *http.Client, procedure, body, auth string) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, "http://local"+procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// waitServing waits until the server in dir answers on its socket.
func waitServing(t *testing.T, dir string, c *child) {
	t.Helper()
	hc := socketClient(dir)
	defer hc.CloseIdleConnections()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if status, err := call(hc, "/nopacs.v1.AccessService/WhoAmI", "{}", ""); err == nil && status == http.StatusOK {
			return
		}
		select {
		case err := <-c.done:
			t.Fatalf("the server exited: %v\n%s", err, c.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("the server did not answer:\n%s", c.stderr.String())
}

// TestOneServerPerDirectory guards the data directory's lock with real
// processes: a second serve on the same directory is refused with a clear
// line naming the first one's process; after the first is killed (signal 9 on
// Unix, TerminateProcess on Windows), a new one starts.
func TestOneServerPerDirectory(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	first := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, first)

	second := startChild(t, "serve", "--data-dir", dir)
	select {
	case err := <-second.done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("the second serve: %v", err)
		}
		want := fmt.Sprintf("another nopacs-server serve is running on the data directory %s (process %d)", dir, first.cmd.Process.Pid)
		if !strings.Contains(second.stderr.String(), want) {
			t.Fatalf("the second serve's refusal does not say %q:\n%s", want, second.stderr.String())
		}
		t.Logf("second serve refused: %s", strings.TrimSpace(second.stderr.String()))
	case <-time.After(15 * time.Second):
		t.Fatal("the second serve did not exit")
	}

	// Kill is signal 9 on Unix and TerminateProcess on Windows: the process
	// ends without running any of its own code, so the lock is dropped by the
	// system alone.
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-first.done; err == nil {
		t.Fatal("the first serve exited cleanly after signal 9")
	}
	third := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, third)
	t.Log("after kill -9 of the first, a new serve started on the same directory")
}

// TestStopUnderLoad guards a graceful stop with a real process: with requests
// in flight and refusals queued for the audit writer, SIGTERM lets every
// request in flight finish, the process exits 0 within the drain deadline,
// and every audit entry queued is in the master afterwards (counted from the
// log's own count and from the database).
func TestStopUnderLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be sent to a process on Windows")
	}
	const inFlight, refusals = 50, 300
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	c := startChild(t, "serve", "--data-dir", dir, "--drain", "10s")
	waitServing(t, dir, c)
	hc := socketClient(dir)
	defer hc.CloseIdleConnections()

	// Refusals first, each queuing one audit entry.
	for range refusals {
		if status, err := call(hc, "/nopacs.v1.AccessService/WhoAmI", "{}", "Bearer nopacs_111111111111_1111111111111111111111111111111111111111111"); err != nil || status != http.StatusUnauthorized {
			t.Fatalf("a refusal: %d %v", status, err)
		}
	}
	var wg sync.WaitGroup
	statuses := make(chan int, inFlight)
	for range inFlight {
		wg.Go(func() {
			status, err := call(hc, "/nopacs.v1.TenantService/ListTenants", `{"pageToken":"wait"}`, "")
			if err != nil {
				status = -1
			}
			statuses <- status
		})
	}
	waitInFlight(t, dir, inFlight)
	start := time.Now()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("a request in flight ended with %d\n%s", s, c.stderr.String())
		}
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("the server exited with %v\n%s", err, c.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not exit within the drain deadline")
	}
	took := time.Since(start)
	logText := c.stderr.String()
	if !strings.Contains(logText, `"audit_dropped":0`) || !strings.Contains(logText, `"audit_failed":0`) {
		t.Fatalf("audit entries were lost at stop:\n%s", logText)
	}
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(dir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	entries, _, err := m.ListAudit(context.Background(), master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != refusals {
		t.Fatalf("%d refusals were queued and %d entries written", refusals, len(entries))
	}
	t.Logf("%d requests in flight finished, %d audit entries queued and written, exit 0, %v after SIGTERM", inFlight, len(entries), took)
}

// waitInFlight waits until the server's metrics show n requests in flight
// besides the scrape itself.
func waitInFlight(t *testing.T, dir string, n int) {
	t.Helper()
	hc := socketClient(dir)
	defer hc.CloseIdleConnections()
	want := fmt.Sprintf("nopacs_requests_in_flight %d\n", n+1)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get("http://local/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(b), want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %d requests in flight", n)
}
