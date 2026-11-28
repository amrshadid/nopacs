package cli_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
	"github.com/amrshadid/nopacs/internal/version"
)

// TestWhoamiWarnsOnVersionMismatch guards the warning about the server's
// build: one line on standard error when the server's version differs from the
// program's in the way version.Differ defines, none when they are the same,
// and the command succeeds either way with the server's version in its answer.
func TestWhoamiWarnsOnVersionMismatch(t *testing.T) {
	n := newNetworkRig(t)

	// The same build as the program: no warning.
	res := n.run("whoami")
	if res.code != 0 || res.stderr != "" || !strings.Contains(res.stdout, "Server:") || !strings.Contains(res.stdout, version.Get().Version) {
		t.Errorf("same version: %+v", res)
	}

	// A release the test binary (a development build) is not: a warning, and
	// the answer still shown.
	n.fake.Version = "v9.9.9"
	res = n.run("whoami")
	if res.code != 0 || !strings.Contains(res.stdout, "v9.9.9") {
		t.Errorf("differing version, whoami did not succeed with the version: %+v", res)
	}
	if lines := strings.Split(strings.TrimSpace(res.stderr), "\n"); len(lines) != 1 ||
		!strings.HasPrefix(lines[0], "nopacs: warning: the server is v9.9.9 and this program is ") {
		t.Errorf("differing version, standard error %q", res.stderr)
	}
	// The warning goes to standard error and leaves the document alone.
	js := n.run("whoami", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(js.stdout), &doc); err != nil || js.code != 0 {
		t.Fatalf("whoami --json: %v: %+v", err, js)
	}
	if doc["serverVersion"] != "v9.9.9" {
		t.Errorf("--json serverVersion = %v", doc["serverVersion"])
	}
}

// TestContextAddWarnsOnVersionMismatch guards the same warning where a context
// is saved, the call that checks its key: the context is saved regardless and
// the warning names the context.
func TestContextAddWarnsOnVersionMismatch(t *testing.T) {
	n := newNetworkRig(t)
	n.fake.Version = "v9.9.9"
	env, _ := ctxEnv(t)
	keyFile := fakeserver.PrivateFile(t, testKey+"\n")
	res := runWith(env, "context", "add", "prod", "--server", n.env[client.EnvServer], "--key-file", keyFile, "--ca-file", n.env[client.EnvCAFile])
	if res.code != 0 || !strings.Contains(res.stderr, "warning: the server (prod) is v9.9.9") {
		t.Errorf("context add: %+v", res)
	}
	if got := runWith(env, "context", "show", "prod"); got.code != 0 {
		t.Errorf("the context was not saved: %+v", got)
	}
}

// checkDocs runs "context list --check --json" and returns the contexts by
// name with the exit code.
func checkDocs(t *testing.T, env map[string]string, args ...string) (map[string]map[string]any, result, map[string]any) {
	t.Helper()
	res := runWith(env, append([]string{"context", "list", "--check", "--json"}, args...)...)
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("the output is not a document: %v: %+v", err, res)
	}
	byName := map[string]map[string]any{}
	for _, c := range doc["contexts"].([]any) {
		m := c.(map[string]any)
		byName[m["name"].(string)] = m
	}
	return byName, res, doc
}

// TestContextListCheck guards "context list --check": with one reachable
// server and one address nothing listens on, both are shown with the fields
// the check gives (the reason for the one, the key, its holder, expiry,
// grants and the server's version for the other), the dead one does not stop
// the good one, the exit code is 1 while any context does not answer and 0
// when all do, and no key is printed.
func TestContextListCheck(t *testing.T) {
	n := newNetworkRig(t)
	n.fake.Version = "v9.9.9"
	env, file := ctxEnv(t)
	addContext(t, env, n, "good", "--tenant", "lab1")
	addContext(t, env, n, "dead")
	// Point "dead" at a loopback port nothing listens on.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	f["contexts"].(map[string]any)["dead"].(map[string]any)["server"] = "http://127.0.0.1:1"
	raw, err = json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The table.
	res := runWith(env, "context", "list", "--check")
	if res.code != 1 {
		t.Errorf("exit code %d, want 1 while a context does not answer: %+v", res.code, res)
	}
	if !strings.Contains(res.stderr, "context list --check: 1 of 2 contexts did not answer") {
		t.Errorf("standard error %q", res.stderr)
	}
	var goodRow, deadRow string
	for _, l := range strings.Split(res.stdout, "\n") {
		switch {
		case strings.Contains(l, "good"):
			goodRow = l
		case strings.Contains(l, "dead"):
			deadRow = l
		}
	}
	for _, want := range []string{"reachable", "k00000000001", "never", "installation admin", "v9.9.9"} {
		if !strings.Contains(goodRow, want) {
			t.Errorf("the good row lacks %q: %q", want, goodRow)
		}
	}
	if !strings.Contains(deadRow, "unreachable:") {
		t.Errorf("the dead row: %q", deadRow)
	}
	if strings.Contains(res.stdout+res.stderr, testKey) {
		t.Errorf("the key was printed: %+v", res)
	}

	// The document.
	byName, res, doc := checkDocs(t, env)
	if res.code != 1 || doc["reachable"] != float64(1) || doc["failed"] != float64(1) {
		t.Errorf("document: %+v", res)
	}
	good, dead := byName["good"], byName["dead"]
	if good["reachable"] != true || good["keyId"] != "k00000000001" || good["serverVersion"] != "v9.9.9" || good["versionDiffers"] != true {
		t.Errorf("good: %v", good)
	}
	grants, _ := good["grants"].([]any)
	if len(grants) != 1 || grants[0].(map[string]any)["scope"] != "installation" || grants[0].(map[string]any)["role"] != "admin" {
		t.Errorf("good grants: %v", good["grants"])
	}
	if dead["reachable"] != false || dead["error"] == "" || dead["error"] == nil || dead["keyId"] != nil {
		t.Errorf("dead: %v", dead)
	}

	// With the dead context gone every one answers: exit 0.
	if res := runWith(env, "context", "remove", "dead"); res.code != 0 {
		t.Fatalf("remove: %+v", res)
	}
	if res := runWith(env, "context", "list", "--check"); res.code != 0 || strings.Contains(res.stdout, "unreachable") {
		t.Errorf("all reachable: %+v", res)
	}
}
