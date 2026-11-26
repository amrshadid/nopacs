package cli_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// s3Secret is a made-up secret access key, long enough to be found by text
// search anywhere it should not be.
const s3Secret = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY0" //nolint:gosec // G101: a made-up value for a test

// s3Args adds an S3 storage whose secret comes from the variable S3SECRET.
var s3Args = []string{"storage", "add", "lab1", "cold", "--type", "s3", "--endpoint", "https://s3.example", "--region", "eu-central-1",
	"--bucket", "lab1-archive", "--prefix", "nopacs/", "--access-key-id", "AKIAEXAMPLE", "--credential-env", "S3SECRET", "--capacity", "2TiB"}

// TestStorageCommands guards every storage command in both forms: what the
// table says, what the document holds, and that an S3 storage answers only
// whether a secret is held.
func TestStorageCommands(t *testing.T) {
	r := newRig(t)
	r.env["S3SECRET"] = s3Secret
	r.must("tenant", "create", "lab1")

	expect(t, r.run("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast", "--path", "main"), 0,
		"Name:              main\nType:              directory\nRoot:              fast\nPath:              main\nEncrypted at rest: not declared\n"+
			"Tier:              online\nReplica of:        -\nSwitch generation: 0\nCapacity:          no limit\n"+
			"Created:           2026-11-17 15:00:00 +03:00\nUpdated:           2026-11-17 15:00:00 +03:00\n", "")
	expect(t, r.run(s3Args...), 0,
		"Name:              cold\nType:              s3\nEndpoint:          https://s3.example\nRegion:            eu-central-1\n"+
			"Bucket:            lab1-archive\nPrefix:            nopacs/\nAddressing:        host name\nOlder versions:    deleted by a purge\nAccess key id:     AKIAEXAMPLE\n"+
			"Secret access key: set\nTier:              online\nReplica of:        -\nSwitch generation: 0\nCapacity:          2TiB\nCreated:           2026-11-17 15:00:00 +03:00\nUpdated:           2026-11-17 15:00:00 +03:00\n", "")

	expect(t, r.run("storage", "list", "lab1"), 0,
		"NAME  TYPE       TIER    REPLICA OF  LOCATION                                               CAPACITY  CREATED\n"+
			"cold  s3         online  -           https://s3.example bucket lab1-archive prefix nopacs/  2TiB      2026-11-17 15:00:00 +03:00\n"+
			"main  directory  online  -           root fast, path main                                   no limit  2026-11-17 15:00:00 +03:00\n", "")
	list := document(t, r.run("storage", "list", "lab1", "--json"))
	if ss, _ := list["storages"].([]any); len(ss) != 2 || list["more"] != false {
		t.Errorf("list --json: %v", list)
	}

	show := r.run("storage", "show", "lab1", "cold")
	if !strings.Contains(show.stdout, "Secret access key: set\n") {
		t.Errorf("show:\n%s", show.stdout)
	}
	doc := document(t, r.run("storage", "show", "lab1", "cold", "--json"))
	s3 := doc["storage"].(map[string]any)["s3"].(map[string]any)
	if s3["secretAccessKeySet"] != true || s3["secretAccessKey"] != "" || s3["bucket"] != "lab1-archive" {
		t.Errorf("show --json: %v", s3)
	}

	// A change names only what it changes: the limit moves, the secret stays.
	set := r.run("storage", "set", "lab1", "cold", "--capacity", "1TiB")
	if set.code != 0 || !strings.Contains(set.stdout, "Capacity:          1TiB\n") || !strings.Contains(set.stdout, "Secret access key: set\n") {
		t.Errorf("set --capacity: %+v", set)
	}
	if got := r.fake.StorageSecret("lab1", "cold"); got != s3Secret {
		t.Errorf("a change of the limit lost the secret: %q", got)
	}
	// A flag that is given changes its field even when empty.
	if set := r.run("storage", "set", "lab1", "cold", "--prefix", "", "--path-style"); set.code != 0 || !strings.Contains(set.stdout, "Prefix:            -\n") || !strings.Contains(set.stdout, "Addressing:        path\n") {
		t.Errorf("set --prefix \"\": %+v", set)
	}
	// A flag set to false changes its field too: back to the host name.
	if set := r.run("storage", "set", "lab1", "cold", "--path-style=false"); set.code != 0 || !strings.Contains(set.stdout, "Addressing:        host name\n") {
		t.Errorf("set --path-style=false: %+v", set)
	}
	document(t, r.run("storage", "set", "lab1", "main", "--path", "elsewhere", "--json"))
	// What a purge does with older versions: kept, then deleted again, and
	// the request names s3.purge_versions with the value given.
	if set := r.run("storage", "set", "lab1", "cold", "--purge-versions=false"); set.code != 0 || !strings.Contains(set.stdout, "Older versions:    kept by a purge (--purge-versions=false)\n") {
		t.Errorf("set --purge-versions=false: %+v", set)
	}
	if pv := document(t, r.run("storage", "show", "lab1", "cold", "--json"))["storage"].(map[string]any)["s3"].(map[string]any)["purgeVersions"]; pv != false {
		t.Errorf("show --json after --purge-versions=false: purgeVersions %v", pv)
	}
	if set := r.run("storage", "set", "lab1", "cold", "--purge-versions"); set.code != 0 || !strings.Contains(set.stdout, "Older versions:    deleted by a purge\n") {
		t.Errorf("set --purge-versions: %+v", set)
	}
	if set := r.run("storage", "set", "lab1", "main", "--purge-versions=false"); set.code == 0 {
		t.Errorf("--purge-versions on a directory storage was taken: %+v", set)
	}
	keeping := append(slices.Clone(s3Args[:len(s3Args)-2]), "--purge-versions=false")
	keeping[3], keeping[11] = "kept", "lab1-kept"
	if add := r.run(keeping...); add.code != 0 || !strings.Contains(add.stdout, "Older versions:    kept by a purge (--purge-versions=false)\n") {
		t.Errorf("add --purge-versions=false: %+v", add)
	}

	expect(t, r.run("storage", "test", "lab1", "main"), 0,
		"STEP     OUTCOME  TOOK  FAULT\nwrite    passed   12ms  -\nread     passed   12ms  -\ncompare  passed   12ms  -\nremove   passed   12ms  -\n", "")
	tdoc := document(t, r.run("storage", "test", "lab1", "main", "--json"))
	if tdoc["passed"] != true || len(tdoc["results"].([]any)) != 4 {
		t.Errorf("test --json: %v", tdoc)
	}

	expect(t, r.run("storage", "remove", "lab1", "cold", "--yes"), 0, "storage cold of tenant lab1 removed (s3); no file was deleted\n", "")
	if res := r.run("storage", "show", "lab1", "cold"); res.code != 4 {
		t.Errorf("the storage is still there: %+v", res)
	}
	rdoc := document(t, r.run("storage", "remove", "lab1", "main", "--yes", "--json"))
	if rdoc["storage"].(map[string]any)["name"] != "main" {
		t.Errorf("remove --json: %v", rdoc)
	}
}

