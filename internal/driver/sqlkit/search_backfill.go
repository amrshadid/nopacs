package sqlkit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's search backfills (tenant.SearchBackfills), as SQL both
// drivers run: the job's row of jobs, its own row of search_backfill_jobs
// (the attributes it covers, the last scan written, the scans skipped),
// and the writes of the search tables of search_write.go. The scans are
// counted as the job's instances, one each, so its counts say how far it is.

// QueueSearchBackfill implements tenant.SearchBackfills.
func (k Sending) QueueSearchBackfill(ctx context.Context, q tenant.SearchBackfillQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := q.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the search backfill cannot be queued", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		var state string
		found, err := tx.QueryRow(ctx, `SELECT state FROM search_attributes WHERE field = ?`, []any{q.Field}, &state)
		if err != nil {
			return err
		}
		switch {
		case !found:
			return fault.New(fault.Invalid, "the tenant's search tables hold no attribute named "+strconv.Quote(q.Field)+"; it is added by a migration first")
		case state == "ready":
			return tenant.Precondition(q.Field + " is filled for every scan already; there is nothing to backfill")
		}
		busy, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE kind = 'search_backfill' AND `+k.D.NotEnded)
		if err != nil {
			return err
		}
		if busy {
			return tenant.Precondition("the tenant has a search backfill job not ended; one runs at a time")
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, q.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		var fields []string
		err = tx.Query(ctx, `SELECT field FROM search_attributes WHERE state = 'building' ORDER BY field`, nil, func(r Row) error {
			var f string
			if err := r.Scan(&f); err != nil {
				return err
			}
			fields = append(fields, f)
			return nil
		})
		if err != nil {
			return err
		}
		var scans, hidden int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM scans`, nil, &scans); err != nil {
			return err
		}
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM hidden_scans`, nil, &hidden); err != nil {
			return err
		}
		scans += hidden
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
			receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
			retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
			scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
			failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
			VALUES (?, 'search_backfill', '', '', '', '', '', '', NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
			?, 0, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
			q.ID, k.D.Bool(false), q.KeyID, k.D.Time(q.Now), k.D.Time(q.Now), k.D.Time(q.Now), k.D.Bool(true), k.D.Bool(true), k.D.Bool(true),
			scans, scans); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO search_backfill_jobs (job_id, fields, after_scan_id, skipped) VALUES (?, ?, 0, 0)`,
			q.ID, strings.Join(fields, ",")); err != nil {
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

// backfillIn reads a search backfill job's own part with the lock given, or
// NotFound.
func (k Sending) backfillIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.SearchBackfillJob, error) {
	var out tenant.SearchBackfillJob
	var fields string
	found, err := tx.QueryRow(ctx, `SELECT fields, after_scan_id, skipped FROM search_backfill_jobs WHERE job_id = ?`+lock, []any{id},
		&fields, &out.After, &out.Skipped)
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no search backfill job with this id")
	}
	if fields != "" {
		out.Fields = strings.Split(fields, ",")
	}
	r, err := jobInTx(ctx, tx, id, lock)
	if err != nil {
		return out, err
	}
	out.Job = r.job
	return out, nil
}

// SearchBackfillJob implements tenant.SearchBackfills.
func (k Sending) SearchBackfillJob(ctx context.Context, id string) (tenant.SearchBackfillJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.SearchBackfillJob{}, err
	}
	var out tenant.SearchBackfillJob
	err := k.Host.Read(ctx, "the search backfill job cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.backfillIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// SearchBackfillScans implements tenant.SearchBackfills.
func (k Sending) SearchBackfillScans(ctx context.Context, after int64, limit int) ([]tenant.SearchBackfillScan, error) {
	if limit < 1 || limit > tenant.MaxSearchBackfillPage {
		return nil, fault.New(fault.Invalid, "a page of scans holds 1 to "+strconv.Itoa(tenant.MaxSearchBackfillPage))
	}
	var out []tenant.SearchBackfillScan
	err := k.Host.Read(ctx, "the scans cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT id, subject_label, session_label, label, storage, manifest_sha256, '' AS correction_id FROM scans WHERE id > ?
			UNION ALL SELECT id, subject_label, session_label, label, storage, manifest_sha256, correction_id FROM hidden_scans WHERE id > ?
			ORDER BY id, correction_id LIMIT ?`,
			[]any{after, after, limit}, func(r Row) error {
				var s tenant.SearchBackfillScan
				var manifest []byte
				err := r.Scan(&s.ID, &s.Path.Subject, &s.Path.Session, &s.Path.Scan, &s.Storage, &manifest, &s.Correction)
				s.Manifest = ToSHA256(manifest)
				out = append(out, s)
				return err
			})
	})
	return out, err
}

// ApplySearchBackfill implements tenant.SearchBackfills.
func (k Sending) ApplySearchBackfill(ctx context.Context, a tenant.SearchBackfillApply) (tenant.SearchBackfillJob, error) {
	if err := a.Validate(); err != nil {
		return tenant.SearchBackfillJob{}, err
	}
	var out tenant.SearchBackfillJob
	err := k.Host.Write(ctx, "the search backfill's page cannot be written", func(tx SendingTx) error {
		bj, err := k.backfillIn(ctx, tx, a.JobID, k.D.Lock)
		if err != nil {
			return err
		}
		if bj.Job.State.Ended() {
			return tenant.Precondition("the search backfill job has ended " + string(bj.Job.State) + "; it writes nothing more")
		}
		if bj.After != a.After {
			return tenant.Precondition("the page begins at scan " + strconv.FormatInt(a.After, 10) + " and the job stands at scan " +
				strconv.FormatInt(bj.After, 10) + "; another runner wrote it")
		}
		var filled, skipped int64
		for _, r := range a.Rows {
			var manifest []byte
			var held bool
			var err error
			if r.Correction != "" {
				held, err = tx.QueryRow(ctx, `SELECT manifest_sha256 FROM hidden_scans WHERE correction_id = ? AND id = ?`, []any{r.Correction, r.ID}, &manifest)
			} else {
				held, err = tx.QueryRow(ctx, `SELECT manifest_sha256 FROM scans WHERE id = ?`, []any{r.ID}, &manifest)
			}
			if err != nil {
				return err
			}
			if !held {
				// The scan was listed and is gone: deleted (its row is now in
				// the hidden tables), undone, or purged since. The page is read
				// again, which lists it where it is, or no more; skipping it
				// would end the job with its attribute ready and the scan's
				// column empty.
				return tenant.SearchBackfillStaleError(r.ID)
			}
			// The metadata was read under a manifest: a scan that has another
			// now was changed by an edit or an arrival, which wrote its own
			// row, and the one read would undo it.
			if ToSHA256(manifest) != r.Manifest {
				return tenant.SearchBackfillStaleError(r.ID)
			}
			if r.Skipped {
				skipped++
				continue
			}
			if r.Correction != "" {
				if err := writeHiddenSearch(ctx, tx, r.Correction, r.ID, r.Row.Normal()); err != nil {
					return err
				}
			} else if err := WriteSearch(ctx, tx, r.ID, r.Row.Normal(), search.LowestOrSame); err != nil {
				return err
			}
			filled++
		}
		c := bj.Job.Counts
		done := c.ScansDone + int64(len(a.Rows))
		sent, refused := c.InstancesSent+filled, c.InstancesRefused+skipped
		if _, err := tx.Exec(ctx, `UPDATE search_backfill_jobs SET after_scan_id = ?, skipped = skipped + ? WHERE job_id = ?`,
			a.Reached(), skipped, a.JobID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = ?, scans = ?, sent = ?, refused = ?, instances = ? WHERE id = ?`,
			done, max(c.Scans, done), sent, refused, max(c.Instances, sent+refused), a.JobID); err != nil {
			return err
		}
		events, err := k.batchProgressed(ctx, tx, bj.Job, a.Now)
		if err != nil {
			return err
		}
		if out, err = k.backfillIn(ctx, tx, a.JobID, ""); err != nil {
			return err
		}
		return tx.WriteEvents(ctx, events...)
	})
	return out, err
}

