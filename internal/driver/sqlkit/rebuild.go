package sqlkit

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's rebuilds (tenant.Rebuilding) on a SQL database, one
// implementation both drivers answer with. The shadow catalog is reached
// through the driver's ShadowHost: tables of the catalog's shape whose
// names the statements here write with ShadowHost.Prefix, so the same
// statement writes "rebuild_shadow.scans" on SQLite (a file attached to the
// tenant's connection) and "scratch.scans" on Postgres (a schema of the
// tenant's database). The shadow also holds the build's own two tables,
// rebuild_state (one row: the job, whether its tables are made, the
// cursor, the counts) and rebuild_tallies, written in the transaction of
// every scan written, so they are atomic with the rows they count on
// either database: on SQLite a transaction over two attached files in
// write-ahead mode is atomic per file, not across them
// (sqlite.org/lang_attach.html), so nothing of the build's progress lives
// in the tenant's own file.
//
// rebuild_state is also the shadow's mark of whose it is. Nothing is made
// in, read from, or removed from a shadow unless its rebuild_state names
// the job: on Postgres the scratch schema is the operator's and may hold
// another job's shadow, or anything else, so a table is the job's only
// because the job's own transaction made it beside its mark.

// ShadowHost is what a driver does for the shadow that statements cannot:
// make it, reach it, read it and remove it.
type ShadowHost interface {
	// Prefix returns how a statement names the shadow's tables: a schema
	// and a dot.
	Prefix(scratch string) string
	// Claim refuses, in the transaction of CreateJob, a scratch schema the
	// job may not build in (the contract's precondition), and marks the
	// schema as the job's: rebuild_state, naming the job, its tables not
	// yet made. A driver whose shadow is a file of its own does nothing.
	Claim(ctx context.Context, tx SendingTx, scratch, job string) error
	// Open makes the shadow reachable from the tenant's write
	// transactions, making an empty one when there is none.
	Open(ctx context.Context, scratch string) error
	// Tables returns the names of the tables the shadow holds.
	Tables(ctx context.Context, scratch string) ([]string, error)
	// Create makes the shadow of the job anew, empty: the catalog's tables
	// in the shape of the catalog in use, and the build's own tables, with
	// catalog_totals' one row of zeros and rebuild_state's row naming the
	// job, its tables made. fenced runs first, in the transaction that
	// makes them where the driver can (Postgres) and just before it where
	// it cannot (SQLite, whose shadow is another file), and its refusal
	// makes nothing. A refusal no attempt gets past holds
	// tenant.ErrRebuildRefused.
	Create(ctx context.Context, scratch, job string, fenced func(SendingTx) error) error
	// Drop removes the shadow of the job: its tables, or its file. A
	// shadow not there, or whose rebuild_state names another job, is left
	// as it is: nothing to do.
	Drop(ctx context.Context, scratch, job string) error
	// Read runs fn in one read transaction in which the shadow is
	// reachable beside the catalog in use.
	Read(ctx context.Context, scratch, message string, fn func(SendingTx) error) error
	// Columns returns the columns of a table of the catalog in use, in
	// their order.
	Columns(ctx context.Context, tx SendingTx, table string) ([]string, error)
}

// shadowTables are the build's own tables a shadow holds.
var shadowTables = []string{"rebuild_state", "rebuild_tallies"}

// ShadowTables returns every table a shadow holds: the catalog's and the
// build's own, so a driver removes exactly these and nothing else.
func ShadowTables() []string { return append(tenant.CatalogTables(), shadowTables...) }

// Rebuild is tenant.Rebuilding on a SQL database: the tenant's jobs'
// transactions, with Sending.Shadow reaching the shadow.
type Rebuild struct {
	// Sending runs the transactions of the tenant's jobs.
	Sending
}

// rebuildRow is a rebuild job as the store reads it in a unit's
// transaction.
type rebuildRow struct {
	job     jobRow
	rebuild tenant.RebuildJob
}

