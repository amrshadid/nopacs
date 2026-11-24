package cli_test

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// stowToken is a made-up credential, long enough to be found by text search
// anywhere it should not be.
const stowToken = "tok-4f9a1c7e2b8d3056-EXAMPLE-ONLY" //nolint:gosec // G101: a made-up value for a test

// TestSenderCommands guards every sender command in both forms: what a
// person reads of a DIMSE and of a stow sender (a plain http one shows no
// TLS line), that a change names only what
// it changes, and that the credential of a stow sender is held by the server
// and never shown.
func TestSenderCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")

	expect(t, r.run("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104",
		"--called-aet", "ORTHANC", "--calling-aet", "NOPACS", "--enabled", "--max-associations", "4"), 0,
		"Name:             pacs\nType:             dimse\nEnabled:          yes\nHealth:           unknown (not connected yet)\n"+
			"Health since:     2026-11-17 15:00:00 +03:00\nHost:             pacs.lab.example\nPort:             104\n"+
			"Called AE title:  ORTHANC\nCalling AE title: NOPACS\nTLS:              none (plain TCP)\n"+
			"Max associations: 4\nRate bound:       no bound of its own\nConnect timeout:  30s\nResponse timeout: 5m\n"+
			"Created:          2026-11-17 15:00:00 +03:00\nUpdated:          2026-11-17 15:00:00 +03:00\n", "")

	// The credential comes from standard input, as a person pipes it in.
	add := typed(r.env, stowToken+"\n", "sender", "add", "lab1", "archive2", "--type", "stow", "--url", "https://pacs2.lab.example/dicom-web",
		"--authentication", "bearer", "--credential-stdin")
	if add.code != 0 || strings.Contains(add.stdout+add.stderr, stowToken) {
		t.Fatalf("add stow: %+v", add)
	}
	for _, want := range []string{"Type:                      stow\n", "Enabled:                   no\n", "Health:                    down (disabled: the sender is disabled)\n",
		"URL:                       https://pacs2.lab.example/dicom-web\n", "Authentication:            bearer\n", "Credential held:           yes\n",
		"TLS:                       the system's roots\n", "Max instances per request: 64\n", "Max request size:          256MiB\n"} {
		if !strings.Contains(add.stdout, want) {
			t.Errorf("add stow lacks %q:\n%s", want, add.stdout)
		}
	}
	if got := r.fake.SenderCredential("lab1", "archive2"); got != stowToken {
		t.Errorf("the server received %q", got)
	}

	expect(t, r.run("sender", "list", "lab1"), 0,
		"NAME      TYPE   TO                                       ENABLED  HEALTH\n"+
			"archive2  stow   https://pacs2.lab.example/dicom-web      no       down (disabled: the sender is disabled)\n"+
			"pacs      dimse  pacs.lab.example:104 (AE title ORTHANC)  yes      unknown (not connected yet)\n", "")
	list := document(t, r.run("sender", "list", "lab1", "--json"))
	if ss, _ := list["senders"].([]any); len(ss) != 2 || list["more"] != false {
		t.Errorf("list --json: %v", list)
	}

	doc := document(t, r.run("sender", "show", "lab1", "archive2", "--json"))
	stow := doc["sender"].(map[string]any)["stow"].(map[string]any)
	if stow["credentialSet"] != true || stow["credential"] != "" {
		t.Errorf("show --json: %v", stow)
	}

	// A change names only what it changes: the limit moves, the credential stays.
	set := r.run("sender", "set", "lab1", "archive2", "--max-associations", "3", "--max-bytes-per-second", "20MiB", "--enabled")
	if set.code != 0 || !strings.Contains(set.stdout, "Max associations:          3\n") || !strings.Contains(set.stdout, "Rate bound:                20MiB/s\n") || !strings.Contains(set.stdout, "Credential held:           yes\n") {
		t.Errorf("set: %+v", set)
	}
	if got := r.fake.SenderCredential("lab1", "archive2"); got != stowToken {
		t.Errorf("a change of the limit lost the credential: %q", got)
	}
	// The TLS flags change the TLS of the type the stored sender has.
	if set := r.run("sender", "set", "lab1", "pacs", "--tls-ca-file", "ca.pem", "--tls-server-name", "pacs.lab.example"); set.code != 0 ||
		!strings.Contains(set.stdout, "TLS:              CA file ca.pem, server name pacs.lab.example\n") {
		t.Errorf("set --tls-ca-file: %+v", set)
	}
	if set := r.run("sender", "set", "lab1", "pacs", "--no-tls"); set.code != 0 || !strings.Contains(set.stdout, "TLS:              none (plain TCP)\n") {
		t.Errorf("set --no-tls: %+v", set)
	}
	// A new URL goes with its credential, which the server insists on; a new
	// one from a file replaces the held one.
	if res := r.run("sender", "set", "lab1", "archive2", "--url", "https://pacs3.lab.example/dicom-web"); res.code != 2 || !strings.Contains(res.stderr, "stow.credential") {
		t.Errorf("set --url without the credential: %+v", res)
	}
	file := fakeserver.PrivateFile(t, "second-"+stowToken+"\n")
	if res := r.run("sender", "set", "lab1", "archive2", "--url", "https://pacs3.lab.example/dicom-web", "--credential-file", file); res.code != 0 ||
		strings.Contains(res.stdout+res.stderr, stowToken) || !strings.Contains(res.stdout, "https://pacs3.lab.example/dicom-web") {
		t.Errorf("set --url with the credential: %+v", res)
	}
	if got := r.fake.SenderCredential("lab1", "archive2"); got != "second-"+stowToken {
		t.Errorf("the new credential is %q", got)
	}

	// A plain http URL has no TLS, so no TLS line describes one.
	plain := r.run("sender", "add", "lab1", "lab-web", "--type", "stow", "--url", "http://web.lab.example/dicom-web", "--authentication", "none")
	if plain.code != 0 || strings.Contains(plain.stdout, "TLS:") || !strings.Contains(plain.stdout, "http://web.lab.example/dicom-web") {
		t.Errorf("add of a plain http stow sender: %+v", plain)
	}
	if show := r.run("sender", "show", "lab1", "lab-web"); show.code != 0 || strings.Contains(show.stdout, "TLS:") {
		t.Errorf("show of a plain http stow sender names TLS: %+v", show)
	}
	r.must("sender", "remove", "lab1", "lab-web", "--yes")

	expect(t, r.run("sender", "remove", "lab1", "archive2", "--yes"), 0, "sender archive2 of tenant lab1 removed; 0 jobs canceled; its jobs stay\n", "")
	if res := r.run("sender", "show", "lab1", "archive2"); res.code != 4 {
		t.Errorf("the sender is still there: %+v", res)
	}
	rdoc := document(t, r.run("sender", "remove", "lab1", "pacs", "--yes", "--json"))
	if rdoc["sender"].(map[string]any)["name"] != "pacs" {
		t.Errorf("remove --json: %v", rdoc)
	}
}

