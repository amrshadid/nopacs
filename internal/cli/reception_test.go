package cli_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// seedReceptions puts three receptions in lab1: a failed one, a finished one
// an hour older, and one still receiving.
func seedReceptions(r *rig) {
	r.fake.AddReception("lab1", &v1.Reception{
		Id: "rcp-failed", Receiver: "scanner1", StudyInstanceUid: "1.2.3.1", Arrival: 1, Step: v1.ReceptionStep_RECEPTION_STEP_FAILED,
		Failure: "storage unreachable", Counts: &v1.ReceptionCounts{FilesReceived: 10, BytesReceived: 1000, FilesArchived: 4, BytesArchived: 400},
		StartTime: timestamppb.New(fixedNow), Associations: 2, CallingAeTitles: []string{"PRISMA1"},
	})
	r.fake.AddReception("lab1", &v1.Reception{
		Id: "rcp-done", Receiver: "scanner1", StudyInstanceUid: "1.2.3.2", Arrival: 1, Step: v1.ReceptionStep_RECEPTION_STEP_DONE,
		Counts: &v1.ReceptionCounts{FilesReceived: 3, BytesReceived: 300, FilesArchived: 3, BytesArchived: 300}, SessionLabel: "ses-abc", SubjectLabel: "sub-xyz",
		StartTime: timestamppb.New(fixedNow.Add(-time.Hour)),
	})
	r.fake.AddReception("lab1", &v1.Reception{
		Id: "rcp-open", Receiver: "other", StudyInstanceUid: "1.2.3.3", Arrival: 1, Step: v1.ReceptionStep_RECEPTION_STEP_RECEIVING,
		Counts: &v1.ReceptionCounts{FilesReceived: 1}, StartTime: timestamppb.New(fixedNow.Add(-2 * time.Hour)),
	})
}

// TestReceptionListAndShow guards the two read commands: the table of what
// became of each reception, every filter that narrows it, and what show adds
// (the failure, the scans, the other arrivals of the study).
func TestReceptionListAndShow(t *testing.T) {
	r := newArchiveRig(t)
	seedReceptions(r)

	expect(t, r.run("reception", "list", "lab1"), 0,
		"ID          RECEIVER  STEP       RECEIVED  ARCHIVED  QUARANTINED  STARTED                     SESSION\n"+
			"rcp-failed  scanner1  failed     10        4         0            2026-11-17 15:00:00 +03:00  -\n"+
			"rcp-done    scanner1  done       3         3         0            2026-11-17 14:00:00 +03:00  ses-abc\n"+
			"rcp-open    other     receiving  1         0         0            2026-11-17 13:00:00 +03:00  -\n", "")
	ids := func(args ...string) string {
		t.Helper()
		res := r.run(append([]string{"reception", "list", "lab1", "--json"}, args...)...)
		doc := document(t, res)
		var got []string
		for _, x := range doc["receptions"].([]any) {
			got = append(got, x.(map[string]any)["id"].(string))
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--receiver", "other"}, "rcp-open"},
		{[]string{"--step", "failed"}, "rcp-failed"},
		{[]string{"--step", "failed", "--step", "done"}, "rcp-failed,rcp-done"},
		{[]string{"--in-progress"}, "rcp-open"},
		{[]string{"--study-uid", "1.2.3.2"}, "rcp-done"},
		{[]string{"--since", "90m"}, "rcp-failed,rcp-done"},
		{[]string{"--until", "2026-11-17T14:30:00+03:00"}, "rcp-done,rcp-open"},
		{[]string{"--receiver", "scanner1", "--step", "receiving"}, ""},
	} {
		if got := ids(c.args...); got != c.want {
			t.Errorf("reception list %v: %q, want %q", c.args, got, c.want)
		}
	}

	failed := r.run("reception", "show", "lab1", "rcp-failed")
	for _, want := range []string{"ID:                rcp-failed\n", "Step:              failed\n", "Failure:           storage unreachable\n",
		"Received:          10 files, 1000 bytes\n", "Archived:          4 files, 400 bytes\n", "Calling AE titles: PRISMA1\n", "Arrivals of this study:\n"} {
		if !strings.Contains(failed.stdout, want) {
			t.Errorf("show of a failed reception lacks %q:\n%s", want, failed.stdout)
		}
	}
	done := r.run("reception", "show", "lab1", "rcp-done")
	if !strings.Contains(done.stdout, "Scans:\nLABEL      SERIES     FILES  BYTES\nscan-aaaa  1.2.3.4.1  3      300\n") || !strings.Contains(done.stdout, "Session:           ses-abc\n") {
		t.Errorf("show of a finished reception:\n%s", done.stdout)
	}
	doc := document(t, r.run("reception", "show", "lab1", "rcp-done", "--json"))
	if len(doc["scans"].([]any)) != 1 || len(doc["arrivals"].([]any)) != 1 || doc["reception"].(map[string]any)["step"] != "RECEPTION_STEP_DONE" {
		t.Errorf("show --json: %v", doc)
	}
}

// TestReceptionListUsageErrors guards what is refused before anything is
// sent: a step that is not one (with the choices), and a span that ends before
// it begins.
func TestReceptionListUsageErrors(t *testing.T) {
	r := newArchiveRig(t)
	before := r.fake.Requests()
	res := r.run("reception", "list", "lab1", "--step", "stuck")
	if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "--step \"stuck\" is not one of: receiving, complete, identified, archived, verified, done, quarantined, failed") {
		t.Errorf("a bad step: %+v", res)
	}
	res = r.run("reception", "list", "lab1", "--since", "2026-11-17", "--until", "2026-11-16")
	if res.code != 2 || !strings.Contains(res.stderr, "--until must be after --since") {
		t.Errorf("a span backwards: %+v", res)
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent", got-before)
	}
}

