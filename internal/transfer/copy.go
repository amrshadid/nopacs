package transfer

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errTemporary says an item stays listed for a later pass: the target did not
// take it now, or a storage or the database did not answer.
var errTemporary = errors.New("transfer: the item is tried again later")

// copyAll copies every listed item once, with Workers at a time, and reports
// whether any is left for a later pass. An item the target already holds with
// the checksum the source's bytes give is recorded copied without being sent
// again: a restart that found the file archived, or still landed, sends
// nothing twice.
func (r *Runner) copyAll(ctx context.Context, tj tenant.TransferJob, tgt Target, rcv tenant.Receiver) (bool, error) {
	stores, closeStores, err := r.cfg.Storages(ctx)
	if err != nil {
		return false, err
	}
	defer closeStores()
	var waiting atomic.Bool
	after := tenant.TransferItemKey{}
	for {
		items, err := r.cfg.Source.TransferItems(ctx, tj.Job.ID, tenant.TransferItemListed, after, pageSize)
		if err != nil {
			return false, err
		}
		if len(items) == 0 {
			return waiting.Load(), nil
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
		held, err := tgt.FindTransferred(ctx, uids)
		if err != nil {
			return false, err
		}
		var wg sync.WaitGroup
		work := make(chan int)
		var firstErr atomic.Pointer[error]
		for range min(r.cfg.Workers, len(items)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				bufs := [2][]byte{make([]byte, checkedread.ChunkSize), make([]byte, checkedread.ChunkSize)}
				for i := range work {
					err := r.copyItem(ctx, tj, tgt, rcv, stores, items[i], rows[i], held, bufs)
					switch {
					case err == nil:
					case errors.Is(err, errTemporary):
						waiting.Store(true)
					default:
						firstErr.CompareAndSwap(nil, &err)
					}
				}
			}()
		}
		for i := range items {
			if ctx.Err() != nil {
				break
			}
			work <- i
		}
		close(work)
		wg.Wait()
		if e := firstErr.Load(); e != nil {
			return false, *e
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

// copyItem copies one item, or records why it is not: it answers nil when the
// item's record moved (copied or failed), errTemporary when it stays listed,
// and any other error for a record that could not be written.
func (r *Runner) copyItem(ctx context.Context, tj tenant.TransferJob, tgt Target, rcv tenant.Receiver, stores Storages, it tenant.TransferItem,
	row receive.TransferRow, held map[string]tenant.TransferFound, bufs [2][]byte) error {
	rec := tenant.TransferRecord{JobID: tj.Job.ID, Key: it.Key(), TargetUID: row.SOPInstanceUID}
	fail := func(code tenant.JobFailureCode, peer int32, detail string) error {
		f := rec
		f.State, f.Code, f.PeerStatus, f.Detail, f.Now = tenant.TransferItemFailed, code, peer, detail, r.now()
		f.TargetUID = ""
		return r.cfg.Source.RecordTransferItem(ctx, f)
	}
	if f, ok := held[row.SOPInstanceUID]; ok {
		if f.SHA256 != row.SHA256 {
			return fail(tenant.FailurePeerRefused, 0, tenant.TransferDetailMismatch)
		}
		rec.State, rec.Now = tenant.TransferItemCopied, r.now()
		return r.cfg.Source.RecordTransferItem(ctx, rec)
	}
	in, err := r.cfg.Source.HeldInstance(ctx, it.Path, it.SOPInstanceUID)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return fail(tenant.FailureDeletedInArchive, 0, tenant.TransferDetailGone)
	case err != nil:
		return errors.Join(errTemporary, err)
	case in.ArchivedSHA256 != it.SHA256:
		return fail(tenant.FailureDeletedInArchive, 0, tenant.TransferDetailChanged)
	}
	// The rule every path that hands archived bytes to someone outside the
	// tenant asks per instance, as it hands it over: a receiver that
	// de-identifies is what makes an instance archived as received fit to
	// leave.
	if code, refuse := tenant.SendRefusal(in.Provenance(), rcv.Deidentifies()); refuse {
		return fail(code, 0, refusalDetail(code))
	}
	// The receiver is read again by the target's pipeline for each file: what
	// must be de-identified goes only where it still is.
	mustDeid, _ := tenant.SendRefusal(in.Provenance(), false)
	needsDeid := mustDeid != ""
	if r.cfg.Reads != nil {
		release, err := r.cfg.Reads.Take("transfer:" + tj.Job.ID)
		if err != nil {
			return errTemporary
		}
		defer release()
	}
	o, err := stores.Open(ctx, checkedread.InstanceObject(in, "", it.Path), in.Offset, in.Length)
	switch {
	case checkedread.StorageFailed(err), ctx.Err() != nil:
		return errTemporary
	case errors.Is(err, blob.ErrNotOnline):
		return fail(tenant.FailureNotOnline, 0, tenant.TransferDetailOffline)
	case errors.Is(err, checkedread.ErrMarked), fault.KindOf(err) == fault.NotFound:
		return fail(tenant.FailureDamagedInStorage, 0, tenant.TransferDetailDamaged)
	case err != nil:
		return errTemporary
	}
	defer func() { _ = o.Body.Close() }()
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	sum := in.ArchivedSHA256
	go func() {
		_, err := checkedread.CopyChecked(o.Body, in.Length, &sum, bufs, func(b []byte) error {
			_, err := pw.Write(b)
			return err
		})
		if checkedread.Mismatched(err) {
			if rerr := stores.Mismatch(ctx, o, err); rerr != nil {
				r.log.Warn("transfer: a replica's bytes found wrong could not be recorded as its damage", "replica", o.Storage, "kind", fault.KindOf(rerr).String())
			}
		}
		_ = pw.CloseWithError(err) // A nil error is the end of the instance.
		copied <- err
	}()
	out := tgt.AcceptTransfer(ctx, receive.Arrival{Receiver: tj.Receiver, Body: pr, RequireDeidentified: needsDeid})
	_ = pr.CloseWithError(io.ErrClosedPipe)
	readErr := <-copied
	switch {
	case checkedread.Mismatched(readErr):
		return fail(tenant.FailureDamagedInStorage, 0, tenant.TransferDetailDamaged)
	case readErr != nil && !errors.Is(readErr, io.ErrClosedPipe):
		return errTemporary
	}
	switch out.Status {
	case receive.Received:
		rec.State, rec.Now = tenant.TransferItemCopied, r.now()
		return r.cfg.Source.RecordTransferItem(ctx, rec)
	case receive.Failed:
		if out.Reason == receive.ReasonNotDeidentified {
			code, _ := tenant.SendRefusal(in.Provenance(), false)
			return fail(code, 0, refusalDetail(code))
		}
		return fail(tenant.FailurePeerRefused, 0, tenant.TransferDetailRefused+": "+out.Reason)
	}
	r.log.Info("transfer: the target did not take a file now; it is tried again", "job", tj.Job.ID, "reason", out.Reason)
	return errTemporary
}

// refusalDetail returns the phrase a refusal of tenant.SendRefusal records.
func refusalDetail(code tenant.JobFailureCode) string {
	switch code {
	case tenant.FailureAsReceived:
		return tenant.TransferDetailAsRecv
	case tenant.FailureNoProfileProvenance:
		return tenant.TransferDetailNoProfile
	}
	return tenant.TransferDetailPixels
}
