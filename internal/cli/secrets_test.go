package cli_test

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// withoutKey returns a copy of an environment without the named variables.
func without(env map[string]string, names ...string) map[string]string {
	out := maps.Clone(env)
	for _, n := range names {
		delete(out, n)
	}
	return out
}

// TestKeyTravelsOnlyInTheAuthorizationHeader guards the rule that the key
// goes to the server in one place and nowhere else. It runs every command, in
// both forms, with a key set (once from the environment, once from a file,
// then a wrong one), plus commands that fail, over TLS, and asserts that the
// key's secret is in no request URL, no header but Authorization, no request
// body, no standard output, no standard error and no error text; that the
// Authorization header, where it is, is exactly "Bearer <key>"; and that every
// request the server saw carried it.
func TestKeyTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	const (
		keyID  = "k00000000001"
		secret = "QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"
		key    = "nopacs_" + keyID + "_" + secret
	)
	fake := fakeserver.New()
	fake.Settings = testSettings()
	fake.Now = func() time.Time { return fixedNow }
	fake.RequireKey = key
	fake.CallerKeyID = keyID
	addr := fake.ServeTLS(t)
	ca := writeCA(t, fake)
	fromEnv := map[string]string{client.EnvServer: addr, client.EnvCAFile: ca, client.EnvKey: key, "LAB2_DB": "postgres://u:p@h/d"} //nolint:gosec // a made-up address for a test
	fromFile := map[string]string{client.EnvServer: addr, client.EnvCAFile: ca, "LAB2_DB": "postgres://u:p@h/d"}                    //nolint:gosec // a made-up address for a test
	keyFile := fakeserver.PrivateFile(t, key+"\n")

	steps := [][]string{
		{"version"}, {"whoami"},
		{"tenant", "create", "lab1"}, {"tenant", "create", "lab2", "--db-env", "LAB2_DB"},
		{"tenant", "list"}, {"tenant", "show", "lab1"},
		{"key", "create", "--holder", "ci"}, {"key", "list"}, {"key", "show", keyID},
		{"grant", "set", keyID, "--role", "reader", "--tenant", "lab1"},
		{"grant", "list", "--key-id", keyID}, {"grant", "remove", keyID, "--tenant", "lab1"},
		{"key", "revoke", keyID},
		{"setting", "get", "test.retention"}, {"setting", "set", "test.retention", "90d", "--tenant", "lab1"},
		{"setting", "list", "--tenant", "lab1"}, {"setting", "unset", "test.retention", "--tenant", "lab1"},
		{"audit", "list"}, {"tenant", "remove", "lab1", "--yes"},
		// Commands that fail, so the error paths are covered too.
		{"tenant", "show", "nope"}, {"tenant", "create", "Bad Name"}, {"grant", "set", "knosuchkey01", "--role", "reader", "--installation"},
		{"tenant", "list", "--bogus"}, {"tenant", "remove", "lab1"},
	}
	var outputs []string
	runAll := func(env map[string]string, extra ...string) {
		for _, step := range steps {
			for _, form := range [][]string{nil, {"--json"}} {
				args := append(append(append([]string{}, extra...), step...), form...)
				res := runWith(env, args...)
				outputs = append(outputs, res.stdout, res.stderr)
			}
		}
	}
	runAll(fromEnv)
	runAll(fromFile, "--key-file", keyFile)
	// Forced failures of every kind, whose messages come from the server.
	for _, code := range []connect.Code{connect.CodeInvalidArgument, connect.CodePermissionDenied, connect.CodeInternal, connect.CodeUnavailable} {
		fake.FailWith(nopacsv1connect.TenantServiceListTenantsProcedure, code, "forced")
		outputs = append(outputs, runWith(fromEnv, "tenant", "list").stderr)
	}
	// A wrong key, which the server refuses: its secret must not come back
	// either.
	const wrongSecret = "WWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWW"
	wrong := without(fromEnv, client.EnvKey)
	wrong[client.EnvKey] = "nopacs_" + keyID + "_" + wrongSecret
	res := runWith(wrong, "whoami")
	outputs = append(outputs, res.stdout, res.stderr)

	for i, out := range outputs {
		for _, s := range []string{secret, wrongSecret} {
			// key create answers a secret of its own, made by the fake; it
			// is "SSSS...", never the caller's.
			if strings.Contains(out, s) {
				t.Errorf("output %d shows a key secret %q...:\n%s", i, s[:4], out)
			}
		}
	}

	seen := fake.SeenRequests()
	if len(seen) < 2*len(steps) {
		t.Fatalf("the server saw only %d requests", len(seen))
	}
	authorized := 0
	for i, r := range seen {
		if strings.Contains(r.URL, "QQQQ") || strings.Contains(r.URL, "WWWW") {
			t.Errorf("request %d: the URL holds a secret: %s", i, r.URL)
		}
		for name, values := range r.Header {
			for _, v := range values {
				if name == http.CanonicalHeaderKey("Authorization") {
					if v != "Bearer "+key && v != "Bearer nopacs_"+keyID+"_"+wrongSecret {
						t.Errorf("request %d: Authorization is %q", i, v)
					}
					authorized++
					continue
				}
				if strings.Contains(v, "QQQQ") || strings.Contains(v, "WWWW") {
					t.Errorf("request %d: header %s holds a secret", i, name)
				}
			}
		}
		if strings.Contains(string(r.Body), "QQQQ") || strings.Contains(string(r.Body), "WWWW") {
			t.Errorf("request %d: the body holds a secret", i)
		}
	}
	if authorized != len(seen) {
		t.Errorf("%d of %d requests carried an Authorization header", authorized, len(seen))
	}
}