// TestReceptionRetry guards the retry command: a failed reception names the
// reception that runs its files again, a repeat starts nothing more, and one
// that has not failed is a refusal (exit 5) while an unknown one is 4.
func TestReceptionRetry(t *testing.T) {
	r := newArchiveRig(t)
	seedReceptions(r)
	res := r.run("reception", "retry", "lab1", "rcp-failed")
	if res.code != 0 || !strings.HasPrefix(res.stdout, "reception rcp-failed retried as rcp-failed-retry\n\n") ||
		!strings.Contains(res.stdout, "Step:              complete\n") || !strings.Contains(res.stdout, "Retry of:          rcp-failed\n") {
		t.Errorf("retry: %+v", res)
	}
	if got := r.fake.Reception("lab1", "rcp-failed").GetRetryReception(); got != "rcp-failed-retry" {
		t.Errorf("the failed reception names %q", got)
	}
	n := len(r.fake.SeenRequests())
	again := r.run("reception", "retry", "lab1", "rcp-failed", "--json")
	doc := document(t, again)
	if doc["retry"].(map[string]any)["id"] != "rcp-failed-retry" || len(r.fake.SeenRequests()) != n+1 {
		t.Errorf("a repeated retry: %v", doc)
	}
	if res := r.run("reception", "retry", "lab1", "rcp-done"); res.code != 5 || !strings.Contains(res.stderr, "the reception has not failed") {
		t.Errorf("retry of one that did not fail: %+v", res)
	}
	if res := r.run("reception", "retry", "lab1", "nope"); res.code != 4 {
		t.Errorf("retry of an unknown id: %+v", res)
	}
	r.fake.AddReception("lab1", &v1.Reception{Id: "rcp-failed2", Receiver: "scanner1", StudyInstanceUid: "1.2.3.9", Arrival: 1, Step: v1.ReceptionStep_RECEPTION_STEP_FAILED, StartTime: timestamppb.New(fixedNow)})
	if res := r.run("reception", "retry", "lab1", "rcp-failed2", "--receiver", "nope"); res.code != 5 || !strings.Contains(res.stderr, "the receiver named does not exist") {
		t.Errorf("retry at a receiver that does not exist: %+v", res)
	}
	if res := r.run("reception", "retry", "lab1", "rcp-failed2", "--receiver", "scanner1"); res.code != 0 || !strings.Contains(res.stdout, "retried as rcp-failed2-retry") {
		t.Errorf("retry at a named receiver: %+v", res)
	}
}

// TestReceptionDiscard guards the discard command: the reason is required and
// kept, the id is typed again unless --yes, a terminal's wrong answer sends
// nothing, and a reception that was retried is a refusal (exit 5).
func TestReceptionDiscard(t *testing.T) {
	r := newArchiveRig(t)
	seedReceptions(r)
	before := r.fake.Requests()

	expect(t, r.run("reception", "discard", "lab1", "rcp-failed", "--reason", "sender resent it"), 2, "",
		"nopacs: reception discard: nothing was discarded: this would remove for good the files of reception \"rcp-failed\" that were not archived; run it again with --yes to do it\n")
	if res := r.run("reception", "discard", "lab1", "rcp-failed"); res.code != 2 || !strings.Contains(res.stderr, "required flag(s) \"reason\" not set") {
		t.Errorf("no reason: %+v", res)
	}
	if res := r.run("reception", "discard", "lab1", "rcp-failed", "--reason", "  ", "--yes"); res.code != 2 || !strings.Contains(res.stderr, "--reason must say why") {
		t.Errorf("a blank reason: %+v", res)
	}
	wrong := typed(r.env, "rcp-done\n", "reception", "discard", "lab1", "rcp-failed", "--reason", "x")
	if wrong.code != 2 || !strings.Contains(wrong.stderr, "Type the id rcp-failed again to go on: ") || !strings.Contains(wrong.stderr, "nothing was discarded: the id typed is not the id given") {
		t.Errorf("a wrong id typed: %+v", wrong)
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("%d requests were sent before the id was confirmed", got-before)
	}

	right := typed(r.env, "rcp-failed\n", "reception", "discard", "lab1", "rcp-failed", "--reason", "sender resent it")
	if right.code != 0 || !strings.Contains(right.stdout, "reception rcp-failed: files not archived were removed (sender resent it)\n") || !strings.Contains(right.stdout, "Discard reason:    sender resent it\n") {
		t.Errorf("the id typed: %+v", right)
	}
	if got := r.fake.Reception("lab1", "rcp-failed").GetDiscardReason(); got != "sender resent it" {
		t.Errorf("the record holds %q", got)
	}
	// Discarded already: answered as it stands, with its first reason.
	doc := document(t, r.run("reception", "discard", "lab1", "rcp-failed", "--reason", "another", "--yes", "--json"))
	if doc["reception"].(map[string]any)["discardReason"] != "sender resent it" {
		t.Errorf("a repeated discard: %v", doc)
	}
	if res := r.run("reception", "discard", "lab1", "rcp-done", "--reason", "x", "--yes"); res.code != 5 {
		t.Errorf("discard of one that did not fail: %+v", res)
	}
	if res := r.run("reception", "discard", "lab1", "nope", "--reason", "x", "--yes"); res.code != 4 {
		t.Errorf("discard of an unknown id: %+v", res)
	}
}
