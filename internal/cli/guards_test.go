package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// parseRFC3339 reads a time for a test's seed data.
func parseRFC3339(s string) (*timestamppb.Timestamp, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

// TestEveryErrorKindHasItsExitCode guards the error contract of the commands:
// each code the contract names, answered by the server for a real command
// over the socket, gives the exit code of its kind and exactly one line on
// standard error with the message and the request id, and nothing on standard
// output; with --json that line is the error document, with the code's
// general reason (UNKNOWN for a code past the protocol's sixteen, which the
// RPC library receives as UNKNOWN).
func TestEveryErrorKindHasItsExitCode(t *testing.T) {
	cases := []struct {
		code   connect.Code
		exit   int
		reason string
	}{
		{connect.CodeInvalidArgument, 2, "INVALID_ARGUMENT"},
		{connect.CodeUnauthenticated, 3, "UNAUTHENTICATED"},
		{connect.CodePermissionDenied, 3, "PERMISSION_DENIED"},
		{connect.CodeNotFound, 4, "NOT_FOUND"},
		{connect.CodeAlreadyExists, 1, "ALREADY_EXISTS"},
		{connect.CodeUnavailable, 1, "UNAVAILABLE"},
		{connect.CodeInternal, 1, "INTERNAL"},
		{connect.CodeFailedPrecondition, 5, "FAILED_PRECONDITION"},
		{connect.CodeOutOfRange, 6, "OUT_OF_RANGE"},
		{connect.CodeResourceExhausted, 7, "RESOURCE_EXHAUSTED"},
		{connect.CodeUnimplemented, 1, "UNIMPLEMENTED"},
		{connect.CodeUnknown, 1, "UNKNOWN"},
		{connect.Code(99), 1, "UNKNOWN"},
	}
	for _, c := range cases {
		r := newRig(t)
		r.fake.FailWith(nopacsv1connect.TenantServiceGetTenantProcedure, c.code, "what the server said")
		expect(t, r.run("tenant", "show", "lab1"), c.exit, "", "nopacs: tenant show: what the server said (request id req-000001)\n")
		expectJSONError(t, r.run("tenant", "show", "lab1", "--json"), c.exit, "tenant show: what the server said (request id req-000002)", c.reason)
	}
}

// TestDeadlineExceededIsAFailureWithTheTimeout guards the one code the
// contract has any method answer outside its list of kinds: a server that
// says DEADLINE_EXCEEDED gives exit 1 and a line that names the --timeout
// that ran out, with the request id, rather than the server's own words.
func TestDeadlineExceededIsAFailureWithTheTimeout(t *testing.T) {
	r := newRig(t)
	r.fake.FailWith(nopacsv1connect.TenantServiceGetTenantProcedure, connect.CodeDeadlineExceeded, "the deadline passed")
	res := r.run("tenant", "show", "lab1")
	if res.code != 1 || res.stdout != "" || !strings.HasSuffix(res.stderr, " within 30s (see --timeout) (request id req-000001)\n") || strings.Count(res.stderr, "\n") != 1 {
		t.Errorf("%+v", res)
	}
}

// TestRefusedLooksTheSame guards the contract's rule that a refusal has one
// look: a wrong key over the network is exit 3 with the same single line, and
// the line never holds the key that was sent.
func TestRefusedLooksTheSame(t *testing.T) {
	n := newNetworkRig(t)
	n.env[client.EnvKey] = "nopacs_wrongwrongwr_" + strings.Repeat("W", 44)
	res := n.run("tenant", "list")
	expect(t, res, 3, "", "nopacs: tenant list: the key is missing, unknown, expired or revoked (request id req-000001)\n")
	if strings.Contains(res.stderr, "WWWW") {
		t.Errorf("the line shows the key: %q", res.stderr)
	}
}

// TestNoFlagCanCarryASecret guards the rule that a secret is never an
// argument: it walks every command of the tree and fails on any flag, local or
// inherited, whose name is one that carries a secret, with no exception. The
// filters that take a key's public id are --key-id, which is not on the list;
// the flags that take a path (--key-file, --db-file, --ca-file) or the name of
// a variable (--db-env) carry no secret either: the secret is in the file or
// the variable, and the path or the name is not. --ca-file in particular names
// a file of public certificates. A whole key given where an id goes is refused
// (TestWholeKeyAsAnArgumentIsRefused).
func TestNoFlagCanCarryASecret(t *testing.T) {
	forbidden := []string{"db", "key", "password", "secret", "token"}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		visit := func(f *pflag.Flag) {
			name := strings.ToLower(f.Name)
			carries := slices.Contains(forbidden, name) || strings.Contains(name, "password") || strings.Contains(name, "secret") || strings.Contains(name, "token")
			if carries {
				t.Errorf("%s --%s: a flag named for a secret", strings.TrimPrefix(c.CommandPath(), "nopacs "), f.Name)
			}
		}
		c.LocalFlags().VisitAll(visit)
		c.InheritedFlags().VisitAll(visit)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.Command(cli.Env{}))
}

