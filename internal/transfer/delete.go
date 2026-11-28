package transfer

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deleteReason is the reason of the delete a move makes of its source: where
// the data went, and the job, which a pass that finds the source gone and
// its own delete unrecorded tells its delete by.
func deleteReason(tj tenant.TransferJob) string {
	return "moved to " + tj.TargetTenant + " (transfer " + tj.Job.ID + ")"
}

// deleteSource runs a move's last step: the source's own preview and delete
// of the subject or session, with the reason "moved to <target>" and the
// job's id. It is made only when every item of the job is confirmed (the
// job's store refuses the step otherwise), the target is read again and
// still holds every confirmed instance with the checksum the confirmation
// found, the source still holds exactly the instances the job copied, and
// the delete's own preview covers those instances and no others. A hold or a
// retention rule that stops the delete, or any of those checks failing, ends
// the job partial, and the copy stays. The delete a pass made is recorded on
// the job, so a pass that follows a stop finds it and makes no second; a
// stop between the delete and its record is found by the job's id in the
// delete's reason.
func (r *Runner) deleteSource(ctx context.Context, tj tenant.TransferJob, tgt Target) (Result, error) {
	wait := Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}
	if tj.DeleteCorrection != "" {
		done, res, err := r.settleDelete(ctx, tj)
		if done || err != nil {
			return res, err
		}
	}
	if r.cfg.Deleter == nil {
		return r.end(ctx, tj, tenant.JobPartial, "the server runs no corrections; the source is kept")
	}
	// The target is read again: a copy lost or changed since the
	// confirmation, which the source's delete would make the only loss of
	// the data, keeps the source.
	held, err := r.targetHolds(ctx, tj, tgt)
	if err != nil {
		return wait, err
	}
	if !held {
		return r.end(ctx, tj, tenant.JobPartial, reasonTargetChanged)
	}
	cov, err := r.coverage(ctx, tj)
	if fault.KindOf(err) == fault.NotFound {
		return r.sourceGone(ctx, tj)
	}
	if err != nil {
		return wait, err
	}
	if !cov.same {
		return r.end(ctx, tj, tenant.JobPartial, reasonSourceChanged)
	}
	del, release, err := r.cfg.Deleter(ctx)
	if err != nil {
		return wait, err
	}
	defer release()
	t := tenant.CorrectionTarget{Subject: tj.Job.Target.SubjectLabel, Session: tj.Job.Target.SessionLabel}
	p, err := del.Preview(ctx, t)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return r.sourceGone(ctx, tj)
	case err != nil:
		return wait, err
	case len(p.Holds) > 0:
		return r.end(ctx, tj, tenant.JobPartial, reasonHeld)
	case p.SessionsUnderMinimum > 0:
		return r.end(ctx, tj, tenant.JobPartial, reasonKept)
	case p.Counts.Instances != cov.instances || p.Counts.Scans != cov.scans:
		return r.end(ctx, tj, tenant.JobPartial, reasonPreviewDiffer)
	}
	req := correct.DeleteRequest{Target: t, Reason: deleteReason(tj), Confirm: p.Confirm, KeyID: tj.Job.CreatedByKeyID}
	if r.cfg.DeleteAudit != nil {
		req.Audit = r.cfg.DeleteAudit(tj, r.now())
	}
	if r.cfg.DeleteGuard != nil {
		if req.Guard, err = r.cfg.DeleteGuard(ctx, tj.Job.CreatedByKeyID); err != nil {
			return wait, err
		}
	}
	c, err := del.Delete(ctx, req)
	var refused *correct.KeyRefused
	switch {
	case errors.As(err, &refused):
		return r.end(ctx, tj, tenant.JobFailed, "stopped: the key that made the transfer may no longer delete ("+refused.Why+")")
	case errors.Is(err, tenant.ErrHeld):
		return r.end(ctx, tj, tenant.JobPartial, reasonHeld)
	case errors.Is(err, tenant.ErrRetentionMinimum):
		return r.end(ctx, tj, tenant.JobPartial, reasonKept)
	case c.ID == "" && err != nil && (fault.KindOf(err) == fault.Precondition || fault.KindOf(err) == fault.Invalid):
		r.log.Warn("transfer: the delete of a move's source was refused", "job", tj.Job.ID, "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		return r.end(ctx, tj, tenant.JobPartial, reasonDeleteFailed)
	case c.ID == "" && err != nil:
		return wait, err
	}
	if rerr := r.cfg.Source.RecordTransferDelete(ctx, tj.Job.ID, c.ID); rerr != nil {
		return Result{Step: tj.Step}, rerr
	}
	if err != nil {
		// Prepared and then not published (a storage that did not take its
		// record): the engine settles it, and a later pass finds the state.
		return wait, nil
	}
	tj.DeleteCorrection = c.ID
	_, res, serr := r.settleDelete(ctx, tj)
	return res, serr
}

