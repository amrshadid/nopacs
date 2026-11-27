package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestCorrectionDeleteBatch guards the batch delete's flow: the targets
// named neither or both ways, a line that is not a path of labels, a
// search with a filter it does not know, and a missing reason are refused
// before anything is sent; --preview shows every target and makes no job;
// without --yes and with no terminal nothing is made (exit 2); --yes makes
// the job and names how to follow it; a second batch while it runs is
// refused (exit 5); --from-search makes a target of each scan the filters
// select; the targets' outcomes are listed.
func TestCorrectionDeleteBatch(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	seedArchive(r)
	list := filepath.Join(t.TempDir(), "withdrawn.txt")
	if err := os.WriteFile(list, []byte("# consent withdrawn\nsub-a/ses-2\n\nsub-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(bad, []byte("sub-a//ses\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"correction", "delete-batch", "lab1", "--reason", "x"},
		{"correction", "delete-batch", "lab1", "--file", list, "--from-search", "modality=MR", "--reason", "x"},
		{"correction", "delete-batch", "lab1", "--file", bad, "--reason", "x"},
		{"correction", "delete-batch", "lab1", "--from-search", "shade=red", "--reason", "x"},
		{"correction", "delete-batch", "lab1", "--from-search", "modality=MR modality=CT", "--reason", "x"},
		{"correction", "delete-batch", "lab1", "--file", list, "--reason", " "},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	res := r.run("correction", "delete-batch", "lab1", "--file", list, "--reason", "consent withdrawn", "--preview")
	if res.code != 0 || !strings.Contains(res.stdout, "sub-a/ses-2") || !strings.Contains(res.stdout, "sub-b") || !strings.Contains(res.stdout, "KEPT UNTIL") {
		t.Errorf("a preview: %+v", res)
	}
	if out := mustOut(r, "job", "list", "lab1"); strings.Contains(out, "delete batch") {
		t.Errorf("a preview made a job: %s", out)
	}
	res = r.run("correction", "delete-batch", "lab1", "--file", list, "--reason", "consent withdrawn")
	if res.code != 2 || !strings.Contains(res.stderr, "nothing was deleted") {
		t.Errorf("a batch without --yes: %+v", res)
	}
	out := mustOut(r, "correction", "delete-batch", "lab1", "--file", list, "--reason", "consent withdrawn", "--yes")
	if !strings.Contains(out, "batch job job_1 is queued: 2 targets") || !strings.Contains(out, "nopacs job watch lab1 job_1") {
		t.Errorf("the batch said %q", out)
	}
	if res := r.run("correction", "delete-batch", "lab1", "--from-search", "modality=CT", "--reason", "x", "--yes"); res.code != 5 {
		t.Errorf("a second batch while one runs: %+v, want exit 5", res)
	}
	r.fake.SetBatchOutcome("lab1", "job_1", 1, v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_DELETED, "01jd000000000000000000000a")
	r.fake.SetBatchOutcome("lab1", "job_1", 2, v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_HELD, "")
	out = mustOut(r, "correction", "batch-targets", "lab1", "job_1")
	if !strings.Contains(out, "deleted") || !strings.Contains(out, "01jd000000000000000000000a") || !strings.Contains(out, "skipped-held") {
		t.Errorf("the targets are %q", out)
	}
	if res := r.run("job", "cancel", "lab1", "job_1"); res.code != 0 {
		t.Fatalf("job cancel: %+v", res)
	}
	doc := document(t, r.run("correction", "delete-batch", "lab1", "--from-search", "modality=MR", "--reason", "scanner test data", "--yes", "--json"))
	if job, _ := doc["job"].(map[string]any); job == nil || job["deleteBatch"].(map[string]any)["targets"] != "2" {
		t.Errorf("a batch from a search: %v", doc)
	}
}

// TestJobPauseResume guards job pause and resume: a batch job answered
// paused, then running again; a job of another kind is refused (exit 5).
func TestJobPauseResume(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	seedArchive(r)
	r.must("correction", "delete-batch", "lab1", "--from-search", "modality=CT", "--reason", "scanner test data", "--yes")
	if out := mustOut(r, "job", "pause", "lab1", "job_1"); !strings.Contains(out, "job job_1 is queued (paused)") || !strings.Contains(out, "Paused") {
		t.Errorf("job pause said %q", out)
	}
	if out := mustOut(r, "job", "resume", "lab1", "job_1"); !strings.Contains(out, "(running again)") {
		t.Errorf("job resume said %q", out)
	}
	r.fake.AddJob("lab1", &v1.Job{Id: "send_1", Kind: &v1.Job_Send{Send: &v1.SendJob{Sender: "peer", SubjectLabel: "sub-a"}},
		State: v1.JobState_JOB_STATE_RUNNING, Counts: &v1.JobCounts{}})
	if res := r.run("job", "pause", "lab1", "send_1"); res.code != 5 || !strings.Contains(res.stderr, "only a batch delete job pauses") {
		t.Errorf("a pause of a send job: %+v, want exit 5", res)
	}
}