// TestKeyAndAddressAreNotFlags guards the other side of the same rule from the
// command line: the flags a person might try for a secret are unknown flags
// (exit 2), the key is read only from NOPACS_KEY or --key-file, and the
// address only from --db-env or --db-file.
func TestKeyAndAddressAreNotFlags(t *testing.T) {
	n := newNetworkRig(t)
	for _, args := range [][]string{
		{"tenant", "list", "--key", testKey},
		{"grant", "list", "--key", "k1"},
		{"audit", "list", "--key", "k1"},
		{"tenant", "list", "--token", "x"},
		{"tenant", "list", "--password", "x"},
		{"tenant", "list", "--secret", "x"},
		{"tenant", "create", "a", "--db", "postgres://u:p@h/d"},
	} {
		res := n.run(args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "unknown flag") || strings.Contains(res.stderr, testKey) {
			t.Errorf("nopacs %s: %+v", strings.Join(args[:2], " "), res)
		}
	}
	if n.fake.Requests() != 0 {
		t.Errorf("%d requests were sent", n.fake.Requests())
	}
}

// TestKeyFile guards --key-file: the key in the file is what the server
// receives, and the file wins over the environment.
func TestKeyFile(t *testing.T) {
	n := newNetworkRig(t)
	file := fakeserver.PrivateFile(t, testKey+"\n")
	n.env[client.EnvKey] = "ignored"
	n.must("--key-file", file, "whoami")
	if h := n.fake.AuthorizationHeaders(); len(h) != 1 || h[0] != "Bearer "+testKey {
		t.Errorf("headers = %q", h)
	}
	res := n.run("--key-file", file+"-missing", "whoami")
	if res.code != 2 || !strings.Contains(res.stderr, "cannot read the key file") {
		t.Errorf("a missing key file: %+v", res)
	}
}

// TestTenantListShowsGrantedTenants guards the fake against the real
// server on what tenant list shows: a key with no grant on the installation
// sees only the tenants it holds a grant on.
func TestTenantListShowsGrantedTenants(t *testing.T) {
	n := newNetworkRigAs(t, false)
	for _, name := range []string{"lab1", "lab2", "lab3"} {
		n.fake.AddTenant(&v1.Tenant{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_READY})
	}
	n.fake.AddGrant("k00000000001", &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: "lab2"}}, v1.Role_ROLE_READER)
	res := n.run("tenant", "list")
	if res.code != 0 || !strings.Contains(res.stdout, "lab2") || strings.Contains(res.stdout, "lab1") || strings.Contains(res.stdout, "lab3") {
		t.Errorf("tenant list by a reader of lab2: %+v", res)
	}
}

// TestNetworkOverTLS guards the network path end to end: TLS with a
// certificate made in the test, the key from the environment, the table.
func TestNetworkOverTLS(t *testing.T) {
	n := newNetworkRig(t)
	expect(t, n.run("tenant", "create", "lab1"), 0,
		"Name:     lab1\nDatabase: sqlite\nState:    ready\nSince:    2026-11-17 15:00:00 +03:00\nCreated:  2026-11-17 15:00:00 +03:00\n", "")
	doc := document(t, n.run("tenant", "list", "--json"))
	if len(doc["tenants"].([]any)) != 1 {
		t.Errorf("list: %v", doc)
	}
	for _, h := range n.fake.AuthorizationHeaders() {
		if h != "Bearer "+testKey {
			t.Errorf("a request carried %q", h)
		}
	}
}

