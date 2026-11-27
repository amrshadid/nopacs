package sqlkit

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A replica's backfill followed as a job (tenant.ReplicateJob), on the
// replicate_jobs table of each driver's tenant baseline schema beside the
// jobs table, and the reads of a storage's objects a recall's preview and a
// sample need.

// insertReplicateJob writes a replicate job of replica, running, that
// counts the scans its backfill marked; followReplicateJobs, run after the
// replica's counts are made again in the same transaction, moves it and
// ends it done once the backfill owes nothing. It writes the job's event.
func (k Sending) insertReplicateJob(ctx context.Context, tx SendingTx, id, keyID string, r tenant.ReplicateJob, scans int64, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO replicate_jobs (job_id, replica, primary_name, scheduled) VALUES (?, ?, ?, ?)`,
		id, r.Replica, r.Primary, k.D.Bool(r.Scheduled)); err != nil {
		return err
	}
	// No unit of work: the replica's rows are the work, which the
	// replicator copies. Every scan is listed at once.
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
		receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
		retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
		scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
		failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
		VALUES (?, 'replicate', '', '', '', '', '', '', NULL, 'running', ?, '', ?, ?, ?, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0, ?,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
		id, k.D.Bool(false), keyID, k.D.Time(now), k.D.Time(now), k.D.Time(now), k.D.Time(now), k.D.Bool(true), k.D.Bool(true),
		k.D.Bool(true), scans); err != nil {
		return err
	}
	j, err := jobInTx(ctx, tx, id, "")
	if err != nil {
		return err
	}
	e, err := tenant.JobStepEvent("", j.job, now)
	if err != nil {
		return err
	}
	return tx.WriteEvents(ctx, e)
}

// openReplicateJob reads the replica's replicate job queued or running, and
// whether there is one: at most one is, since a backfill answers the one
// open and an attach makes one only for a storage that was no replica.
func (k Sending) openReplicateJob(ctx context.Context, tx SendingTx, replica, lock string) (tenant.Job, bool, error) {
	var id string
	found, err := tx.QueryRow(ctx, `SELECT j.id FROM replicate_jobs AS r JOIN jobs AS j ON j.id = r.job_id
		WHERE r.replica = ? AND j.ended = ? ORDER BY j.created DESC, j.id DESC LIMIT 1`, []any{replica, k.D.Bool(false)}, &id)
	if err != nil || !found {
		return tenant.Job{}, false, err
	}
	r, err := jobInTx(ctx, tx, id, lock)
	return r.job, err == nil, err
}

// followReplicateJobs brings each replicate job of replica ("" for every
// replica) that is running to its replica's backfill count: the scans the
// replica still owes its backfill are not done, and once it owes none the
// job ends done, with its event.
func (k Sending) followReplicateJobs(ctx context.Context, tx SendingTx, replica string, now time.Time) error {
	q := `SELECT r.job_id, t.backfill FROM replicate_jobs AS r JOIN jobs AS j ON j.id = r.job_id
		JOIN replica_totals AS t ON t.replica = r.replica WHERE j.ended = ?`
	args := []any{k.D.Bool(false)}
	if replica != "" {
		q += ` AND r.replica = ?`
		args = append(args, replica)
	}
	type open struct {
		id   string
		owed int64
	}
	var jobs []open
	if err := tx.Query(ctx, q, args, func(r Row) error {
		var o open
		err := r.Scan(&o.id, &o.owed)
		jobs = append(jobs, o)
		return err
	}); err != nil {
		return err
	}
	for _, o := range jobs {
		before, err := jobInTx(ctx, tx, o.id, k.D.Lock)
		if err != nil {
			return err
		}
		c := tenant.BackfillCounts(before.job.Counts.Scans, o.owed)
		if o.owed > 0 {
			if c.ScansDone != before.job.Counts.ScansDone {
				if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = ? WHERE id = ?`, c.ScansDone, o.id); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'done', ended = ?, reason = '', state_at = ?, ended_at = ?, scans_done = scans
			WHERE id = ?`, k.D.Bool(true), k.D.Time(now), k.D.Time(now), o.id); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, o.id, "")
		if err != nil {
			return err
		}
		e, err := tenant.JobStepEvent(before.job.State, after.job, now)
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// cancelReplicateJobs cancels each replicate job of replica still running,
// as a detach of it does, with their events.
func (k Sending) cancelReplicateJobs(ctx context.Context, tx SendingTx, replica string, now time.Time) error {
	var ids []string
	if err := tx.Query(ctx, `SELECT r.job_id FROM replicate_jobs AS r JOIN jobs AS j ON j.id = r.job_id WHERE r.replica = ? AND j.ended = ?`,
		[]any{replica, k.D.Bool(false)}, func(r Row) error {
			var id string
			err := r.Scan(&id)
			ids = append(ids, id)
			return err
		}); err != nil {
		return err
	}
	for _, id := range ids {
		_, e, err := k.cancelInTx(ctx, tx, id, tenant.JobReasonCanceled, now)
		if err != nil {
			return err
		}
		if e != nil {
			if err := tx.WriteEvents(ctx, *e); err != nil {
				return err
			}
		}
	}
	return nil
}

// fillReplicate gives a replicate job what its replicate row holds.
func fillReplicate(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	r := &tenant.ReplicateJob{}
	found, err := tx.QueryRow(ctx, `SELECT replica, primary_name, scheduled FROM replicate_jobs WHERE job_id = ?`, []any{j.ID},
		&r.Replica, &r.Primary, &r.Scheduled)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "a replicate job has no replicate row")
	}
	j.Replicate = r
	return nil
}

// ReplicateJob implements tenant.Replication.
func (k Sending) ReplicateJob(ctx context.Context, replica string) (tenant.Job, bool, error) {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return tenant.Job{}, false, err
	}
	var out tenant.Job
	var found bool
	err := k.Host.Read(ctx, "the replica's replicate job cannot be read", func(tx SendingTx) error {
		var err error
		out, found, err = k.openReplicateJob(ctx, tx, replica, "")
		return err
	})
	return out, found, err
}

// ListStorageObjects is the name of StorageObjects' list, the first field
// of its cursors.
const ListStorageObjects = "tenant-storage-objects"

// StorageObjects implements tenant.Replication. The scans are read from
// scans_by_storage (a primary's) or by their rows in step on the replica,
// each scan's instances as a range of the instances' key, so a page stops
// at its limit; a cursor holds the last scan's id and SOP Instance UID, so
// it is good for one walk, not across a rebuild that makes the catalog's
// ids again.
func (k Sending) StorageObjects(ctx context.Context, q tenant.StorageObjectQuery, page master.Page) ([]tenant.StoredObject, string, error) {
	if err := q.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(q.Storage, q.Path.Subject, q.Path.Session, q.Path.Scan)
	var afterScan int64
	afterUID := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListStorageObjects, binding, 2)
		if err != nil {
			return nil, "", err
		}
		if afterScan, err = DecodeCursorInt(f[0]); err != nil {
			return nil, "", err
		}
		afterUID = f[1]
	}
	type row struct {
		scan int64
		o    tenant.StoredObject
	}
	var out []row
	err := k.Host.Read(ctx, "the storage's objects cannot be read", func(tx SendingTx) error {
		out = nil
		s, found, err := storageIn(ctx, tx, q.Storage, "")
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + q.Storage)
		}
		where := `s.storage = ?`
		args := []any{q.Storage}
		if s.ReplicaOf != "" {
			// In step at the catalog's digest as a read that turns to a
			// replica judges it (sameManifest): a scan with no manifest
			// digest is in step at none.
			where = `s.storage = ? AND EXISTS (SELECT 1 FROM replica_scans AS r WHERE r.replica = ? AND r.series_uid = s.series_uid
				AND r.state = 'in_step' AND ` + sameManifest + `)`
			args = []any{s.ReplicaOf, q.Storage}
		}
		if p := q.Path; p.Subject != "" {
			where += ` AND s.subject_folded = ? AND s.subject_label = ?`
			args = append(args, label.Fold(p.Subject), p.Subject)
			if p.Session != "" {
				where += ` AND s.session_folded = ? AND s.session_label = ?`
				args = append(args, label.Fold(p.Session), p.Session)
			}
			if p.Scan != "" {
				where += ` AND s.folded = ? AND s.label = ?`
				args = append(args, label.Fold(p.Scan), p.Scan)
			}
		}
		args = append(args, afterScan, afterScan, afterUID, page.Limit+1)
		return tx.Query(ctx, `SELECT s.id, s.subject_label, s.session_label, s.label, s.series_uid, i.sop_instance_uid, i.storage_key,
			i.size, i.sha256, i.archived_sha256 FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
			WHERE `+where+` AND (s.id > ? OR (s.id = ? AND i.sop_instance_uid > ?))
			ORDER BY s.id, i.sop_instance_uid LIMIT ?`, args, func(r Row) error { // #nosec G202 -- every fragment is a constant above; values are parameters.
			var x row
			var raw string
			var sum, archived []byte
			if err := r.Scan(&x.scan, &x.o.Path.Subject, &x.o.Path.Session, &x.o.Path.Scan, &x.o.SeriesUID, &x.o.SOPInstanceUID,
				&raw, &x.o.Size, &sum, &archived); err != nil {
				return err
			}
			key, err := instanceKey(raw, x.o.Path, x.o.SOPInstanceUID)
			if err != nil {
				return err
			}
			x.o.Storage, x.o.Key = q.Storage, key
			x.o.ArchivedSHA256 = ToSHA256(sum)
			if len(archived) > 0 {
				x.o.ArchivedSHA256 = ToSHA256(archived)
			}
			out = append(out, x)
			return nil
		})
	})
	if err != nil {
		return nil, "", err
	}
	got, last, more := Cut(out, page.Limit)
	objects := make([]tenant.StoredObject, len(got))
	for i, x := range got {
		objects[i] = x.o
	}
	if !more {
		return objects, "", nil
	}
	return objects, EncodeCursor(ListStorageObjects, binding, strconv.FormatInt(last.scan, 10), last.o.SOPInstanceUID), nil
}

// RecallsOf implements tenant.Recalls.
func (k Sending) RecallsOf(ctx context.Context, storage string, keys []string) (map[string]tenant.Recall, error) {
	if len(keys) > tenant.MaxRecallBatch {
		return nil, fault.New(fault.Invalid, "a read of recalls names at most 1000 objects")
	}
	for _, key := range keys {
		if err := tenant.ValidateRecallKey(storage, key); err != nil {
			return nil, err
		}
	}
	out := make(map[string]tenant.Recall, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(keys)+1)
	args = append(args, storage)
	for _, key := range keys {
		args = append(args, key)
	}
	err := k.Host.Read(ctx, "the recalls cannot be read", func(tx SendingTx) error {
		clear(out)
		return tx.Query(ctx, `SELECT `+recallColumns+` FROM recalls WHERE storage = ? AND object_key IN (`+marks(len(keys))+`)`, args,
			func(r Row) error {
				x, err := scanRecall(r)
				out[x.Key] = x
				return err
			})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
