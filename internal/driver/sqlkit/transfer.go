package sqlkit

import (
	"context"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ListTransfers is the name of the list of a tenant's transfer jobs, the
// first field of its cursor.
const ListTransfers = "tenant-transfers"

// Transfer jobs (tenant.Transfers) on a SQL database: the job's own part,
// and its items, the instance list, which stays in the source tenant.

// transferJobIn reads a transfer job's own part with the lock given on its
// job's row, or NotFound.
func (k Sending) transferJobIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.TransferJob, error) {
	var out tenant.TransferJob
	var step string
	found, err := tx.QueryRow(ctx, `SELECT target_tenant, target_tenant_id, receiver, move, reason, step, delete_correction
		FROM transfer_jobs WHERE job_id = ?`+lock, []any{id}, &out.TargetTenant, &out.TargetTenantID, &out.Receiver, &out.Move, &out.Reason,
		&step, &out.DeleteCorrection)
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no transfer job with this id")
	}
	out.Step = tenant.TransferStep(step)
	r, err := jobInTx(ctx, tx, id, lock)
	if err != nil {
		return out, err
	}
	out.Job = r.job
	err = tx.Query(ctx, `SELECT state, count(*) FROM transfer_items WHERE job_id = ? GROUP BY state`, []any{id}, func(row Row) error {
		var state string
		var n int64
		if err := row.Scan(&state, &n); err != nil {
			return err
		}
		switch tenant.TransferItemState(state) {
		case tenant.TransferItemListed:
			out.Items.Listed = n
		case tenant.TransferItemCopied:
			out.Items.Copied = n
		case tenant.TransferItemConfirmed:
			out.Items.Confirmed = n
		case tenant.TransferItemFailed:
			out.Items.Failed = n
		}
		return nil
	})
	return out, err
}