// TestKeyFileMustBePrivate guards --key-file: a key file that other users can
// open is refused as a usage error before anything is sent, with the way to
// fix it and none of the file's content; the same key in a private file works.
func TestKeyFileMustBePrivate(t *testing.T) {
	n := newNetworkRig(t)
	env := without(n.env, client.EnvKey)

	wide := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(wide, []byte(testKey+"\n"), 0o644); err != nil { //nolint:gosec // G306: the mode is the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil { //nolint:gosec // G302: the mode is the point of the test.
		t.Fatal(err)
	}
	res := runWith(env, "--key-file", wide, "whoami")
	if res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 ||
		!strings.Contains(res.stderr, "make it private to") || !strings.Contains(res.stderr, wide) || strings.Contains(res.stderr, "SSSS") {
		t.Errorf("a key file others can open: %+v", res)
	}
	if n.fake.Requests() != 0 {
		t.Errorf("%d requests were sent with a key from a file that others can read", n.fake.Requests())
	}

	private := fakeserver.PrivateFile(t, testKey+"\n")
	if res := runWith(env, "--key-file", private, "whoami"); res.code != 0 {
		t.Errorf("a private key file: %+v", res)
	}
}

// TestCAFileThroughTheCommands guards --ca-file and NOPACS_CA_FILE as a
// person meets them: the flag or the variable names the authorities for
// --server and replaces the system's; the flag beats the variable; a file that
// is not certificates is a usage error naming the file and not its content;
// another server's certificate is refused as untrusted by that file; and with
// the local socket the option does nothing.
func TestCAFileThroughTheCommands(t *testing.T) {
	n := newNetworkRig(t)
	ca := n.env[client.EnvCAFile]
	noCA := without(n.env, client.EnvCAFile)

	// Without it the system's store judges the test's own certificate.
	res := runWith(noCA, "whoami")
	if res.code != 1 || !strings.Contains(res.stderr, "not trusted by this system") {
		t.Errorf("without a CA file: %+v", res)
	}
	// The variable and the flag each work.
	if res := runWith(n.env, "whoami"); res.code != 0 {
		t.Errorf("NOPACS_CA_FILE: %+v", res)
	}
	if res := runWith(noCA, "--ca-file", ca, "whoami"); res.code != 0 {
		t.Errorf("--ca-file: %+v", res)
	}

	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("SECRETWORD is not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The flag beats a good variable; the variable alone is checked too.
	for name, args := range map[string]struct {
		env  map[string]string
		argv []string
	}{
		"flag over variable": {n.env, []string{"--ca-file", garbage, "whoami"}},
		"variable":           {with(noCA, client.EnvCAFile, garbage), []string{"whoami"}},
	} {
		res := runWith(args.env, args.argv...)
		if res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 ||
			!strings.Contains(res.stderr, "the CA file "+garbage+" holds no certificate in PEM form") || strings.Contains(res.stderr, "SECRETWORD") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if n.fake.Requests() != 2 {
		t.Errorf("the server saw %d requests, want the 2 that used a good file", n.fake.Requests())
	}

	// Another server's certificate does not trust this one.
	other := fakeserver.New()
	other.ServeTLS(t)
	wrong := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(wrong, other.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	res = runWith(n.env, "--ca-file", wrong, "whoami")
	if res.code != 1 || !strings.Contains(res.stderr, "not trusted by the CA file "+wrong) {
		t.Errorf("a wrong CA file: %+v", res)
	}

	// The local socket never reads the file.
	r := newRig(t)
	res = runWith(r.env, "--ca-file", filepath.Join(t.TempDir(), "none.pem"), "whoami")
	if res.code != 0 {
		t.Errorf("--ca-file with the local socket: %+v", res)
	}
}

// with returns a copy of an environment with one variable set.
func with(env map[string]string, name, value string) map[string]string {
	out := maps.Clone(env)
	out[name] = value
	return out
}