// EndSearchBackfill implements tenant.SearchBackfills.
func (k Sending) EndSearchBackfill(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the search backfill job cannot be ended", func(tx SendingTx) error {
		bj, err := k.backfillIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj.Job
		if bj.Job.State.Ended() {
			return nil
		}
		if more, err := exists(ctx, tx, `SELECT 1 FROM scans WHERE id > ? UNION ALL SELECT 1 FROM hidden_scans WHERE id > ?`, bj.After, bj.After); err != nil || more {
			if err != nil {
				return err
			}
			return tenant.Precondition("the search backfill job has scans past its place left to read")
		}
		for _, f := range bj.Fields {
			if _, err := tx.Exec(ctx, `UPDATE search_attributes SET state = 'ready' WHERE field = ?`, f); err != nil {
				return err
			}
		}
		// The planner counts rows from the statistics, and the column is new:
		// without them an index on it is chosen blind.
		if _, err := tx.Exec(ctx, `ANALYZE `+search.ScanTable); err != nil {
			return err
		}
		// The scans a page found gone are not the job's; its counts are of
		// the scans it wrote or could not read.
		c := bj.Job.Counts
		if _, err := tx.Exec(ctx, `UPDATE jobs SET instances = ?, scans = ? WHERE id = ?`, c.InstancesSent+c.InstancesRefused, c.ScansDone, id); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		state, reason := tenant.EndState(after.job.Counts)
		ended, events, err := k.endJob(ctx, tx, after.job, state, reason, now)
		if err != nil {
			return err
		}
		out = ended
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// StopSearchBackfill implements tenant.SearchBackfills.
func (k Sending) StopSearchBackfill(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if reason == "" || len(reason) > tenant.MaxFailureLength {
		return tenant.Job{}, fault.New(fault.Invalid, "a search backfill job is stopped with a reason of 1 to "+strconv.Itoa(tenant.MaxFailureLength)+" bytes")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the search backfill job cannot be stopped", func(tx SendingTx) error {
		bj, err := k.backfillIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj.Job
		if bj.Job.State.Ended() {
			return nil
		}
		ended, events, err := k.endJob(ctx, tx, bj.Job, tenant.JobFailed, reason, now)
		if err != nil {
			return err
		}
		out = ended
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// writeHiddenSearch gives the row a delete hid, of the scan with the id, the
// values of row, as LowestOrSame gives a held scan's (search.UpdateHiddenSQL),
// with the rows of its values tables. A scan with no hidden row has nothing
// to give.
func writeHiddenSearch(ctx context.Context, tx SendingTx, correction string, scanID int64, row search.Row) error {
	if row.IsZero() {
		row = search.Empty()
	}
	args := search.Args(scanID, row)[1:]
	args = append(args, correction, scanID, row.Source)
	n, err := tx.Exec(ctx, search.UpdateHiddenSQL(), args...)
	if err != nil || n == 0 {
		return err
	}
	for _, q := range search.DeleteHiddenValuesSQL() {
		if _, err := tx.Exec(ctx, q, correction, scanID); err != nil {
			return err
		}
	}
	for _, v := range search.Values(row) {
		if _, err := tx.Exec(ctx, search.InsertHiddenValueSQL(v), correction, scanID, v.Text, v.FoldedText); err != nil {
			return err
		}
	}
	return nil
}
