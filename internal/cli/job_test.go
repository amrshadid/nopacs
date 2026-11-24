package cli_test

import (
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// jobRig is a rig with tenant lab1 and an enabled sender pacs.
func jobRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104", "--called-aet", "ORTHANC", "--calling-aet", "NOPACS", "--enabled")
	return r
}

// counts makes the counts of a job with n instances of which sent are sent,
// warned warned and refused refused.
func counts(n, sent, warned, refused int64) *v1.JobCounts {
	return &v1.JobCounts{Instances: n, Bytes: n * 1000, InstancesSent: sent, BytesSent: sent * 1000, InstancesWarned: warned, BytesWarned: warned * 1000,
		InstancesRefused: refused, BytesRefused: refused * 1000, InstancesRemaining: n - sent - warned - refused, BytesRemaining: (n - sent - warned - refused) * 1000,
		Scans: 2, ScansDone: 1, Listed: true}
}

// jobStep is the event of a job reaching a state with counts.
func jobStep(job string, state v1.JobState, c *v1.JobCounts) *v1.Event {
	return &v1.Event{Cursor: "e-" + job + "-" + state.String(), Time: timestamppb.New(fixedNow), Type: v1.EventType_EVENT_TYPE_JOB_STEP,
		Payload: &v1.Event_JobStep{JobStep: &v1.JobStepReached{Job: job, State: state, Counts: c}}}
}

// TestJobCommands guards every job command in both forms: the job as a person
// reads it with its progress bar, the target by labels at each level, the
// filters of the list, cancel and retry with the exit codes of what they refuse.
func TestJobCommands(t *testing.T) {
	r := jobRig(t)

	create := r.run("job", "create", "lab1", "--sender", "pacs", "sub-0017/ses-1/scan-4")
	for label, want := range map[string]string{"ID": "job_1", "State": "queued", "Sends": "sub-0017/ses-1/scan-4", "Sender": "pacs",
		"Progress": "[                    ] 0 of 0 instances answered (0%): 0 sent; 0 remaining; still listing the scans, so the total grows"} {
		if got := field(create.stdout, label); create.code != 0 || got != want {
			t.Errorf("job create: %s is %q, want %q:\n%s", label, got, want, create.stdout)
		}
	}
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017/ses-1")
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	doc := document(t, r.run("job", "create", "lab1", "--sender", "pacs", "sub-0018", "--json"))
	send := doc["job"].(map[string]any)["send"].(map[string]any)
	if send["subjectLabel"] != "sub-0018" || send["sessionLabel"] != "" || send["sender"] != "pacs" {
		t.Errorf("create --json: %v", send)
	}

	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_RUNNING, counts(100, 40, 2, 3))
	got := r.run("job", "show", "lab1", "job_1")
	for label, want := range map[string]string{"State": "running", "Refused": "3, 2.9 KiB", "Remaining": "55, 53.7 KiB",
		"Progress": "[=========>          ] 45 of 100 instances answered (45%): 40 sent, 2 with a warning, 3 refused; 55 remaining; scans 1 of 2 done"} {
		if g := field(got.stdout, label); got.code != 0 || g != want {
			t.Errorf("job show: %s is %q, want %q:\n%s", label, g, want, got.stdout)
		}
	}

	r.fake.SetJob("lab1", "job_2", v1.JobState_JOB_STATE_PARTIAL, counts(10, 8, 0, 2))
	r.fake.SetJob("lab1", "job_3", v1.JobState_JOB_STATE_DONE, counts(10, 10, 0, 0))
	list := r.run("job", "list", "lab1", "--state", "partial", "--state", "done")
	if list.code != 0 || !strings.Contains(list.stdout, "job_2") || !strings.Contains(list.stdout, "job_3") || strings.Contains(list.stdout, "job_1") ||
		!strings.Contains(list.stdout, "ID     KIND  ON              STATE    DONE  OK  NOT OK  REMAINING  CREATED") {
		t.Errorf("job list --state:\n%s", list.stdout)
	}
	if res := r.run("job", "list", "lab1", "--in-progress", "--sender", "pacs", "--json"); res.code != 0 {
		t.Errorf("job list --in-progress: %+v", res)
	} else if jobs := document(t, res)["jobs"].([]any); len(jobs) != 2 {
		t.Errorf("job list --in-progress: %d jobs", len(jobs))
	}
	if res := r.run("job", "list", "lab1", "--state", "paused"); res.code != 2 || !strings.Contains(res.stderr, "--state \"paused\" is not one of: queued, running, done, partial, failed, canceled") {
		t.Errorf("job list --state paused: %+v", res)
	}

	// Cancel ends a job in progress; repeated, it answers as it stands; a job
	// that ended otherwise is a state that forbids it (exit 5).
	if res := r.run("job", "cancel", "lab1", "job_1"); res.code != 0 || !strings.HasPrefix(res.stdout, "job job_1 is canceled\n\n") {
		t.Errorf("job cancel: %+v", res)
	}
	if res := r.run("job", "cancel", "lab1", "job_1"); res.code != 0 {
		t.Errorf("a repeated cancel: %+v", res)
	}
	if res := r.run("job", "cancel", "lab1", "job_3"); res.code != 5 || !strings.Contains(res.stderr, "has ended done and cannot be canceled") {
		t.Errorf("cancel a done job: %+v", res)
	}
	if res := r.run("job", "cancel", "lab1", "job_99"); res.code != 4 {
		t.Errorf("cancel a job that is not there: %+v", res)
	}

	retry := r.run("job", "retry", "lab1", "job_2")
	if retry.code != 0 || !strings.HasPrefix(retry.stdout, "job job_2 retried as job_5\n\n") || field(retry.stdout, "Retry of") != "job_2" {
		t.Errorf("job retry: %+v", retry)
	}
	if res := r.run("job", "retry", "lab1", "job_2"); res.code != 0 || !strings.HasPrefix(res.stdout, "job job_2 retried as job_5\n") {
		t.Errorf("a repeated retry makes nothing more: %+v", res)
	}
	if res := r.run("job", "retry", "lab1", "job_3"); res.code != 5 {
		t.Errorf("retry a job that sent everything: %+v", res)
	}
	if res := r.run("job", "create", "lab1", "--sender", "ghost", "sub-1"); res.code != 5 || !strings.Contains(res.stderr, "no sender with that name") {
		t.Errorf("create for a sender that is not there: %+v", res)
	}
	for name, args := range map[string][]string{
		"no sender":     {"job", "create", "lab1", "sub-1"},
		"empty part":    {"job", "create", "lab1", "--sender", "pacs", "sub-1//scan"},
		"too deep":      {"job", "create", "lab1", "--sender", "pacs", "a/b/c/d"},
		"no target":     {"job", "create", "lab1", "--sender", "pacs"},
		"watch no id":   {"job", "watch", "lab1"},
		"failures none": {"job", "failures", "lab1"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line", name, res)
		}
	}
}