// rebuildOf reads the rebuild job whose unit holds the fence, refusing a
// unit of any other job, after checking the fence.
func (k Rebuild) rebuildOf(ctx context.Context, tx SendingTx, fence tenant.WorkFence, lock string) (rebuildRow, error) {
	if err := tx.CheckFence(ctx, fence); err != nil {
		return rebuildRow{}, err
	}
	id, err := listingJobOf(ctx, tx, fence.ID)
	if err != nil {
		return rebuildRow{}, err
	}
	if id == "" {
		return rebuildRow{}, errNotJobUnit()
	}
	r, err := jobInTx(ctx, tx, id, lock)
	if err != nil {
		return rebuildRow{}, err
	}
	if r.job.Kind != tenant.JobRebuild || r.job.Rebuild == nil {
		return rebuildRow{}, tenant.Precondition("the unit of work is not a rebuild job's")
	}
	return rebuildRow{job: r, rebuild: *r.job.Rebuild}, nil
}

// fillRebuild gives a rebuild job what its rebuild row holds.
func fillRebuild(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	var mode string
	r := &tenant.RebuildJob{}
	found, err := tx.QueryRow(ctx, `SELECT mode, scratch, activated FROM rebuild_jobs WHERE job_id = ?`, []any{j.ID}, &mode, &r.Scratch,
		&r.Activated)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "a rebuild job has no rebuild row")
	}
	r.Mode = tenant.RebuildMode(mode)
	j.Rebuild = r
	return nil
}

// checkRebuildJob refuses a rebuild CreateJob may not make: its scratch
// schema (required where the driver builds in one, refused where it does
// not), and another rebuild queued or running; and marks the scratch
// schema as the job's. It serializes with the tenant's changes of
// configuration first, as a scrub's check does: the condition that no
// other rebuild is queued or running is on rows that do not exist yet, so
// no row lock holds it, and two rebuilds made at once on several writers
// would otherwise both pass.
func (k Sending) checkRebuildJob(ctx context.Context, tx SendingTx, j tenant.NewJob) error {
	if k.Shadow == nil {
		return fault.New(fault.Internal, "the store has no shadow host for rebuilds")
	}
	scratch := j.Rebuild.Scratch
	switch {
	case k.D.ScratchRequired && scratch == "":
		return fault.New(fault.Invalid, "a rebuild on a Postgres tenant names the scratch schema its shadow is built in")
	case !k.D.ScratchRequired && scratch != "":
		return fault.New(fault.Invalid, "a rebuild on a SQLite tenant names no scratch schema: its shadow is a file beside the tenant's database")
	}
	if err := tx.LockConfig(ctx); err != nil {
		return err
	}
	busy, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE kind = 'rebuild' AND `+k.D.NotEnded+k.D.Lock)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("another rebuild of the tenant is queued or running")
	}
	if scratch != "" {
		return k.Shadow.Claim(ctx, tx, scratch, j.ID)
	}
	return nil
}

// cancelRebuildInTx ends a rebuild job as canceled, as cancelInTx does a
// send job, but leaves its unit queued or leased: the unit's worker meets
// the job ended, drops its shadow, and completes the unit, so a cancel
// never leaves a shadow behind.
func (k Sending) cancelRebuildInTx(ctx context.Context, tx SendingTx, id, reason string, now time.Time) (tenant.Job, *tenant.EventRecord, error) {
	r, err := jobInTx(ctx, tx, id, k.D.Lock)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	if r.job.State == tenant.JobCanceled {
		return r.job, nil, nil
	}
	if r.job.State.Ended() {
		return tenant.Job{}, nil, tenant.Precondition("the job has ended " + string(r.job.State) + "; only a job queued or running is canceled")
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'canceled', ended = ?, reason = ?, state_at = ?, ended_at = ? WHERE id = ?`,
		k.D.Bool(true), reason, k.D.Time(now), k.D.Time(now), id); err != nil {
		return tenant.Job{}, nil, err
	}
	after, err := jobInTx(ctx, tx, id, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	e, err := tenant.JobStepEvent(r.job.State, after.job, now)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	return after.job, &e, nil
}

