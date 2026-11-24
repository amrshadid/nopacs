package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
)

// childArgsEnv carries the arguments of a child run of this test binary as
// one of the two programs: TestMain runs the server's main for the commands
// that belong to nopacs-server, and the command line's for every other, so
// each command of the end-to-end test is a process of the real program's code
// through the real server and handlers, as built binaries would be.
const childArgsEnv = "NOPACS_E2E_ARGS"

// TestMain runs the tests, or, in a child, the program.
func TestMain(m *testing.M) {
	if raw := os.Getenv(childArgsEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			fmt.Fprintln(os.Stderr, "e2e child:", err)
			os.Exit(1)
		}
		if serverCommand(args) {
			os.Args = append([]string{"nopacs-server"}, args...)
			main()
			return
		}
		// The command line, wired as cmd/nopacs wires it.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		code := cli.Run(ctx, args, cli.Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// serverCommand says whether the arguments name a command of nopacs-server.
func serverCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "init", "serve", "backup":
		return true
	case "tenant":
		return len(args) > 1 && args[1] == "import"
	}
	return false
}

// result is what one command printed and how it exited.
type result struct {
	stdout, stderr string
	code           int
}

// requestID matches the request id at the end of a refusal's line.
var requestID = regexp.MustCompile(` \(request id [a-z2-7]+\)`)

// session is one installation and the environments that reach its server.
type session struct {
	t      testing.TB
	dir    string
	local  []string // reaches the local socket
	remote []string // reaches the TLS listener with a key
	port   int
	cert   string
	key    string
}

// command returns a process of the program with args and the extra
// environment.
func command(env []string, args ...string) *exec.Cmd {
	raw, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: this test's own binary.
	cmd.Env = append(append(cleanEnv(), childArgsEnv+"="+string(raw)), env...)
	return cmd
}

// cleanEnv is the test's environment without the program's own variables,
// so a developer's NOPACS_* settings cannot change what a command reaches.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NOPACS_") {
			out = append(out, kv)
		}
	}
	return append(out, "NO_COLOR=1")
}

// run runs one command to its end and returns what it printed.
func (s *session) run(env []string, args ...string) result {
	s.t.Helper()
	cmd := command(env, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		s.t.Fatalf("nopacs %s: %v", strings.Join(args, " "), err)
	}
	s.t.Logf("$ nopacs %s  [exit %d]\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// ok runs a command that must succeed with nothing on standard error, and
// returns its standard output.
func (s *session) ok(env []string, args ...string) string {
	s.t.Helper()
	r := s.run(env, args...)
	if r.code != 0 || r.stderr != "" {
		s.t.Fatalf("nopacs %s: exit %d, standard error %q", strings.Join(args, " "), r.code, r.stderr)
	}
	return r.stdout
}

// want fails the test unless text holds each part.
func (s *session) want(what, text string, parts ...string) {
	s.t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			s.t.Errorf("%s does not hold %q:\n%s", what, p, text)
		}
	}
}

// server is a running serve process.
type server struct {
	cmd    *exec.Cmd
	stderr *lockedBuffer
	done   chan error
}

// lockedBuffer is a buffer a process's pipe writes to while the test reads
// it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p.
func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns what was written.
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// serve starts nopacs-server serve on the session's data directory, with the
// local socket and TLS on loopback, and waits until it answers.
func (s *session) serve() *server {
	s.t.Helper()
	cmd := command(nil, "serve", "--data-dir", s.dir, "--listen", fmt.Sprintf("127.0.0.1:%d", s.port),
		"--tls-cert", s.cert, "--tls-key", s.key, "--log-format", "text")
	srv := &server{cmd: cmd, stderr: &lockedBuffer{}, done: make(chan error, 1)}
	cmd.Stderr = srv.stderr
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		srv.done <- cmd.Wait()
		close(exited)
	}()
	s.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		probe := command(s.local, "whoami")
		if probe.Run() == nil {
			return srv
		}
		select {
		case err := <-srv.done:
			s.t.Fatalf("serve exited: %v\n%s", err, srv.stderr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.t.Fatalf("serve did not answer:\n%s", srv.stderr.String())
	return nil
}

// stop sends serve a stop signal and waits for it to exit 0.
func (s *session) stop(srv *server) {
	s.t.Helper()
	if err := srv.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.t.Fatal(err)
	}
	select {
	case err := <-srv.done:
		if err != nil {
			s.t.Fatalf("serve exited with %v on SIGTERM:\n%s", err, srv.stderr.String())
		}
	case <-time.After(40 * time.Second):
		s.t.Fatalf("serve did not stop:\n%s", srv.stderr.String())
	}
}

// newSession makes a short data directory (a socket's path is short on
// macOS), a certificate for 127.0.0.1 and a free port.
func newSession(t testing.TB) *session {
	t.Helper()
	dir, err := os.MkdirTemp("", "core")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	work := t.TempDir()
	cert, key := selfSigned(t, work)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	data := filepath.Join(dir, "d")
	return &session{
		t: t, dir: data, port: port, cert: cert, key: key,
		local: []string{"NOPACS_DATA_DIR=" + data},
	}
}

