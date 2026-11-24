package cli_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestReceiverCommands guards every receiver command in both forms: the
// table of what a receiver admits and how it is, the health a person reads,
// and the document's shape.
func TestReceiverCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")

	add := r.run("receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11112", "--aet", "NOPACS",
		"--allow-aet", "PRISMA1", "--allow-from", "10.20.0.0/24", "--allow-aet-from", "SKYRA@10.0.0.0/8", "--enabled")
	expect(t, add, 0,
		"Name:              scanner1\nType:              dimse\nEnabled:           yes\nHealth:            up\nHealth since:      2026-11-17 15:00:00 +03:00\n"+
			"Storage:           main\nPort:              11112\nBind address:      -\nAE title:          NOPACS\n"+
			"Admits:            PRISMA1; from 10.20.0.0/24; SKYRA from 10.0.0.0/8\nTLS:               none (plain TCP)\n"+
			"SOP classes:       the server's default set\nTransfer syntaxes: the server's default set\nNaming:            default\n"+
			"Quiet period:      1m\nMax open time:     1h\nMax file size:     4GiB\n"+
			"Created:           2026-11-17 15:00:00 +03:00\nUpdated:           2026-11-17 15:00:00 +03:00\n", "")

	r.must("receiver", "add", "lab1", "closed", "--storage", "main", "--port", "11113", "--aet", "CLOSED")
	expect(t, r.run("receiver", "list", "lab1"), 0,
		"NAME      PORT   AET     STORAGE  HEALTH\n"+
			"closed    11113  CLOSED  main     down (disabled: the receiver is disabled)\n"+
			"scanner1  11112  NOPACS  main     up\n", "")
	if show := r.run("receiver", "show", "lab1", "closed"); !strings.Contains(show.stdout, "Admits:            nobody\n") || !strings.Contains(show.stdout, "Enabled:           no\n") {
		t.Errorf("a receiver added with no caller admits nobody and is stopped:\n%s", show.stdout)
	}

	list := document(t, r.run("receiver", "list", "lab1", "--json"))
	if rs, _ := list["receivers"].([]any); len(rs) != 2 || list["more"] != false {
		t.Errorf("list --json: %v", list)
	}
	doc := document(t, r.run("receiver", "show", "lab1", "scanner1", "--json"))
	rec := doc["receiver"].(map[string]any)
	dimse := rec["dimse"].(map[string]any)
	if rec["enabled"] != true || dimse["port"] != float64(11112) || dimse["aeTitle"] != "NOPACS" || rec["quietPeriod"] != "60s" || rec["health"].(map[string]any)["state"] != "RECEIVER_STATE_UP" {
		t.Errorf("show --json: %v", rec)
	}

	set := r.run("receiver", "set", "lab1", "scanner1", "--quiet-period", "5m", "--max-open-time", "2h", "--max-file-size", "1GiB", "--enabled=false")
	if set.code != 0 || !strings.Contains(set.stdout, "Quiet period:      5m\n") || !strings.Contains(set.stdout, "Max open time:     2h\n") ||
		!strings.Contains(set.stdout, "Max file size:     1GiB\n") || !strings.Contains(set.stdout, "Health:            down (disabled: the receiver is disabled)\n") {
		t.Errorf("set:\n%+v", set)
	}
	document(t, r.run("receiver", "set", "lab1", "scanner1", "--port", "11120", "--json"))

	expect(t, r.run("receiver", "remove", "lab1", "closed", "--yes"), 0, "receiver closed of tenant lab1 stopped and removed; its receptions stay\n", "")
	if res := r.run("receiver", "show", "lab1", "closed"); res.code != 4 {
		t.Errorf("the receiver is still there: %+v", res)
	}
	rdoc := document(t, r.run("receiver", "remove", "lab1", "scanner1", "--yes", "--json"))
	if rdoc["receiver"].(map[string]any)["name"] != "scanner1" {
		t.Errorf("remove --json: %v", rdoc)
	}
}

