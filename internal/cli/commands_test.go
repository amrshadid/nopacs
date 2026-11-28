package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// expect fails the test unless the run ended with the exit code and wrote
// exactly the standard output and standard error given.
func expect(t *testing.T, res result, code int, stdout, stderr string) {
	t.Helper()
	if res.code != code {
		t.Errorf("exit code %d, want %d (stdout %q, stderr %q)", res.code, code, res.stdout, res.stderr)
	}
	if res.stdout != stdout {
		t.Errorf("standard output:\n%s\nwant:\n%s", res.stdout, stdout)
	}
	if res.stderr != stderr {
		t.Errorf("standard error:\n%s\nwant:\n%s", res.stderr, stderr)
	}
}

// document parses a --json output and checks what every document must have:
// that it is one JSON object, with schemaVersion 1 first, and nothing else on
// the line before or after it.
func document(t *testing.T, res result) map[string]any {
	t.Helper()
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("exit code %d, standard error %q", res.code, res.stderr)
	}
	if !strings.HasPrefix(res.stdout, "{\n  \"schemaVersion\": 1") || !strings.HasSuffix(res.stdout, "}\n") {
		t.Fatalf("not a document that starts with schemaVersion 1:\n%s", res.stdout)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("standard output is not JSON: %v\n%s", err, res.stdout)
	}
	if doc["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v", doc["schemaVersion"])
	}
	return doc
}

// must runs a command that has to succeed, to set up a test.
func (r *rig) must(args ...string) result {
	r.t.Helper()
	res := r.run(args...)
	if res.code != 0 {
		r.t.Fatalf("nopacs %s: exit %d: %s", strings.Join(args, " "), res.code, res.stderr)
	}
	return res
}

// TestVersion guards the version command: the same line as before the command
// tree, in the table form, and a document with the build's fields in --json.
func TestVersion(t *testing.T) {
	// No server is needed, so no environment is given.
	res := runWith(nil, "version")
	if res.code != 0 || res.stderr != "" || !strings.HasPrefix(res.stdout, "nopacs ") || strings.Count(res.stdout, "\n") != 1 {
		t.Errorf("version: %+v", res)
	}
	doc := document(t, runWith(nil, "version", "--json"))
	for _, k := range []string{"version", "commit", "goVersion"} {
		if s, _ := doc[k].(string); s == "" {
			t.Errorf("version --json has no %s: %v", k, doc)
		}
	}
	if long := runWith(nil, "--version"); long.stdout != res.stdout {
		t.Errorf("--version printed %q, version printed %q", long.stdout, res.stdout)
	}
}

// TestWhoami guards whoami as the local operator and as a key with grants, in
// the table form and in --json.
func TestWhoami(t *testing.T) {
	r := newRig(t)
	expect(t, r.run("whoami"), 0,
		"Caller: local operator (user 501)\nAccess: admin on the installation and on every tenant\n", "")
	doc := document(t, r.run("whoami", "--json"))
	if _, ok := doc["principal"].(map[string]any)["localOperator"]; !ok {
		t.Errorf("no localOperator in %v", doc)
	}

	n := newNetworkRig(t)
	n.fake.AddKey(&v1.Key{Id: "k00000000001", Holder: "ci", CreateTime: timestamppb.New(fixedNow), ExpireTime: timestamppb.New(fixedNow.AddDate(0, 0, 30))})
	n.must("tenant", "create", "lab1")
	n.must("grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1")
	n.must("grant", "set", "k00000000001", "--role", "admin", "--installation")
	expect(t, n.run("whoami"), 0,
		"Caller:  key k00000000001 (ci)\nExpires: 2026-12-17 15:00:00 +03:00\n\n"+
			"SCOPE         ROLE\ninstallation  admin\ntenant lab1   reader\n", "")
	doc = document(t, n.run("whoami", "--json"))
	if grants := doc["principal"].(map[string]any)["grants"].([]any); len(grants) != 2 {
		t.Errorf("grants = %v", grants)
	}
}

