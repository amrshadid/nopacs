package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// ctxEnv is an environment that has no server, key or context, only a file of
// contexts in a private directory of the test's own, so what a command reaches
// is decided by the contexts alone.
func ctxEnv(t *testing.T) (map[string]string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contexts.json")
	return map[string]string{"NOPACS_CONFIG": path}, path
}

// addContext saves a context for the network rig under name, with the rig's
// own key in a private file, and returns the path of that key file.
func addContext(t *testing.T, env map[string]string, n *rig, name string, extra ...string) string {
	t.Helper()
	keyFile := fakeserver.PrivateFile(t, testKey+"\n")
	args := append([]string{"context", "add", name, "--server", n.env[client.EnvServer], "--key-file", keyFile, "--ca-file", n.env[client.EnvCAFile]}, extra...)
	if res := runWith(env, args...); res.code != 0 {
		t.Fatalf("%v: %+v", args, res)
	}
	return keyFile
}

// TestContextLifecycle guards the five commands: add checks the key and says
// who it is, list marks the current one, show, use and remove act on a name,
// each unknown or taken name has its own exit code, and the key itself is
// never in the file or in anything printed.
func TestContextLifecycle(t *testing.T) {
	n := newNetworkRig(t)
	env, file := ctxEnv(t)
	keyFile := fakeserver.PrivateFile(t, testKey+"\n")
	add := []string{"context", "add", "prod", "--server", n.env[client.EnvServer], "--key-file", keyFile, "--ca-file", n.env[client.EnvCAFile], "--tenant", "lab1"}
	res := runWith(env, add...)
	if res.code != 0 || !strings.Contains(res.stdout, "Context") || !strings.Contains(res.stdout, "prod") ||
		!strings.Contains(res.stdout, "key k00000000001") || !strings.Contains(res.stdout, "installation") || !strings.Contains(res.stdout, "admin") {
		t.Fatalf("add: %+v", res)
	}
	if got := n.fake.AuthorizationHeaders(); len(got) != 1 || got[0] != "Bearer "+testKey {
		t.Errorf("the key was checked with headers %q", got)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), testKey) || strings.Contains(string(data), "SSSSSSSS") {
		t.Errorf("the file holds the key: %s", data)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the file's mode is %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	for _, s := range []string{res.stdout, res.stderr} {
		if strings.Contains(s, testKey) {
			t.Errorf("the key was printed: %s", s)
		}
	}

	// A second one is not current; the first one was made current by add.
	other := addContext(t, env, n, "lab1", "--tenant", "lab1")
	if other == "" {
		t.Fatal("no key file")
	}
	res = runWith(env, "context", "list")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if res.code != 0 || len(lines) != 3 || !strings.HasPrefix(strings.TrimSpace(lines[2]), "*") || !strings.Contains(lines[2], "prod") || strings.Contains(lines[1], "*") {
		t.Errorf("list: %+v", res)
	}
	res = runWith(env, "context", "show", "prod")
	if res.code != 0 || !strings.Contains(res.stdout, keyFile) || !strings.Contains(res.stdout, "lab1") || !strings.Contains(res.stdout, "yes") {
		t.Errorf("show: %+v", res)
	}
	if res = runWith(env, "context", "use", "lab1"); res.code != 0 || res.stdout != "Current context: lab1\n" {
		t.Errorf("use: %+v", res)
	}
	res = runWith(env, "--json", "context", "list")
	var doc struct {
		SchemaVersion int
		Current       string
		Contexts      []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil || doc.SchemaVersion != 1 || doc.Current != "lab1" || len(doc.Contexts) != 2 {
		t.Errorf("list --json: %v %+v", err, res)
	}

	// Refusals and their codes.
	res = runWith(env, add...)
	if res.code != 5 || !strings.Contains(res.stderr, "prod") || !strings.Contains(res.stderr, file) || !strings.Contains(res.stderr, "--replace") {
		t.Errorf("a taken name: %+v", res)
	}
	if res = runWith(env, append(add, "--replace")...); res.code != 0 {
		t.Errorf("--replace: %+v", res)
	}
	for _, args := range [][]string{{"show", "nope"}, {"use", "nope"}, {"remove", "nope"}} {
		res = runWith(env, append([]string{"context"}, args...)...)
		if res.code != 4 || res.stdout != "" || !strings.Contains(res.stderr, "no context nope") || !strings.Contains(res.stderr, file) {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if res = runWith(env, "context", "add", "bad name", "--server", "https://x", "--key-file", keyFile); res.code != 2 {
		t.Errorf("a bad name: %+v", res)
	}
	if res = runWith(env, "context", "add", "x"); res.code != 2 || !strings.Contains(res.stderr, "--server and --key-file") {
		t.Errorf("no flags: %+v", res)
	}

	// Removing the current one leaves none.
	res = runWith(env, "--json", "context", "remove", "lab1")
	if res.code != 0 || !strings.Contains(res.stdout, `"removed": "lab1"`) || !strings.Contains(res.stdout, `"current": ""`) {
		t.Errorf("remove: %+v", res)
	}
	if res = runWith(env, "context", "show", "lab1"); res.code != 4 {
		t.Errorf("a removed context still shows: %+v", res)
	}
}

// TestContextAddChecksTheKey guards that a key the server refuses saves
// nothing: the exit code is the refusal's, the message names the context, and
// the file is not made.
func TestContextAddChecksTheKey(t *testing.T) {
	n := newNetworkRig(t)
	env, file := ctxEnv(t)
	wrong := fakeserver.PrivateFile(t, "nopacs_k99999999999_"+strings.Repeat("W", 44)+"\n")
	res := runWith(env, "context", "add", "prod", "--server", n.env[client.EnvServer], "--key-file", wrong, "--ca-file", n.env[client.EnvCAFile])
	if res.code != 3 || !strings.Contains(res.stderr, "context prod") || !strings.Contains(res.stderr, "nothing was saved") || !strings.Contains(res.stderr, "revoked") || strings.Contains(res.stderr, "WWWW") {
		t.Errorf("a refused key: %+v", res)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the file exists after a refused key: %v", err)
	}
}

// TestContextSelection guards the order --context, NOPACS_CONTEXT, the current
// context, and that flags and variables for the server win over a context,
// with the context's key never sent to another server.
func TestContextSelection(t *testing.T) {
	a := newNetworkRig(t)
	b := newNetworkRigAs(t, false)
	env, _ := ctxEnv(t)
	addContext(t, env, a, "a")
	addContext(t, env, b, "b")
	if res := runWith(env, "context", "use", "a"); res.code != 0 {
		t.Fatal(res)
	}
	seen := func(r *rig) int { return len(r.fake.AuthorizationHeaders()) }
	base := [2]int{seen(a), seen(b)}
	delta := func() [2]int { return [2]int{seen(a) - base[0], seen(b) - base[1]} }

	// The current context.
	if res := runWith(env, "tenant", "list"); res.code != 0 {
		t.Fatalf("current: %+v", res)
	}
	if d := delta(); d != [2]int{1, 0} {
		t.Errorf("the current context reached %v", d)
	}
	// NOPACS_CONTEXT beats the current one.
	envB := map[string]string{"NOPACS_CONFIG": env["NOPACS_CONFIG"], "NOPACS_CONTEXT": "b"}
	base = [2]int{seen(a), seen(b)}
	if res := runWith(envB, "tenant", "list"); res.code != 0 {
		t.Fatalf("NOPACS_CONTEXT: %+v", res)
	}
	if d := delta(); d != [2]int{0, 1} {
		t.Errorf("NOPACS_CONTEXT reached %v", d)
	}
	// --context beats NOPACS_CONTEXT.
	base = [2]int{seen(a), seen(b)}
	if res := runWith(envB, "--context", "a", "tenant", "list"); res.code != 0 {
		t.Fatalf("--context: %+v", res)
	}
	if d := delta(); d != [2]int{1, 0} {
		t.Errorf("--context reached %v", d)
	}
	// A context the person named does not mix with a server named another
	// way: a usage error naming both, and nothing is sent anywhere.
	local := newRig(t)
	cfg := env["NOPACS_CONFIG"]
	mixes := []struct {
		name string
		env  map[string]string
		args []string
		both string
	}{
		{"--context and --server", map[string]string{"NOPACS_CONFIG": cfg}, []string{"--context", "a", "--server", b.env[client.EnvServer]}, "--context cannot be used with --server"},
		{"--context and --data-dir", map[string]string{"NOPACS_CONFIG": cfg}, []string{"--context", "a", "--data-dir", local.env[client.EnvDataDir]}, "--context cannot be used with --data-dir"},
		{"--context and NOPACS_SERVER", map[string]string{"NOPACS_CONFIG": cfg, client.EnvServer: b.env[client.EnvServer]}, []string{"--context", "a"}, "--context cannot be used with NOPACS_SERVER"},
		{"--context and NOPACS_DATA_DIR", map[string]string{"NOPACS_CONFIG": cfg, client.EnvDataDir: local.env[client.EnvDataDir]}, []string{"--context", "a"}, "--context cannot be used with NOPACS_DATA_DIR"},
		{"NOPACS_CONTEXT and NOPACS_SERVER", map[string]string{"NOPACS_CONFIG": cfg, "NOPACS_CONTEXT": "a", client.EnvServer: b.env[client.EnvServer]}, nil, "NOPACS_CONTEXT cannot be used with NOPACS_SERVER"},
		{"NOPACS_CONTEXT and NOPACS_DATA_DIR", map[string]string{"NOPACS_CONFIG": cfg, "NOPACS_CONTEXT": "a", client.EnvDataDir: local.env[client.EnvDataDir]}, nil, "NOPACS_CONTEXT cannot be used with NOPACS_DATA_DIR"},
	}
	for _, m := range mixes {
		base = [2]int{seen(a), seen(b)}
		res := runWith(m.env, append(m.args, "tenant", "list")...)
		if res.code != 2 || !strings.Contains(res.stderr, m.both) || res.stdout != "" || delta() != [2]int{0, 0} || local.fake.Requests() != 0 {
			t.Errorf("%s: %+v %v", m.name, res, delta())
		}
	}
	// The file's current context is not a named one: a server named another
	// way simply wins, and the local socket is reached.
	base = [2]int{seen(a), seen(b)}
	envData := map[string]string{"NOPACS_CONFIG": cfg, client.EnvDataDir: local.env[client.EnvDataDir]}
	if res := runWith(envData, "tenant", "list"); res.code != 0 || delta() != [2]int{0, 0} || local.fake.Requests() == 0 {
		t.Errorf("NOPACS_DATA_DIR with only a current context: %+v %v", res, delta())
	}
	// An exported key is not sent to a context's server: refused, nothing
	// sent, for a named context and for the current one alike.
	for _, args := range [][]string{{"--context", "a"}, {}} {
		base = [2]int{seen(a), seen(b)}
		res := runWith(map[string]string{"NOPACS_CONFIG": cfg, client.EnvKey: testKey}, append(args, "tenant", "list")...)
		if res.code != 2 || !strings.Contains(res.stderr, "NOPACS_KEY is set; it would be sent to the server of context a") || strings.Contains(res.stderr, testKey) || delta() != [2]int{0, 0} {
			t.Errorf("NOPACS_KEY with %v: %+v %v", args, res, delta())
		}
	}
	// A flag key beats the context's key file for the context's own server,
	// and it is the key the server receives, even with NOPACS_KEY set.
	other := fakeserver.PrivateFile(t, "nopacs_k99999999999_"+strings.Repeat("W", 44)+"\n")
	res := runWith(map[string]string{"NOPACS_CONFIG": cfg, client.EnvKey: testKey}, "--context", "a", "--key-file", other, "tenant", "list")
	hs := a.fake.AuthorizationHeaders()
	if res.code != 3 || hs[len(hs)-1] != "Bearer nopacs_k99999999999_"+strings.Repeat("W", 44) {
		t.Errorf("--key-file over a context: %+v, last header %q", res, hs[len(hs)-1])
	}
	// A name that is not in the file.
	res = runWith(env, "--context", "nope", "tenant", "list")
	if res.code != 4 || !strings.Contains(res.stderr, "no context nope") || !strings.Contains(res.stderr, env["NOPACS_CONFIG"]) {
		t.Errorf("an unknown --context: %+v", res)
	}
	// With nothing, the usage error as before.
	res = runWith(map[string]string{"NOPACS_CONFIG": filepath.Join(t.TempDir(), "none.json")}, "tenant", "list")
	if res.code != 2 || !strings.Contains(res.stderr, "do not know where the server is") {
		t.Errorf("no context, no flags: %+v", res)
	}
}

// TestContextFileMustBePrivate guards the refusal of a file or a directory
// that others can reach: the message names the file, and no command that uses
// the file runs on.
func TestContextFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are checked on Unix; Windows checks the access list")
	}
	n := newNetworkRig(t)
	env, file := ctxEnv(t)
	addContext(t, env, n, "a")
	if err := os.Chmod(file, 0o644); err != nil { //nolint:gosec // a test of the refusal of a readable file
		t.Fatal(err)
	}
	for _, args := range [][]string{{"context", "list"}, {"--context", "a", "tenant", "list"}} {
		res := runWith(env, args...)
		if res.code != 2 || !strings.Contains(res.stderr, file) || !strings.Contains(res.stderr, "chmod 600") || res.stdout != "" {
			t.Errorf("%v with a world-readable file: %+v", args, res)
		}
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(file)
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // a test of the refusal of an open directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory the test made, put back
	if res := runWith(env, "context", "list"); res.code != 2 || !strings.Contains(res.stderr, "directory") || !strings.Contains(res.stderr, file) {
		t.Errorf("a directory others can write: %+v", res)
	}
	// A refused file does not stop a command that names its own server.
	if res := runWith(map[string]string{"NOPACS_CONFIG": file, client.EnvServer: n.env[client.EnvServer], client.EnvKey: testKey, client.EnvCAFile: n.env[client.EnvCAFile]}, "tenant", "list"); res.code != 0 {
		t.Errorf("a refused file broke a command with --server: %+v", res)
	}
}

// TestContextFileRefusesASecret guards that a file with a key in it is
// refused and the key is not repeated in the message.
func TestContextFileRefusesASecret(t *testing.T) {
	env, file := ctxEnv(t)
	body := `{"version":1,"contexts":{"a":{"server":"https://x","keyFile":"/k","key":"` + testKey + `"}}}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runWith(env, "context", "list")
	if res.code != 2 || !strings.Contains(res.stderr, "never a key") || strings.Contains(res.stderr, testKey) || !strings.Contains(res.stderr, file) {
		t.Errorf("a file with a key: %+v", res)
	}
}

// TestContextDefaultTenant guards the default tenant: it fills --tenant
// where the command makes it optional (setting get, set, unset and list, audit
// list), says so on standard error, an explicit --tenant (even an empty one)
// wins, and a command whose tenant is an argument is unchanged.
func TestContextDefaultTenant(t *testing.T) {
	n := newNetworkRig(t)
	env, _ := ctxEnv(t)
	addContext(t, env, n, "t", "--tenant", "lab1")
	n.run("tenant", "create", "lab1")
	say := "nopacs: using tenant lab1 from context t\n"
	// set: the value lands on the tenant, not on the installation.
	res := runWith(env, "setting", "set", "test.retention", "90d")
	if res.code != 0 || !strings.Contains(res.stderr, say) {
		t.Fatalf("set: %+v", res)
	}
	if got := n.run("setting", "get", "test.retention", "--tenant", "lab1"); !strings.Contains(got.stdout, "90d") {
		t.Errorf("the value is not on the tenant: %+v", got)
	}
	if got := n.run("setting", "get", "test.retention"); strings.Contains(got.stdout, "90d") {
		t.Errorf("the value reached the installation: %+v", got)
	}
	// get reads the tenant's value.
	if res = runWith(env, "setting", "get", "test.retention"); res.code != 0 || !strings.Contains(res.stdout, "90d") || !strings.Contains(res.stderr, say) {
		t.Errorf("get: %+v", res)
	}
	// list shows the tenant's overrides only; --tenant "" the installation's.
	count := func(args ...string) (int, string) {
		t.Helper()
		res := runWith(env, append(args, "--json")...)
		if res.code != 0 {
			t.Fatalf("%v: %+v", args, res)
		}
		var doc struct{ Settings []struct{ Name string } }
		if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
			t.Fatal(err)
		}
		return len(doc.Settings), res.stderr
	}
	if got, note := count("setting", "list"); got != 1 || note != say {
		t.Errorf("list with the default tenant: %d settings, note %q", got, note)
	}
	if got, note := count("setting", "list", "--tenant", ""); got <= 1 || note != "" {
		t.Errorf("list with --tenant \"\": %d settings, note %q", got, note)
	}
	// unset removes the tenant's value.
	if res = runWith(env, "setting", "unset", "test.retention"); res.code != 0 || !strings.Contains(res.stderr, say) {
		t.Errorf("unset: %+v", res)
	}
	if got := n.run("setting", "get", "test.retention", "--tenant", "lab1"); strings.Contains(got.stdout, "90d") {
		t.Errorf("unset left the value: %+v", got)
	}
	// audit list shows the tenant's entries, not the installation's.
	n.fake.AddAudit(&v1.AuditEntry{Id: "1", Time: timestamppb.New(fixedNow), KeyId: "k1", Method: "/nopacs.v1.TenantService/GetTenant", Tenant: "lab1", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-tenant"})
	n.fake.AddAudit(&v1.AuditEntry{Id: "2", Time: timestamppb.New(fixedNow), KeyId: "k1", Method: "/nopacs.v1.TenantService/ListTenants", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-install"})
	res = runWith(env, "audit", "list")
	if res.code != 0 || !strings.Contains(res.stdout, "req-tenant") || strings.Contains(res.stdout, "req-install") || !strings.Contains(res.stderr, say) {
		t.Errorf("audit list: %+v", res)
	}
	res = runWith(env, "audit", "list", "--tenant", "")
	if res.code != 0 || strings.Contains(res.stdout, "req-tenant") || !strings.Contains(res.stdout, "req-install") {
		t.Errorf("audit list --tenant \"\": %+v", res)
	}
	// A tenant argument is not filled and not needed: the grammar is as it was.
	if res := runWith(env, "storage", "list"); res.code != 2 || !strings.Contains(res.stderr, "wrong number of arguments") {
		t.Errorf("storage list without a tenant: %+v", res)
	}
}

// TestContextDirectoryRules guards the directory of the file: the default
// place is refused when others can read it, a NOPACS_CONFIG directory when
// others can write it, each with a message that names the file and how to fix
// it, and a private directory passes.
func TestContextDirectoryRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directories are checked on Unix")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "nopacs")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test of the refusal of an open directory
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // MkdirAll applies the umask; this is the mode under test
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home}
	res := runWith(env, "context", "list")
	if res.code != 2 || !strings.Contains(res.stderr, "chmod 700 "+dir) || !strings.Contains(res.stderr, filepath.Join(dir, "contexts.json")) {
		t.Errorf("a default directory others can read: %+v", res)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its execute bit
		t.Fatal(err)
	}
	if res = runWith(env, "context", "list"); res.code != 0 {
		t.Errorf("a private default directory: %+v", res)
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o777); err != nil { //nolint:gosec // a test of the refusal of a shared directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) }) //nolint:gosec // put back
	res = runWith(map[string]string{"NOPACS_CONFIG": filepath.Join(shared, "c.json")}, "context", "list")
	if res.code != 2 || !strings.Contains(res.stderr, "private directory of your own") || strings.Contains(res.stderr, "chmod 700") {
		t.Errorf("a shared NOPACS_CONFIG directory: %+v", res)
	}
}

// TestContextFileHandEdits guards what a hand-edited file cannot make the
// program do: print or send a secret. Each file is refused, naming the file and
// the field, and no message holds the value.
func TestContextFileHandEdits(t *testing.T) {
	key := testKey
	ok := func(over string) string {
		m := map[string]string{"server": "https://h:8443", "keyFile": "/k", "tenant": "lab1"}
		var parts []string
		for k, v := range m {
			if k == over {
				continue
			}
			parts = append(parts, `"`+k+`":"`+v+`"`)
		}
		return strings.Join(parts, ",")
	}
	for name, c := range map[string]string{
		"user information":  `{"server":"https://user:` + key + `@h:8443","keyFile":"/k"}`,
		"key as the path":   `{"server":"https://h:8443","keyFile":"` + key + `"}`,
		"key as the tenant": `{"server":"https://h:8443","keyFile":"/k","tenant":"` + key + `"}`,
		"relative path":     `{"server":"https://h:8443","keyFile":"k"}`,
		"relative CA":       `{"server":"https://h:8443","keyFile":"/k","caFile":"ca.pem"}`,
		"bad tenant":        `{"server":"https://h:8443","keyFile":"/k","tenant":"a/b"}`,
		"certificate alone": `{"server":"https://h:8443","keyFile":"/k","tlsClientCert":"/c"}`,
		"plain http":        `{"server":"http://example.org","keyFile":"/k"}`,
		"unknown field":     `{"server":"https://h:8443","keyFile":"/k","` + key + `":"x"}`,
	} {
		env, file := ctxEnv(t)
		if err := os.WriteFile(file, []byte(`{"version":1,"contexts":{"a":`+c+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// "context list" reads only the file, so what refuses is the file's own check.
		res := runWith(env, "context", "list")
		if res.code != 2 || !strings.Contains(res.stderr, file) || strings.Contains(res.stderr, key) || strings.Contains(res.stderr, "SSSSSSSS") {
			t.Errorf("%s: %+v", name, res)
		}
		// Refused by the file's own check, not later when the path is used.
		if strings.HasPrefix(name, "relative") && !strings.Contains(res.stderr, "absolute path") {
			t.Errorf("%s: not refused as a relative path: %+v", name, res)
		}
		if strings.HasPrefix(name, "key as") && !strings.Contains(res.stderr, "looks like a key") {
			t.Errorf("%s: not refused as a key: %+v", name, res)
		}
		if name == "user information" && !strings.Contains(res.stderr, "user name or password") {
			t.Errorf("%s: not refused for its user information: %+v", name, res)
		}
	}
	_ = ok
	// A key used as a context's name.
	env, file := ctxEnv(t)
	if err := os.WriteFile(file, []byte(`{"version":1,"contexts":{"`+testKey+`":{`+ok("")+`}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runWith(env, "context", "list"); res.code != 2 || strings.Contains(res.stderr, "SSSSSSSS") {
		t.Errorf("a key as a name: %+v", res)
	}
}
