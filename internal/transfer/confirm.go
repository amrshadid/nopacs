package transfer

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// confirmAll finds each copied item in the target's catalog and compares
// what the target received with what the source archived. The target gives
// an instance the UID its receiver's mapping gives (the source's UID when the
// receiver archives as received, its keyed remapping when it de-identifies)
// and records the checksum it received the bytes with: the SHA-256 of the
// source's archived bytes as they are, or the keyed form of it when the
// receiver de-identifies (receive.Pipeline.TransferRow computes the form that
// applies). An item that agrees has its provenance recorded in the target,
// then is confirmed; one whose checksum differs fails and is never
// confirmed; one the target does not hold yet waits, and fails once it has
// waited ConfirmWait.
func (r *Runner) confirmAll(ctx context.Context, tj tenant.TransferJob, tgt Target) (Result, error) {
	pending := 0
	until := r.now()
	after := tenant.TransferItemKey{}
	for {
		items, err := r.cfg.Source.TransferItems(ctx, tj.Job.ID, tenant.TransferItemCopied, after, pageSize)
		if err != nil {
			return Result{Step: tj.Step}, err
		}
		if len(items) == 0 {
			break
		}
		after = items[len(items)-1].Key()
		rows := make([]receive.TransferRow, len(items))
		uids := make([]string, len(items))
		for i, it := range items {
			if rows[i], err = tgt.TransferRow(ctx, tj.Receiver, it.SOPInstanceUID, it.SHA256); err != nil {
				return Result{Step: tj.Step}, err
			}
			uids[i] = rows[i].SOPInstanceUID
		}
		held, err := tgt.FindTransferred(ctx, uids)
		if err != nil {
			return Result{Step: tj.Step}, err
		}
		var provenance []tenant.TransferProvenance
		var agreed []int
		for i, it := range items {
			f, ok := held[rows[i].SOPInstanceUID]
			switch {
			case !ok && r.now().Sub(it.Copied) >= r.cfg.ConfirmWait:
				if err := r.recordFailed(ctx, tj, it, tenant.FailurePeerUnaccounted, tenant.TransferDetailNotFound); err != nil {
					return Result{Step: tj.Step}, err
				}
			case !ok:
				pending++
				until = r.now().Add(retryWait)
			case f.SHA256 != rows[i].SHA256:
				if err := r.recordFailed(ctx, tj, it, tenant.FailurePeerRefused, tenant.TransferDetailMismatch); err != nil {
					return Result{Step: tj.Step}, err
				}
			default:
				p := tenant.TransferProvenance{SOPInstanceUID: rows[i].SOPInstanceUID, SourceTenantID: r.cfg.SourceID, Revision: it.Revision,
					JobID: tj.Job.ID, Recorded: r.now()}
				// The labels of an instance the source archived as received
				// may be an original identifier; those of one it
				// de-identified are the archive's own.
				p.Path = label.Path{}
				if !it.AsReceived {
					p.Path = it.Path
				}
				provenance = append(provenance, p)
				agreed = append(agreed, i)
			}
		}
		if err := tgt.RecordTransferProvenance(ctx, provenance); err != nil {
			return Result{Step: tj.Step}, err
		}
		for _, i := range agreed {
			it := items[i]
			rec := tenant.TransferRecord{JobID: tj.Job.ID, Key: it.Key(), State: tenant.TransferItemConfirmed, TargetUID: rows[i].SOPInstanceUID, Now: r.now()}
			if err := r.cfg.Source.RecordTransferItem(ctx, rec); err != nil {
				return Result{Step: tj.Step}, err
			}
		}
	}
	if pending > 0 {
		// Items that wait stay copied, and the cursor passed them: the next
		// pass reads them again.
		return Result{Step: tj.Step, Waiting: true, Until: until}, nil
	}
	return r.afterConfirm(ctx, tj)
}

// recordFailed records an item as failed with the code and detail.
func (r *Runner) recordFailed(ctx context.Context, tj tenant.TransferJob, it tenant.TransferItem, code tenant.JobFailureCode, detail string) error {
	return r.cfg.Source.RecordTransferItem(ctx, tenant.TransferRecord{JobID: tj.Job.ID, Key: it.Key(), State: tenant.TransferItemFailed,
		Code: code, Detail: detail, Now: r.now()})
}