// QueueTransfer implements tenant.Transfers.
func (k Sending) QueueTransfer(ctx context.Context, q tenant.TransferQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := q.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the transfer cannot be queued", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		if held, err := targetExists(ctx, tx, q.Target); err != nil || !held {
			if err != nil {
				return err
			}
			return notFound("the tenant holds no " + targetLevel(q.Target) + " with these labels")
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, q.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		busy, err := exists(ctx, tx, `SELECT 1 FROM jobs JOIN transfer_jobs ON transfer_jobs.job_id = jobs.id
			WHERE jobs.subject_label = ? AND `+k.D.NotEnded, q.Target.SubjectLabel)
		if err != nil {
			return err
		}
		if busy {
			return tenant.Precondition("a transfer job of this subject has not ended; one runs at a time over a subject")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
			receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
			retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
			scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
			failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
			VALUES (?, 'transfer', '', ?, ?, '', '', ?, NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
			q.ID, q.Target.SubjectLabel, q.Target.SessionLabel, q.Receiver, k.D.Bool(false), q.KeyID, k.D.Time(q.Now), k.D.Time(q.Now),
			k.D.Time(q.Now), k.D.Bool(false), k.D.Bool(true), k.D.Bool(true)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO transfer_jobs (job_id, target_tenant, target_tenant_id, receiver, move, reason, step,
			delete_correction) VALUES (?, ?, ?, ?, ?, ?, 'listing', '')`, q.ID, q.TargetTenant, q.TargetTenantID, q.Receiver,
			k.D.Bool(q.Move), q.Reason); err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, q.ID, "")
		if err != nil {
			return err
		}
		e, err := tenant.JobStepEvent("", r.job, q.Now)
		if err != nil {
			return err
		}
		out = r.job
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}

// TransferJob implements tenant.Transfers.
func (k Sending) TransferJob(ctx context.Context, id string) (tenant.TransferJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.TransferJob{}, err
	}
	var out tenant.TransferJob
	err := k.Host.Read(ctx, "the transfer job cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.transferJobIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// ListTransfers implements tenant.Transfers.
func (k Sending) ListTransfers(ctx context.Context, page master.Page) ([]tenant.TransferJob, string, error) {
	binding := k.Host.Binding("transfers")
	pos, err := PageAfterTime(page, ListTransfers, binding)
	if err != nil {
		return nil, "", err
	}
	query := `SELECT jobs.id FROM jobs JOIN transfer_jobs ON transfer_jobs.job_id = jobs.id`
	args := []any{}
	if pos != nil {
		query += ` WHERE (jobs.created, jobs.id) < (?, ?)`
		args = append(args, k.D.Time(pos.At), pos.ID)
	}
	query += ` ORDER BY jobs.created DESC, jobs.id DESC LIMIT ?`
	args = append(args, page.Limit+1)
	var out []tenant.TransferJob
	err = k.Host.Read(ctx, "the transfer jobs cannot be listed", func(tx SendingTx) error {
		out = nil
		var ids []string
		if err := tx.Query(ctx, query, args, func(r Row) error {
			var id string
			err := r.Scan(&id)
			ids = append(ids, id)
			return err
		}); err != nil {
			return err
		}
		for _, id := range ids {
			j, err := k.transferJobIn(ctx, tx, id, "")
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, TimeCursor(ListTransfers, binding, last.Job.Created, last.Job.ID), nil
}

// AddTransferItems implements tenant.Transfers.
func (k Sending) AddTransferItems(ctx context.Context, id string, items []tenant.TransferNewItem) (int, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return 0, err
	}
	if len(items) > tenant.MaxTransferItemBatch {
		return 0, fault.New(fault.Invalid, "a call adds at most 1000 items")
	}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return 0, err
		}
	}
	added := 0
	err := k.Host.Write(ctx, "the transfer's items cannot be added", func(tx SendingTx) error {
		added = 0
		tj, err := k.transferJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if tj.Job.State.Ended() {
			return fault.Wrap(fault.Precondition, tenant.ErrTransferEnded, "the transfer job has ended "+string(tj.Job.State))
		}
		if tj.Step != tenant.TransferListing {
			return tenant.Precondition("the transfer job lists no more: it is at the step " + string(tj.Step))
		}
		for _, it := range items {
			n, err := tx.Exec(ctx, `INSERT INTO transfer_items (job_id, subject_label, session_label, scan_label, sop_instance_uid, size,
				sha256, revision, as_received, state, target_uid, copied_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1 + (SELECT count(*) FROM instance_revisions WHERE sop_instance_uid = ? AND fate = 'superseded'),
				?, 'listed', '', NULL) ON CONFLICT DO NOTHING`, id, it.Path.Subject, it.Path.Session, it.Path.Scan, it.SOPInstanceUID,
				it.Size, it.SHA256[:], it.SOPInstanceUID, k.D.Bool(it.AsReceived))
			if err != nil {
				return err
			}
			added += int(n)
		}
		return nil
	})
	return added, err
}

// nextTransferStep returns the step that follows step in a job, and false
// for the last.
func nextTransferStep(step tenant.TransferStep, move bool) (tenant.TransferStep, bool) {
	switch step {
	case tenant.TransferListing:
		return tenant.TransferCopying, true
	case tenant.TransferCopying:
		return tenant.TransferConfirming, true
	case tenant.TransferConfirming:
		return tenant.TransferDeleting, move
	}
	return "", false
}

// StartTransferStep implements tenant.Transfers.
func (k Sending) StartTransferStep(ctx context.Context, id string, to tenant.TransferStep, now time.Time) (tenant.TransferJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.TransferJob{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.TransferJob{}, err
	}
	var out tenant.TransferJob
	err := k.Host.Write(ctx, "the transfer's step cannot be started", func(tx SendingTx) error {
		tj, err := k.transferJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = tj
		if tj.Step == to {
			return nil
		}
		if tj.Job.State.Ended() {
			return fault.Wrap(fault.Precondition, tenant.ErrTransferEnded, "the transfer job has ended "+string(tj.Job.State))
		}
		if next, ok := nextTransferStep(tj.Step, tj.Move); !ok || next != to {
			return tenant.Precondition("the transfer job is at the step " + string(tj.Step) + "; it cannot start " + string(to))
		}
		switch tj.Step {
		case tenant.TransferListing:
			if _, err := tx.Exec(ctx, `UPDATE jobs SET listed = ?, instances = (SELECT count(*) FROM transfer_items WHERE job_id = ?),
				bytes = (SELECT coalesce(sum(size), 0) FROM transfer_items WHERE job_id = ?) WHERE id = ?`, k.D.Bool(true), id, id, id); err != nil {
				return err
			}
		case tenant.TransferCopying:
			if tj.Items.Listed > 0 {
				return tenant.Precondition("the transfer job has items not yet copied")
			}
		case tenant.TransferConfirming:
			// Only a job whose every item is confirmed deletes its source: a
			// failed or an unconfirmed item keeps the copy and the source.
			if tj.Items.Copied > 0 || tj.Items.Failed > 0 || tj.Items.Listed > 0 {
				return tenant.Precondition("the transfer job has items not confirmed in the target; the source is not deleted")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE transfer_jobs SET step = ? WHERE job_id = ?`, string(to), id); err != nil {
			return err
		}
		var events []tenant.EventRecord
		if tj.Job.State == tenant.JobQueued {
			if events, err = k.batchProgressed(ctx, tx, tj.Job, now); err != nil {
				return err
			}
		}
		after, err := k.transferJobIn(ctx, tx, id, "")
		if err != nil {
			return err
		}
		out = after
		return tx.WriteEvents(ctx, events...)
	})
	return out, err
}

