package tui

import (
	"testing"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// sendTenant puts tenant t000 with a DIMSE sender, a stow sender holding a
// credential, and a running job to the first, in the fake.
func sendTenant(f *fakeserver.Server) {
	addTenants(f, 1)
	f.AddSender("t000", &v1.Sender{Name: "pacs", Enabled: true, MaxAssociations: 2, CreateTime: timestamp(-2),
		Type: &v1.Sender_Dimse{Dimse: &v1.DimseSender{Host: "pacs.lab.example", Port: 104, CalledAeTitle: "ORTHANC", CallingAeTitle: "NOPACS"}}})
	f.AddSender("t000", &v1.Sender{Name: "web", MaxAssociations: 2, CreateTime: timestamp(-2),
		Type: &v1.Sender_Stow{Stow: &v1.StowSender{Url: "https://pacs2.lab.example/dicom-web", Authentication: v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, CredentialSet: true, MaxRequestBytes: 1 << 28}}})
	f.SetSenderHealth("t000", "pacs", &v1.SenderHealth{State: v1.SenderState_SENDER_STATE_UP, Since: timestamp(-1)})
	f.AddJob("t000", &v1.Job{Id: "job_1", State: v1.JobState_JOB_STATE_RUNNING, CreateTime: timestamp(-1), StateTime: timestamp(-1),
		Kind:   &v1.Job_Send{Send: &v1.SendJob{Sender: "pacs", SubjectLabel: "sub-0017", SessionLabel: "ses-1"}},
		Counts: &v1.JobCounts{Instances: 100, InstancesSent: 20, InstancesRemaining: 80, Scans: 2, ScansDone: 0, Listed: true}})
}

// jobCounts makes the counts of a job of 100 instances of which sent are sent.
func jobCounts(sent, refused int64) *v1.JobCounts {
	return &v1.JobCounts{Instances: 100, InstancesSent: sent, InstancesRefused: refused, InstancesRemaining: 100 - sent - refused, Scans: 2, ScansDone: 1, Listed: true}
}

// TestSendersShowTheirHealthAndTheirTest guards a tenant's senders: the list
// shows each with its health, a sender opens on its detail, which says only
// whether a credential is held, and "t" tries its peer and shows each step and
// what was found in plain words.
func TestSendersShowTheirHealthAndTheirTest(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, sendTenant)
		r.key("enter", "enter", "n")
		r.expect("Senders of t000", "pacs", "web", "up", "down (")
		r.key("down", "enter")
		r.expect("Sender web", "Credential held:", "yes", "https://pacs2.lab.example/dicom-web")
		r.refuse("tok-", "Bearer")
		r.fake.SetSenderTest("t000", "web", []*v1.SenderTestResult{
			{Step: v1.SenderTestStep_SENDER_TEST_STEP_CONNECT, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED, Duration: durationpb.New(3e6)},
			{Step: v1.SenderTestStep_SENDER_TEST_STEP_REQUEST, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED, Duration: durationpb.New(2e6),
				Fault: v1.SenderFault_SENDER_FAULT_REJECTED, Detail: "HTTP 401"},
		})
		r.key("t")
		r.expect("Test of sender web", "connect:", "passed, 3ms", "request:", "failed, 2ms, rejected: HTTP 401", "the peer was not reached: step request failed")
		r.key("esc", "esc", "esc")
		r.expect("Tenant t000")
	})
}

// TestJobProgressFollowsTheEventFeed guards a job's screen: it draws the
// progress as a bar, an event of the job moves the bar without a key, an event
// of another job does not, and the event that ends the job makes the screen
// read it again, so the state it ends on is the server's.
func TestJobProgressFollowsTheEventFeed(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, sendTenant)
		r.key("enter", "enter", "w")
		r.expect("Jobs of t000", "job_1", "pacs", "[====>")
		// Events wait for the stream the job's screen opens: one of another
		// job, one of this job's progress, and the one that ends it.
		r.fake.QueueEvent("t000", &v1.Event{Cursor: "a", Type: v1.EventType_EVENT_TYPE_JOB_PROGRESS,
			Payload: &v1.Event_JobProgress{JobProgress: &v1.JobProgressed{Job: "job_other", Counts: jobCounts(99, 0)}}})
		r.fake.QueueEvent("t000", &v1.Event{Cursor: "b", Type: v1.EventType_EVENT_TYPE_JOB_PROGRESS,
			Payload: &v1.Event_JobProgress{JobProgress: &v1.JobProgressed{Job: "job_1", Counts: jobCounts(60, 0)}}})
		r.fake.QueueEvent("t000", &v1.Event{Cursor: "c", Time: timestamppb.New(fixedNow), Type: v1.EventType_EVENT_TYPE_JOB_STEP,
			Payload: &v1.Event_JobStep{JobStep: &v1.JobStepReached{Job: "job_1", State: v1.JobState_JOB_STATE_PARTIAL, Counts: jobCounts(90, 10), Reason: "10 refused"}}})
		watches := r.calls("Watch")
		r.key("enter")
		r.expect("Job job_1", "State:", "partial", "100%", "Refused:", "10", "Reason:", "10 refused")
		r.refuse("waiting for the server")
		if got := r.calls("Watch") - watches; got != 3 {
			t.Errorf("the feed was followed %d times, want 3: once for each event, the last ending the job", got)
		}
		if r.calls("GetJob") < 1 {
			t.Error("the job was not read again when it ended")
		}
		r.key("esc")
		r.expect("Jobs of t000")
	})
}

