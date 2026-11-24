package cli_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestTenantStatus guards "tenant status": the tenant's counts, the
// receptions and jobs in progress by step and state, and a table each of
// its receivers, storages and senders with their health, in words; the
// document is the API's status with the feed's cursor; a tenant that is not
// there is exit 4.
func TestTenantStatus(t *testing.T) {
	r := newArchiveRig(t)
	r.must("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104", "--called-aet", "ORTHANC", "--calling-aet", "NOPACS", "--enabled")
	r.fake.SetSenderHealth("lab1", "pacs", &v1.SenderHealth{State: v1.SenderState_SENDER_STATE_UP, Since: timestamppb.New(fixedNow)})
	r.fake.SetStatusCounts("lab1", &v1.TenantCounts{Subjects: 2, Sessions: 3, Scans: 5, Instances: 120,
		ReceptionsInProgress: []*v1.ReceptionStepCount{{Step: v1.ReceptionStep_RECEPTION_STEP_RECEIVING, Receptions: 1}},
		JobsInProgress:       []*v1.JobStateCount{{State: v1.JobState_JOB_STATE_QUEUED, Jobs: 2}, {State: v1.JobState_JOB_STATE_RUNNING, Jobs: 1}}})

	res := r.run("tenant", "status", "lab1")
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("tenant status: %+v", res)
	}
	for label, want := range map[string]string{
		"Tenant":  "lab1",
		"Catalog": "2 subjects, 3 sessions, 5 scans, 120 instances",
		"Jobs":    "2 queued, 1 running",
	} {
		if got := field(res.stdout, label); got != want {
			t.Errorf("%s is %q, want %q:\n%s", label, got, want, res.stdout)
		}
	}
	if !strings.HasPrefix(field(res.stdout, "Receptions"), "1 ") {
		t.Errorf("receptions in progress: %q", field(res.stdout, "Receptions"))
	}
	for _, want := range []string{"RECEIVER", "scanner1", "STORAGE", "main", "SENDER", "pacs", "up"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("tenant status lacks %q:\n%s", want, res.stdout)
		}
	}
	doc := document(t, r.run("tenant", "status", "lab1", "--json"))
	ts := doc["tenants"].([]any)[0].(map[string]any)
	if ts["tenant"] != "lab1" || ts["eventCursor"] != "c0" || len(ts["senders"].([]any)) != 1 {
		t.Errorf("tenant status --json: %v", ts)
	}
	if res := r.run("tenant", "status", "nope"); res.code != 4 {
		t.Errorf("status of a tenant that is not there: %+v", res)
	}
}

// TestWatch guards "watch": a line per event of the tenant's feed in words,
// following on when the server ends a stream, ending after --count; with
// --json one document per event with its cursor; a type it does not know is
// a usage error.
func TestWatch(t *testing.T) {
	r := jobRig(t)
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	r.fake.QueueEvent("lab1", jobStep("job_1", v1.JobState_JOB_STATE_RUNNING, counts(10, 0, 0, 0)))
	r.fake.QueueEvent("lab1", &v1.Event{Cursor: "h1", Time: timestamppb.New(fixedNow), Type: v1.EventType_EVENT_TYPE_SENDER_HEALTH,
		Payload: &v1.Event_SenderHealth{SenderHealth: &v1.SenderHealthChanged{Sender: "pacs",
			Health: &v1.SenderHealth{State: v1.SenderState_SENDER_STATE_UP, Since: timestamppb.New(fixedNow)}}}})

	res := r.run("watch", "lab1", "--count", "2")
	lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
	if res.code != 0 || len(lines) != 2 {
		t.Fatalf("watch --count 2: %+v", res)
	}
	if !strings.Contains(lines[0], "job-step") || !strings.Contains(lines[0], "job job_1 is running") {
		t.Errorf("the job's line: %q", lines[0])
	}
	if !strings.Contains(lines[1], "sender-health") || !strings.Contains(lines[1], "sender pacs is up") {
		t.Errorf("the sender's line: %q", lines[1])
	}

	r.fake.QueueEvent("lab1", jobStep("job_1", v1.JobState_JOB_STATE_DONE, counts(10, 10, 0, 0)))
	js := r.run("watch", "lab1", "--count", "1", "--type", "job-step", "--json")
	if js.code != 0 || !strings.Contains(js.stdout, `"cursor": "e-job_1-JOB_STATE_DONE"`) {
		t.Errorf("watch --json: %+v", js)
	}
	if res := r.run("watch", "lab1", "--type", "everything"); res.code != 2 || res.stdout != "" {
		t.Errorf("an unknown type: %+v", res)
	}
	if res := r.run("watch", "lab1", "--count", "-1"); res.code != 2 {
		t.Errorf("a negative count: %+v", res)
	}
}