// TransferItems implements tenant.Transfers.
func (k Sending) TransferItems(ctx context.Context, id string, state tenant.TransferItemState, after tenant.TransferItemKey, limit int) ([]tenant.TransferItem, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return nil, err
	}
	if !state.Valid() {
		return nil, fault.New(fault.Invalid, "an item's state is listed, copied, confirmed or failed")
	}
	if limit < 1 || limit > tenant.MaxTransferItemPage {
		return nil, fault.New(fault.Invalid, "a page of a transfer's items holds 1 to 1000")
	}
	var out []tenant.TransferItem
	err := k.Host.Read(ctx, "the transfer's items cannot be read", func(tx SendingTx) error {
		out = nil
		if has, err := exists(ctx, tx, `SELECT 1 FROM transfer_jobs WHERE job_id = ?`, id); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the tenant has no transfer job with this id")
		}
		return tx.Query(ctx, `SELECT i.subject_label, i.session_label, i.scan_label, i.sop_instance_uid, i.size, i.sha256, i.revision,
			i.as_received, i.state, i.target_uid, i.copied_at, coalesce(f.code, ''), coalesce(f.detail, '')
			FROM transfer_items i LEFT JOIN job_failures f ON f.job_id = i.job_id AND f.subject_label = i.subject_label
			AND f.session_label = i.session_label AND f.scan_label = i.scan_label AND f.sop_instance_uid = i.sop_instance_uid
			WHERE i.job_id = ? AND i.state = ? AND (i.subject_label, i.session_label, i.scan_label, i.sop_instance_uid) > (?, ?, ?, ?)
			ORDER BY i.subject_label, i.session_label, i.scan_label, i.sop_instance_uid LIMIT ?`,
			[]any{id, string(state), after.Path.Subject, after.Path.Session, after.Path.Scan, after.SOPInstanceUID, limit}, func(row Row) error {
				var it tenant.TransferItem
				var sum []byte
				var st, code string
				var copied DBTime
				err := row.Scan(&it.Path.Subject, &it.Path.Session, &it.Path.Scan, &it.SOPInstanceUID, &it.Size, &sum, &it.Revision,
					&it.AsReceived, &st, &it.TargetUID, &copied, &code, &it.Detail)
				copy(it.SHA256[:], sum)
				it.State, it.Code, it.Copied = tenant.TransferItemState(st), tenant.JobFailureCode(code), copied.T
				out = append(out, it)
				return err
			})
	})
	return out, err
}

