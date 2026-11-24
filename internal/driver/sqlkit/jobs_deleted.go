package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// hiddenScanOf fills the labels, storage and series of s, a job's scan a
// delete hid whole, from its hidden row. A scan hidden by no delete still
// held (its delete purged) keeps them empty: its unit then meets no
// instance, held or deleted, and ends, its instances not answered staying
// remaining.
func hiddenScanOf(ctx context.Context, tx SendingTx, s *jobScanRow) error {
	_, err := tx.QueryRow(ctx, hiddenScanSQL, []any{s.scanID}, &s.scan.Path.Subject, &s.scan.Path.Session, &s.scan.Path.Scan,
		&s.scan.Storage, &s.series)
	return err
}

// DeletedPage returns the statement and arguments of a page of the
// instances of a job's scan's stream that a committed delete hid, after
// the SOP Instance UID after: the stream's bound (SendingDialect.stream)
// over hidden_instances, of the corrections that name the scan, in the
// stream's order.
func (d SendingDialect) DeletedPage(j tenant.Job, bound time.Time, scanID int64, subject, series, startAfter, after string,
	limit int) (string, []any) {
	cond := "h.scan_id = ?"
	args := []any{label.Fold(subject), series, scanID}
	if j.ReceptionID != "" {
		cond += " AND h.reception_id = ?"
		args = append(args, j.ReceptionID)
	} else {
		cond += " AND h.held_at <= ?"
		args = append(args, d.Time(bound))
	}
	cond += ` AND (h.sop_instance_uid > ? OR h.sop_instance_uid IN (SELECT sop_instance_uid FROM job_resends
		WHERE job_id = ? AND scan_id = ?)) AND h.sop_instance_uid > ? ORDER BY h.sop_instance_uid LIMIT ?`
	return deletedStreamSQL + cond, append(args, startAfter, j.ID, scanID, after, limit)
}

// DeletedJobInstances implements tenant.JobWork.
func (k Sending) DeletedJobInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	if err := fence.Validate(); err != nil {
		return nil, err
	}
	if after != "" {
		if err := tenant.ValidateInstanceUID(after); err != nil {
			return nil, err
		}
	}
	if limit < 1 || limit > tenant.MaxJobInstancePage {
		return nil, fault.New(fault.Invalid, "a page of a job's instances is 1 to 1000")
	}
	var out []tenant.ScanInstance
	err := k.Host.Read(ctx, "the job's deleted instances cannot be read", func(tx SendingTx) error {
		out = nil
		s, found, err := jobScanOf(ctx, tx, fence.ID, "")
		if err != nil {
			return err
		}
		if !found {
			return errNotJobUnit()
		}
		if s.series == "" {
			return nil // hidden by a delete purged since: nothing left to list
		}
		r, err := jobInTx(ctx, tx, s.jobID, "")
		if err != nil {
			return err
		}
		query, args := k.D.DeletedPage(r.job, r.bound, s.scanID, s.scan.Path.Subject, s.series, s.startAfter, after, limit)
		return tx.Query(ctx, query, args, func(row Row) error {
			var in tenant.ScanInstance
			var held DBTime
			err := row.Scan(&in.SOPInstanceUID, &in.ReceptionID, &in.Size, &held)
			in.Held = held.T
			out = append(out, in)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
