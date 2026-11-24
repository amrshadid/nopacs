package cli_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// seedQuarantine puts four records in lab1: two held (a bad label with a
// header, a too-large file whose bytes were never kept), one rejected and one
// released.
func seedQuarantine(r *rig) {
	caller := &v1.Caller{CallingAeTitle: "PRISMA1", CalledAeTitle: "NOPACS", Address: "192.0.2.7:51234"}
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-label", Reason: v1.QuarantineReason_QUARANTINE_REASON_BAD_LABEL, Details: map[string]string{"label": "subject", "fault": "missing", "attribute": "PatientID"},
		State: v1.QuarantineState_QUARANTINE_STATE_HELD, Receiver: "scanner1", Reception: "rcp-1", Caller: caller, SizeBytes: 2048, Sha256: strings.Repeat("ab", 32),
		SopInstanceUid: "1.2.3.4", StudyInstanceUid: "1.2.3", ContentKept: true, QuarantineTime: timestamppb.New(fixedNow),
	}, `{"00080060":{"vr":"CS","Value":["MR"]}}`)
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-large", Reason: v1.QuarantineReason_QUARANTINE_REASON_TOO_LARGE, Details: map[string]string{"limit": "1024"},
		State: v1.QuarantineState_QUARANTINE_STATE_HELD, Receiver: "scanner1", Caller: caller, SizeBytes: 1024, QuarantineTime: timestamppb.New(fixedNow.Add(-time.Hour)),
	}, "")
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-rejected", Reason: v1.QuarantineReason_QUARANTINE_REASON_NOT_DICOM, State: v1.QuarantineState_QUARANTINE_STATE_REJECTED, RejectReason: "a text file",
		Receiver: "scanner1", SizeBytes: 10, QuarantineTime: timestamppb.New(fixedNow.Add(-2 * time.Hour)), DecideTime: timestamppb.New(fixedNow.Add(-time.Hour)),
	}, "")
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-released", Reason: v1.QuarantineReason_QUARANTINE_REASON_LABEL_CONFLICT, State: v1.QuarantineState_QUARANTINE_STATE_RELEASED, ReleaseReceiver: "scanner1",
		ReleaseReception: "rcp-9", ReleaseMode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Receiver: "scanner1", SizeBytes: 20, ContentKept: true,
		QuarantineTime: timestamppb.New(fixedNow.Add(-3 * time.Hour)), DecideTime: timestamppb.New(fixedNow.Add(-time.Hour)),
	}, "")
}