// StartRebuildUnit implements tenant.Rebuilding.
func (k Rebuild) StartRebuildUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the rebuild's unit cannot be started", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, k.D.Lock)
		if err != nil {
			return err
		}
		out = r.job.job
		if r.job.job.State != tenant.JobQueued {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'running', started = ?, state_at = ? WHERE id = ? AND state = 'queued'`,
			k.D.Time(now), k.D.Time(now), out.ID); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, out.ID, "")
		if err != nil {
			return err
		}
		out = after.job
		e, err := tenant.JobStepEvent(tenant.JobQueued, after.job, now)
		if err != nil {
			return err
		}
		return tx.WriteEvents(ctx, e)
	})
	return out, err
}

// unitJob reads the job of the unit, fenced, refusing one that has ended.
func (k Rebuild) unitJob(ctx context.Context, fence tenant.WorkFence) (rebuildRow, error) {
	var out rebuildRow
	err := k.Host.Write(ctx, "the rebuild's job cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.rebuildOf(ctx, tx, fence, "")
		return err
	})
	if err == nil && out.job.job.State.Ended() {
		err = tenant.Precondition("the rebuild job has ended " + string(out.job.job.State))
	}
	return out, err
}

// PrepareShadow implements tenant.Rebuilding.
func (k Rebuild) PrepareShadow(ctx context.Context, fence tenant.WorkFence) (tenant.RebuildProgress, error) {
	if err := fence.Validate(); err != nil {
		return tenant.RebuildProgress{}, err
	}
	r, err := k.unitJob(ctx, fence)
	if err != nil {
		return tenant.RebuildProgress{}, err
	}
	scratch, job := r.rebuild.Scratch, r.job.job.ID
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.RebuildProgress{}, err
	}
	holder, made, err := k.holder(ctx, scratch)
	if err != nil {
		return tenant.RebuildProgress{}, err
	}
	if holder == job && made {
		return k.progress(ctx, fence, scratch)
	}
	// The shadow is made, or made again, in a transaction that checks the
	// lease where the driver can: a worker whose lease was taken over
	// never makes a shadow under the one that took it.
	if err := k.Shadow.Create(ctx, scratch, job, func(tx SendingTx) error {
		_, err := k.rebuildOf(ctx, tx, fence, "")
		return err
	}); err != nil {
		return tenant.RebuildProgress{}, err
	}
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.RebuildProgress{}, err
	}
	return k.progress(ctx, fence, scratch)
}

// holder reads whose the shadow is, and whether its tables are made: ""
// and false when it holds no mark.
func (k Rebuild) holder(ctx context.Context, scratch string) (string, bool, error) {
	tables, err := k.Shadow.Tables(ctx, scratch)
	if err != nil || !slices.Contains(tables, "rebuild_state") {
		return "", false, err
	}
	var job string
	var made bool
	err = k.Host.Write(ctx, "the shadow cannot be read", func(tx SendingTx) error {
		job, made = "", false
		_, err := tx.QueryRow(ctx, `SELECT job_id, made FROM `+k.Shadow.Prefix(scratch)+`rebuild_state`, nil, &job, &made)
		return err
	})
	return job, made, err
}

// progress reads how far the shadow's build got, fenced.
func (k Rebuild) progress(ctx context.Context, fence tenant.WorkFence, scratch string) (tenant.RebuildProgress, error) {
	var out tenant.RebuildProgress
	err := k.Host.Write(ctx, "the shadow cannot be read", func(tx SendingTx) error {
		if _, err := k.rebuildOf(ctx, tx, fence, ""); err != nil {
			return err
		}
		var err error
		out, err = k.stateIn(ctx, tx, scratch)
		return err
	})
	return out, err
}

// stateIn reads the shadow's progress in the transaction, refusing a
// shadow whose tables are not made.
func (k Rebuild) stateIn(ctx context.Context, tx SendingTx, scratch string) (tenant.RebuildProgress, error) {
	p := k.Shadow.Prefix(scratch)
	var out tenant.RebuildProgress
	var made bool
	c := &out.Cursor
	found, err := tx.QueryRow(ctx, `SELECT job_id, made, storage, after_key, partial_key, partial_uid, complete, scans, left_out,
		left_out_bytes, instances, bytes FROM `+p+`rebuild_state`, nil, &out.Job, &made, &c.Storage, &c.After, &c.Partial, &c.PartialUID,
		&out.Complete, &out.Scans, &out.LeftOut, &out.LeftOutBytes, &out.Instances, &out.Bytes)
	if err != nil {
		return out, err
	}
	if !found || !made {
		return out, tenant.Precondition("the shadow holds no build")
	}
	out.Tallies = map[string]int64{}
	err = tx.Query(ctx, `SELECT tally, count FROM `+p+`rebuild_tallies`, nil, func(r Row) error {
		var key string
		var n int64
		err := r.Scan(&key, &n)
		out.Tallies[key] = n
		return err
	})
	return out, err
}

// buildIn reads the job of the unit and its shadow's progress, refusing a
// job that has ended, a shadow of another job, and a build complete.
func (k Rebuild) buildIn(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (rebuildRow, tenant.RebuildProgress, error) {
	r, err := k.rebuildOf(ctx, tx, fence, "")
	if err != nil {
		return r, tenant.RebuildProgress{}, err
	}
	if r.job.job.State.Ended() {
		return r, tenant.RebuildProgress{}, tenant.Precondition("the rebuild job has ended " + string(r.job.job.State))
	}
	st, err := k.stateIn(ctx, tx, r.rebuild.Scratch)
	if err != nil {
		return r, st, err
	}
	if st.Job != r.job.job.ID {
		return r, st, tenant.Precondition("the shadow is another rebuild's")
	}
	return r, st, nil
}

// FinishRebuildStorage implements tenant.Rebuilding.
func (k Rebuild) FinishRebuildStorage(ctx context.Context, fence tenant.WorkFence, storage, next string,
	tallies map[string]int64) (tenant.RebuildProgress, error) {
	if err := fence.Validate(); err != nil {
		return tenant.RebuildProgress{}, err
	}
	if len(tallies) > 0 {
		if err := tenant.ValidateScrubTallies(tallies); err != nil {
			return tenant.RebuildProgress{}, err
		}
	}
	for _, name := range []string{storage, next} {
		if name != "" {
			if err := tenant.ValidateName("storage", name); err != nil {
				return tenant.RebuildProgress{}, err
			}
		}
	}
	if next != "" && next <= storage {
		return tenant.RebuildProgress{}, fault.New(fault.Invalid, "a rebuild reads the storages in order of name")
	}
	r, err := k.unitJob(ctx, fence)
	if err != nil {
		return tenant.RebuildProgress{}, err
	}
	scratch := r.rebuild.Scratch
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.RebuildProgress{}, err
	}
	var out tenant.RebuildProgress
	err = k.Host.Write(ctx, "the shadow cannot be written", func(tx SendingTx) error {
		_, st, err := k.buildIn(ctx, tx, fence)
		if err != nil {
			return err
		}
		if st.Complete {
			out = st
			return nil
		}
		if st.Cursor.Storage != storage || st.Cursor.Partial != "" {
			return tenant.RebuildMoved()
		}
		p := k.Shadow.Prefix(scratch)
		if err := addShadowTallies(ctx, tx, p, tallies); err != nil {
			return err
		}
		if next != "" {
			if _, err := tx.Exec(ctx, `UPDATE `+p+`rebuild_state SET storage = ?, after_key = '', partial_key = '', partial_uid = ''`,
				next); err != nil {
				return err
			}
		} else if err := finishShadow(ctx, tx, p, k.D); err != nil {
			return err
		}
		out, err = k.stateIn(ctx, tx, scratch)
		return err
	})
	return out, err
}

// finishShadow gives every row of the shadow its derived values, computed
// from the rows it holds as a reception's commits keep them running in the
// catalog in use (the scans', sessions' and subjects' counts and times, the
// sessions' modalities, the receptions' scans, the totals), and marks the
// build complete.
func finishShadow(ctx context.Context, tx SendingTx, p string, d SendingDialect) error {
	for _, stmt := range []string{
		`UPDATE ` + p + `scans SET instances = (SELECT count(*) FROM ` + p + `instances AS i WHERE i.scan_id = scans.id),
			bytes = (SELECT coalesce(sum(i.size), 0) FROM ` + p + `instances AS i WHERE i.scan_id = scans.id)`,
		`DELETE FROM ` + p + `scans WHERE instances = 0`,
		`DELETE FROM ` + p + `sessions WHERE NOT EXISTS (SELECT 1 FROM ` + p + `scans AS c WHERE c.session_id = sessions.id)`,
		`DELETE FROM ` + p + `subjects WHERE NOT EXISTS (SELECT 1 FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id)`,
		`UPDATE ` + p + `sessions SET scan_count = (SELECT count(*) FROM ` + p + `scans AS c WHERE c.session_id = sessions.id),
			instance_count = (SELECT coalesce(sum(c.instances), 0) FROM ` + p + `scans AS c WHERE c.session_id = sessions.id),
			bytes = (SELECT coalesce(sum(c.bytes), 0) FROM ` + p + `scans AS c WHERE c.session_id = sessions.id),
			created = (SELECT min(c.created) FROM ` + p + `scans AS c WHERE c.session_id = sessions.id),
			updated = (SELECT max(c.updated) FROM ` + p + `scans AS c WHERE c.session_id = sessions.id)`,
		`UPDATE ` + p + `subjects SET session_count = (SELECT count(*) FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id),
			instance_count = (SELECT coalesce(sum(e.instance_count), 0) FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id),
			bytes = (SELECT coalesce(sum(e.bytes), 0) FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id),
			created = (SELECT min(e.created) FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id),
			updated = (SELECT max(e.updated) FROM ` + p + `sessions AS e WHERE e.subject_id = subjects.id)`,
		`DELETE FROM ` + p + `session_modalities`,
		`INSERT INTO ` + p + `session_modalities (session_id, modality, created) SELECT DISTINCT c.session_id, c.modality, e.created
			FROM ` + p + `scans AS c JOIN ` + p + `sessions AS e ON e.id = c.session_id WHERE c.modality <> ''`,
		`DELETE FROM ` + p + `reception_scans`,
		`INSERT INTO ` + p + `reception_scans (reception_id, scan_id, instances, bytes) SELECT reception_id, scan_id, count(*), sum(size)
			FROM ` + p + `instances GROUP BY reception_id, scan_id`,
		`DELETE FROM ` + p + `storage_totals`,
		`INSERT INTO ` + p + `storage_totals (storage, bytes) SELECT c.storage, sum(i.size) FROM ` + p + `instances AS i
			JOIN ` + p + `scans AS c ON c.id = i.scan_id GROUP BY c.storage HAVING sum(i.size) > 0`,
		`UPDATE ` + p + `catalog_totals SET subjects = (SELECT count(*) FROM ` + p + `subjects),
			sessions = (SELECT count(*) FROM ` + p + `sessions), scans = (SELECT count(*) FROM ` + p + `scans),
			instances = (SELECT count(*) FROM ` + p + `instances)`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE `+p+`rebuild_state SET complete = ?, storage = '', after_key = '', partial_key = '', partial_uid = '',
		instances = (SELECT count(*) FROM `+p+`instances), bytes = (SELECT coalesce(sum(size), 0) FROM `+p+`instances)`, d.Bool(true))
	return err
}

