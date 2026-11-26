package cli_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestRetentionRuleCommands guards every rule command: the periods are read
// as years and days, the selector is changed whole, "none" removes a period,
// the mask names only what a flag changed, a name that is taken is refused
// (exit 1), removal asks for the name typed unless --yes, and list and show
// answer as the table and the document.
func TestRetentionRuleCommands(t *testing.T) {
	r := newArchiveRig(t)
	res := r.run("retention", "rule", "add", "lab1", "seven-years", "--keep-at-least", "7y", "--dispose-after", "10y 30d", "--action", "delete")
	if res.code != 0 {
		t.Fatalf("add: %+v", res)
	}
	for label, want := range map[string]string{"Name": "seven-years", "Matches": "every session", "Keep at least": "7y", "Due after": "10y 30d", "Action": "delete"} {
		if got := field(res.stdout, label); got != want {
			t.Errorf("add: %s is %q, want %q:\n%s", label, got, want, res.stdout)
		}
	}
	if res := r.run("retention", "rule", "add", "lab1", "seven-years", "--keep-at-least", "1y"); res.code != 1 || !strings.Contains(res.stderr, "has a rule with that name") {
		t.Errorf("a name taken: %+v", res)
	}
	r.must("retention", "rule", "add", "lab1", "scanner-days", "--receiver", "scanner1", "--keep-at-least", "180d")

	expect(t, r.run("retention", "rule", "list", "lab1"), 0,
		"NAME          MATCHES            KEEP AT LEAST  DUE AFTER  ACTION  DUE NOW\n"+
			"scanner-days  receiver scanner1  180d           -          review  0\n"+
			"seven-years   every session      7y             10y 30d    delete  0\n", "")

	// A change names only what a flag changed: the action stays delete.
	res = r.run("retention", "rule", "set", "lab1", "seven-years", "--dispose-after", "12y")
	if res.code != 0 || field(res.stdout, "Due after") != "12y" || field(res.stdout, "Action") != "delete" || field(res.stdout, "Keep at least") != "7y" {
		t.Errorf("set one period: %+v", res)
	}
	// "none" removes a period; the selector is changed as a whole, and an
	// empty --receiver returns to every session.
	res = r.run("retention", "rule", "set", "lab1", "scanner-days", "--receiver", "", "--keep-at-least", "none", "--dispose-after", "20y")
	if res.code != 0 || field(res.stdout, "Matches") != "every session" || field(res.stdout, "Keep at least") != "-" || field(res.stdout, "Due after") != "20y" {
		t.Errorf("set the selector and two periods: %+v", res)
	}
	for _, args := range [][]string{
		{"retention", "rule", "set", "lab1", "seven-years"},
		{"retention", "rule", "set", "lab1", "seven-years", "--action", "erase"},
		{"retention", "rule", "set", "lab1", "seven-years", "--keep-at-least", "seven"},
		{"retention", "rule", "add", "lab1", "x", "--keep-at-least", "7y7y"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if res := r.run("retention", "rule", "set", "lab1", "seven-years", "--keep-at-least", "20y"); res.code != 2 || !strings.Contains(res.stderr, "shorter than keep_at_least") {
		t.Errorf("a dispose period shorter than the keep period: %+v", res)
	}

	if res := r.run("retention", "rule", "show", "lab1", "seven-years"); res.code != 0 || field(res.stdout, "Due after") != "12y" {
		t.Errorf("show: %+v", res)
	}
	doc := document(t, r.run("retention", "rule", "show", "lab1", "seven-years", "--json"))
	if rule := doc["rule"].(map[string]any); rule["name"] != "seven-years" || rule["action"] != "RETENTION_ACTION_DELETE" {
		t.Errorf("show --json: %v", doc)
	}
	if rs, _ := document(t, r.run("retention", "rule", "list", "lab1", "--json"))["rules"].([]any); len(rs) != 2 {
		t.Errorf("list --json: %v", rs)
	}

	// Removal is guarded by the name typed.
	if res := r.run("retention", "rule", "remove", "lab1", "seven-years"); res.code != 2 || !strings.Contains(res.stderr, "nothing was removed") {
		t.Errorf("remove with no terminal and no --yes: %+v", res)
	}
	if res := typed(r.env, "scanner-days\n", "retention", "rule", "remove", "lab1", "seven-years"); res.code != 2 || !strings.Contains(res.stderr, "not the id given") {
		t.Errorf("remove with a wrong name typed: %+v", res)
	}
	if out := mustOut(r, "retention", "rule", "list", "lab1"); !strings.Contains(out, "seven-years") {
		t.Errorf("a refused removal removed:\n%s", out)
	}
	if res := typed(r.env, "seven-years\n", "retention", "rule", "remove", "lab1", "seven-years"); res.code != 0 {
		t.Errorf("remove with the name typed: %+v", res)
	}
	if res := r.run("retention", "rule", "remove", "lab1", "seven-years", "--yes"); res.code != 4 {
		t.Errorf("a repeated removal: %+v, want exit 4", res)
	}
}

// dueSession is a due session for the tests of the due commands.
func dueSession(subject, session, rule string, held bool) *v1.DueSession {
	return &v1.DueSession{SubjectLabel: subject, SessionLabel: session, Rule: rule, Action: v1.RetentionAction_RETENTION_ACTION_DELETE,
		StartTime: timestamppb.New(fixedNow.AddDate(-11, 0, 0)), DueTime: timestamppb.New(fixedNow.AddDate(0, -1, 0)), Held: held, Instances: 120, Bytes: 50 << 20}
}

// TestRetentionDue guards the due listing: held sessions are left out unless
// --held, and then marked; a rule that is not there is refused (exit 4).
func TestRetentionDue(t *testing.T) {
	r := newArchiveRig(t)
	r.must("retention", "rule", "add", "lab1", "seven-years", "--dispose-after", "10y", "--action", "delete")
	r.fake.AddDueSession("lab1", dueSession("sub-1", "ses-1", "seven-years", false))
	r.fake.AddDueSession("lab1", dueSession("sub-2", "ses-1", "seven-years", true))
	out := mustOut(r, "retention", "due", "lab1")
	if !strings.Contains(out, "sub-1/ses-1") || strings.Contains(out, "sub-2") || !strings.Contains(out, "50.0 MiB") {
		t.Errorf("due:\n%s", out)
	}
	out = mustOut(r, "retention", "due", "lab1", "--held", "--rule", "seven-years")
	if !strings.Contains(out, "sub-2/ses-1") || !strings.Contains(out, "held") {
		t.Errorf("due --held:\n%s", out)
	}
	if res := r.run("retention", "due", "lab1", "--rule", "nope"); res.code != 4 {
		t.Errorf("due of a rule that is not there: %+v", res)
	}
	if ss, _ := document(t, r.run("retention", "due", "lab1", "--json"))["sessions"].([]any); len(ss) != 1 {
		t.Errorf("due --json: %v", ss)
	}
}

// TestRetentionDisposeNeedsItsPreviewsToken guards the disposal: --preview
// lists what would go with a token and changes nothing; the real call needs
// the token (there is no --yes), carries the reason, and is refused when
// data fell due since the preview; the job it queues can be followed.
func TestRetentionDisposeNeedsItsPreviewsToken(t *testing.T) {
	r := newArchiveRig(t)
	r.must("retention", "rule", "add", "lab1", "seven-years", "--dispose-after", "10y", "--action", "delete")
	r.fake.AddDueSession("lab1", dueSession("sub-1", "ses-1", "seven-years", false))
	r.fake.AddDueSession("lab1", dueSession("sub-2", "ses-1", "seven-years", true))
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "period over"},
		{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "  ", "--preview"},
		{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--confirm", "t"},
		{"retention", "dispose", "lab1", "--reason", "x", "--preview"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	res := r.run("retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "period over", "--preview")
	if res.code != 0 {
		t.Fatalf("preview: %+v", res)
	}
	for _, want := range []string{"Sessions listed", "Sessions held (skipped)", "Confirm token", "dp.seven-years.1", "sub-1/ses-1",
		`--reason "period over" --confirm dp.seven-years.1`} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the preview lacks %q:\n%s", want, res.stdout)
		}
	}
	if r.fake.Job("lab1", "job_1") != nil {
		t.Errorf("a preview queued a job")
	}
	// A session falls due after the preview: its token no longer matches.
	r.fake.AddDueSession("lab1", dueSession("sub-3", "ses-1", "seven-years", false))
	if res := r.run("retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "period over", "--confirm", "dp.seven-years.1"); res.code != 5 || !strings.Contains(res.stderr, "does not match") {
		t.Errorf("a stale token: %+v", res)
	}
	finishLater(t, r, "job_1", v1.JobState_JOB_STATE_DONE)
	res = r.run("retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "period over", "--confirm", "dp.seven-years.2", "--wait")
	if res.code != 0 || !strings.Contains(res.stdout, "Disposes by rule:") || !strings.Contains(res.stdout, "period over") {
		t.Errorf("dispose --wait: %+v", res)
	}
}

// TestRetentionClock guards the clock: without --confirm it shows whether
// retention is paused; --confirm takes the time typed by the person with its
// offset (never one from the server) and a reason, and ends the pause; a time
// the server finds wrong is refused (exit 5).
func TestRetentionClock(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.SetRetentionStatus("lab1", &v1.RetentionStatus{Rules: 2, SessionsDue: 5, SessionsDueHeld: 1, Paused: true, PauseReason: "clock jumped",
		ClockStep: durationpb.New(72 * time.Hour), ActiveHolds: 3, LastPassTime: timestamppb.New(fixedNow.Add(-24 * time.Hour))})
	res := r.run("retention", "clock", "lab1")
	if res.code != 0 || !strings.Contains(res.stdout, "Paused:") || !strings.Contains(res.stdout, "yes: clock jumped") ||
		field(res.stdout, "Clock step seen") != "3d" || !strings.Contains(res.stdout, "Active holds:") {
		t.Errorf("clock status: %+v", res)
	}
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"retention", "clock", "lab1", "--confirm", "2026-11-17T12:00:00Z"},
		{"retention", "clock", "lab1", "--reason", "x"},
		{"retention", "clock", "lab1", "--confirm", "noon", "--reason", "x"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	if res := r.run("retention", "clock", "lab1", "--confirm", "2026-11-17T18:00:00Z", "--reason", "misread"); res.code != 5 || !strings.Contains(res.stderr, "more than 5 minutes") {
		t.Errorf("a time that is wrong: %+v", res)
	}
	res = r.run("retention", "clock", "lab1", "--confirm", "2026-11-17T12:01:00Z", "--reason", "read from the site's reference clock")
	if res.code != 0 || !strings.Contains(res.stdout, "time confirmed for 1 tenant(s); retention resumes") || field(res.stdout, "Paused") != "no" {
		t.Errorf("the confirmation: %+v", res)
	}
	if out := mustOut(r, "retention", "clock", "lab1"); field(out, "Paused") != "no" {
		t.Errorf("clock status after the confirmation:\n%s", out)
	}
}

// TestDeleteOverrideAndRefusals guards a delete's retention override: the
// override and its reason go together and reach the server, the correction
// shows that retention was overridden, and a refusal for a hold or a
// retention minimum shows the server's reason, with exit 5.
func TestDeleteOverrideAndRefusals(t *testing.T) {
	r := newArchiveRig(t)
	if res := r.run("correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--override-retention", "--yes"); res.code != 2 || !strings.Contains(res.stderr, "go together") {
		t.Errorf("an override without its reason: %+v", res)
	}
	if res := r.run("correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--override-reason", "x", "--yes"); res.code != 2 {
		t.Errorf("a reason without the override: %+v", res)
	}
	out := mustOut(r, "correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--override-retention",
		"--override-reason", "participant withdrew consent in writing", "--yes")
	id := fakeCorrection(1)
	if !strings.Contains(out, "deleted sub-1: correction "+id) {
		t.Fatalf("the delete said %q", out)
	}
	if out := mustOut(r, "correction", "show", "lab1", id); field(out, "Retention overridden") == "" || !strings.Contains(out, "participant withdrew consent in writing") {
		t.Errorf("show lacks the override:\n%s", out)
	}
	for _, c := range []struct{ reason, want string }{
		{"HELD", "correction delete: HELD: a hold covers this subject"},
		{"RETENTION_MINIMUM", "correction delete: RETENTION_MINIMUM: a hold covers this subject"},
	} {
		r.fake.FailWithReason(nopacsv1connect.CorrectionServiceDeleteProcedure, fault.Precondition, "a hold covers this subject", c.reason, 0)
		if res := r.run("correction", "delete", "lab1", "sub-2", "--reason", "consent withdrawn", "--yes"); res.code != 5 || !strings.Contains(res.stderr, c.want) {
			t.Errorf("%s: %+v", c.reason, res)
		}
	}
}
