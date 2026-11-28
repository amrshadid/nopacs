package sqlkit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A job's listing: the scans its target holds, or a retry's ended job's
// scans, listed a page at a time by its listing unit, each queued as a
// unit of its own with the stream of instances it sends.

// listedScan is one scan a listing found, with its instances and bytes in
// the job's stream when the listing knows them, and where the listing goes
// on after it.
type listedScan struct {
	scanID           int64
	instances, bytes int64
	// skip says the scan has nothing to send, and only moves the listing.
	skip bool
	// sessionFolded and scanFolded are the folded labels a catalog's
	// listing is ordered by.
	sessionFolded, scanFolded string
	// startAfter is where a retry's stream of the scan starts, and
	// resendFrom the ended job's scan it sends again.
	startAfter string
	resendFrom *jobScanRow
}

// ListJobScans implements tenant.JobWork.
func (k Sending) ListJobScans(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, bool, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, false, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, false, err
	}
	var out tenant.Job
	var more bool
	err := k.Host.Write(ctx, "the job's scans cannot be listed", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, fence); err != nil {
			return err
		}
		jobID, err := listingJobOf(ctx, tx, fence.ID)
		if err != nil {
			return err
		}
		if jobID == "" {
			return errNotJobUnit()
		}
		r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return err
		}
		if r.job.State.Ended() || r.job.Counts.Listed {
			out, more = r.job, false
			return k.completeUnit(ctx, tx, fence, now)
		}
		var found []listedScan
		var full bool
		if !r.retryListed {
			found, full, err = k.listRetried(ctx, tx, r)
		} else {
			found, full, err = k.listTarget(ctx, tx, r)
		}
		if err != nil {
			return err
		}
		var scans, instances, bytes int64
		for _, s := range found {
			added, n, b, err := k.addJobScan(ctx, tx, r, s, now)
			if err != nil {
				return err
			}
			if added {
				scans, instances, bytes = scans+1, instances+n, bytes+b
			}
		}
		retryListed, targetListed := r.retryListed, r.targetListed
		retryAfter, listSession, listScan, listID := r.retryAfter, r.listSession, r.listScan, r.listID
		if n := len(found); n > 0 || !full {
			if !r.retryListed {
				if n > 0 {
					retryAfter = found[n-1].resendFrom.scanID
				}
				retryListed = !full
			} else {
				if n > 0 {
					last := found[n-1]
					listSession, listScan, listID = last.sessionFolded, last.scanFolded, last.scanID
				}
				targetListed = !full
			}
		}
		listed := retryListed && targetListed
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans = scans + ?, instances = instances + ?, bytes = bytes + ?,
			retry_listed = ?, target_listed = ?, listed = ?, retry_after = ?, list_session = ?, list_scan = ?, list_id = ?
			WHERE id = ?`, scans, instances, bytes, k.D.Bool(retryListed), k.D.Bool(targetListed),
			k.D.Bool(listed), retryAfter, listSession, listScan, listID, jobID); err != nil {
			return err
		}
		more = !listed
		if !listed {
			after, err := jobInTx(ctx, tx, jobID, "")
			out = after.job
			return err
		}
		if err := k.completeUnit(ctx, tx, fence, now); err != nil {
			return err
		}
		job, events, err := k.endIfDone(ctx, tx, jobID, now)
		if err != nil {
			return err
		}
		out = job
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, false, err
	}
	return out, more, nil
}

// listTarget lists the next page of what the job's target holds: a
// reception's scans for a forward, by reception_scans' key; a storage's
// scans for a scrub of one storage (a replica's primary's for a replica),
// by the scans' index on their storage; every scan of the tenant for a deid
// or scrub job of the whole tenant, by the scans' key; a scan (a scrub of
// one scan among them), a session's scans or a subject's scans of the
// catalog by scans_by_label,
// each with the instances held at or before the job's bound. full says the
// page was full, so more may follow.
func (k Sending) listTarget(ctx context.Context, tx SendingTx, r jobRow) ([]listedScan, bool, error) {
	j := r.job
	var found []listedScan
	if j.ReceptionID != "" {
		q, args := forwardScansSQL, []any{j.ReceptionID, r.listID, tenant.JobListPage}
		if j.Target.ScanLabel != "" {
			// A forward rule's series filter: the one scan of the
			// reception the job names.
			q, args = forwardScanSQL, []any{j.ReceptionID, label.Fold(j.Target.SubjectLabel), label.Fold(j.Target.SessionLabel),
				label.Fold(j.Target.ScanLabel), r.listID, tenant.JobListPage}
		}
		err := tx.Query(ctx, q, args, func(row Row) error {
			var s listedScan
			err := row.Scan(&s.scanID, &s.instances, &s.bytes)
			found = append(found, s)
			return err
		})
		return found, len(found) == tenant.JobListPage, err
	}
	t := j.Target
	type catalogScan struct {
		id                     int64
		instances, bytes       int64
		sessionFolded, scanKey string
		updated                DBTime
	}
	var rows []catalogScan
	scanRow := func(row Row) error {
		var c catalogScan
		err := row.Scan(&c.id, &c.instances, &c.bytes, &c.sessionFolded, &c.scanKey, &c.updated)
		rows = append(rows, c)
		return err
	}
	var err error
	switch {
	case j.Kind == tenant.JobScrub && j.Storage != "" && t.Whole():
		// A replica holds its primary's scans, under the same keys: a scrub
		// of a replica lists the primary's, and each scan's unit reads only
		// the replica.
		held := j.Storage
		var replicaOf string
		if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{j.Storage}, &replicaOf); err != nil {
			return nil, false, err
		}
		if replicaOf != "" {
			held = replicaOf
		}
		err = tx.Query(ctx, storageScansSQL, []any{held, r.listID, tenant.JobListPage}, scanRow)
	case t.Whole():
		err = tx.Query(ctx, tenantScansSQL, []any{r.listID, tenant.JobListPage}, scanRow)
	case t.ScanLabel != "":
		if r.listScan == "" {
			err = tx.Query(ctx, targetScanSQL, []any{label.Fold(t.SubjectLabel), label.Fold(t.SessionLabel),
				label.Fold(t.ScanLabel), t.SubjectLabel, t.SessionLabel, t.ScanLabel}, scanRow)
		}
	case t.SessionLabel != "":
		err = tx.Query(ctx, sessionScansSQL, []any{label.Fold(t.SubjectLabel), label.Fold(t.SessionLabel),
			t.SubjectLabel, t.SessionLabel, r.listScan, tenant.JobListPage}, scanRow)
	default:
		err = tx.Query(ctx, subjectScansSQL, []any{label.Fold(t.SubjectLabel), t.SubjectLabel, r.listSession,
			r.listScan, tenant.JobListPage}, scanRow)
	}
	if err != nil {
		return nil, false, err
	}
	for _, c := range rows {
		s := listedScan{scanID: c.id, instances: c.instances, bytes: c.bytes, sessionFolded: c.sessionFolded, scanFolded: c.scanKey}
		if c.updated.T.After(r.bound) {
			// Committed to since the job's bound: only what was held by
			// then is the job's.
			if _, err := tx.QueryRow(ctx, heldByCountSQL, []any{c.id, k.D.Time(r.bound)}, &s.instances, &s.bytes); err != nil {
				return nil, false, err
			}
		}
		found = append(found, s)
	}
	full := len(rows) == tenant.JobListPage && t.ScanLabel == ""
	return found, full, nil
}

// listRetried lists the next page of the scans of the ended job a retry
// sends again, those with an instance refused or not sent, each with the
// stream the retry sends of it: the ended scan's instances after its
// cursor, and its refused instances, which lie before it; or, when its
// refused instances were not all listed, its whole stream again.
func (k Sending) listRetried(ctx context.Context, tx SendingTx, r jobRow) ([]listedScan, bool, error) {
	var olds []jobScanRow
	err := tx.Query(ctx, retriedScansSQL, []any{r.job.RetryOf, r.retryAfter, tenant.JobListPage}, func(row Row) error {
		s, err := scanJobScan(row)
		olds = append(olds, s)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	var found []listedScan
	for i := range olds {
		o := &olds[i]
		s := listedScan{scanID: o.scanID, resendFrom: o}
		if o.scan.Counts.NothingToRetry() {
			// Nothing of it to send again; it still moves the listing on.
			s.skip = true
			found = append(found, s)
			continue
		}
		s.startAfter = tenant.RetryStart(o.startAfter, o.scan.Cursor, o.refusedNotListed)
		found = append(found, s)
	}
	return found, len(olds) == tenant.JobListPage, nil
}

// addJobScan writes a scan the listing found, its resends for a retry, and
// its unit, and returns whether it wrote it and its stream's instances and
// bytes: a scan whose stream holds nothing is not written.
func (k Sending) addJobScan(ctx context.Context, tx SendingTx, r jobRow, s listedScan, now time.Time) (bool, int64, int64, error) {
	if s.skip {
		return false, 0, 0, nil
	}
	j := r.job
	part := strconv.FormatInt(s.scanID, 10)
	workID := tenant.JobUnitID(j.ID, part)
	if o := s.resendFrom; o != nil {
		if err := k.copyResends(ctx, tx, j.ID, o); err != nil {
			return false, 0, 0, err
		}
		q, args := k.D.stream(j, r.bound, s.scanID, s.startAfter)
		if _, err := tx.QueryRow(ctx, streamCountSQL+q, args, &s.instances, &s.bytes); err != nil {
			return false, 0, 0, err
		}
	}
	if s.instances == 0 {
		return false, 0, 0, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_scans (job_id, scan_id, work_id, start_after, cursor, instances, bytes, sent,
		sent_bytes, warned, warned_bytes, refused, refused_bytes, refused_not_listed, ended)
		VALUES (?, ?, ?, ?, '', ?, ?, 0, 0, 0, 0, 0, 0, 0, ?)`, j.ID, s.scanID, workID, s.startAfter, s.instances, s.bytes,
		k.D.Bool(false)); err != nil {
		return false, 0, 0, err
	}
	if _, _, err := tx.Enqueue(ctx, tenant.Work{ID: workID, Kind: tenant.JobWorkKind(j.Kind, j.Sender), Subject: j.ID + "/" + part,
		UniqueKey: tenant.JobScanKey(j.ID, part), Due: now, MaxAttempts: tenant.JobUnitAttempts}, now); err != nil {
		return false, 0, 0, err
	}
	return true, s.instances, s.bytes, nil
}