// TestSenderCredentialSources guards where a stow credential comes from and
// where it goes: only from standard input, the variable or the private file a
// flag names, to the server in the body of AddSender and nowhere else, never
// printed; and a refused source is a usage error that sends nothing and does
// not quote the credential.
func TestSenderCredentialSources(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.env["STOWSECRET"] = stowToken
	base := []string{"sender", "add", "lab1", "x", "--type", "stow", "--url", "https://pacs2.lab.example/dicom-web", "--authentication", "header", "--header-name", "X-Api-Key"}

	for name, extra := range map[string][]string{"env": {"--credential-env", "STOWSECRET"}, "file": {"--credential-file", fakeserver.PrivateFile(t, stowToken+"\n")}} {
		res := r.run(append(append([]string{}, base...), extra...)...)
		res2 := r.run(append(append([]string{}, base...), extra...)...) // a second add of the name is a conflict
		if res.code != 0 || strings.Contains(res.stdout+res.stderr, stowToken) || res2.code != 1 {
			t.Fatalf("add from %s: %+v then %+v", name, res, res2)
		}
		if got := r.fake.SenderCredential("lab1", "x"); got != stowToken {
			t.Errorf("the server received %q from %s", got, name)
		}
		r.must("sender", "remove", "lab1", "x", "--yes")
	}
	// Every request that carried the credential was a call that takes it.
	for _, s := range r.fake.SeenRequests() {
		if strings.Contains(string(s.Body), stowToken) && !strings.HasSuffix(s.URL, ".SenderService/AddSender") {
			t.Errorf("the credential was sent to %s", s.URL)
		}
		if strings.Contains(s.URL, stowToken) {
			t.Errorf("the credential is in a URL: %s", s.URL)
		}
	}

	wide := t.TempDir() + "/cred"
	writeWide(t, wide, stowToken+"\n")
	empty := fakeserver.PrivateFile(t, "\n")
	cases := map[string]struct {
		in    string
		extra []string
		want  string
	}{
		"unset variable":       {"", []string{"--credential-env", "NOPE"}, "--credential-env names NOPE, which is not set or is empty"},
		"file others can read": {"", []string{"--credential-file", wide}, "the credential file " + wide + " may be opened by other users"},
		"missing file":         {"", []string{"--credential-file", t.TempDir() + "/none"}, "it does not exist"},
		"empty file":           {"", []string{"--credential-file", empty}, "is empty"},
		"empty stdin":          {"\n", []string{"--credential-stdin"}, "standard input held no credential"},
		"two sources":          {"", []string{"--credential-env", "STOWSECRET", "--credential-file", empty}, "none of the others can be"},
	}
	before := r.fake.Requests()
	for name, c := range cases {
		res := typed(r.env, c.in, append(append([]string{}, base...), c.extra...)...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, c.want) || strings.Count(res.stderr, "\n") != 1 || strings.Contains(res.stderr, stowToken) {
			t.Errorf("%s: %+v, want exit 2 and one line with %q", name, res, c.want)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent for a refused credential", got-before)
	}
}

// TestSenderUsageErrors guards what is refused before anything is sent: a
// type that is not one, the flags of the other type, a size that is not one,
// and a change that changes nothing; and what the server refuses in its own
// words, with the exit code of its kind.
func TestSenderUsageErrors(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	cases := map[string]struct {
		args []string
		code int
		want string
	}{
		"bad type":     {[]string{"sender", "add", "lab1", "x", "--type", "tape"}, 2, "--type \"tape\" is not a sender type; use dimse or stow"},
		"missing type": {[]string{"sender", "add", "lab1", "x", "--host", "h"}, 2, "required flag(s) \"type\" not set"},
		"dimse with stow flags": {[]string{"sender", "add", "lab1", "x", "--type", "dimse", "--url", "https://h"}, 2,
			"--type dimse takes --host, --port, --called-aet and --calling-aet"},
		"stow with dimse flags": {[]string{"sender", "add", "lab1", "x", "--type", "stow", "--port", "104"}, 2, "--type stow takes --url and --authentication"},
		"bad size":              {[]string{"sender", "add", "lab1", "x", "--type", "dimse", "--max-bytes-per-second", "5GB"}, 2, "--max-bytes-per-second \"5GB\""},
		"bad authentication":    {[]string{"sender", "add", "lab1", "x", "--type", "stow", "--authentication", "digest"}, 2, "--authentication \"digest\" is not one of: none, basic, bearer, header"},
		"no authentication":     {[]string{"sender", "add", "lab1", "x", "--type", "stow", "--url", "https://h/dicom-web"}, 2, "authentication: is required"},
		"set nothing":           {[]string{"sender", "set", "lab1", "x"}, 2, "nothing to change"},
		"tls both":              {[]string{"sender", "set", "lab1", "x", "--no-tls", "--tls-ca-file", "ca.pem"}, 2, "none of the others can be"},
		"remove no yes":         {[]string{"sender", "remove", "lab1", "x"}, 2, "nothing was removed: this would remove sender \"x\" of tenant \"lab1\" and its credential; its jobs stay; run it again with --yes to do it"},
		"no sender":             {[]string{"sender", "show", "lab1", "ghost"}, 4, "the tenant has no sender with that name"},
		"test no sender":        {[]string{"sender", "test", "lab1", "ghost"}, 4, "the tenant has no sender with that name"},
	}
	for name, c := range cases {
		res := r.run(c.args...)
		if res.code != c.code || res.stdout != "" || !strings.Contains(res.stderr, c.want) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit %d and one line with %q", name, res, c.code, c.want)
		}
	}
}