// TestStorageTestFailureIsExitOne guards what a script relies on: a storage
// that fails a step still answers the steps, and the command exits 1 with one
// line, since the contract calls a failed step a result and not an error.
func TestStorageTestFailureIsExitOne(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.FailStorageTest("lab1", "main", v1.StorageTestStep_STORAGE_TEST_STEP_WRITE)
	expect(t, r.run("storage", "test", "lab1", "main"), 1,
		"STEP     OUTCOME  TOOK  FAULT\nwrite    failed   12ms  unreachable\nread     skipped  -     -\ncompare  skipped  -     -\nremove   passed   12ms  -\n",
		"nopacs: storage main of tenant lab1 failed its test\n")
}

// TestStorageSecretComesFromEnvOrFile guards where an S3 secret access key
// comes from and where it goes: only from the variable or the private file a
// flag names, to the server in the body of AddStorage and nowhere else, never
// printed, and a refused source is a usage error that sends nothing and does
// not quote the secret.
func TestStorageSecretComesFromEnvOrFile(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.env["S3SECRET"] = s3Secret
	res := r.run(s3Args...)
	if res.code != 0 || strings.Contains(res.stdout+res.stderr, s3Secret) {
		t.Fatalf("add from a variable: %+v", res)
	}
	if got := r.fake.StorageSecret("lab1", "cold"); got != s3Secret {
		t.Errorf("the server received %q", got)
	}
	file := fakeserver.PrivateFile(t, s3Secret+"\n")
	fromFile := func(name string, extra ...string) []string {
		return append([]string{"storage", "add", "lab1", name, "--type", "s3", "--endpoint", "https://s3.example", "--region", "r",
			"--bucket", "bucket", "--access-key-id", "AKIAEXAMPLE", "--credential-file", file}, extra...)
	}
	for _, form := range [][]string{nil, {"--json"}} {
		name := "warm"
		if form != nil {
			name = "warm-json"
		}
		res := r.run(fromFile(name, form...)...)
		if res.code != 0 || strings.Contains(res.stdout+res.stderr, s3Secret) {
			t.Fatalf("add from a file %v: %+v", form, res)
		}
		if got := r.fake.StorageSecret("lab1", name); got != s3Secret {
			t.Errorf("the server received %q from the file", got)
		}
	}
	// Every request that carried the secret was a call that takes it.
	for _, s := range r.fake.SeenRequests() {
		if strings.Contains(string(s.Body), s3Secret) && !strings.HasSuffix(s.URL, ".StorageService/AddStorage") {
			t.Errorf("the secret was sent to %s", s.URL)
		}
		if strings.Contains(s.URL, s3Secret) {
			t.Errorf("the secret is in a URL: %s", s.URL)
		}
	}

	wide := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(wide, []byte(s3Secret+"\n"), 0o644); err != nil { //nolint:gosec // G306: the mode is the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil { //nolint:gosec // G302: the mode is the point of the test.
		t.Fatal(err)
	}
	empty := fakeserver.PrivateFile(t, "\n")
	base := []string{"storage", "add", "lab1", "x", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "bucket", "--access-key-id", "AKIA"}
	cases := map[string]struct {
		extra []string
		want  string
	}{
		"unset variable":       {[]string{"--credential-env", "NOPE"}, "--credential-env names NOPE, which is not set or is empty"},
		"file others can read": {[]string{"--credential-file", wide}, "the credential file " + wide + " may be opened by other users"},
		"missing file":         {[]string{"--credential-file", filepath.Join(t.TempDir(), "none")}, "it does not exist"},
		"empty file":           {[]string{"--credential-file", empty}, "is empty"},
		"both":                 {[]string{"--credential-env", "S3SECRET", "--credential-file", empty}, "none of the others can be"},
		"neither":              {nil, "needs --access-key-id and its secret access key"},
	}
	before := r.fake.Requests()
	for name, c := range cases {
		res := r.run(append(append([]string{}, base...), c.extra...)...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, c.want) || strings.Count(res.stderr, "\n") != 1 || strings.Contains(res.stderr, s3Secret) {
			t.Errorf("%s: %+v, want exit 2 and one line with %q", name, res, c.want)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent for a refused secret", got-before)
	}
}

// TestStorageUsageErrors guards what is refused before anything is sent: a
// type that is not one, the flags of the other type, a size that is not one,
// a change that changes nothing, and the two credentials given apart.
func TestStorageUsageErrors(t *testing.T) {
	r := newArchiveRig(t)
	r.env["S3SECRET"] = s3Secret
	cases := map[string]struct {
		args []string
		want string
	}{
		"bad type":          {[]string{"storage", "add", "lab1", "x", "--type", "tape"}, "--type \"tape\" is not a storage type; use dir or s3"},
		"missing type":      {[]string{"storage", "add", "lab1", "x", "--root", "fast"}, "required flag(s) \"type\" not set"},
		"dir with s3":       {[]string{"storage", "add", "lab1", "x", "--type", "dir", "--root", "fast", "--bucket", "b"}, "--type dir takes --root and --path"},
		"s3 with dir":       {[]string{"storage", "add", "lab1", "x", "--type", "s3", "--root", "fast"}, "--type s3 takes --endpoint"},
		"bad size":          {[]string{"storage", "add", "lab1", "x", "--type", "dir", "--root", "fast", "--capacity", "5GB"}, "--capacity \"5GB\" is a whole number of bytes"},
		"set nothing":       {[]string{"storage", "set", "lab1", "main"}, "nothing to change"},
		"set both types":    {[]string{"storage", "set", "lab1", "main", "--root", "fast", "--bucket", "b"}, "a storage has one type"},
		"id without secret": {[]string{"storage", "set", "lab1", "main", "--access-key-id", "AKIA"}, "change together"},
		"secret without id": {[]string{"storage", "set", "lab1", "main", "--credential-env", "S3SECRET"}, "change together"},
		"too few words":     {[]string{"storage", "show", "lab1"}, "wrong number of arguments (want 2, got 1)"},
		"remove no yes":     {[]string{"storage", "remove", "lab1", "main"}, "nothing was removed: this would remove the configuration of storage \"main\" of tenant \"lab1\"; no file is deleted; run it again with --yes to do it"},
	}
	before := r.fake.Requests()
	for name, c := range cases {
		res := r.run(c.args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, c.want) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line with %q", name, res, c.want)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent", got-before)
	}
}