// EventHistory implements tenant.Rebuilding.
func (k Rebuild) EventHistory(ctx context.Context) (string, error) {
	var out string
	err := k.Host.Read(ctx, "the event feed's history cannot be read", func(tx SendingTx) error {
		found, err := tx.QueryRow(ctx, `SELECT id FROM event_history`, nil, &out)
		if err == nil && !found {
			err = fault.New(fault.Internal, "the tenant's database has no event history")
		}
		return err
	})
	return out, err
}

// RebuildReplacing implements tenant.Rebuilding.
func (k Rebuild) RebuildReplacing(ctx context.Context) (bool, error) {
	var out bool
	err := k.Host.Read(ctx, "the tenant's rebuilds cannot be read", func(tx SendingTx) error {
		var err error
		// ended is a column of jobs alone, so the condition names it bare,
		// as the partial index of the jobs in progress is written.
		out, err = exists(ctx, tx, `SELECT 1 FROM rebuild_jobs AS r JOIN jobs ON jobs.id = r.job_id WHERE r.mode = 'replace' AND `+
			k.D.NotEnded)
		return err
	})
	return out, err
}

// FailRebuild implements tenant.Rebuilding.
func (k Rebuild) FailRebuild(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := tenant.ValidateWorkReason(reason); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the rebuild job cannot be ended", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, k.D.Lock)
		if err != nil {
			return err
		}
		if r.job.job.State.Ended() {
			out = r.job.job
			return nil
		}
		j, events, err := k.endJob(ctx, tx, r.job.job, tenant.JobFailed, reason, now)
		if err != nil {
			return err
		}
		out = j
		return tx.WriteEvents(ctx, events...)
	})
	return out, err
}