// TestJobFailures guards the list of what a job did not send: each instance
// with its labels, the reason in words and the peer's status (a DICOM status
// in hexadecimal, an HTTP status in decimal after "HTTP"), warnings only when
// asked for, and the document.
func TestJobFailures(t *testing.T) {
	r := jobRig(t)
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_PARTIAL, counts(10, 7, 1, 2))
	at := timestamppb.New(fixedNow)
	r.fake.AddJobFailure("lab1", "job_1", &v1.JobFailure{InstanceId: "i-1", SubjectLabel: "sub-0017", SessionLabel: "ses-1", ScanLabel: "scan-4",
		Code: v1.JobFailureCode_JOB_FAILURE_CODE_PEER_REFUSED, PeerStatus: 0xA900, Detail: "cannot understand", Time: at})
	r.fake.AddJobFailure("lab1", "job_1", &v1.JobFailure{InstanceId: "i-2", SubjectLabel: "sub-0017", SessionLabel: "ses-1", ScanLabel: "scan-5",
		Code: v1.JobFailureCode_JOB_FAILURE_CODE_CLASS_NOT_ACCEPTED, Time: at})
	r.fake.AddJobFailure("lab1", "job_1", &v1.JobFailure{InstanceId: "i-3", SubjectLabel: "sub-0017", SessionLabel: "ses-1", ScanLabel: "scan-5",
		Code: v1.JobFailureCode_JOB_FAILURE_CODE_PEER_WARNING, PeerStatus: 0xB000, Detail: "coerced", Time: at})

	expect(t, r.run("job", "failures", "lab1", "job_1"), 0,
		"SCAN                   INSTANCE  WHY                 PEER STATUS  DETAIL             TIME\n"+
			"sub-0017/ses-1/scan-4  i-1       peer refused        A900         cannot understand  2026-11-17 15:00:00 +03:00\n"+
			"sub-0017/ses-1/scan-5  i-2       class not accepted  -            -                  2026-11-17 15:00:00 +03:00\n", "")
	with := r.run("job", "failures", "lab1", "job_1", "--warnings")
	if with.code != 0 || !strings.Contains(with.stdout, "peer warning") || !strings.Contains(with.stdout, "B000") {
		t.Errorf("--warnings: %+v", with)
	}
	doc := document(t, r.run("job", "failures", "lab1", "job_1", "--json"))
	if fs := doc["failures"].([]any); len(fs) != 2 || doc["more"] != false {
		t.Errorf("failures --json: %v", doc)
	}
	if res := r.run("job", "failures", "lab1", "job_99"); res.code != 4 {
		t.Errorf("failures of a job that is not there: %+v", res)
	}

	// An HTTP status, of a stow request refused whole, is shown in decimal
	// after "HTTP"; a DICOM status stays hexadecimal.
	r.fake.AddJobFailure("lab1", "job_1", &v1.JobFailure{InstanceId: "i-3", SubjectLabel: "sub-0017", SessionLabel: "ses-1", ScanLabel: "scan-6",
		Code: v1.JobFailureCode_JOB_FAILURE_CODE_PEER_REFUSED, PeerStatus: 415, PeerStatusHttp: true, Detail: "the peer refused the whole request", Time: at})
	http := r.run("job", "failures", "lab1", "job_1")
	if http.code != 0 || !strings.Contains(http.stdout, "HTTP 415") || strings.Contains(http.stdout, "019F") || !strings.Contains(http.stdout, "A900") {
		t.Errorf("an HTTP status among the failures:\n%s", http.stdout)
	}
}