// TestReceiverSTOWCommands guards a stow receiver through the commands: an
// add with --type stow sends its keys and no port or AE title, its details
// say what it takes, and a change of its keys names only stow.key_ids.
func TestReceiverSTOWCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	add := r.run("receiver", "add", "lab1", "web", "--type", "stow", "--storage", "main", "--allow-key", "k1", "--allow-key", "k2",
		"--max-request-size", "1GiB", "--enabled")
	if add.code != 0 || !strings.Contains(add.stdout, "Type:             stow\n") || !strings.Contains(add.stdout, "Takes keys:       k1, k2\n") ||
		!strings.Contains(add.stdout, "Max request size: 1GiB\n") || strings.Contains(add.stdout, "Port:") {
		t.Errorf("add --type stow:\n%+v", add)
	}
	set := r.run("receiver", "set", "lab1", "web", "--no-keys")
	if set.code != 0 || !strings.Contains(set.stdout, "Takes keys:       no key\n") || !strings.Contains(set.stdout, "Max request size: 1GiB\n") {
		t.Errorf("set --no-keys:\n%+v", set)
	}
	expect(t, r.run("receiver", "list", "lab1"), 0,
		"NAME  PORT  AET  STORAGE  HEALTH\n"+
			"web   -     -    main     up\n", "")
}

// TestReceiverSetNamesOnlyWhatChanges guards the mask a change sends: each
// field of the callers, TLS, lists and naming is replaced whole or reset the
// way its flag says, and a field without a flag keeps its value.
func TestReceiverSetNamesOnlyWhatChanges(t *testing.T) {
	r := newArchiveRig(t)
	show := func(label string, args ...string) string {
		t.Helper()
		res := r.run(append([]string{"receiver", "set", "lab1", "scanner1"}, args...)...)
		if res.code != 0 {
			t.Fatalf("%s: %+v", label, res)
		}
		return res.stdout
	}
	if out := show("any caller", "--any-caller"); !strings.Contains(out, "Admits:            any caller\n") {
		t.Errorf("--any-caller:\n%s", out)
	}
	if out := show("a list", "--allow-aet", "A1", "--allow-aet", "A2"); !strings.Contains(out, "Admits:            A1; A2\n") {
		t.Errorf("a list replaces whatever was there:\n%s", out)
	}
	if out := show("nobody", "--no-callers"); !strings.Contains(out, "Admits:            nobody\n") {
		t.Errorf("--no-callers:\n%s", out)
	}
	out := show("tls", "--tls-certificate-file", "chain.pem", "--tls-private-key-file", "private.pem")
	if !strings.Contains(out, "TLS:               certificate chain.pem, private key private.pem\n") || !strings.Contains(out, "Admits:            nobody\n") || !strings.Contains(out, "Port:              11112\n") {
		t.Errorf("TLS on, the rest as it was:\n%s", out)
	}
	if out := show("tls off", "--no-tls"); !strings.Contains(out, "TLS:               none (plain TCP)\n") {
		t.Errorf("--no-tls:\n%s", out)
	}
	if out := show("lists", "--sop-class", "1.2.840.10008.5.1.4.1.1.2", "--transfer-syntax", "1.2.840.10008.1.2.1"); !strings.Contains(out, "SOP classes:       1.2.840.10008.5.1.4.1.1.2\n") || !strings.Contains(out, "Transfer syntaxes: 1.2.840.10008.1.2.1\n") {
		t.Errorf("lists:\n%s", out)
	}
	if out := show("default lists", "--sop-class", "", "--transfer-syntax", ""); !strings.Contains(out, "SOP classes:       the server's default set\n") || !strings.Contains(out, "Transfer syntaxes: the server's default set\n") {
		t.Errorf("an empty value returns to the default set:\n%s", out)
	}
	out = show("naming", "--naming-subject", "sub-{#PatientID}", "--naming-session", "ses-{StudyDate}", "--naming-scan", "scan-{SeriesNumber}",
		"--naming-attribute", "PatientComments", "--naming-expression", "(?P<project>[^^]+)")
	if !strings.Contains(out, "Naming:            subject sub-{#PatientID}, session ses-{StudyDate}, scan scan-{SeriesNumber}; pattern (?P<project>[^^]+) on PatientComments\n") {
		t.Errorf("naming:\n%s", out)
	}
	if out := show("default naming", "--default-naming"); !strings.Contains(out, "Naming:            default\n") {
		t.Errorf("--default-naming:\n%s", out)
	}
	if out := show("storage", "--storage", "main", "--bind-address", "192.0.2.1", "--aet", "OTHER"); !strings.Contains(out, "Bind address:      192.0.2.1\n") || !strings.Contains(out, "AE title:          OTHER\n") {
		t.Errorf("bind and AE:\n%s", out)
	}
}

