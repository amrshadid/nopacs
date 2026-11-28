package transfer

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestMoveReadsTheTargetAgainBeforeDeleting guards the move's last check:
// copies that left the target's catalog after they were confirmed keep the
// source, whose delete would otherwise be the only loss of the data. The job
// ends partial with its reason and the delete is never asked.
func TestMoveReadsTheTargetAgainBeforeDeleting(t *testing.T) {
	r := newRig(t, true)
	r.tgt.afterProvenance = func() {
		// Called inside the confirmation, with the target's lock held: the
		// copies are lost once every instance is confirmed.
		clear(r.tgt.held)
	}
	id := r.queue("ses-1", true)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Job.Reason != reasonTargetChanged || tj.DeleteCorrection != "" ||
		r.deleter.previews.Load() != 0 || r.deleter.deletes.Load() != 0 {
		t.Fatalf("the move ended %+v (previews %d, deletes %d), want partial %q and no delete asked", tj, r.deleter.previews.Load(),
			r.deleter.deletes.Load(), reasonTargetChanged)
	}
}

// TestReceiverChangedMidCopyTakesNoMoreAsReceived guards what leaves the
// source when the target's receiver is changed while the job copies: once it
// archives as received, an instance the source archived as received is
// refused by the receiver itself (nothing lands) and failed as as_received.
func TestReceiverChangedMidCopyTakesNoMoreAsReceived(t *testing.T) {
	r := newRig(t, true)
	r.cfg.Workers = 1
	r.src.archive("ses-3", true, scanFiles{label: "scan-a", files: generated(t, 12)[8:11]})
	r.tgt.afterAccept = func(n int) {
		if n == 1 {
			// Called with the target's lock held, after the first file.
			r.tgt.rcv.Profile = tenant.ProfileRef{}
		}
	}
	id := r.queue("ses-3", false)
	tj := r.runUntilEnded(id)
	if n := r.tgt.timesAccepted(); n != 1 {
		t.Fatalf("the target took %d files, want only the one before the change", n)
	}
	if tj.Job.State != tenant.JobPartial || tj.Items.Confirmed != 1 || tj.Items.Failed != 2 {
		t.Fatalf("the job ended %+v, want partial with one confirmed and two failed", tj)
	}
	fails, _, err := r.src.ListJobFailures(r.src.ctx, id, true, master.Page{Limit: 10})
	if err != nil || len(fails) != 2 || fails[0].Code != tenant.FailureAsReceived {
		t.Fatalf("failures: %+v, %v", fails, err)
	}
}

// TestStopAfterTheDeleteIsFoundByTheJobsReason guards a stop between a move's
// delete and its record: the next pass finds the source gone, finds the delete
// by the job named in its reason, records it and ends done; and a source
// deleted by something else ends partial.
func TestStopAfterTheDeleteIsFoundByTheJobsReason(t *testing.T) {
	r := newRig(t, false)
	r.src.failRecord.Store(true)
	id := r.queue("ses-1", true)
	run := r.runner()
	if _, err := run.Run(r.src.ctx, id); err == nil {
		t.Fatal("the first pass was not stopped at the record of the delete")
	}
	if r.deleter.deletes.Load() != 1 {
		t.Fatalf("the source was deleted %d times in the first pass, want once", r.deleter.deletes.Load())
	}
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobDone || tj.DeleteCorrection == "" || r.deleter.deletes.Load() != 1 {
		t.Fatalf("after the stop the job ended %+v (deletes %d), want done, its delete recorded and no second", tj, r.deleter.deletes.Load())
	}

	other := newRig(t, false)
	oid := other.queue("ses-1", true)
	other.cfg.Deleter = nil
	// Everything is copied and confirmed; then the subject goes without the job.
	other.tgt.afterProvenance = func() { other.src.gone.Store(true) }
	other.cfg.Deleter = func(context.Context) (Deleter, func(), error) { return other.deleter, func() {}, nil }
	got := other.runUntilEnded(oid)
	if got.Job.State != tenant.JobPartial || got.Job.Reason != reasonSourceGone || other.deleter.deletes.Load() != 0 {
		t.Fatalf("a source deleted by someone else: the job ended %+v, want partial %q", got, reasonSourceGone)
	}
}

// TestDeletePreviewMustCoverWhatWasCopied guards the move's delete against an
// instance archived between the source's check and the delete's own preview:
// the preview counts more than was copied, so nothing is deleted and the job
// ends partial.
func TestDeletePreviewMustCoverWhatWasCopied(t *testing.T) {
	r := newRig(t, false)
	id := r.queue("", true)
	r.deleter.beforePreview = func() {
		r.src.archive("ses-9", false, scanFiles{label: "scan-c", files: [][]byte{generated(t, 7)[6]}})
	}
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Job.Reason != reasonPreviewDiffer || r.deleter.deletes.Load() != 0 {
		t.Fatalf("the move ended %+v (deletes %d), want partial %q with nothing deleted", tj, r.deleter.deletes.Load(), reasonPreviewDiffer)
	}
}

// TestWaitingPastTransferWaitEndsTheJobFailed guards the bound on retrying:
// a target that refuses every file keeps the job waiting until transfer.wait
// has passed on the clock, and then the job ends failed, so its subject is
// free again.
func TestWaitingPastTransferWaitEndsTheJobFailed(t *testing.T) {
	r := newRig(t, false)
	r.tgt.busy.Store(true)
	r.cfg.Wait = func(context.Context) (time.Duration, error) { return 2 * time.Hour, nil }
	id := r.queue("ses-1", false)
	run := r.runner()
	res, err := run.Run(r.src.ctx, id)
	if err != nil || !res.Waiting || res.Ended {
		t.Fatalf("a pass against a busy target: %+v, %v, want waiting", res, err)
	}
	r.src.now.Add(int64(time.Hour / time.Microsecond))
	if res, err = run.Run(r.src.ctx, id); err != nil || !res.Waiting {
		t.Fatalf("a pass within transfer.wait: %+v, %v, want still waiting", res, err)
	}
	r.src.now.Add(int64(2 * time.Hour / time.Microsecond))
	res, err = run.Run(r.src.ctx, id)
	if err != nil || !res.Ended {
		t.Fatalf("a pass past transfer.wait: %+v, %v, want ended", res, err)
	}
	tj, _ := r.src.TransferJob(r.src.ctx, id)
	if tj.Job.State != tenant.JobFailed || tj.Job.Reason != reasonWaited {
		t.Fatalf("the job is %+v, want failed %q", tj.Job, reasonWaited)
	}
	// The subject is free: another transfer of it can be made.
	if _, err := r.src.QueueTransfer(r.src.ctx, tenant.TransferQueue{ID: r.src.id("job"), Target: tenant.JobTarget{SubjectLabel: "sub-01"}, TargetTenant: "other",
		TargetTenantID: "T0000000000002", Receiver: "xfer", SourceTenant: "lab", Reason: "again", Now: r.src.clock()}, r.src.audit(), nil); err != nil {
		t.Fatalf("a transfer of the subject after the first failed: %v", err)
	}
}