// TestSenderTest guards what a person reads of a peer test: the table of steps
// with the peer's answer, the sentence in plain words for a DIMSE peer and for
// a stow peer, and that a failed step is exit 1 with the step and its fault said.
func TestSenderTest(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104", "--called-aet", "ORTHANC", "--calling-aet", "NOPACS")
	r.must("sender", "add", "lab1", "web", "--type", "stow", "--url", "https://pacs2.lab.example/dicom-web", "--authentication", "none")

	expect(t, r.run("sender", "test", "lab1", "pacs"), 0,
		"STEP       OUTCOME  TOOK  FAULT  DETAIL\nresolve    passed   3ms   -      -\nconnect    passed   3ms   -      -\n"+
			"associate  passed   3ms   -      -\nrequest    passed   3ms   -      0000\nrelease    passed   3ms   -      -\n"+
			"the peer accepted the association and answered the C-ECHO with status 0000\n", "")
	expect(t, r.run("sender", "test", "lab1", "web"), 0,
		"STEP     OUTCOME  TOOK  FAULT  DETAIL\nresolve  passed   3ms   -      -\nconnect  passed   3ms   -      -\nrequest  passed   3ms   -      HTTP 200\n"+
			"the peer answered the request and takes the credential (HTTP 200)\n", "")
	tdoc := document(t, r.run("sender", "test", "lab1", "pacs", "--json"))
	if tdoc["passed"] != true || len(tdoc["results"].([]any)) != 5 {
		t.Errorf("test --json: %v", tdoc)
	}

	r.fake.SetSenderTest("lab1", "pacs", []*v1.SenderTestResult{
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_RESOLVE, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED, Duration: durationpb.New(2e6)},
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_CONNECT, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED, Duration: durationpb.New(2e6)},
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_ASSOCIATE, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED, Duration: durationpb.New(2e6),
			Fault: v1.SenderFault_SENDER_FAULT_REJECTED, Detail: "rejected by the peer: called AE title not recognized"},
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_REQUEST, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_SKIPPED},
	})
	expect(t, r.run("sender", "test", "lab1", "pacs"), 1,
		"STEP       OUTCOME  TOOK  FAULT     DETAIL\nresolve    passed   2ms   -         -\nconnect    passed   2ms   -         -\n"+
			"associate  failed   2ms   rejected  rejected by the peer: called AE title not recognized\nrequest    skipped  -     -         -\n"+
			"the peer was not reached: step associate failed (rejected): rejected by the peer: called AE title not recognized\n",
		"nopacs: sender pacs of tenant lab1 failed its test\n")

	// A stow peer that answers 401 failed its request step, and the sentence
	// carries the HTTP status.
	r.fake.SetSenderTest("lab1", "web", []*v1.SenderTestResult{
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_CONNECT, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED, Duration: durationpb.New(2e6)},
		{Step: v1.SenderTestStep_SENDER_TEST_STEP_REQUEST, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED, Duration: durationpb.New(2e6),
			Fault: v1.SenderFault_SENDER_FAULT_REJECTED, Detail: "HTTP 401"},
	})
	res := r.run("sender", "test", "lab1", "web")
	if res.code != 1 || !strings.Contains(res.stdout, "the peer was not reached: step request failed (rejected): HTTP 401\n") {
		t.Errorf("a 401: %+v", res)
	}
}

