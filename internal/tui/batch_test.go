package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// batchSetup is tenant t000 with one session of two scans, and a hold and a
// retention minimum on the first scan's preview.
func batchSetup(f *fakeserver.Server) {
	catalogSetup(f)
	f.SetBatchItem(func(it *v1.DeleteBatchItem) {
		if it.GetTarget().GetScanLabel() == "scan-1" {
			it.Holds = []string{"litigation"}
		}
		if it.GetTarget().GetScanLabel() == "scan-2" {
			it.SessionsUnderMinimum, it.KeepUntil, it.Replicas = 1, timestamp(24*30), []string{"copy"}
		}
	})
}

// markBoth opens the scans of the first session and marks both for a batch.
func markBoth(r *rig) {
	r.key("enter", "enter", "c", "enter", "enter")
	r.expect("Scans of sub-a/ses-1", "scan-1", "scan-2")
	r.key("x")
	r.expect("sub-a/ses-1/scan-1 marked for a batch delete (1 marked)")
	r.key("down", "x")
	r.expect("sub-a/ses-1/scan-2 marked for a batch delete (2 marked)")
}

// TestBatchDeleteFromMarkedRowsPreviewsThenFollowsItsJob guards the whole batch
// flow: rows marked in the catalog become the targets; the preview shows each with
// its hold, its retention minimum and its replicas; the confirmation needs the
// tenant's name; and the job the batch made is followed, with each target's
// outcome, and paused, resumed and canceled.
func TestBatchDeleteFromMarkedRowsPreviewsThenFollowsItsJob(t *testing.T) {
	r := newRig(t, 120, 40, batchSetup)
	markBoth(r)
	r.key("b")
	r.expect("Delete a batch in t000", "2 marked in the catalog")
	batches := r.calls("DeleteBatch")
	// No reason: refused before anything is sent.
	r.key("enter", "enter", "enter")
	r.expect("say why the data is deleted")
	if r.calls("DeleteBatch") != batches {
		t.Fatalf("a preview was asked for with no reason")
	}
	r.typeText("consent withdrawn")
	r.key("enter")
	if r.calls("DeleteBatch") != batches+1 {
		t.Fatalf("DeleteBatch was called %d times for the preview", r.calls("DeleteBatch")-batches)
	}
	r.expect("Delete 2 targets of t000", "Targets:", "Held (skipped):", "Instances:", "2", "Confirm token:", "fake-batch.",
		"sub-a/ses-1/scan-1:", "HELD by litigation", "sub-a/ses-1/scan-2:", "KEPT by a retention rule until", "replicas copy")

	r.key("c")
	r.expect("deletes each target in turn")
	r.key("enter")
	r.expect("not done: type t000 exactly")
	if r.calls("DeleteBatch") != batches+1 {
		t.Fatalf("a batch was started with the tenant's name not typed")
	}
	r.typeText("t000")
	r.key("enter")
	if r.calls("DeleteBatch") != batches+2 {
		t.Fatalf("the batch was not started")
	}
	r.expect("Job job_1", "Deletes:", "2 targets, one at a time", "Delete reason:", "consent withdrawn", "t targets", "p pause", "u resume")

	// The targets, and what became of each.
	r.key("t")
	r.expect("Targets of job job_1", "sub-a/ses-1/scan-1", "pending", "sub-a/ses-1/scan-2")
	r.fake.SetBatchOutcome("t000", "job_1", 1, v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_HELD, "")
	r.fake.SetBatchOutcome("t000", "job_1", 2, v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_DELETED, "cor_9")
	r.key("ctrl+r")
	r.expect("skipped-held", "deleted", "cor_9")
	r.key("esc")

	// Pause, resume and cancel.
	r.key("p")
	r.expect("Paused:", "yes (job resume goes on)", "job job_1 is paused")
	if r.calls("PauseJob") != 1 {
		t.Fatalf("the pause was not sent")
	}
	r.key("u")
	r.refuse("yes (job resume goes on)")
	r.expect("Paused:", "no", "job job_1 is running again")
	if r.calls("ResumeJob") != 1 {
		t.Fatalf("the resume was not sent")
	}
	r.key("c")
	r.typeText("job_1")
	r.key("enter")
	if r.calls("CancelJob") != 1 {
		t.Fatalf("the cancel was not sent")
	}
	// The marks were used up by the batch.
	if got := r.m.c.marked.marked("t000"); len(got) != 0 {
		t.Errorf("the marks stayed after the batch: %v", got)
	}
}