// TestClientCertificateOptions guards --tls-client-cert and --tls-client-key
// and their variables through the commands: either way the handshake
// presents the certificate, a flag beats the variable, and one of the two
// alone is a usage error that says what is missing.
func TestClientCertificateOptions(t *testing.T) {
	n := newNetworkRig(t)
	dir := t.TempDir()
	cert, key, sum := fakeserver.WriteClientCert(t, dir, "flag")
	envCert, envKey, envSum := fakeserver.WriteClientCert(t, dir, "env")
	presented := func() string {
		seen := n.fake.SeenRequests()
		return seen[len(seen)-1].ClientCertSHA256
	}

	if res := n.run("--tls-client-cert", cert, "--tls-client-key", key, "whoami"); res.code != 0 || presented() != sum {
		t.Errorf("by flags: %+v, presented %q, want %q", res, presented(), sum)
	}
	withEnv := map[string]string{client.EnvClientCert: envCert, client.EnvClientKey: envKey}
	for k, v := range n.env {
		withEnv[k] = v
	}
	if res := runWith(withEnv, "whoami"); res.code != 0 || presented() != envSum {
		t.Errorf("by variables: %+v, presented %q, want %q", res, presented(), envSum)
	}
	if res := runWith(withEnv, "--tls-client-cert", cert, "--tls-client-key", key, "whoami"); res.code != 0 || presented() != sum {
		t.Errorf("flags over variables: %+v, presented %q, want %q", res, presented(), sum)
	}
	res := n.run("--tls-client-cert", cert, "whoami")
	if res.code != 2 || !strings.Contains(res.stderr, "a client certificate needs both its file and its private key's file") {
		t.Errorf("a certificate without its key: %+v", res)
	}
}

// TestServerAddressRules guards the rules for --server through the commands,
// so a key is never sent in clear over a network: http to a host that is not
// loopback is refused before anything is sent; https to a certificate the
// system does not trust is refused with a clear line; http on loopback works.
func TestServerAddressRules(t *testing.T) {
	env := map[string]string{client.EnvKey: testKey}
	res := runWith(env, "--server", "http://example.com", "tenant", "list")
	expect(t, res, 2, "",
		"nopacs: tenant list: the server address http://example.com is refused: http is allowed only for a loopback host (localhost, 127.0.0.1, ::1); use https://\n")

	// The certificate is the test's own, and the command is given no CA file,
	// so it is judged by the system's authorities, which do not know it.
	n := newNetworkRig(t)
	res = runWith(without(n.env, client.EnvCAFile), "tenant", "list")
	if res.code != 1 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 ||
		!strings.Contains(res.stderr, "refusing to connect to https://127.0.0.1:") ||
		!strings.Contains(res.stderr, "its certificate is not trusted by this system (signed by an unknown authority)") {
		t.Errorf("untrusted certificate: %+v", res)
	}
	if n.fake.Requests() != 0 {
		t.Errorf("the key was sent to a server that was not trusted (%d requests)", n.fake.Requests())
	}

	fake := newNetworkRig(t).fake
	addr := fake.ServeHTTP(t)
	res = runWith(map[string]string{client.EnvServer: addr, client.EnvKey: testKey}, "whoami")
	if res.code != 0 || !strings.Contains(res.stdout, "key k00000000001") {
		t.Errorf("loopback http: %+v", res)
	}
}

// TestChoosingTheServer guards how the way to reach the server is chosen: a
// flag beats the environment, two flags that disagree are refused, and with
// nothing given the command says what to give.
func TestChoosingTheServer(t *testing.T) {
	r := newRig(t)
	dir := r.env[client.EnvDataDir]
	// The flag beats a server in the environment that could not be reached.
	res := runWith(map[string]string{client.EnvServer: "https://127.0.0.1:1", client.EnvKey: testKey}, "--data-dir", dir, "whoami")
	if res.code != 0 {
		t.Errorf("--data-dir did not beat NOPACS_SERVER: %+v", res)
	}
	res = runWith(r.env, "--data-dir", dir, "--server", "https://example.org", "whoami")
	expect(t, res, 2, "", "nopacs: whoami: give --server or --data-dir, not both\n")
	res = runWith(nil, "whoami")
	if res.code != 2 || !strings.Contains(res.stderr, "do not know where the server is: give --data-dir") {
		t.Errorf("nothing given: %+v", res)
	}
}