// copyResends writes the instances a retry's scan sends again that lie at
// or before its start: the ended scan's own resends after its cursor, its
// refused instances and those it still owed, or, when its refused were not
// all listed, all its resends and those it owed (its whole stream is sent
// again). Each set may hold an instance another holds, hence ON CONFLICT
// on the resends' key.
func (k Sending) copyResends(ctx context.Context, tx SendingTx, jobID string, o *jobScanRow) error {
	if o.refusedNotListed > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO job_resends (job_id, scan_id, sop_instance_uid) SELECT CAST(? AS TEXT), scan_id, sop_instance_uid
			FROM job_resends WHERE job_id = ? AND scan_id = ? ON CONFLICT DO NOTHING`, jobID, o.jobID, o.scanID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO job_resends (job_id, scan_id, sop_instance_uid) SELECT CAST(? AS TEXT), scan_id, sop_instance_uid
			FROM job_resends WHERE job_id = ? AND scan_id = ? AND sop_instance_uid > ? ON CONFLICT DO NOTHING`,
			jobID, o.jobID, o.scanID, o.scan.Cursor); err != nil {
			return err
		}
		// The refused, found by the scan's key, which no relabeling of
		// the catalog changes.
		if _, err := tx.Exec(ctx, `INSERT INTO job_resends (job_id, scan_id, sop_instance_uid) SELECT CAST(? AS TEXT), `+
			strings.TrimPrefix(scanRefusedSQL, "SELECT ")+` ON CONFLICT DO NOTHING`, jobID, o.jobID, o.scanID, k.D.Bool(false)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO job_resends (job_id, scan_id, sop_instance_uid) SELECT CAST(? AS TEXT), scan_id, sop_instance_uid
		FROM job_owed WHERE job_id = ? AND scan_id = ? ON CONFLICT DO NOTHING`, jobID, o.jobID, o.scanID)
	return err
}

// stream returns the condition and arguments of a job's scan's stream:
// the instances of the scan held at or before the job's bound (for a
// forward, those its reception archived) after startAfter, and those the
// job resends.
func (d SendingDialect) stream(j tenant.Job, bound time.Time, scanID int64, startAfter string) (string, []any) {
	cond := "i.scan_id = ?"
	args := []any{scanID}
	if j.ReceptionID != "" {
		cond += " AND i.reception_id = ?"
		args = append(args, j.ReceptionID)
	} else {
		cond += " AND i.held_at <= ?"
		args = append(args, d.Time(bound))
	}
	cond += ` AND (i.sop_instance_uid > ? OR i.sop_instance_uid IN (SELECT sop_instance_uid FROM job_resends
		WHERE job_id = ? AND scan_id = ?))`
	args = append(args, startAfter, j.ID, scanID)
	return cond, args
}

// StreamPage returns the statement and arguments of a page of a job's
// scan's stream after the SOP Instance UID after: a range of the instances'
// key, (scan_id, sop_instance_uid), in its order.
func (d SendingDialect) StreamPage(j tenant.Job, bound time.Time, scanID int64, startAfter, after string, limit int) (string, []any) {
	cond, args := d.stream(j, bound, scanID, startAfter)
	return streamPageSQL + cond + ` AND i.sop_instance_uid > ? ORDER BY i.sop_instance_uid LIMIT ?`, append(args, after, limit)
}