// sourceGone ends a move whose source is not there at its delete: when the
// job's own delete is found (its reason names the job) it is recorded and the
// job ends done, every instance having been confirmed; when it is not, the
// source went without the job, and the job ends partial.
func (r *Runner) sourceGone(ctx context.Context, tj tenant.TransferJob) (Result, error) {
	reason := deleteReason(tj)
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		cs, next, err := r.cfg.Source.ListCorrections(ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindDelete}}, page)
		if err != nil {
			return Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}, err
		}
		for _, c := range cs {
			if c.Reason == reason && c.State != record.StateAbandoned {
				if err := r.cfg.Source.RecordTransferDelete(ctx, tj.Job.ID, c.ID); err != nil {
					return Result{Step: tj.Step}, err
				}
				return r.end(ctx, tj, tenant.JobDone, "")
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return r.end(ctx, tj, tenant.JobPartial, reasonSourceGone)
}

// targetHolds reports whether the target still holds every confirmed item,
// found by the UID its receiver's mapping gives and with the received
// checksum that mapping gives for the source's archived bytes, as the
// confirmation compared them.
func (r *Runner) targetHolds(ctx context.Context, tj tenant.TransferJob, tgt Target) (bool, error) {
	after := tenant.TransferItemKey{}
	for {
		items, err := r.cfg.Source.TransferItems(ctx, tj.Job.ID, tenant.TransferItemConfirmed, after, pageSize)
		if err != nil {
			return false, err
		}
		if len(items) == 0 {
			return true, nil
		}
		after = items[len(items)-1].Key()
		rows := make([]receive.TransferRow, len(items))
		uids := make([]string, len(items))
		for i, it := range items {
			if rows[i], err = tgt.TransferRow(ctx, tj.Receiver, it.SOPInstanceUID, it.SHA256); err != nil {
				return false, err
			}
			uids[i] = rows[i].SOPInstanceUID
		}
		found, err := tgt.FindTransferred(ctx, uids)
		if err != nil {
			return false, err
		}
		for i := range items {
			if f, ok := found[rows[i].SOPInstanceUID]; !ok || f.SHA256 != rows[i].SHA256 {
				return false, nil
			}
		}
	}
}

// settleDelete ends a move whose delete is recorded, and reports whether it
// settled the job's delete: one still prepared waits for the corrections
// engine to publish or abandon it; one abandoned (a storage that did not take
// its record) is not settled, and the caller makes the delete again; any
// other state means the source was deleted (and may have been undone since,
// which is the person's act, not the job's).
func (r *Runner) settleDelete(ctx context.Context, tj tenant.TransferJob) (bool, Result, error) {
	wait := Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}
	c, err := r.cfg.Source.Correction(ctx, tj.DeleteCorrection)
	if err != nil {
		return true, wait, err
	}
	switch c.State {
	case record.StatePrepared:
		return true, wait, nil
	case record.StateAbandoned:
		return false, Result{}, nil
	}
	res, err := r.end(ctx, tj, tenant.JobDone, "")
	return true, res, err
}

// sourceCoverage is what the source holds of the job's target, compared with
// what the job copied.
type sourceCoverage struct {
	// same says the source holds exactly the instances the job listed, each
	// with the archived checksum it had.
	same bool
	// instances and scans count what the job copied.
	instances, scans int64
}

// coverage reads the source's catalog over the job's target against the
// confirmed items: no instance arrived, none was replaced or deleted since.
// A subject or session no longer there is NotFound.
func (r *Runner) coverage(ctx context.Context, tj tenant.TransferJob) (sourceCoverage, error) {
	type key struct {
		path label.Path
		uid  string
	}
	want := make(map[key]tenant.SHA256, tj.Items.Confirmed)
	scans := map[label.Path]bool{}
	after := tenant.TransferItemKey{}
	for {
		items, err := r.cfg.Source.TransferItems(ctx, tj.Job.ID, tenant.TransferItemConfirmed, after, pageSize)
		if err != nil {
			return sourceCoverage{}, err
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			want[key{it.Path, it.SOPInstanceUID}] = it.SHA256
			scans[it.Path] = true
		}
		after = items[len(items)-1].Key()
	}
	seen := 0
	changed := false
	err := EachScan(ctx, r.cfg.Source, tj.Job.Target, func(path label.Path, _ tenant.CatalogScan) error {
		page := master.Page{Limit: tenant.MaxTransferItemBatch}
		for {
			held, next, err := r.cfg.Source.ListHeldInstances(ctx, path, page)
			if err != nil {
				return err
			}
			for _, in := range held {
				sum, ok := want[key{path, in.SOPInstanceUID}]
				if !ok || sum != in.ArchivedSHA256 {
					changed = true
				}
				seen++
			}
			if next == "" {
				return nil
			}
			page.Cursor = next
		}
	})
	if err != nil {
		return sourceCoverage{}, err
	}
	return sourceCoverage{same: !changed && seen == len(want), instances: int64(len(want)), scans: int64(len(scans))}, nil
}