// RecordTransferItem implements tenant.Transfers.
func (k Sending) RecordTransferItem(ctx context.Context, r tenant.TransferRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the transfer item's state cannot be recorded", func(tx SendingTx) error {
		tj, err := k.transferJobIn(ctx, tx, r.JobID, k.D.Lock)
		if err != nil {
			return err
		}
		if tj.Job.State.Ended() {
			return fault.Wrap(fault.Precondition, tenant.ErrTransferEnded, "the transfer job has ended "+string(tj.Job.State))
		}
		var state string
		var size int64
		p := r.Key.Path
		found, err := tx.QueryRow(ctx, `SELECT state, size FROM transfer_items WHERE job_id = ? AND subject_label = ? AND session_label = ?
			AND scan_label = ? AND sop_instance_uid = ?`+k.D.Lock, []any{r.JobID, p.Subject, p.Session, p.Scan, r.Key.SOPInstanceUID}, &state, &size)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the transfer job lists no such instance")
		}
		cur := tenant.TransferItemState(state)
		if cur == r.State {
			return nil
		}
		var from []string
		switch r.State {
		case tenant.TransferItemCopied:
			from = []string{string(tenant.TransferItemListed)}
		case tenant.TransferItemConfirmed:
			from = []string{string(tenant.TransferItemCopied)}
		case tenant.TransferItemFailed:
			from = []string{string(tenant.TransferItemListed), string(tenant.TransferItemCopied)}
		}
		ok := false
		for _, f := range from {
			ok = ok || f == state
		}
		if !ok {
			return tenant.Precondition("an item " + state + " does not move to " + string(r.State))
		}
		key := []any{r.JobID, p.Subject, p.Session, p.Scan, r.Key.SOPInstanceUID}
		where := ` WHERE job_id = ? AND subject_label = ? AND session_label = ? AND scan_label = ? AND sop_instance_uid = ?`
		switch {
		case r.State == tenant.TransferItemCopied:
			_, err = tx.Exec(ctx, `UPDATE transfer_items SET state = ?, target_uid = ?, copied_at = ?`+where,
				append([]any{string(r.State), r.TargetUID, k.D.Time(r.Now)}, key...)...)
		case r.TargetUID != "":
			_, err = tx.Exec(ctx, `UPDATE transfer_items SET state = ?, target_uid = ?`+where, append([]any{string(r.State), r.TargetUID}, key...)...)
		default:
			_, err = tx.Exec(ctx, `UPDATE transfer_items SET state = ?`+where, append([]any{string(r.State)}, key...)...)
		}
		if err != nil {
			return err
		}
		var events []tenant.EventRecord
		switch r.State {
		case tenant.TransferItemConfirmed:
			if _, err := tx.Exec(ctx, `UPDATE jobs SET sent = sent + 1, sent_bytes = sent_bytes + ? WHERE id = ?`, size, r.JobID); err != nil {
				return err
			}
		case tenant.TransferItemFailed:
			if _, err := tx.Exec(ctx, `UPDATE jobs SET refused = refused + 1, refused_bytes = refused_bytes + ?, failures_listed = failures_listed + 1
				WHERE id = ?`, size, r.JobID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO job_failures (job_id, subject_label, session_label, scan_label, sop_instance_uid, scan_id,
				warning, code, peer_status, sop_class_uid, transfer_syntax_uid, detail, at) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, '', '', ?, ?)
				ON CONFLICT DO NOTHING`, r.JobID, p.Subject, p.Session, p.Scan, r.Key.SOPInstanceUID, k.D.Bool(false), string(r.Code),
				r.PeerStatus, r.Detail, k.D.Time(r.Now)); err != nil {
				return err
			}
		}
		if r.State != tenant.TransferItemCopied {
			if events, err = k.batchProgressed(ctx, tx, tj.Job, r.Now); err != nil {
				return err
			}
		} else if tj.Job.State == tenant.JobQueued {
			if events, err = k.batchProgressed(ctx, tx, tj.Job, r.Now); err != nil {
				return err
			}
		}
		return tx.WriteEvents(ctx, events...)
	})
}

// RecordTransferDelete implements tenant.Transfers.
func (k Sending) RecordTransferDelete(ctx context.Context, id, correction string) error {
	if err := master.ValidateID("job id", id); err != nil {
		return err
	}
	if err := tenant.ValidateCorrectionID(correction); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the transfer's delete cannot be recorded", func(tx SendingTx) error {
		tj, err := k.transferJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		switch {
		case tj.Job.State.Ended():
			return fault.Wrap(fault.Precondition, tenant.ErrTransferEnded, "the transfer job has ended "+string(tj.Job.State))
		case tj.Step != tenant.TransferDeleting:
			return tenant.Precondition("the transfer job is not at the step deleting")
		case tj.DeleteCorrection != "" && tj.DeleteCorrection != correction:
			// Only a delete abandoned (a storage that did not take its
			// record) is replaced by another: a delete that was published is
			// the move's, and a second would misstate what became of it.
			if err := checkBatchDelete(ctx, tx, tj.DeleteCorrection); err != nil {
				return tenant.Precondition("the transfer job recorded another delete")
			}
		}
		_, err = tx.Exec(ctx, `UPDATE transfer_jobs SET delete_correction = ? WHERE job_id = ?`, correction, id)
		return err
	})
}

// EndTransfer implements tenant.Transfers.
func (k Sending) EndTransfer(ctx context.Context, id string, state tenant.JobState, reason string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if state != tenant.JobDone && state != tenant.JobPartial && state != tenant.JobFailed {
		return tenant.Job{}, fault.New(fault.Invalid, "a transfer job ends done, partial or failed")
	}
	if len(reason) > tenant.MaxFailureLength {
		return tenant.Job{}, fault.New(fault.Invalid, "a transfer job's reason is at most 256 bytes")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the transfer job cannot be ended", func(tx SendingTx) error {
		tj, err := k.transferJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = tj.Job
		if tj.Job.State.Ended() {
			return nil
		}
		if state == tenant.JobDone && (tj.Items.Confirmed != tj.Items.Total() || tj.Items.Total() != tj.Job.Counts.Instances) {
			return tenant.Precondition("a transfer job is done only when every instance it lists is confirmed")
		}
		ended, events, err := k.endJob(ctx, tx, tj.Job, state, reason, now)
		if err != nil {
			return err
		}
		out = ended
		if _, err := tx.Exec(ctx, `UPDATE transfer_jobs SET step = ? WHERE job_id = ?`, string(tenant.TransferEnded), id); err != nil {
			return err
		}
		return tx.WriteEvents(ctx, events...)
	})
	return out, err
}

// FindTransferred implements tenant.TransferTargets.
func (k Sending) FindTransferred(ctx context.Context, uids []string) (map[string]tenant.TransferFound, error) {
	if err := tenant.ValidateTransferUIDs(uids); err != nil {
		return nil, err
	}
	out := make(map[string]tenant.TransferFound, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	args := make([]any, len(uids))
	for i, u := range uids {
		args[i] = u
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(uids)), ", ")
	err := k.Host.Read(ctx, "the instances cannot be looked up", func(tx SendingTx) error {
		return tx.Query(ctx, `SELECT i.sop_instance_uid, i.sha256, s.subject_label, s.session_label, s.label FROM instances i
			JOIN scans s ON s.id = i.scan_id WHERE i.sop_instance_uid IN (`+marks+`)`, args, func(row Row) error { // #nosec G202 -- only placeholders are joined; values are parameters.
			var uid string
			var sum []byte
			var f tenant.TransferFound
			if err := row.Scan(&uid, &sum, &f.Path.Subject, &f.Path.Session, &f.Path.Scan); err != nil {
				return err
			}
			copy(f.SHA256[:], sum)
			out[uid] = f
			return nil
		})
	})
	return out, err
}

// RecordTransferProvenance implements tenant.TransferTargets.
func (k Sending) RecordTransferProvenance(ctx context.Context, rows []tenant.TransferProvenance) error {
	for _, r := range rows {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return k.Host.Write(ctx, "the transfer's provenance cannot be recorded", func(tx SendingTx) error {
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO transfer_provenance (sop_instance_uid, source_tenant_id, subject_label, session_label, scan_label,
				revision, job_id, recorded) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (sop_instance_uid) DO NOTHING`,
				r.SOPInstanceUID, r.SourceTenantID, r.Path.Subject, r.Path.Session, r.Path.Scan, r.Revision, r.JobID, k.D.Time(r.Recorded)); err != nil {
				return err
			}
		}
		return nil
	})
}