// TestQuarantineListAndShow guards the two read commands: the table of what is
// held and why, every filter that narrows it, the record with the details of
// its reason, and the header only when asked for.
func TestQuarantineListAndShow(t *testing.T) {
	r := newArchiveRig(t)
	seedQuarantine(r)

	expect(t, r.run("quarantine", "list", "lab1", "--state", "held"), 0,
		"ID       STATE  REASON     RECEIVER  RECEPTION  BYTES  QUARANTINED\n"+
			"q-label  held   bad label  scanner1  rcp-1      2048   2026-11-17 15:00:00 +03:00\n"+
			"q-large  held   too large  scanner1  -          1024   2026-11-17 14:00:00 +03:00\n", "")
	ids := func(args ...string) string {
		t.Helper()
		doc := document(t, r.run(append([]string{"quarantine", "list", "lab1", "--json"}, args...)...))
		var got []string
		for _, x := range doc["files"].([]any) {
			got = append(got, x.(map[string]any)["id"].(string))
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "q-label,q-large,q-rejected,q-released"},
		{[]string{"--state", "rejected"}, "q-rejected"},
		{[]string{"--reason", "too-large"}, "q-large"},
		{[]string{"--reason", "label-conflict", "--state", "released"}, "q-released"},
		{[]string{"--reception", "rcp-1"}, "q-label"},
		{[]string{"--study-uid", "1.2.3"}, "q-label"},
		{[]string{"--sop-uid", "1.2.3.4"}, "q-label"},
		{[]string{"--since", "90m"}, "q-label,q-large"},
		{[]string{"--until", "2026-11-17T14:30:00+03:00"}, "q-large,q-rejected,q-released"},
		{[]string{"--receiver", "other"}, ""},
	} {
		if got := ids(c.args...); got != c.want {
			t.Errorf("quarantine list %v: %q, want %q", c.args, got, c.want)
		}
	}

	expect(t, r.run("quarantine", "show", "lab1", "q-label"), 0,
		"ID:               q-label\nState:            held\nReason:           bad label\n"+
			"  attribute:      PatientID\n  fault:          missing\n  label:          subject\n"+
			"Receiver:         scanner1\nReception:        rcp-1\nCalling AE title: PRISMA1\nCalled AE title:  NOPACS\nAddress:          192.0.2.7:51234\n"+
			"Size:             2048 bytes\nSHA-256:          "+strings.Repeat("ab", 32)+"\nSOP class:        -\nSOP instance:     1.2.3.4\nStudy:            1.2.3\n"+
			"Series:           -\nTransfer syntax:  -\nBytes held:       yes\nQuarantined:      2026-11-17 15:00:00 +03:00\nDecided:          -\n", "")
	with := r.run("quarantine", "show", "lab1", "q-label", "--header")
	if !strings.HasSuffix(with.stdout, "\nHeader:\n{\"00080060\":{\"vr\":\"CS\",\"Value\":[\"MR\"]}}\n") {
		t.Errorf("show --header:\n%s", with.stdout)
	}
	if res := r.run("quarantine", "show", "lab1", "q-large", "--header"); !strings.HasSuffix(res.stdout, "\nHeader:\n(not available: the file's bytes are gone or its data set could not be read)\n") {
		t.Errorf("a header that is not there:\n%s", res.stdout)
	}
	if res := r.run("quarantine", "show", "lab1", "q-rejected"); !strings.Contains(res.stdout, "Reject reason:    a text file\n") || !strings.Contains(res.stdout, "Bytes held:       no\n") {
		t.Errorf("a rejected record:\n%s", res.stdout)
	}
	if res := r.run("quarantine", "show", "lab1", "q-released"); !strings.Contains(res.stdout, "Released at:      scanner1\n") || !strings.Contains(res.stdout, "Released into:    rcp-9\n") {
		t.Errorf("a released record:\n%s", res.stdout)
	}
	// The header is asked for only with the flag, whatever the document form.
	plain := document(t, r.run("quarantine", "show", "lab1", "q-label", "--json"))
	if plain["headerJson"] != "" {
		t.Errorf("a header was answered without being asked for: %v", plain["headerJson"])
	}
	asked := document(t, r.run("quarantine", "show", "lab1", "q-label", "--header", "--json"))
	if !strings.Contains(asked["headerJson"].(string), "00080060") || asked["file"].(map[string]any)["reason"] != "QUARANTINE_REASON_BAD_LABEL" {
		t.Errorf("show --header --json: %v", asked)
	}
}

// TestQuarantineListUsageErrors guards what is refused before anything is
// sent: a state or reason that is not one, with the choices.
func TestQuarantineListUsageErrors(t *testing.T) {
	r := newArchiveRig(t)
	before := r.fake.Requests()
	res := r.run("quarantine", "list", "lab1", "--state", "kept")
	if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "--state \"kept\" is not one of: held, released, rejected") {
		t.Errorf("a bad state: %+v", res)
	}
	res = r.run("quarantine", "list", "lab1", "--reason", "odd")
	if res.code != 2 || !strings.Contains(res.stderr, "--reason \"odd\" is not one of: not-dicom, sop-class-not-accepted, too-large, bad-uid, bad-label, label-conflict, different-file, uid-elsewhere, storage-refused, read-back-mismatch, header-too-large, incoming-lost") {
		t.Errorf("a bad reason: %+v", res)
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent", got-before)
	}
}