// TestReceiverUsageErrors guards what is refused before anything is sent: a
// receiver without its required flags, callers that contradict each other,
// halves of a pair, a length of time or size that is not one, and a change
// that changes nothing.
func TestReceiverUsageErrors(t *testing.T) {
	r := newArchiveRig(t)
	add := []string{"receiver", "add", "lab1", "x", "--storage", "main", "--port", "11114", "--aet", "X"}
	set := []string{"receiver", "set", "lab1", "scanner1"}
	with := func(base []string, more ...string) []string { return append(append([]string{}, base...), more...) }
	cases := map[string]struct {
		args []string
		want string
	}{
		"missing flags":      {[]string{"receiver", "add", "lab1", "x", "--port", "11114"}, "a receiver needs --storage, and a dimse receiver --aet"},
		"stow flag on dimse": {with(add, "--allow-key", "k1"), "give --type stow"},
		"dimse flag on stow": {[]string{"receiver", "add", "lab1", "w", "--type", "stow", "--storage", "main", "--port", "1"}, "a stow receiver has no port"},
		"unknown type":       {[]string{"receiver", "add", "lab1", "w", "--type", "web", "--storage", "main"}, "is not a receiver type"},
		"both types on set":  {with(set, "--allow-key", "k1", "--aet", "A"), "a stow receiver has no port"},
		"any with a list":    {with(add, "--any-caller", "--allow-aet", "A"), "--any-caller admits everyone"},
		"any false":          {with(set, "--any-caller=false"), "--any-caller=false is not a choice"},
		"no callers + list":  {with(set, "--no-callers", "--allow-from", "10.0.0.0/8"), "--no-callers admits nobody"},
		"both is not a rule": {with(add, "--allow-aet-from", "PRISMA1"), "is not TITLE@CIDR"},
		"half of tls":        {with(set, "--tls-certificate-file", "c.pem"), "go together: give both"},
		"tls both ways":      {with(set, "--tls-certificate-file", "c", "--tls-private-key-file", "k", "--no-tls"), "none of the others can be"},
		"bad span":           {with(add, "--quiet-period", "soon"), "--quiet-period \"soon\" is not a length of time"},
		"bad size":           {with(add, "--max-file-size", "4GB"), "--max-file-size \"4GB\" is a whole number of bytes"},
		"two templates":      {with(set, "--naming-subject", "a", "--naming-session", "b"), "three templates"},
		"half a pattern":     {with(set, "--naming-attribute", "PatientComments"), "naming-attribute"},
		"default and rule":   {with(set, "--default-naming", "--naming-subject", "a"), "none of the others can be"},
		"nothing":            {set, "nothing to change"},
		"remove no yes":      {[]string{"receiver", "remove", "lab1", "scanner1"}, "nothing was removed: this would stop receiver \"scanner1\" of tenant \"lab1\" and remove its configuration; its receptions stay; run it again with --yes to do it"},
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

// TestReceiverErrorsKeepTheirExitCodes guards the one mapping for the kinds
// the receiver commands meet: not found is 4, a storage the tenant does not
// have is 5, a name taken is 1.
func TestReceiverErrorsKeepTheirExitCodes(t *testing.T) {
	r := newArchiveRig(t)
	cases := []struct {
		args []string
		code int
		line string
	}{
		{[]string{"receiver", "show", "lab1", "nope"}, 4, "nopacs: receiver show: the tenant has no receiver with that name (request id "},
		{[]string{"receiver", "remove", "lab1", "nope", "--yes"}, 4, "nopacs: receiver remove: the tenant has no receiver with that name (request id "},
		{[]string{"receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11115", "--aet", "A"}, 1, "nopacs: receiver add: the tenant has a receiver with that name (request id "},
		{[]string{"receiver", "add", "lab1", "r2", "--storage", "none", "--port", "11115", "--aet", "A"}, 5, "nopacs: receiver add: the tenant has no storage with that name (request id "},
		{[]string{"receiver", "set", "lab1", "nope", "--port", "11115"}, 4, "nopacs: receiver set: the tenant has no receiver with that name (request id "},
	}
	for _, c := range cases {
		res := r.run(c.args...)
		if res.code != c.code || res.stdout != "" || !strings.HasPrefix(res.stderr, c.line) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("nopacs %s: %+v, want exit %d and a line starting %q", strings.Join(c.args[:2], " "), res, c.code, c.line)
		}
	}
}

// TestReceiverHealthWords guards how a receiver that is down is read: the
// reason in words with the server's detail, and a state or reason this client
// does not know shown as one it cannot name, as the contract says to.
func TestReceiverHealthWords(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.SetReceiverHealth("lab1", "scanner1", &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_DOWN, DownReason: v1.ReceiverDownReason_RECEIVER_DOWN_REASON_ADDRESS_IN_USE, Detail: "another program holds the port"})
	if res := r.run("receiver", "list", "lab1"); !strings.Contains(res.stdout, "down (address in use: another program holds the port)\n") {
		t.Errorf("a port in use:\n%s", res.stdout)
	}
	r.fake.SetReceiverHealth("lab1", "scanner1", &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_DOWN, DownReason: v1.ReceiverDownReason(99), Detail: "an odd cause"})
	if res := r.run("receiver", "list", "lab1"); !strings.Contains(res.stdout, "down (") || !strings.Contains(res.stdout, "an odd cause)\n") {
		t.Errorf("a reason this client does not know:\n%s", res.stdout)
	}
	r.fake.SetReceiverHealth("lab1", "scanner1", &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_STARTING})
	if res := r.run("receiver", "list", "lab1"); !strings.Contains(res.stdout, "starting\n") {
		t.Errorf("starting:\n%s", res.stdout)
	}
}