// provenanceColumns are a provenance row's columns.
const provenanceColumns = `sop_instance_uid, source_tenant_id, subject_label, session_label, scan_label, revision, job_id, recorded`

// scanProvenance reads one row of provenanceColumns.
func scanProvenance(row Row) (tenant.TransferProvenance, error) {
	var p tenant.TransferProvenance
	var at DBTime
	err := row.Scan(&p.SOPInstanceUID, &p.SourceTenantID, &p.Path.Subject, &p.Path.Session, &p.Path.Scan, &p.Revision, &p.JobID, &at)
	p.Recorded = at.T
	return p, err
}

// TransferProvenanceOf implements tenant.TransferTargets.
func (k Sending) TransferProvenanceOf(ctx context.Context, uid string) (tenant.TransferProvenance, bool, error) {
	if err := tenant.ValidateInstanceUID(uid); err != nil {
		return tenant.TransferProvenance{}, false, err
	}
	var out tenant.TransferProvenance
	found := false
	err := k.Host.Read(ctx, "the provenance cannot be read", func(tx SendingTx) error {
		found = false
		return tx.Query(ctx, `SELECT `+provenanceColumns+` FROM transfer_provenance WHERE sop_instance_uid = ?`, []any{uid}, func(row Row) error {
			found = true
			var err error
			out, err = scanProvenance(row)
			return err
		})
	})
	return out, found, err
}

// listTransferProvenance is the name of the list of one job's provenance
// rows, the first field of its cursor.
const listTransferProvenance = "tenant-transfer-provenance"

// ListTransferProvenance implements tenant.TransferTargets.
func (k Sending) ListTransferProvenance(ctx context.Context, sourceJob string, page master.Page) ([]tenant.TransferProvenance, string, error) {
	if err := master.ValidateID("job id", sourceJob); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(sourceJob)
	after := []string{""}
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, listTransferProvenance, binding, 1)
		if err != nil {
			return nil, "", err
		}
		after = f
	}
	var out []tenant.TransferProvenance
	err := k.Host.Read(ctx, "the provenance cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+provenanceColumns+` FROM transfer_provenance WHERE job_id = ? AND sop_instance_uid > ?
			ORDER BY sop_instance_uid LIMIT ?`, []any{sourceJob, after[0], page.Limit + 1}, func(row Row) error {
			p, err := scanProvenance(row)
			out = append(out, p)
			return err
		})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(listTransferProvenance, binding, last.SOPInstanceUID), nil
}