// TestQuarantineRelease guards the release command: a held file joins a
// reception, the output says why it had been held, a repeat answers as it
// stands, a file whose bytes were never kept or that was rejected is a refusal
// (exit 5), and an unknown id is 4.
func TestQuarantineRelease(t *testing.T) {
	r := newArchiveRig(t)
	seedQuarantine(r)
	expect(t, r.run("quarantine", "release", "lab1", "q-label"), 0,
		"file q-label released (it was quarantined: bad label) into reception rec-released at receiver scanner1\n", "")
	doc := document(t, r.run("quarantine", "release", "lab1", "q-label", "--json"))
	f := doc["file"].(map[string]any)
	if f["state"] != "QUARANTINE_STATE_RELEASED" || f["releaseMode"] != "RELEASE_MODE_AS_IS" || f["releaseReception"] != "rec-released" {
		t.Errorf("release --json: %v", f)
	}
	for id, want := range map[string]int{"q-large": 5, "q-rejected": 5, "nope": 4} {
		if res := r.run("quarantine", "release", "lab1", id); res.code != want || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("release of %s: %+v, want exit %d", id, res, want)
		}
	}
	if res := r.run("quarantine", "release", "lab1", "q-large", "--receiver", "nope"); res.code != 5 {
		t.Errorf("release at a receiver that does not exist: %+v", res)
	}
}

// TestQuarantineReleaseReason guards the reason a release may carry: it is sent
// with the request and kept with the record, "quarantine show" prints it only
// for a record that has one, a reason of only spaces is refused before anything
// is sent, one past 1024 bytes is the server's refusal (exit 2), and a file
// already released keeps the reason of its first release.
func TestQuarantineReleaseReason(t *testing.T) {
	r := newArchiveRig(t)
	seedQuarantine(r)
	before := r.fake.Requests()
	if res := r.run("quarantine", "release", "lab1", "q-label", "--reason", "   "); res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "--reason must say why") {
		t.Errorf("a reason of spaces: %+v", res)
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent for a reason of spaces", got-before)
	}
	if res := r.run("quarantine", "release", "lab1", "q-label", "--reason", strings.Repeat("x", 1025)); res.code != 2 || !strings.Contains(res.stderr, "reason: is 1 to 1024 bytes") {
		t.Errorf("a reason of 1025 bytes: %+v", res)
	}
	if show := r.run("quarantine", "show", "lab1", "q-label"); !strings.Contains(show.stdout, "State:            held\n") {
		t.Errorf("a refused release changed the record:\n%s", show.stdout)
	}

	expect(t, r.run("quarantine", "release", "lab1", "q-label", "--reason", "naming rule fixed"), 0,
		"file q-label released (it was quarantined: bad label) into reception rec-released at receiver scanner1\n", "")
	if show := r.run("quarantine", "show", "lab1", "q-label"); !strings.Contains(show.stdout, "Released into:    rec-released\nRelease reason:   naming rule fixed\n") {
		t.Errorf("a released record with its reason:\n%s", show.stdout)
	}
	doc := document(t, r.run("quarantine", "show", "lab1", "q-label", "--json"))
	if doc["file"].(map[string]any)["releaseReason"] != "naming rule fixed" {
		t.Errorf("show --json: %v", doc["file"])
	}
	// A second release, with another reason, answers as the record stands.
	again := document(t, r.run("quarantine", "release", "lab1", "q-label", "--reason", "another", "--json"))
	if again["file"].(map[string]any)["releaseReason"] != "naming rule fixed" {
		t.Errorf("a repeated release replaced the reason: %v", again["file"])
	}
	// Optional: a release without one leaves none, and show prints no line for it.
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-plain", Reason: v1.QuarantineReason_QUARANTINE_REASON_BAD_UID, State: v1.QuarantineState_QUARANTINE_STATE_HELD,
		Receiver: "scanner1", SizeBytes: 5, ContentKept: true, QuarantineTime: timestamppb.New(fixedNow),
	}, "")
	r.must("quarantine", "release", "lab1", "q-plain", "--receiver", "scanner1")
	if show := r.run("quarantine", "show", "lab1", "q-plain"); !strings.Contains(show.stdout, "State:            released\n") || strings.Contains(show.stdout, "Release reason") {
		t.Errorf("a record without a release reason shows one:\n%s", show.stdout)
	}
}