// selfSigned writes a certificate valid for 127.0.0.1 and its key, and
// returns their paths.
func selfSigned(t testing.TB, dir string) (certPath, keyPath string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nopacs e2e"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// listDoc parses a list's --json document, fails unless it carries
// schemaVersion 1, and returns its items under field.
func (s *session) listDoc(out, field string) []any {
	s.t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		s.t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc["schemaVersion"] != float64(1) {
		s.t.Errorf("no schemaVersion 1: %v", doc)
	}
	items, _ := doc[field].([]any)
	return items
}

// auditIDs returns the ids of the entries the installation's log holds,
// read with the command line.
func (s *session) auditIDs() map[string]bool {
	s.t.Helper()
	ids := map[string]bool{}
	for _, e := range s.listDoc(s.ok(s.local, "audit", "list", "--all", "--json"), "entries") {
		if m, ok := e.(map[string]any); ok {
			ids[fmt.Sprint(m["id"])] = true
		}
	}
	return ids
}

// e2eEnv is the variable that runs TestRealServerEndToEnd: it starts some
// fifty processes, which under the race detector take half a minute, so it
// runs in the job of the server's outside checks (make check-e2e) rather
// than in every go test.
const e2eEnv = "NOPACS_E2E"

// TestRealServerEndToEnd is the real program end to end: init, serve with
// the local socket and TLS on loopback, then the real commands, each its own
// process, over the socket as the local operator and over TLS with a key
// made in the session; a second serve refused; a stop by signal that writes
// every audit entry held in memory; and a restart that serves the same
// tenants, keys and audit log.
func TestRealServerEndToEnd(t *testing.T) {
	if os.Getenv(e2eEnv) == "" {
		t.Skip("starts real processes; set " + e2eEnv + "=1, as make check-e2e does")
	}
	s := newSession(t)
	out := s.ok(nil, "init", "--data-dir", s.dir)
	s.want("init", out, "Made a new installation", "The key file is")
	srv := s.serve()

	// Over the local socket, as the local operator.
	s.want("whoami", s.ok(s.local, "whoami"), "Caller: local operator (user ", "Access: admin on the installation and on every tenant")
	s.want("tenant create", s.ok(s.local, "tenant", "create", "lab1"), "Name:     lab1", "Database: sqlite", "State:    ready", "Since:")
	s.want("tenant list", s.ok(s.local, "tenant", "list"), "lab1  sqlite    ready")
	s.want("tenant show", s.ok(s.local, "tenant", "show", "lab1"), "State:    ready", "Since:", "Created:")
	r := s.run(s.local, "key", "create", "--holder", "e2e reader")
	if r.code != 0 {
		t.Fatalf("key create: %+v", r)
	}
	secret := strings.TrimSpace(r.stdout)
	parts := strings.Split(secret, "_")
	if len(parts) != 3 || parts[0] != "nopacs" {
		t.Fatalf("key create printed %q, not a key", secret)
	}
	keyID := parts[1]
	s.want("key create's notice", r.stderr, "key "+keyID+" (e2e reader)", "only time the key is shown")
	s.want("grant set", s.ok(s.local, "grant", "set", keyID, "--role", "reader", "--tenant", "lab1"),
		"key "+keyID+" now holds reader on tenant lab1 (before: no grant)")
	s.want("setting set", s.ok(s.local, "setting", "set", "key.default-lifetime", "30d"), "Value: 30d", "From:  installation")
	s.want("setting get", s.ok(s.local, "setting", "get", "key.default-lifetime"), "Value: 30d", "From:  installation")
	s.want("setting get on a tenant", s.ok(s.local, "setting", "get", "key.default-lifetime", "--tenant", "lab1"), "Value: 30d", "From:  installation")
	s.want("setting unset", s.ok(s.local, "setting", "unset", "key.default-lifetime"), "now applies: 90d (built-in default)")
	// The server's own installation id, under the reserved prefix, does not
	// exist for the API: the answer of any unknown name.
	for _, args := range [][]string{
		{"setting", "get", "system.installation-id"},
		{"setting", "set", "system.installation-id", "x"},
		{"setting", "unset", "system.installation-id"},
	} {
		if r := s.run(s.local, args...); r.code != 4 || !strings.Contains(r.stderr, "no setting has this name") {
			t.Errorf("%v: %+v, want exit 4 and the answer of an unknown name", args, r)
		}
	}
	if out := s.ok(s.local, "setting", "list"); strings.Contains(out, "system.") {
		t.Errorf("setting list shows a reserved setting:\n%s", out)
	}
	s.ok(s.local, "tenant", "create", "lab2")
	audit := s.ok(s.local, "audit", "list", "--all")
	s.want("audit list", audit,
		"TenantService/CreateTenant", "AccessService/CreateKey", "AccessService/SetGrant",
		"SettingsService/SetSetting", "SettingsService/UnsetSetting", " done ")

	// Over TLS with the key made above: reader on lab1 only.
	s.remote = []string{
		fmt.Sprintf("NOPACS_SERVER=https://127.0.0.1:%d", s.port),
		"NOPACS_CA_FILE=" + s.cert,
		"NOPACS_KEY=" + secret,
	}
	s.want("whoami over TLS", s.ok(s.remote, "whoami"), "Caller:  key "+keyID+" (e2e reader)", "tenant lab1  reader")
	list := s.ok(s.remote, "tenant", "list")
	if !strings.Contains(list, "lab1") || strings.Contains(list, "lab2") {
		t.Errorf("tenant list over TLS shows other than lab1:\n%s", list)
	}
	if got := s.listDoc(s.ok(s.remote, "tenant", "list", "--json"), "tenants"); len(got) != 1 {
		t.Errorf("tenant list --json over TLS: %v", got)
	}
	noGrant := s.run(s.remote, "tenant", "show", "lab2")
	missing := s.run(s.remote, "tenant", "show", "nolab")
	if noGrant.code != 3 || missing.code != 3 || noGrant.stdout != "" || missing.stdout != "" {
		t.Errorf("a tenant with no grant and one that does not exist: %+v / %+v, want exit 3 for both", noGrant, missing)
	}
	if a, b := requestID.ReplaceAllString(noGrant.stderr, ""), requestID.ReplaceAllString(missing.stderr, ""); a != b {
		t.Errorf("the two refusals differ:\n%q\n%q", a, b)
	}
	if r := s.run(s.remote, "key", "list"); r.code != 3 || !strings.Contains(r.stderr, "the key may not do this") {
		t.Errorf("key list by a reader: %+v", r)
	}

	// --json on each list.
	s.listDoc(s.ok(s.local, "key", "list", "--json"), "keys")
	s.listDoc(s.ok(s.local, "grant", "list", "--key-id", keyID, "--json"), "grants")
	s.listDoc(s.ok(s.local, "setting", "list", "--json"), "settings")
	s.listDoc(s.ok(s.local, "audit", "list", "--json"), "entries")

	// A revoked key is refused on its next request.
	s.want("key revoke", s.ok(s.local, "key", "revoke", keyID), "key "+keyID+" (e2e reader) revoked at")
	if r := s.run(s.remote, "whoami"); r.code != 3 || !strings.Contains(r.stderr, "the request carries no valid key") {
		t.Errorf("whoami with the revoked key: %+v", r)
	}

	// The removal leaves the tenant's file and says where.
	removed := s.ok(s.local, "tenant", "remove", "lab1", "--yes")
	file := filepath.Join(s.dir, "tenants", "lab1.db")
	s.want("tenant remove", removed, "lab1 unregistered; no data was deleted", "Data left at: "+file)
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the removed tenant's file: %v", err)
	}

	// A second serve on the same directory is refused while the first runs.
	second := s.run(nil, "serve", "--data-dir", s.dir)
	if second.code != 1 || !strings.Contains(second.stderr, "another nopacs-server serve is running on the data directory") {
		t.Errorf("a second serve: %+v", second)
	}

	if runtime.GOOS == "windows" {
		// The stop by signal and the restart need a signal the process can
		// catch; Windows has none to send it.
		return
	}
	// Stop by signal: every entry held in memory is written. The entries of
	// reads and refusals are written in batches, at most a fifth of a second
	// apart, so after a pause the log holds all but the listing's own entry,
	// which the stop must write.
	time.Sleep(time.Second)
	before := len(s.auditIDs())
	s.stop(srv)
	ctx := context.Background()
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(s.dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]bool{}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := m.ListAudit(ctx, master.AuditFilter{}, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			written[e.ID] = true
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	_ = m.Close()
	if len(written) != before+1 {
		t.Errorf("after the stop the master holds %d audit entries; the last listing saw %d, and its own makes %d", len(written), before, before+1)
	}

	// A restart serves the same tenants, keys and log.
	s.serve()
	if list := s.ok(s.local, "tenant", "list"); !strings.Contains(list, "lab2") || strings.Contains(list, "lab1") {
		t.Errorf("tenant list after the restart:\n%s", list)
	}
	s.want("key show after the restart", s.ok(s.local, "key", "show", keyID), keyID, "revoked")
	// Every entry the master held is served after the restart (the restart
	// adds its own, such as the probes that waited for it to answer).
	after := s.auditIDs()
	for id := range written {
		if !after[id] {
			t.Errorf("the entry %s written before the stop is not listed after the restart", id)
		}
	}
	// The failed and refused requests of the session are in the log too.
	s.want("failed entries after the restart", s.ok(s.local, "audit", "list", "--all", "--outcome", "failed"), "SettingsService/GetSetting")
	s.want("refused entries after the restart", s.ok(s.local, "audit", "list", "--all", "--outcome", "refused"), "TenantService/GetTenant", "AccessService/ListKeys")
}