// TestReceiverAllowAETFromSplitsOnTheLastAt guards how --allow-aet-from reads
// TITLE@CIDR: at the last "@", since an AE title may hold one and a range never
// does, so a title with an "@" in it reaches the server whole; and a value
// that lacks its title or its range is a usage error that names the missing
// part and sends nothing.
func TestReceiverAllowAETFromSplitsOnTheLastAt(t *testing.T) {
	r := newArchiveRig(t)
	before := r.fake.Requests()
	add := []string{"receiver", "add", "lab1", "at", "--storage", "main", "--port", "11116", "--aet", "AT"}
	for value, want := range map[string]string{
		"@10.0.0.0/8": "lacks its title",
		"PRISMA1@":    "lacks its range",
		"@":           "lacks its title",
		"PRISMA1":     "is not TITLE@CIDR",
		"":            "is not TITLE@CIDR",
	} {
		res := r.run(append(append([]string{}, add...), "--allow-aet-from", value)...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, want) || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("--allow-aet-from %q: %+v, want exit 2 and one line with %q", value, res, want)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent for a malformed rule", got-before)
	}

	res := r.run(append(append([]string{}, add...), "--allow-aet-from", "A@B@10.0.0.0/8", "--json")...)
	if res.code != 0 {
		t.Fatalf("a title with an @: %+v", res)
	}
	rules := document(t, res)["receiver"].(map[string]any)["dimse"].(map[string]any)["allowedCallers"].(map[string]any)["rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["callingAeTitle"] != "A@B" || rules[0].(map[string]any)["addressRange"] != "10.0.0.0/8" {
		t.Errorf("the rule sent: %v", rules)
	}
}