// TestQuarantineReleaseProfile guards --profile on a release: the version
// named is sent with the request and "quarantine show" prints it with the
// storage named; a version that is not NAME@VERSION is refused before
// anything is sent (exit 2).
func TestQuarantineReleaseProfile(t *testing.T) {
	r := newArchiveRig(t)
	seedQuarantine(r)
	before := r.fake.Requests()
	for _, bad := range []string{"research", "research@0", "Research@1"} {
		if res := r.run("quarantine", "release", "lab1", "q-label", "--profile", bad); res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "is not NAME@VERSION") {
			t.Errorf("--profile %s: %+v", bad, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent for a version not well formed", got-before)
	}
	r.must("quarantine", "release", "lab1", "q-label", "--profile", "research@2", "--storage", "lab-a")
	show := r.run("quarantine", "show", "lab1", "q-label")
	if !strings.Contains(show.stdout, "Release storage:  lab-a\nRelease profile:  research@2\n") {
		t.Errorf("a record released through a version:\n%s", show.stdout)
	}
	doc := document(t, r.run("quarantine", "show", "lab1", "q-label", "--json"))
	if pv, _ := doc["file"].(map[string]any)["releaseProfile"].(map[string]any); pv["name"] != "research" || pv["version"] != float64(2) {
		t.Errorf("show --json: %v", doc["file"])
	}
}

// TestQuarantineNewReasons guards the words, the filter, the choices in a usage
// error and the shell completion for the reasons of a header past the bound
// and of an incoming copy that was lost: a held header-too-large file can be
// released, an incoming-lost one has no bytes and cannot (exit 5).
func TestQuarantineNewReasons(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-header", Reason: v1.QuarantineReason_QUARANTINE_REASON_HEADER_TOO_LARGE, Details: map[string]string{"limit": "1048576"},
		State: v1.QuarantineState_QUARANTINE_STATE_HELD, Receiver: "scanner1", SizeBytes: 3000000, ContentKept: true, QuarantineTime: timestamppb.New(fixedNow),
	}, "")
	r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{
		Id: "q-lost", Reason: v1.QuarantineReason_QUARANTINE_REASON_INCOMING_LOST,
		State: v1.QuarantineState_QUARANTINE_STATE_HELD, Receiver: "scanner1", SizeBytes: 500, QuarantineTime: timestamppb.New(fixedNow.Add(-time.Hour)),
	}, "")
	expect(t, r.run("quarantine", "list", "lab1"), 0,
		"ID        STATE  REASON            RECEIVER  RECEPTION  BYTES    QUARANTINED\n"+
			"q-header  held   header too large  scanner1  -          3000000  2026-11-17 15:00:00 +03:00\n"+
			"q-lost    held   incoming lost     scanner1  -          500      2026-11-17 14:00:00 +03:00\n", "")
	if res := r.run("quarantine", "list", "lab1", "--reason", "incoming-lost", "--json"); !strings.Contains(res.stdout, `"id": "q-lost"`) || strings.Contains(res.stdout, "q-header") {
		t.Errorf("the filter --reason incoming-lost: %+v", res)
	}
	if res := r.run("quarantine", "show", "lab1", "q-header"); !strings.Contains(res.stdout, "Reason:           header too large\n  limit:          1048576\n") {
		t.Errorf("show of a header-too-large record:\n%s", res.stdout)
	}
	if res := r.run("quarantine", "release", "lab1", "q-lost"); res.code != 5 {
		t.Errorf("release of a file whose incoming copy was lost: %+v", res)
	}
	if res := r.run("quarantine", "release", "lab1", "q-header"); res.code != 0 || !strings.Contains(res.stdout, "(it was quarantined: header too large)") {
		t.Errorf("release of a header-too-large file: %+v", res)
	}
	offered := runWith(nil, "__complete", "quarantine", "list", "lab1", "--reason", "").stdout
	for _, want := range []string{"header-too-large\n", "incoming-lost\n"} {
		if !strings.Contains(offered, want) {
			t.Errorf("--reason completion does not offer %q:\n%s", want, offered)
		}
	}
}