// TestJobListFollowsTheEventFeed guards the list of jobs: an event of a job
// makes it ask for its page again while it is the screen on top, so the bar and
// the state of a row move without a key.
func TestJobListFollowsTheEventFeed(t *testing.T) {
	r := newRig(t, 160, 30, func(f *fakeserver.Server) {
		sendTenant(f)
		f.QueueEvent("t000", &v1.Event{Cursor: "c", Time: timestamppb.New(fixedNow), Type: v1.EventType_EVENT_TYPE_JOB_STEP,
			Payload: &v1.Event_JobStep{JobStep: &v1.JobStepReached{Job: "job_1", State: v1.JobState_JOB_STATE_DONE, Counts: jobCounts(100, 0)}}})
	})
	pages := r.calls("ListJobs")
	r.key("enter", "enter", "w")
	r.expect("Jobs of t000", "job_1", "pacs", "sub-0017/ses-1", "done", "100%")
	r.refuse("running", "20%")
	if got := r.calls("ListJobs") - pages; got != 2 {
		t.Errorf("the jobs were listed %d times, want 2: on opening, and after the event", got)
	}
}

// TestJobFailuresCancelAndRetry guards the keys of a job: "f" lists what it
// did not send and what the peer kept with a warning; cancel and retry each ask
// for the job's id to be typed again, and send nothing for another text.
func TestJobFailuresCancelAndRetry(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		sendTenant(f)
		f.AddJob("t000", &v1.Job{Id: "job_2", State: v1.JobState_JOB_STATE_PARTIAL, CreateTime: timestamp(-3), StateTime: timestamp(-3),
			Kind:   &v1.Job_Send{Send: &v1.SendJob{Sender: "pacs", SubjectLabel: "sub-0018"}},
			Counts: jobCounts(90, 10)})
		f.AddJobFailure("t000", "job_2", &v1.JobFailure{InstanceId: "i-1", SubjectLabel: "sub-0018", SessionLabel: "ses-1", ScanLabel: "scan-4",
			Code: v1.JobFailureCode_JOB_FAILURE_CODE_PEER_REFUSED, PeerStatus: 0xA900, Detail: "cannot understand", Time: timestamp(-3)})
		f.AddJobFailure("t000", "job_2", &v1.JobFailure{InstanceId: "i-2", SubjectLabel: "sub-0018", SessionLabel: "ses-1", ScanLabel: "scan-5",
			Code: v1.JobFailureCode_JOB_FAILURE_CODE_PEER_WARNING, PeerStatus: 0xB000, Detail: "coerced", Time: timestamp(-3)})
	})
	r.key("enter", "enter", "w")
	r.expect("job_1", "job_2")
	// The list is newest first: job_1 is the row under the cursor.
	r.key("down", "enter")
	r.expect("Job job_2", "f failures", "c cancel", "r retry")
	r.key("f")
	r.expect("Failures of job job_2", "sub-0018/ses-1/scan-4", "peer refused", "A900", "peer warning", "B000")
	r.key("enter")
	r.expect("Failure of i-1", "cannot understand")
	r.key("esc", "esc")

	// Cancel needs the id typed: another text sends nothing.
	cancels := r.calls("CancelJob")
	r.key("c")
	r.expect("Cancel job job_2", "Type the id")
	r.typeText("job_9")
	r.key("enter")
	r.expect("type job_2 exactly")
	if r.calls("CancelJob") != cancels {
		t.Fatal("a cancel with the wrong id was sent")
	}
	r.key("esc")

	// Retry of the partial job: the id typed right makes the new job.
	r.key("r")
	r.expect("Retry job job_2")
	r.typeText("job_2")
	r.key("enter")
	if r.calls("RetryJob") != 1 {
		t.Fatal("the retry was not sent")
	}
	r.expect("Job job_2", "Retried as:", "job_3", "retried job_2 as job_3")

	// A running job is canceled with its id.
	r.key("esc")
	r.key("up", "enter")
	r.expect("Job job_1")
	r.key("c")
	r.typeText("job_1")
	r.key("enter")
	if r.calls("CancelJob") != cancels+1 {
		t.Fatal("the cancel was not sent")
	}
	r.expect("Job job_1", "canceled")
}