// TestPaging guards paging: a list of 250 items with --limit 100 fetches one
// page and says more exist; --all fetches all 250, in pages.
func TestPaging(t *testing.T) {
	r := newRig(t)
	for i := range 250 {
		r.fake.AddTenant(&v1.Tenant{Name: fmt.Sprintf("t%03d", i), DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamppb.New(fixedNow)})
	}
	rows := func(s string) int { return strings.Count(s, "\n") - 1 }

	before := r.fake.Requests()
	res := r.run("tenant", "list", "--limit", "100")
	if res.code != 0 || rows(res.stdout) != 100 {
		t.Fatalf("--limit 100: exit %d, %d rows", res.code, rows(res.stdout))
	}
	if got := r.fake.Requests() - before; got != 1 {
		t.Errorf("--limit 100 made %d calls, want 1 page", got)
	}
	if res.stderr != "nopacs: more exist; showing the first 100; use --limit N or --all to see more\n" {
		t.Errorf("the note about more: %q", res.stderr)
	}
	// The default limit is 100 too.
	if res := r.run("tenant", "list"); rows(res.stdout) != 100 || !strings.Contains(res.stderr, "more exist") {
		t.Errorf("default: %d rows, %q", rows(res.stdout), res.stderr)
	}
	doc := document(t, r.run("tenant", "list", "--json"))
	if len(doc["tenants"].([]any)) != 100 || doc["more"] != true {
		t.Errorf("--json with more: %d tenants, more=%v", len(doc["tenants"].([]any)), doc["more"])
	}

	before = r.fake.Requests()
	res = r.run("tenant", "list", "--all")
	if res.code != 0 || rows(res.stdout) != 250 || res.stderr != "" {
		t.Fatalf("--all: exit %d, %d rows, stderr %q", res.code, rows(res.stdout), res.stderr)
	}
	if got := r.fake.Requests() - before; got != 1 {
		t.Errorf("--all made %d calls, want 1 (the page holds up to 1000)", got)
	}
	doc = document(t, r.run("tenant", "list", "--all", "--json"))
	if len(doc["tenants"].([]any)) != 250 || doc["more"] != false {
		t.Errorf("--all --json: %d tenants, more=%v", len(doc["tenants"].([]any)), doc["more"])
	}

	// More than one page: 1200 tenants need two calls with --all, and a limit
	// above one page needs two as well.
	for i := 250; i < 1200; i++ {
		r.fake.AddTenant(&v1.Tenant{Name: fmt.Sprintf("t%04d", i), State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamppb.New(fixedNow)})
	}
	before = r.fake.Requests()
	res = r.run("tenant", "list", "--all")
	if rows(res.stdout) != 1200 || r.fake.Requests()-before != 2 {
		t.Errorf("--all over 1200: %d rows in %d calls, want 1200 in 2", rows(res.stdout), r.fake.Requests()-before)
	}
	before = r.fake.Requests()
	res = r.run("tenant", "list", "--limit", "1100")
	if rows(res.stdout) != 1100 || r.fake.Requests()-before != 2 || !strings.Contains(res.stderr, "more exist; showing the first 1100") {
		t.Errorf("--limit 1100: %d rows in %d calls, stderr %q", rows(res.stdout), r.fake.Requests()-before, res.stderr)
	}
}