// TestQuarantineReject guards the reject command: the reason is required and
// kept, the id is typed again unless --yes, a wrong id sends nothing, a repeat
// answers with the first reason, and a released file is a refusal (exit 5).
func TestQuarantineReject(t *testing.T) {
	r := newArchiveRig(t)
	seedQuarantine(r)
	before := r.fake.Requests()

	expect(t, r.run("quarantine", "reject", "lab1", "q-label", "--reason", "not our study"), 2, "",
		"nopacs: quarantine reject: nothing was rejected: this would remove for good the bytes of quarantined file \"q-label\"; its record stays; run it again with --yes to do it\n")
	if res := r.run("quarantine", "reject", "lab1", "q-label"); res.code != 2 || !strings.Contains(res.stderr, "required flag(s) \"reason\" not set") {
		t.Errorf("no reason: %+v", res)
	}
	if res := r.run("quarantine", "reject", "lab1", "q-label", "--reason", "", "--yes"); res.code != 2 || !strings.Contains(res.stderr, "--reason must say why") {
		t.Errorf("an empty reason: %+v", res)
	}
	for name, input := range map[string]string{"another id": "q-large\n", "nothing": ""} {
		res := typed(r.env, input, "quarantine", "reject", "lab1", "q-label", "--reason", "not our study")
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "Type the id q-label again to go on: ") || !strings.Contains(res.stderr, "nothing was rejected") {
			t.Errorf("%s typed: %+v", name, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent before the id was confirmed", got-before)
	}

	right := typed(r.env, "q-label\n", "quarantine", "reject", "lab1", "q-label", "--reason", "not our study")
	if right.code != 0 || right.stdout != "file q-label rejected: not our study; its bytes were removed and its record stays\n" {
		t.Errorf("the id typed: %+v", right)
	}
	if show := r.run("quarantine", "show", "lab1", "q-label"); !strings.Contains(show.stdout, "State:            rejected\n") || !strings.Contains(show.stdout, "Bytes held:       no\n") {
		t.Errorf("after the rejection:\n%s", show.stdout)
	}
	// Rejected already: answered as it stands, with its first reason.
	expect(t, r.run("quarantine", "reject", "lab1", "q-label", "--reason", "another", "--yes"), 0,
		"file q-label rejected: not our study; its bytes were removed and its record stays\n", "")
	doc := document(t, r.run("quarantine", "reject", "lab1", "q-large", "--reason", "too big", "--yes", "--json"))
	if doc["file"].(map[string]any)["rejectReason"] != "too big" {
		t.Errorf("reject --json: %v", doc)
	}
	if res := r.run("quarantine", "reject", "lab1", "q-released", "--reason", "x", "--yes"); res.code != 5 {
		t.Errorf("reject of a released file: %+v", res)
	}
	if res := r.run("quarantine", "reject", "lab1", "nope", "--reason", "x", "--yes"); res.code != 4 {
		t.Errorf("reject of an unknown id: %+v", res)
	}
}

// TestArchiveCommandsComplete guards the shell completion of the choices a
// person types: the names of states, reasons and steps are offered for their
// flags, as the choices the commands accept.
func TestArchiveCommandsComplete(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"quarantine", "list", "lab1", "--state", ""}, []string{"held", "released", "rejected"}},
		{[]string{"quarantine", "list", "lab1", "--reason", ""}, []string{"not-dicom", "sop-class-not-accepted", "too-large"}},
		{[]string{"reception", "list", "lab1", "--step", ""}, []string{"receiving", "complete"}},
		{[]string{"storage", "add", "lab1", "x", "--type", ""}, []string{"dir", "s3"}},
	} {
		res := runWith(nil, append([]string{"__complete"}, c.args...)...)
		lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
		var got []string
		for _, l := range lines {
			if !strings.HasPrefix(l, ":") && !strings.HasPrefix(l, "Completion ended") {
				got = append(got, l)
			}
		}
		if res.code != 0 || len(got) < len(c.want) {
			t.Errorf("%v: %+v", c.args, res)
			continue
		}
		for i, w := range c.want {
			if !strings.HasPrefix(got[i], w) {
				t.Errorf("%v: offered %q, want %q first", c.args, got, c.want)
			}
		}
	}
}