// TestStorageErrorsKeepTheirExitCodes guards the one mapping for the kinds
// these commands meet: a storage that is not there is 4, one a receiver names
// or that holds files is 5, a name taken is 1, a tenant that is not there is 4,
// and the server's refusal of a field is 2.
func TestStorageErrorsKeepTheirExitCodes(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.AddTenant(&v1.Tenant{Name: "lab9", State: v1.TenantState_TENANT_STATE_READY})
	r.must("storage", "add", "lab1", "old", "--type", "dir", "--root", "slow")
	r.fake.StorageHoldsFiles("lab1", "old")
	cases := []struct {
		args []string
		code int
		line string
	}{
		{[]string{"storage", "show", "lab1", "nope"}, 4, "nopacs: storage show: the tenant has no storage with this name (request id "},
		{[]string{"storage", "list", "nope"}, 4, "nopacs: storage list: no tenant has that name (request id "},
		{[]string{"storage", "add", "lab1", "main", "--type", "dir", "--root", "fast"}, 1, "nopacs: storage add: the tenant has a storage with that name (request id "},
		{[]string{"storage", "remove", "lab1", "main", "--yes"}, 5, "nopacs: storage remove: a receiver names the storage (request id "},
		{[]string{"storage", "remove", "lab1", "old", "--yes"}, 5, "nopacs: storage remove: the storage holds archived files (request id "},
		{[]string{"storage", "add", "lab1", "Bad_Name", "--type", "dir", "--root", "fast"}, 2, "nopacs: storage add: storage.name: is 1 to 63 of a-z, 0-9 and -, starting with a letter (request id "},
		{[]string{"storage", "set", "lab1", "main", "--bucket", "b"}, 2, "nopacs: storage set: update_mask: s3.bucket is a field of the other type (request id "},
	}
	for _, c := range cases {
		res := r.run(c.args...)
		if res.code != c.code || res.stdout != "" || !strings.HasPrefix(res.stderr, c.line) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("nopacs %s: %+v, want exit %d and a line starting %q", strings.Join(c.args[:2], " "), res, c.code, c.line)
		}
	}
}

