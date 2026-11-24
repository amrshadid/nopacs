package cli_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// streamEndedMessage is what the server says when it ends a stream, as it
// does at the latest when the call's deadline passes.
const streamEndedMessage = "the stream ended; resume from its last cursors"

// finishLater marks the tenant's job done once the fake holds it and a
// little time has passed, as a server would while a command follows it.
func finishLater(t *testing.T, r *rig, id string, state v1.JobState) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		for range 500 {
			if r.fake.Job("lab1", id) != nil {
				time.Sleep(50 * time.Millisecond)
				r.fake.SetJob("lab1", id, state, counts(10, 10, 0, 0))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// TestJobWatchFollowsWhenTheServerEndsTheStream guards "job watch" against a
// server that ends every stream as unavailable, as the real one does when a
// stream's deadline passes: the command reads the job again and follows on,
// ending when the job ends, never with the stream's error.
func TestJobWatchFollowsWhenTheServerEndsTheStream(t *testing.T) {
	r := jobRig(t)
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_RUNNING, counts(10, 0, 0, 0))
	r.fake.FailAfter(nopacsv1connect.MonitorServiceWatchProcedure, 0, connect.CodeUnavailable, streamEndedMessage)
	finishLater(t, r, "job_1", v1.JobState_JOB_STATE_DONE)

	res := r.run("job", "watch", "lab1", "job_1")
	if res.code != 0 || res.stderr != "" || field(res.stdout, "State") != "done" {
		t.Fatalf("watch across streams the server ended: %+v", res)
	}
	if strings.Contains(res.stdout+res.stderr, "stream ended") {
		t.Errorf("the stream's end was shown: %+v", res)
	}
}

// TestJobCreateWait guards "job create --wait": it follows the job it made
// as "job watch" does, across streams the server ends, and its exit code
// says how the job ended: 0 done, 1 for any other end.
func TestJobCreateWait(t *testing.T) {
	r := jobRig(t)
	r.fake.FailAfter(nopacsv1connect.MonitorServiceWatchProcedure, 0, connect.CodeUnavailable, streamEndedMessage)
	finishLater(t, r, "job_1", v1.JobState_JOB_STATE_DONE)
	res := r.run("job", "create", "lab1", "--sender", "pacs", "sub-0017", "--wait")
	lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
	if res.code != 0 || res.stderr != "" || len(lines) < 2 || !strings.Contains(lines[0], "queued") || field(res.stdout, "State") != "done" {
		t.Fatalf("create --wait of a job that ends done: %+v", res)
	}

	finishLater(t, r, "job_2", v1.JobState_JOB_STATE_PARTIAL)
	if res := r.run("job", "create", "lab1", "--sender", "pacs", "sub-0018", "--wait"); res.code != 1 ||
		res.stderr != "nopacs: job job_2 ended partial\n" || field(res.stdout, "State") != "partial" {
		t.Errorf("create --wait of a job that ends partial: %+v", res)
	}

	finishLater(t, r, "job_3", v1.JobState_JOB_STATE_DONE)
	js := r.run("job", "create", "lab1", "--sender", "pacs", "sub-0019", "--wait", "--json")
	if js.code != 0 || strings.Count(js.stdout, "\"schemaVersion\": 1") < 2 || !strings.Contains(js.stdout, "\"state\": \"JOB_STATE_DONE\"") {
		t.Errorf("create --wait --json: %+v", js)
	}
}

// TestJobShow guards the name of the command that shows one job: "job show",
// as every noun names it, with "job get" kept as the same command.
func TestJobShow(t *testing.T) {
	r := jobRig(t)
	r.must("job", "create", "lab1", "--sender", "pacs", "sub-0017")
	show := r.run("job", "show", "lab1", "job_1")
	get := r.run("job", "get", "lab1", "job_1")
	if show.code != 0 || field(show.stdout, "ID") != "job_1" || show.stdout != get.stdout || get.code != 0 {
		t.Errorf("job show: %+v\njob get: %+v", show, get)
	}
}