// RebuildDifferences implements tenant.Rebuilding.
func (k Rebuild) RebuildDifferences(ctx context.Context, jobID string) ([]tenant.CatalogDifference, error) {
	if err := master.ValidateID("job id", jobID); err != nil {
		return nil, err
	}
	var out []tenant.CatalogDifference
	err := k.Host.Read(ctx, "the rebuild's differences cannot be read", func(tx SendingTx) error {
		out = nil
		r, err := jobInTx(ctx, tx, jobID, "")
		if err != nil {
			return err
		}
		if r.job.Kind != tenant.JobRebuild {
			return tenant.Precondition("the job is not a rebuild")
		}
		return tx.Query(ctx, `SELECT table_name, kind, row_key FROM rebuild_differences WHERE job_id = ? ORDER BY seq LIMIT ?`,
			[]any{jobID, tenant.MaxCatalogDifferences}, func(row Row) error {
				var d tenant.CatalogDifference
				err := row.Scan(&d.Table, &d.Kind, &d.Key)
				out = append(out, d)
				return err
			})
	})
	return out, err
}

// DropShadow implements tenant.Rebuilding.
func (k Rebuild) DropShadow(ctx context.Context, jobID string) error {
	if err := master.ValidateID("job id", jobID); err != nil {
		return err
	}
	var scratch string
	err := k.Host.Read(ctx, "the rebuild job cannot be read", func(tx SendingTx) error {
		r, err := jobInTx(ctx, tx, jobID, "")
		if err != nil {
			return err
		}
		if r.job.Kind != tenant.JobRebuild || r.job.Rebuild == nil {
			return tenant.Precondition("the job is not a rebuild")
		}
		if !r.job.State.Ended() {
			return tenant.Precondition("a rebuild's shadow is dropped once its job has ended")
		}
		scratch = r.job.Rebuild.Scratch
		return nil
	})
	if err != nil {
		return err
	}
	// Drop removes only what names this job, so a shadow another job made
	// since (a SQLite shadow file is one per tenant) is left to that job.
	if err := k.Shadow.Drop(ctx, scratch, jobID); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the rebuild's shadow cannot be marked dropped", func(tx SendingTx) error {
		_, err := tx.Exec(ctx, `UPDATE rebuild_jobs SET dropped = ? WHERE job_id = ?`, k.D.Bool(true), jobID)
		return err
	})
}

// ShadowsLeft implements tenant.Rebuilding.
func (k Rebuild) ShadowsLeft(ctx context.Context) ([]string, error) {
	var out []string
	err := k.Host.Read(ctx, "the tenant's rebuilds cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT r.job_id FROM rebuild_jobs AS r JOIN jobs ON jobs.id = r.job_id WHERE r.dropped = ? AND jobs.ended = ?
			ORDER BY r.job_id LIMIT ?`, []any{k.D.Bool(false), k.D.Bool(true), tenant.MaxShadowsLeft}, func(r Row) error {
			var id string
			err := r.Scan(&id)
			out = append(out, id)
			return err
		})
	})
	return out, err
}