// TestStorageListPages guards that a long list is fetched the way every list
// is: up to --limit, a note about more, and every page with --all.
func TestStorageListPages(t *testing.T) {
	r := newArchiveRig(t)
	for _, n := range []string{"s01", "s02", "s03"} {
		r.must("storage", "add", "lab1", n, "--type", "dir", "--root", "fast", "--path", n)
	}
	res := r.run("storage", "list", "lab1", "--limit", "2")
	if res.code != 0 || strings.Count(res.stdout, "\n") != 3 || !strings.Contains(res.stderr, "more exist; showing the first 2") {
		t.Errorf("--limit 2: %+v", res)
	}
	if res := r.run("storage", "list", "lab1", "--all"); strings.Count(res.stdout, "\n") != 5 || res.stderr != "" {
		t.Errorf("--all: %+v", res)
	}
}

// TestStorageEndpointRules guards the rules the commands meet when an S3
// storage's endpoint is involved: a change of the endpoint is refused unless it
// is given with both credentials, so credentials given for one store are never
// kept for another (exit 2, and nothing changes); with both, the endpoint and
// the secret move together; and at an endpoint the installation marks
// private-address the bucket must be addressed in the path, on add and on set
// (precondition, exit 5), and passes with --path-style.
func TestStorageEndpointRules(t *testing.T) {
	r := newRig(t)
	r.env["S3SECRET"] = s3Secret
	r.env["NEWSECRET"] = "a-different-secret-access-key-0000000"
	r.must("tenant", "create", "lab1")
	r.must(s3Args...)

	res := r.run("storage", "set", "lab1", "cold", "--endpoint", "https://other.example")
	if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "s3.endpoint is named with s3.access_key_id and s3.secret_access_key") || strings.Count(res.stderr, "\n") != 1 {
		t.Errorf("an endpoint without credentials: %+v", res)
	}
	if show := r.run("storage", "show", "lab1", "cold"); !strings.Contains(show.stdout, "Endpoint:          https://s3.example\n") {
		t.Errorf("a refused change moved the endpoint:\n%s", show.stdout)
	}
	if got := r.fake.StorageSecret("lab1", "cold"); got != s3Secret {
		t.Errorf("a refused change replaced the secret: %q", got)
	}
	res = r.run("storage", "set", "lab1", "cold", "--endpoint", "https://other.example", "--access-key-id", "AKIANEW", "--credential-env", "NEWSECRET")
	if res.code != 0 || !strings.Contains(res.stdout, "Endpoint:          https://other.example\n") || !strings.Contains(res.stdout, "Access key id:     AKIANEW\n") {
		t.Errorf("an endpoint with credentials: %+v", res)
	}
	if got := r.fake.StorageSecret("lab1", "cold"); got != r.env["NEWSECRET"] {
		t.Errorf("the secret did not move with the endpoint: %q", got)
	}

	r.must("setting", "set", "storage.s3-endpoints", "https://s3.example private-address\nhttps://other.example")
	private := append([]string{}, s3Args...)
	private[3] = "cold2"
	if res := r.run(private...); res.code != 5 || res.stdout != "" || !strings.Contains(res.stderr, "storage.s3.path_style") {
		t.Errorf("host-name addressing at a private-address endpoint: %+v", res)
	}
	if res := r.run(append(private, "--path-style")...); res.code != 0 {
		t.Errorf("path-style at a private-address endpoint: %+v", res)
	}
	if res := r.run("storage", "set", "lab1", "cold2", "--path-style=false"); res.code != 5 || !strings.Contains(res.stderr, "storage.s3.path_style") {
		t.Errorf("a change back to host-name addressing at a private-address endpoint: %+v", res)
	}
	// An endpoint without the flag takes either addressing.
	if res := r.run("storage", "set", "lab1", "cold", "--path-style"); res.code != 0 {
		t.Errorf("path-style at an ordinary endpoint: %+v", res)
	}
}