// TestSenderRemoveRefusals guards what a removal refuses: a sender a receiver
// forwards to, and one with a job in progress unless the jobs are canceled with
// it; the exit code is the one of a state that forbids the change.
func TestSenderRemoveRefusals(t *testing.T) {
	r := newArchiveRig(t)
	r.must("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104", "--called-aet", "ORTHANC", "--calling-aet", "NOPACS", "--enabled")
	r.must("receiver", "set", "lab1", "scanner1", "--forward-to", "pacs")
	if res := r.run("receiver", "show", "lab1", "scanner1"); !strings.Contains(res.stdout, "Forwards to:       pacs\n") {
		t.Errorf("receiver show:\n%s", res.stdout)
	}
	if res := r.run("sender", "remove", "lab1", "pacs", "--yes"); res.code != 5 || !strings.Contains(res.stderr, "forwards to this sender") {
		t.Errorf("a receiver forwards to it: %+v", res)
	}
	r.must("receiver", "set", "lab1", "scanner1", "--forward-to", "")
	job := r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	if !strings.Contains(job.stdout, "State:") {
		t.Fatalf("job create: %+v", job)
	}
	if res := r.run("sender", "remove", "lab1", "pacs", "--yes"); res.code != 5 || !strings.Contains(res.stderr, "queued or running") {
		t.Errorf("a job is in progress: %+v", res)
	}
	expect(t, r.run("sender", "remove", "lab1", "pacs", "--yes", "--cancel-jobs"), 0, "sender pacs of tenant lab1 removed; 1 jobs canceled; its jobs stay\n", "")
	if res := r.run("receiver", "add", "lab1", "r2", "--storage", "main", "--port", "11115", "--aet", "RECV2", "--forward-to", "gone"); res.code != 5 || !strings.Contains(res.stderr, "no sender named gone") {
		t.Errorf("forward to a sender that is not there: %+v", res)
	}
}

// writeWide writes a file that other users may read, the mode a credential
// file must be refused for.
func writeWide(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: the mode is the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the mode is the point of the test.
		t.Fatal(err)
	}
}