// TestJobWatch guards "job watch": it shows the job, then a line per event the
// feed carries for it and none for another job's, follows again when the server
// ends a stream before the job ends, ends with the job as it stands, and its
// exit code says whether everything was sent.
func TestJobWatch(t *testing.T) {
	r := jobRig(t)
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0018")
	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_RUNNING, counts(100, 0, 0, 0))
	progress := func(job string, c *v1.JobCounts) *v1.Event {
		return &v1.Event{Cursor: "p-" + job + "-" + itoa(c.GetInstancesSent()), Time: timestamppb.New(fixedNow), Type: v1.EventType_EVENT_TYPE_JOB_PROGRESS,
			Payload: &v1.Event_JobProgress{JobProgress: &v1.JobProgressed{Job: job, Counts: c}}}
	}
	// One stream carries one event of the fake, so the command has to follow
	// again between them, as it does when a real stream ends.
	r.fake.QueueEvent("lab1", progress("job_2", counts(5, 5, 0, 0)))
	r.fake.QueueEvent("lab1", progress("job_1", counts(100, 50, 0, 0)))
	r.fake.QueueEvent("lab1", jobStep("job_1", v1.JobState_JOB_STATE_DONE, counts(100, 100, 0, 0)))

	res := r.run("job", "watch", "lab1", "job_1")
	lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
	if res.code != 0 || res.stderr != "" || len(lines) < 4 {
		t.Fatalf("watch: %+v", res)
	}
	for i, want := range []string{
		"2026-11-17 15:00:00 +03:00  running   [                    ] 0 of 100 instances answered (0%): 0 sent; 100 remaining; scans 1 of 2 done",
		"2026-11-17 15:00:00 +03:00  running   [==========>         ] 50 of 100 instances answered (50%): 50 sent; 50 remaining; scans 1 of 2 done",
		"2026-11-17 15:00:00 +03:00  done      [====================] 100 of 100 instances answered (100%): 100 sent; 0 remaining; scans 1 of 2 done",
	} {
		if lines[i] != want {
			t.Errorf("line %d:\n%s\nwant:\n%s", i, lines[i], want)
		}
	}
	if strings.Contains(res.stdout, "5 of 5") {
		t.Errorf("another job's event was shown:\n%s", res.stdout)
	}
	if field(res.stdout, "State") != "done" {
		t.Errorf("the job as it finally stands is missing:\n%s", res.stdout)
	}

	// A job that ends partial is exit 1, so a script sees something was not sent.
	r.fake.SetJob("lab1", "job_2", v1.JobState_JOB_STATE_RUNNING, counts(10, 0, 0, 0))
	r.fake.QueueEvent("lab1", jobStep("job_2", v1.JobState_JOB_STATE_PARTIAL, counts(10, 8, 0, 2)))
	if res := r.run("job", "watch", "lab1", "job_2"); res.code != 1 || res.stderr != "nopacs: job job_2 ended partial\n" || !strings.Contains(res.stdout, "partial") {
		t.Errorf("watch of a job that ends partial: %+v", res)
	}

	// A job that has ended is shown as it stands, without waiting for any event.
	if res := r.run("job", "watch", "lab1", "job_1"); res.code != 0 || field(res.stdout, "State") != "done" {
		t.Errorf("watch of an ended job: %+v", res)
	}
	if res := r.run("job", "watch", "lab1", "job_99"); res.code != 4 {
		t.Errorf("watch of a job that is not there: %+v", res)
	}

	// With --json each update is one document, the last the job as read.
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0019")
	r.fake.SetJob("lab1", "job_3", v1.JobState_JOB_STATE_RUNNING, counts(4, 0, 0, 0))
	r.fake.QueueEvent("lab1", jobStep("job_3", v1.JobState_JOB_STATE_DONE, counts(4, 4, 0, 0)))
	js := r.run("job", "watch", "lab1", "job_3", "--json")
	if js.code != 0 || strings.Count(js.stdout, "\"schemaVersion\": 1") != 3 || !strings.Contains(js.stdout, "\"state\": \"JOB_STATE_DONE\"") {
		t.Errorf("watch --json: %+v", js)
	}
}

// itoa writes n in decimal.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// field returns the value a table of labeled lines gives a label: what follows
// "label:" and its padding on that line; empty when there is no such line.
func field(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, label+":"); ok {
			return strings.TrimLeft(rest, " ")
		}
	}
	return ""
}