// TestUsageErrors guards the grammar's refusals: no command, an unknown word, a
// noun with no verb or a wrong one, a wrong number of arguments and an unknown
// flag are each one line on standard error and exit 2, with nothing on
// standard output, and text from the arguments cannot move the terminal.
func TestUsageErrors(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"no command":     {nil, "nopacs: no command (usage: nopacs <noun> <verb> [flags], for example nopacs tenant list); try \"nopacs --help\"\n"},
		"unknown":        {[]string{"nope"}, "nopacs: unknown command \"nope\" for \"nopacs\"; try \"nopacs --help\"\n"},
		"terminal codes": {[]string{"\x1b[2J"}, "nopacs: unknown command \"\\x1b[2J\" for \"nopacs\"; try \"nopacs --help\"\n"},
		"no verb":        {[]string{"tenant"}, "nopacs: tenant needs a verb: create, export, import, list, reclaim, remove, show, status; try \"nopacs tenant --help\"\n"},
		"wrong verb":     {[]string{"tenant", "nope"}, "nopacs: unknown command \"nope\" for \"nopacs tenant\"; try \"nopacs tenant --help\"\n"},
		"extra words":    {[]string{"version", "now"}, "nopacs: unknown command \"now\" for \"nopacs version\"; try \"nopacs version --help\"\n"},
		"missing name":   {[]string{"tenant", "show"}, "nopacs: wrong number of arguments (want 1, got 0); usage: nopacs tenant show <name> [flags]; try \"nopacs tenant show --help\"\n"},
		"unknown flag":   {[]string{"whoami", "--nope"}, "nopacs: unknown flag: --nope; try \"nopacs whoami --help\"\n"},
		"bad timeout":    {[]string{"--timeout", "soon", "whoami"}, "nopacs: invalid argument \"soon\" for \"--timeout\" flag: time: invalid duration \"soon\"; try \"nopacs whoami --help\"\n"},
	}
	for name, c := range cases {
		res := runWith(nil, c.args...)
		// Unknown commands print the argument through %q, so the escape byte
		// is written as text; cobra's own messages are cleaned by the
		// command line before they are written.
		if res.code != 2 || res.stdout != "" || res.stderr != c.want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 2 and %q", name, res.code, res.stdout, res.stderr, c.want)
		}
		if strings.Contains(res.stderr, "\x1b") {
			t.Errorf("%s: an escape byte reached standard error", name)
		}
	}
}

// failingWriter is a standard output that cannot be written, as a full disk.
type failingWriter struct{}

// Write fails every time.
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// TestOutputFailureIsAFailure guards the exit code when the output cannot be
// written: a script reading it must see exit 1, not success with nothing
// printed.
func TestOutputFailureIsAFailure(t *testing.T) {
	var stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"version"}, cli.Env{Stdout: failingWriter{}, Stderr: &stderr})
	if code != 1 || !strings.Contains(stderr.String(), "no space left on device") || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

