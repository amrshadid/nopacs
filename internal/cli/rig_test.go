package cli_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// testKey is a made-up key for the network tests; it is not a real credential
// and the fake accepts it only because the test says so.
const testKey = "nopacs_k00000000001_" + "SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS"

// fixedNow is the clock of every test, so times in the output are the same on
// every run.
var fixedNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// zone is the zone the tests show times in: not UTC, so a test that forgot to
// convert would show a different hour, with an offset that is easy to read.
var zone = time.FixedZone("EAT", 3*60*60)

// result is what one run of the command line wrote and returned.
type result struct {
	stdout string
	stderr string
	code   int
}

// testSettings is the list of settings of the tests' fake: the server's own,
// with one a tenant may override, which the server's list does not have yet.
// It is made here, in the tests, and never ships.
func testSettings() *settings.Table {
	period := func(v string) error {
		_, err := settings.ParseDuration(v)
		return err
	}
	t, err := settings.New(append(settings.Builtin(),
		settings.Definition{Name: "test.retention", Default: "365d", TenantOverride: true, Validate: period})...)
	if err != nil {
		panic(err)
	}
	return t
}

// rig is a fake server and the environment to reach it.
type rig struct {
	t    *testing.T
	fake *fakeserver.Server
	env  map[string]string
}

// newRig serves a fake on a unix socket and points the commands at it through
// NOPACS_DATA_DIR, the default way a person reaches a local server.
func newRig(t *testing.T) *rig {
	t.Helper()
	fake := fakeserver.New()
	fake.Now = func() time.Time { return fixedNow }
	fake.Settings = testSettings()
	dir := fake.ServeUnix(t)
	return &rig{t: t, fake: fake, env: map[string]string{client.EnvDataDir: dir}}
}

// newNetworkRig serves a fake over TLS that accepts only testKey, and points
// the commands at it through NOPACS_SERVER and NOPACS_KEY.
func newNetworkRig(t *testing.T) *rig {
	t.Helper()
	return newNetworkRigAs(t, true)
}

// newNetworkRigAs is newNetworkRig with the rig's key an installation admin
// or a key with no grant at all.
func newNetworkRigAs(t *testing.T, admin bool) *rig {
	t.Helper()
	fake := fakeserver.New()
	fake.Now = func() time.Time { return fixedNow }
	fake.Settings = testSettings()
	fake.RequireKey = testKey
	fake.CallerKeyID = "k00000000001"
	// The rig's key acts as an installation admin would, as the real
	// server lets only such a key list every tenant.
	if admin {
		fake.AddGrant("k00000000001", &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}, v1.Role_ROLE_ADMIN)
	}
	addr := fake.ServeTLS(t)
	return &rig{t: t, fake: fake, env: map[string]string{
		client.EnvServer: addr, client.EnvKey: testKey,
		// The certificate is the test's own, so it is trusted the way a
		// person with a private authority would trust it.
		client.EnvCAFile: writeCA(t, fake),
	}}
}

// run runs the command line with the arguments and the rig's environment and
// returns what it wrote and its exit code.
func (r *rig) run(args ...string) result {
	r.t.Helper()
	return runWith(r.env, args...)
}

// runWith is run with an environment of the test's own.
func runWith(env map[string]string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, cli.Env{
		Stdout:   &stdout,
		Stderr:   &stderr,
		Getenv:   func(k string) string { return env[k] },
		Now:      func() time.Time { return fixedNow },
		Location: zone,
		// A watch that has to follow again does so at once.
		WatchPause: time.Millisecond,
	})
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// writeCA writes the fake's certificate as a PEM file of authorities and
// returns its path, for --ca-file or NOPACS_CA_FILE.
func writeCA(t *testing.T, fake *fakeserver.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, fake.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newArchiveRig is a rig with tenant lab1, a directory storage "main" and an
// enabled receiver "scanner1" that archives to it: what the commands of the
// archive's four services act on.
func newArchiveRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	r.must("receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11112", "--aet", "NOPACS", "--allow-aet", "PRISMA1", "--enabled")
	return r
}

// typed runs the command line as a person at a terminal who types input: both
// ends are a terminal, standard input holds the text, and what the command
// asks is on standard error.
func typed(env map[string]string, input string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, cli.Env{
		Stdout:   &stdout,
		Stderr:   &stderr,
		Stdin:    strings.NewReader(input),
		Terminal: func(io.Reader, io.Writer) bool { return true },
		Getenv:   func(k string) string { return env[k] },
		Now:      func() time.Time { return fixedNow },
		Location: zone,
	})
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}