// TestTenantCommands guards every tenant command in both forms: what is
// printed, what the server was sent, and that a Postgres address never comes
// back on either stream.
func TestTenantCommands(t *testing.T) {
	r := newRig(t)
	expect(t, r.run("tenant", "create", "lab1"), 0,
		"Name:     lab1\nDatabase: sqlite\nState:    ready\nSince:    2026-11-17 15:00:00 +03:00\nCreated:  2026-11-17 15:00:00 +03:00\n", "")

	// A Postgres address from the environment, then from a file.
	const address = "postgres://lab2:pw-from-env@db.example.org/lab2" //nolint:gosec // a made-up address for a test
	r.env["LAB2_DB"] = address
	res := r.run("tenant", "create", "lab2", "--db-env", "LAB2_DB")
	expect(t, res, 0, "Name:     lab2\nDatabase: postgres\nState:    ready\nSince:    2026-11-17 15:00:00 +03:00\nCreated:  2026-11-17 15:00:00 +03:00\n", "")
	if got := r.fake.DatabaseAddress("lab2"); got != address {
		t.Errorf("the server received the address %q, want the one in the environment", got)
	}
	file := filepath.Join(t.TempDir(), "lab3.dsn")
	const fileAddress = "postgres://lab3:pw-from-file@db.example.org/lab3" //nolint:gosec // a made-up address for a test
	if err := os.WriteFile(file, []byte(fileAddress+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = r.run("tenant", "create", "lab3", "--db-file", file, "--json")
	doc := document(t, res)
	if doc["tenant"].(map[string]any)["databaseType"] != "DATABASE_TYPE_POSTGRES" {
		t.Errorf("create --json: %v", doc)
	}
	if got := r.fake.DatabaseAddress("lab3"); got != fileAddress {
		t.Errorf("the server received the address %q, want the one in the file", got)
	}
	for _, s := range []string{res.stdout, res.stderr} {
		if strings.Contains(s, "pw-from") || strings.Contains(s, "db.example.org") {
			t.Errorf("an address leaked into the output: %q", s)
		}
	}

	expect(t, r.run("tenant", "list"), 0,
		"NAME  DATABASE  STATE  CREATED\n"+
			"lab1  sqlite    ready  2026-11-17 15:00:00 +03:00\n"+
			"lab2  postgres  ready  2026-11-17 15:00:00 +03:00\n"+
			"lab3  postgres  ready  2026-11-17 15:00:00 +03:00\n", "")
	list := document(t, r.run("tenant", "list", "--json"))
	if ts := list["tenants"].([]any); len(ts) != 3 || list["more"] != false {
		t.Errorf("list --json: %v", list)
	}
	expect(t, r.run("tenant", "show", "lab2"), 0,
		"Name:     lab2\nDatabase: postgres\nState:    ready\nSince:    2026-11-17 15:00:00 +03:00\nCreated:  2026-11-17 15:00:00 +03:00\n", "")
	document(t, r.run("tenant", "show", "lab2", "--json"))

	// A tenant whose database is down is listed with its reason.
	r.fake.AddTenant(&v1.Tenant{Name: "lab9", DatabaseType: v1.DatabaseType_DATABASE_TYPE_POSTGRES, State: v1.TenantState_TENANT_STATE_UNREACHABLE, StateReason: "the database does not answer", CreateTime: timestamppb.New(fixedNow)})
	res = r.run("tenant", "list")
	if !strings.Contains(res.stdout, "REASON") || !strings.Contains(res.stdout, "unreachable  2026-11-17 15:00:00 +03:00  the database does not answer") {
		t.Errorf("list with a down tenant:\n%s", res.stdout)
	}
	res = r.run("tenant", "show", "lab9")
	if !strings.Contains(res.stdout, "Reason:   the database does not answer\n") {
		t.Errorf("show of a down tenant:\n%s", res.stdout)
	}

	expect(t, r.run("tenant", "remove", "lab1", "--yes"), 0,
		"Tenant:       lab1 unregistered; no data was deleted\nDatabase:     sqlite\nData left at: /var/lib/nopacs/tenants/lab1.db\n", "")
	doc = document(t, r.run("tenant", "remove", "lab2", "--yes", "--json"))
	if doc["dataLocation"] != "db.example.org:5432/lab2" || doc["databaseType"] != "DATABASE_TYPE_POSTGRES" {
		t.Errorf("remove --json: %v", doc)
	}
}

// TestTenantRemoveNeedsYes guards the safety of removal: without --yes the
// command says what it would do, exits 2, and sends nothing; with --yes it
// sends the name twice.
func TestTenantRemoveNeedsYes(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	before := r.fake.Requests()
	const refusal = "tenant remove: nothing was removed: this would unregister tenant \"lab1\"; its database and files stay where they are; run it again with --yes to do it"
	expect(t, r.run("tenant", "remove", "lab1"), 2, "", "nopacs: "+refusal+"\n")
	expectJSONError(t, r.run("tenant", "remove", "lab1", "--json"), 2, refusal, "INVALID_ARGUMENT")
	if got := r.fake.Requests(); got != before {
		t.Errorf("a removal without --yes sent %d requests", got-before)
	}
	r.must("tenant", "show", "lab1")
	r.must("tenant", "remove", "lab1", "--yes")
	if res := r.run("tenant", "show", "lab1"); res.code != 4 {
		t.Errorf("the tenant is still there: %+v", res)
	}
}

// TestTenantCreateSecretsComeFromEnvOrFile guards where a database address
// comes from: an unset variable, an empty or a missing file, and both flags
// at once are refused as usage errors before anything is sent, with messages
// that name the variable or file and not its content.
func TestTenantCreateSecretsComeFromEnvOrFile(t *testing.T) {
	r := newRig(t)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(t.TempDir(), "huge")
	if err := os.WriteFile(huge, []byte(strings.Repeat("x", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args []string
		want string
	}{
		"unset variable": {[]string{"tenant", "create", "a", "--db-env", "NOPE"}, "--db-env names NOPE, which is not set or is empty"},
		"empty file":     {[]string{"tenant", "create", "a", "--db-file", empty}, "is empty"},
		"missing file":   {[]string{"tenant", "create", "a", "--db-file", filepath.Join(t.TempDir(), "none")}, "cannot read --db-file"},
		"huge file":      {[]string{"tenant", "create", "a", "--db-file", huge}, "longer than 4 KiB"},
		"both flags":     {[]string{"tenant", "create", "a", "--db-env", "X", "--db-file", empty}, "none of the others can be"},
	}
	for name, c := range cases {
		res := r.run(c.args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, c.want) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line with %q", name, res, c.want)
		}
	}
	if r.fake.Requests() != 0 {
		t.Errorf("%d requests were sent", r.fake.Requests())
	}
}

// TestKeyCommands guards every key command in both forms, and the one rule
// of key create: the key alone on standard output, the note on standard error.
func TestKeyCommands(t *testing.T) {
	r := newRig(t)
	const secret = "nopacs_k00000000001_SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS"
	expect(t, r.run("key", "create", "--holder", "ci"), 0, secret+"\n",
		"nopacs: key k00000000001 (ci), expires 2027-02-15 15:00:00 +03:00\n"+
			"nopacs: this is the only time the key is shown; it cannot be shown again, so store it now\n")
	res := r.run("key", "create", "--holder", "nightly export", "--expires", "30d")
	if !strings.Contains(res.stderr, "expires 2026-12-17 15:00:00 +03:00") {
		t.Errorf("--expires 30d: %q", res.stderr)
	}
	doc := document(t, r.run("key", "create", "--holder", "svc", "--no-expiry", "--json"))
	if doc["secret"] != "nopacs_k00000000003_SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS" || doc["key"].(map[string]any)["expireTime"] != nil {
		t.Errorf("create --json: %v", doc)
	}

	expect(t, r.run("key", "list"), 0,
		"ID            HOLDER          STATUS  CREATED                     EXPIRES                     LAST USED\n"+
			"k00000000001  ci              active  2026-11-17 15:00:00 +03:00  2027-02-15 15:00:00 +03:00  never\n"+
			"k00000000002  nightly export  active  2026-11-17 15:00:00 +03:00  2026-12-17 15:00:00 +03:00  never\n"+
			"k00000000003  svc             active  2026-11-17 15:00:00 +03:00  never                       never\n", "")
	if keys := document(t, r.run("key", "list", "--json"))["keys"].([]any); len(keys) != 3 {
		t.Errorf("list --json has %d keys", len(keys))
	}
	expect(t, r.run("key", "show", "k00000000001"), 0,
		"ID:        k00000000001\nHolder:    ci\nStatus:    active\nCreated:   2026-11-17 15:00:00 +03:00\n"+
			"Expires:   2027-02-15 15:00:00 +03:00\nLast used: never\nRevoked:   -\n", "")
	expect(t, r.run("key", "revoke", "k00000000001"), 0, "key k00000000001 (ci) revoked at 2026-11-17 15:00:00 +03:00\n", "")
	if s := r.run("key", "show", "k00000000001").stdout; !strings.Contains(s, "Status:    revoked") {
		t.Errorf("show after revoke:\n%s", s)
	}
	document(t, r.run("key", "revoke", "k00000000002", "--json"))
	document(t, r.run("key", "show", "k00000000002", "--json"))

	// An expired key is told from an active one by the clock.
	r.fake.AddKey(&v1.Key{Id: "kexpired0001", Holder: "old", CreateTime: timestamppb.New(fixedNow.AddDate(-1, 0, 0)), ExpireTime: timestamppb.New(fixedNow.AddDate(0, 0, -1))})
	if s := r.run("key", "show", "kexpired0001").stdout; !strings.Contains(s, "Status:    expired") {
		t.Errorf("show of an expired key:\n%s", s)
	}

	for name, args := range map[string][]string{
		"bad span":    {"key", "create", "--holder", "x", "--expires", "soon"},
		"zero span":   {"key", "create", "--holder", "x", "--expires", "0d"},
		"no holder":   {"key", "create"},
		"both expiry": {"key", "create", "--holder", "x", "--expires", "1d", "--no-expiry"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line", name, res)
		}
	}
}

// TestKeyIsNotShownAnywhereElse guards the one place a secret is printed: the
// key made by key create is on standard output and in no other output of any
// command, and not on standard error.
func TestKeyIsNotShownAnywhereElse(t *testing.T) {
	r := newRig(t)
	const secret = "SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS"
	created := r.must("key", "create", "--holder", "ci")
	if strings.Contains(created.stderr, secret) {
		t.Errorf("the note on standard error holds the secret: %q", created.stderr)
	}
	r.must("tenant", "create", "lab1")
	for _, args := range [][]string{
		{"key", "list"}, {"key", "list", "--json"}, {"key", "show", "k00000000001"}, {"key", "show", "k00000000001", "--json"},
		{"key", "revoke", "k00000000001"}, {"key", "revoke", "k00000000001", "--json"},
		{"grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1"},
		{"grant", "list", "--key-id", "k00000000001", "--json"}, {"whoami"}, {"audit", "list"},
	} {
		res := r.run(args...)
		if strings.Contains(res.stdout+res.stderr, secret) {
			t.Errorf("nopacs %s shows the secret", strings.Join(args, " "))
		}
	}
}

// TestWholeKeyAsAnArgumentIsRefused guards the mistake of typing a key where
// an id goes: it is refused before anything is sent, and the message never
// repeats it, so it is not copied into a terminal's scrollback twice.
func TestWholeKeyAsAnArgumentIsRefused(t *testing.T) {
	r := newRig(t)
	const whole = "nopacs_k00000000001_SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS"
	for _, args := range [][]string{
		{"key", "show", whole}, {"key", "revoke", whole},
		{"grant", "set", whole, "--role", "reader", "--installation"},
		{"grant", "remove", whole, "--installation"},
		{"grant", "list", "--key-id", whole},
		{"audit", "list", "--key-id", whole},
	} {
		res := r.run(args...)
		if res.code != 2 || res.stdout != "" || strings.Contains(res.stderr, "SSSS") || !strings.Contains(res.stderr, "is a secret and is never an argument") {
			t.Errorf("nopacs %s: %+v", strings.Join(args[:2], " "), res)
		}
	}
	if r.fake.Requests() != 0 {
		t.Errorf("%d requests were sent", r.fake.Requests())
	}
}

// TestGrantCommands guards every grant command in both forms, including a
// role replaced, a grant removed twice, and the refusals the server makes.
func TestGrantCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("key", "create", "--holder", "ci")
	expect(t, r.run("grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1"), 0,
		"key k00000000001 now holds reader on tenant lab1 (before: no grant)\n", "")
	expect(t, r.run("grant", "set", "k00000000001", "--role", "Operator", "--tenant", "lab1"), 0,
		"key k00000000001 now holds operator on tenant lab1 (before: reader)\n", "")
	doc := document(t, r.run("grant", "set", "k00000000001", "--role", "admin", "--installation", "--json"))
	if doc["grant"].(map[string]any)["role"] != "ROLE_ADMIN" || doc["previousRole"] != "ROLE_UNSPECIFIED" {
		t.Errorf("set --json: %v", doc)
	}

	expect(t, r.run("grant", "list", "--key-id", "k00000000001"), 0,
		"KEY           SCOPE         ROLE\nk00000000001  installation  admin\nk00000000001  tenant lab1   operator\n", "")
	expect(t, r.run("grant", "list", "--tenant", "lab1"), 0,
		"KEY           SCOPE        ROLE\nk00000000001  tenant lab1  operator\n", "")
	expect(t, r.run("grant", "list", "--installation"), 0,
		"KEY           SCOPE         ROLE\nk00000000001  installation  admin\n", "")
	if gs := document(t, r.run("grant", "list", "--key-id", "k00000000001", "--json"))["grants"].([]any); len(gs) != 2 {
		t.Errorf("list --json: %v", gs)
	}

	expect(t, r.run("grant", "remove", "k00000000001", "--tenant", "lab1"), 0,
		"key k00000000001 holds no grant on tenant lab1 (it was operator)\n", "")
	expect(t, r.run("grant", "remove", "k00000000001", "--tenant", "lab1"), 0,
		"key k00000000001 holds no grant on tenant lab1 (it held none)\n", "")
	if doc := document(t, r.run("grant", "remove", "k00000000001", "--installation", "--json")); doc["previousRole"] != "ROLE_ADMIN" {
		t.Errorf("remove --json: %v", doc)
	}

	// What the server refuses, shown as the server said it.
	res := r.run("grant", "set", "k00000000001", "--role", "writer", "--installation")
	expect(t, res, 2, "", "nopacs: grant set: the writer role is not given on the installation (request id req-000013)\n")
	res = r.run("grant", "set", "knosuchkey01", "--role", "reader", "--tenant", "lab1")
	expect(t, res, 4, "", "nopacs: grant set: no key has that id (request id req-000014)\n")
	r.must("key", "revoke", "k00000000001")
	res = r.run("grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1")
	expect(t, res, 1, "", "nopacs: grant set: the key is revoked (request id req-000016)\n")

	for name, args := range map[string][]string{
		"no scope":    {"grant", "set", "k1", "--role", "reader"},
		"two scopes":  {"grant", "set", "k1", "--role", "reader", "--tenant", "a", "--installation"},
		"no role":     {"grant", "set", "k1", "--installation"},
		"bad role":    {"grant", "set", "k1", "--role", "god", "--installation"},
		"list no arg": {"grant", "list"},
		"list two":    {"grant", "list", "--key-id", "k1", "--installation"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line", name, res)
		}
	}
}

// TestSettingCommands guards every setting command in both forms: where a
// value comes from (built-in default, installation, a tenant's override),
// a tenant's override and the way back from it, the installation's value and
// the way back to the built-in default, and a tenant scope refused for a
// setting set on the installation only.
func TestSettingCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	expect(t, r.run("setting", "get", "test.retention"), 0,
		"Name:  test.retention\nValue: 365d\nFrom:  built-in default\n", "")
	expect(t, r.run("setting", "get", "test.retention", "--tenant", "lab1"), 0,
		"Name:  test.retention\nValue: 365d\nFrom:  built-in default\n", "")
	expect(t, r.run("setting", "set", "test.retention", "90d", "--tenant", "lab1"), 0,
		"Name:  test.retention\nValue: 90d\nFrom:  tenant lab1 (override)\n", "")
	expect(t, r.run("setting", "get", "test.retention", "--tenant", "lab1"), 0,
		"Name:  test.retention\nValue: 90d\nFrom:  tenant lab1 (override)\n", "")
	expect(t, r.run("setting", "set", "test.retention", "400d"), 0,
		"Name:  test.retention\nValue: 400d\nFrom:  installation\n", "")
	expect(t, r.run("setting", "list", "--tenant", "lab1"), 0,
		"NAME            VALUE  FROM\ntest.retention  90d    tenant lab1 (override)\n", "")
	expect(t, r.run("setting", "list"), 0,
		"NAME                            VALUE            FROM\n"+
			"access.allowed-networks                          built-in default\n"+
			"access.block-time               15m              built-in default\n"+
			"access.denied-networks                           built-in default\n"+
			"access.failure-limit            10               built-in default\n"+
			"access.failure-window           1m               built-in default\n"+
			"access.forwarded-header         x-forwarded-for  built-in default\n"+
			"access.tenant-networks                           built-in default\n"+
			"access.trusted-proxies                           built-in default\n"+
			"api.cors-origins                                 built-in default\n"+
			"audit.retention                 0                built-in default\n"+
			"audit.seal-interval             10m              built-in default\n"+
			"backup.max-duration             6h               built-in default\n"+
			"correction.batch-max            10000            built-in default\n"+
			"deid.list-cache-bytes           256MiB           built-in default\n"+
			"deid.memory                     2GiB             built-in default\n"+
			"deid.pixel-limit                1GiB             built-in default\n"+
			"delete.grace                    7d               built-in default\n"+
			"events.retention                90d              built-in default\n"+
			"import.allowed-roots                             built-in default\n"+
			"import.max-dir-entries          200000           built-in default\n"+
			"import.max-units                4                built-in default\n"+
			"import.roots                                     built-in default\n"+
			"import.server-max-units         8                built-in default\n"+
			"incoming.limit                  0                built-in default\n"+
			"job.failures-kept               10000            built-in default\n"+
			"key.default-lifetime            90d              built-in default\n"+
			"key.expiry-warning              7d               built-in default\n"+
			"key.max-lifetime                3650d            built-in default\n"+
			"quarantine.warn-age             30d              built-in default\n"+
			"quarantine.warn-bytes           0                built-in default\n"+
			"quota.bytes                                      built-in default\n"+
			"quota.instances                                  built-in default\n"+
			"quota.warn                      90%              built-in default\n"+
			"read.per-caller                 16               built-in default\n"+
			"read.per-server                 256              built-in default\n"+
			"recall.wait                     48h              built-in default\n"+
			"receiver.allow-as-received      false            built-in default\n"+
			"receiver.bind-addresses                          built-in default\n"+
			"receiver.header-limit           64MiB            built-in default\n"+
			"receiver.ports                                   built-in default\n"+
			"replica.lag-alert               1h               built-in default\n"+
			"replica.rate                    0                built-in default\n"+
			"replica.repair                  manual           built-in default\n"+
			"replica.repair-lease            1m               built-in default\n"+
			"retention.max-clock-step        2d               built-in default\n"+
			"retention.max-per-pass          1000             built-in default\n"+
			"retention.shorten-notice        30d              built-in default\n"+
			"scrub.archive-sample            0%               built-in default\n"+
			"scrub.interval                  30d              built-in default\n"+
			"scrub.rate                      64MiB            built-in default\n"+
			"search.facet-rows               1000000          built-in default\n"+
			"sender.allow-plain-http         false            built-in default\n"+
			"sender.destinations                              built-in default\n"+
			"sender.max-associations         32               built-in default\n"+
			"sender.tenant-max-associations  8                built-in default\n"+
			"storage.directory-roots                          built-in default\n"+
			"storage.reserve                 10%,10GiB        built-in default\n"+
			"storage.s3-endpoints                             built-in default\n"+
			"test.retention                  400d             installation\n"+
			"transfer.wait                   24h              built-in default\n", "")
	expect(t, r.run("setting", "unset", "test.retention", "--tenant", "lab1"), 0,
		"tenant lab1 has no override of test.retention; now applies: 400d (installation)\n", "")
	expect(t, r.run("setting", "list", "--tenant", "lab1"), 0, "NAME  VALUE  FROM\n", "")
	expect(t, r.run("setting", "unset", "test.retention"), 0,
		"the installation has no value of test.retention; now applies: 365d (built-in default)\n", "")

	doc := document(t, r.run("setting", "get", "test.retention", "--json"))
	if s := doc["setting"].(map[string]any); s["name"] != "test.retention" || s["value"] != "365d" || s["source"] != "SETTING_SOURCE_BUILT_IN" {
		t.Errorf("get --json: %v", doc)
	}
	document(t, r.run("setting", "set", "key.default-lifetime", "30d", "--json"))
	if ss := document(t, r.run("setting", "list", "--json"))["settings"].([]any); len(ss) != 60 {
		t.Errorf("list --json: %v", ss)
	}
	document(t, r.run("setting", "unset", "test.retention", "--tenant", "lab1", "--json"))

	if res := r.run("setting", "get", "no.such"); res.code != 4 || !strings.Contains(res.stderr, "no setting has this name") {
		t.Errorf("get of an unknown name: %+v", res)
	}
	if res := r.run("setting", "set", "key.default-lifetime", "30d", "--tenant", "lab1"); res.code != 2 || res.stdout != "" {
		t.Errorf("set of an installation-only setting on a tenant: %+v", res)
	}
	if res := r.run("setting", "set", "key.default-lifetime", "30m"); res.code != 2 || !strings.Contains(res.stderr, "setting.value") {
		t.Errorf("set of a value the setting refuses: %+v", res)
	}
}

// TestAuditCommands guards audit list in both forms: the table, each filter
// reaching the server, and the refusals of a bad time or outcome.
func TestAuditCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	at := func(d string) *timestamppb.Timestamp {
		ts, err := parseRFC3339(d)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	r.fake.AddAudit(&v1.AuditEntry{Id: "3", Time: at("2026-11-17T09:00:00Z"), KeyId: "k1", Method: "/nopacs.v1.TenantService/CreateTenant", Object: "lab1", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-3"})
	r.fake.AddAudit(&v1.AuditEntry{Id: "2", Time: at("2026-11-16T09:00:00Z"), LocalUserId: "501", Method: "/nopacs.v1.AccessService/CreateKey", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_FAILED, RequestId: "req-2", Reason: "the master database does not answer"})
	r.fake.AddAudit(&v1.AuditEntry{Id: "1", Time: at("2026-11-10T09:00:00Z"), KeyId: "k2", Method: "/nopacs.v1.AccessService/RevokeKey", Object: "k1", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_REFUSED, RequestId: "req-1", Reason: "permission denied"})
	r.fake.AddAudit(&v1.AuditEntry{Id: "t1", Time: at("2026-11-16T10:00:00Z"), KeyId: "k1", Tenant: "lab1", Method: "/nopacs.v1.SettingsService/SetSetting", Object: "audit.retention", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-t1"})

	expect(t, r.run("audit", "list"), 0,
		"TIME                        WHO        METHOD                      TENANT  OBJECT  OUTCOME  REQUEST ID  REASON\n"+
			"2026-11-17 12:00:00 +03:00  k1         TenantService/CreateTenant  -       lab1    done     req-3       -\n"+
			"2026-11-16 12:00:00 +03:00  local:501  AccessService/CreateKey     -       -       failed   req-2       the master database does not answer\n"+
			"2026-11-10 12:00:00 +03:00  k2         AccessService/RevokeKey     -       k1      refused  req-1       permission denied\n", "")
	if es := document(t, r.run("audit", "list", "--json"))["entries"].([]any); len(es) != 3 {
		t.Errorf("list --json: %d entries", len(es))
	}
	// Each filter reaches the server and narrows what it answers.
	count := func(args ...string) int {
		res := r.run(append([]string{"audit", "list", "--json"}, args...)...)
		return len(document(t, res)["entries"].([]any))
	}
	if n := count("--key-id", "k1"); n != 1 {
		t.Errorf("--key-id k1: %d entries, want 1", n)
	}
	if n := count("--outcome", "refused"); n != 1 {
		t.Errorf("--outcome refused: %d entries, want 1", n)
	}
	if n := count("--since", "2026-11-16"); n != 2 {
		t.Errorf("--since 2026-11-16: %d entries, want 2", n)
	}
	if n := count("--since", "2026-11-12T00:00:00+03:00", "--until", "2026-11-17"); n != 1 {
		t.Errorf("--since and --until: %d entries, want 1", n)
	}
	if n := count("--since", "1d"); n != 1 {
		t.Errorf("--since 1d (from the fixed clock): %d entries, want 1", n)
	}
	if n := count("--tenant", "lab1"); n != 1 {
		t.Errorf("--tenant lab1: %d entries, want 1", n)
	}
	for name, args := range map[string][]string{
		"bad outcome":  {"audit", "list", "--outcome", "maybe"},
		"bad since":    {"audit", "list", "--since", "yesterday"},
		"bad until":    {"audit", "list", "--until", "2026-13-45"},
		"reversed":     {"audit", "list", "--since", "2026-11-17", "--until", "2026-11-10"},
		"zero limit":   {"audit", "list", "--limit", "0"},
		"limit + all":  {"audit", "list", "--limit", "5", "--all"},
		"unknown flag": {"audit", "list", "--db", "x"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line", name, res)
		}
	}
	expect(t, r.run("audit", "list", "--tenant", "nope"), 4, "", fmt.Sprintf("nopacs: audit list: no tenant has that name (request id req-%06d)\n", r.fake.Requests()))
}