// TestEveryCommandHasHelp guards the rule that help is written, not empty:
// every command has a short and a long text, every command that does a thing
// has an example, and every flag has a usage text. cobra's own commands
// (help, completion) are written by cobra.
func TestEveryCommandHasHelp(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := c.CommandPath()
		switch c.Name() {
		case "help", "completion", "bash", "zsh", "fish", "powershell", "__complete", "__completeNoDesc":
			return
		}
		if strings.TrimSpace(c.Short) == "" || strings.TrimSpace(c.Long) == "" {
			t.Errorf("%s: no short or long help", path)
		}
		if len(c.Commands()) == 0 && strings.TrimSpace(c.Example) == "" {
			t.Errorf("%s: no example", path)
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if strings.TrimSpace(f.Usage) == "" {
				t.Errorf("%s --%s: no help text", path, f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.Command(cli.Env{}))
}

// TestGrammar guards that the commands are exactly the grammar: no verb or
// noun more, none less.
func TestGrammar(t *testing.T) {
	want := map[string]bool{
		"version": true, "whoami": true, "agent-guide": true,
		"context add": true, "context list": true, "context show": true, "context use": true, "context remove": true,
		"tenant create": true, "tenant list": true, "tenant show": true, "tenant status": true, "tenant remove": true,
		"tenant export": true, "tenant reclaim": true,
		"key create": true, "key list": true, "key show": true, "key revoke": true, "key rotate": true, "key set": true, "key reinstate": true,
		"grant set": true, "grant list": true, "grant remove": true,
		"setting get": true, "setting set": true, "setting unset": true, "setting list": true,
		"audit list": true, "audit verify": true, "audit checkpoints": true, "audit export": true, "audit public-key": true,
		"storage add": true, "storage list": true, "storage show": true, "storage set": true, "storage remove": true, "storage test": true,
		"receiver add": true, "receiver list": true, "receiver show": true, "receiver set": true, "receiver remove": true,
		"receiver route list": true, "receiver route add": true, "receiver route set": true, "receiver route remove": true,
		"receiver route move": true, "receiver route default": true, "receiver route export": true, "receiver route import": true,
		"sender add": true, "sender list": true, "sender show": true, "sender set": true, "sender remove": true, "sender test": true,
		"job create": true, "job show": true, "job list": true, "job watch": true, "job cancel": true, "job retry": true, "job failures": true,
		"job import": true, "job files": true, "job units": true,
		"job scrub": true, "job rebuild": true, "job findings": true, "job finding": true,
		"deid check": true, "deid dry-run": true, "deid code": true,
		"profile create": true, "profile get": true, "profile list": true, "profile versions": true, "profile export": true,
		"profile validate": true, "profile describe": true, "profile try": true, "profile explain": true, "profile diff": true,
		"profile lookup import": true, "profile lookup list": true,
		"reception list": true, "reception show": true, "reception retry": true, "reception discard": true,
		"quarantine list": true, "quarantine show": true, "quarantine release": true, "quarantine reject": true,
		"correction delete": true, "correction relabel": true, "correction replace": true, "correction edit": true, "correction list": true,
		"correction show": true, "correction undo": true, "correction purge": true, "correction allow": true,
		"catalog subjects": true, "catalog sessions": true, "catalog scans": true, "scan show": true, "download": true,
		"watch": true, "backup create": true,
		"storage status": true,
		"replica list":   true, "replica attach": true, "replica detach": true, "replica backfill": true,
		"repair": true, "switch": true,
		"retention rule add": true, "retention rule set": true, "retention rule remove": true, "retention rule list": true, "retention rule show": true,
		"retention due": true, "retention dispose": true, "retention clock": true,
		"hold list": true, "hold show": true, "hold place": true, "hold release": true,
	}
	got := map[string]bool{}
	for _, c := range cli.Command(cli.Env{}).Commands() {
		switch c.Name() {
		case "help", "completion":
			continue
		}
		// The commands that moved to nopacs-server are hidden stand-ins
		// that only name where they went (TestMovedCommandsNameTheServerProgram
		// in cmd/nopacs); they are not part of this grammar.
		if c.Hidden {
			continue
		}
		if len(c.Commands()) == 0 {
			got[c.Name()] = true
		}
		for _, v := range c.Commands() {
			if v.Hidden {
				continue
			}
			// A part of a noun with verbs of its own (a receiver's
			// routes) is listed by its verbs.
			if len(v.Commands()) == 0 {
				got[c.Name()+" "+v.Name()] = true
			}
			for _, w := range v.Commands() {
				got[c.Name()+" "+v.Name()+" "+w.Name()] = true
			}
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing command %q", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("command %q is not in the grammar", k)
		}
	}
}

// TestBashCompletionParses guards shell completion: the generated bash
// completion script is accepted by bash -n, which parses it without running
// it.
func TestBashCompletionParses(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not installed, so the completion script cannot be parsed here: %v", err)
	}
	res := runWith(nil, "completion", "bash")
	if res.code != 0 || res.stderr != "" || !strings.Contains(res.stdout, "_nopacs") {
		t.Fatalf("completion bash: exit %d, stderr %q", res.code, res.stderr)
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(res.stdout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash -n rejected the completion script: %v\n%s", err, out)
	}
}

// TestColor guards when emphasis is allowed: only on a terminal, never with
// NO_COLOR set, never on a dumb terminal, and not on Windows.
func TestColor(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		env      map[string]string
		goos     string
		want     bool
	}{
		{"terminal", true, nil, "linux", true},
		{"pipe", false, nil, "linux", false},
		{"NO_COLOR", true, map[string]string{"NO_COLOR": "1"}, "linux", false},
		{"dumb", true, map[string]string{"TERM": "dumb"}, "linux", false},
		{"windows", true, nil, "windows", false},
	}
	for _, c := range cases {
		got := cli.ColorAllowed(c.terminal, func(k string) string { return c.env[k] }, c.goos)
		if got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