// TestBatchDeleteTakesATypedListOrAFilterAndNotBoth guards the targets of a
// batch: typed references, or a catalog filter that selects the scans, one of
// the two; a filter that selects no scan, or is not key=value, says so.
func TestBatchDeleteTakesATypedListOrAFilterAndNotBoth(t *testing.T) {
	r := newRig(t, 120, 40, batchSetup)
	r.key("enter", "enter", "x", "b")
	r.expect("Delete a batch in t000", "0 marked")
	batches := r.calls("DeleteBatch")
	r.fill("sub-a/ses-1", "modality=SR", "duplicates")
	r.key("enter")
	r.expect("name the targets or give a filter, one of the two")
	r.key("shift+tab", "shift+tab", "ctrl+u")
	r.key("tab")
	r.key("ctrl+u")
	r.typeText("color=red")
	r.key("enter", "enter")
	r.expect("is not a filter")
	r.key("shift+tab", "ctrl+u")
	r.typeText("modality=PT")
	r.key("enter", "enter")
	r.expect("no scan of the tenant matches the filter")
	if r.calls("DeleteBatch") != batches {
		t.Fatalf("a batch was previewed with no target")
	}
	r.key("shift+tab", "ctrl+u")
	r.typeText("modality=SR")
	r.key("enter", "enter")
	r.expect("Delete 1 targets of t000", "sub-a/ses-1/scan-2:")
	if r.calls("DeleteBatch") != batches+1 {
		t.Fatalf("the filter's preview was not asked for")
	}
}

// TestBatchDeleteRefusedToAMaskedKeyShowsItsReason guards a masked key's batch:
// the server refuses it and the form shows why; the mark and batch keys are not
// offered to a key known to be masked.
func TestBatchDeleteRefusedToAMaskedKeyShowsItsReason(t *testing.T) {
	r := newRig(t, 120, 40, batchSetup)
	r.fake.SetCallerMasked(true)
	r.key("enter", "enter", "x", "b")
	r.fill("sub-a", "", "duplicates")
	r.key("enter")
	r.expect("a masked key may not delete in bulk; a person does", "Delete a batch in t000")
}

// TestBatchRefusalAtTheConfirmationShowsItsReasonCode guards a batch the server
// refuses at its confirmation for a reason of the contract's own: the code and the
// message show on the form.
func TestBatchRefusalAtTheConfirmationShowsItsReasonCode(t *testing.T) {
	r := newRig(t, 120, 40, batchSetup)
	r.key("enter", "enter", "x", "b")
	r.fill("sub-a", "", "duplicates")
	r.key("enter", "c")
	r.fake.FailWithReason(nopacsv1connect.CorrectionServiceDeleteBatchProcedure, fault.Precondition, "a retention rule keeps sub-a", fault.ReasonRetentionMinimum, 0)
	r.typeText("t000")
	r.key("enter")
	r.expect("RETENTION_MINIMUM: a retention rule keeps sub-a", "deletes each target in turn")
}

// TestBatchJobKeysFollowTheRole guards the keys of a batch job: an operator
// pauses and lists its targets but does not resume, an admin resumes, a masked key
// does not, and a server that refuses a masked key's resume says why.
func TestBatchJobKeysFollowTheRole(t *testing.T) {
	setup := func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.AddJob("t000", &v1.Job{Id: "job_8", State: v1.JobState_JOB_STATE_RUNNING, Paused: true, CreateTime: timestamp(-1), StateTime: timestamp(-1),
			Kind: &v1.Job_DeleteBatch{DeleteBatch: &v1.DeleteBatchJob{Reason: "consent withdrawn", Targets: 3}}, Counts: &v1.JobCounts{Scans: 3, Listed: true}})
	}
	open := func(r *rig) {
		r.key("w", "enter")
		r.expect("Job job_8", "Paused:", "yes (job resume goes on)")
		helpOf(r, "Job job_8")
	}
	r := openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_OPERATOR}}, setup)
	open(r)
	r.expect("targets", "pause")
	r.refuse("resume")

	r = openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, setup)
	open(r)
	r.expect("targets", "pause", "resume")

	r = openKeyTenant(t, []keyGrant{{role: v1.Role_ROLE_ADMIN}}, maskedKeySetup(setup))
	open(r)
	r.expect("pause")
	r.refuse("resume")

	// A server refuses a masked key's resume though the screen offered it.
	r = newRig(t, 120, 40, setup)
	r.fake.SetCallerMasked(true)
	r.key("enter", "enter", "w", "enter")
	r.expect("Job job_8")
	r.key("u")
	r.expect("job resume: a masked key may not delete in bulk; a person does")
}
