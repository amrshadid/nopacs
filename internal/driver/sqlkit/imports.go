package sqlkit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The import jobs of a tenant (tenant.Importing), on the tables of the
// migration named import: one implementation both drivers run, as the
// senders' and jobs' are. A path is never in an error this file returns,
// and never in a cursor: a list resumes after a row it finds by number.

// The import lists' names, the first field of their cursors.
const (
	ListImportUnits = "tenant-import-units"
	ListImportFiles = "tenant-import-files"
)

// importJobColumns are an import job's columns as the store reads them.
const importJobColumns = `job_id, receiver, path, map_file, map_key_columns, map_value_column, map_key_parts, map_gives,
	failures_kept, files_listed, files_not_listed, listed, units, units_walked, next_unit, next_file, fail_reason,
	settling_since, found, skipped, handed_in, archived, already_held, quarantined, lost, failed, discarded, processing,
	bytes_handed_in, label_differs, retention_start`

// importJobRow is an import job's row: what a read answers, and what only
// the store's own work reads.
type importJobRow struct {
	jobID       string
	spec        tenant.ImportJob
	filesListed int64
	nextUnit    int64
	nextFile    int64
	failReason  string
	skipped     int64
	quarantined int64
}

// scanImportJob reads one row of importJobColumns.
func scanImportJob(row Row) (importJobRow, error) {
	var r importJobRow
	s := &r.spec
	c := &s.Counts
	var keyColumns, keyParts string
	var settling, retentionStart DBTime
	err := row.Scan(&r.jobID, &s.Receiver, &s.Path, &s.MapFile, &keyColumns, &s.Map.ValueColumn, &keyParts, &s.Map.Gives,
		&s.FailuresKept, &r.filesListed, &s.FilesNotListed, &s.Listed, &c.Units, &c.UnitsWalked, &r.nextUnit, &r.nextFile,
		&r.failReason, &settling, &c.Found, &r.skipped, &c.HandedIn, &c.Archived, &c.AlreadyHeld, &r.quarantined, &c.Lost,
		&c.Failed, &c.Discarded, &c.Processing, &c.BytesHandedIn, &c.LabelDiffers, &retentionStart)
	if err != nil {
		return r, err
	}
	s.SettlingSince, s.RetentionStart = settling.T, retentionStart.T
	if err := FromJSON(keyColumns, &s.Map.KeyColumns); err != nil {
		return r, err
	}
	if err := FromJSON(keyParts, &s.Map.KeyParts); err != nil {
		return r, err
	}
	if len(s.Map.KeyColumns) == 0 {
		s.Map.KeyColumns = nil
	}
	if len(s.Map.KeyParts) == 0 {
		s.Map.KeyParts = nil
	}
	return r, nil
}

// importJobInTx reads an import job's row, with its counts by code and
// reason, and the lock given; false when there is none.
func importJobInTx(ctx context.Context, tx SendingTx, jobID, lock string) (importJobRow, bool, error) {
	var out importJobRow
	found := false
	err := tx.Query(ctx, `SELECT `+importJobColumns+` FROM import_jobs WHERE job_id = ?`+lock, []any{jobID}, func(r Row) error {
		found = true
		var err error
		out, err = scanImportJob(r)
		return err
	})
	if err != nil || !found {
		return out, found, err
	}
	out.spec.Counts.Skipped, out.spec.Counts.Quarantined, err = importTallies(ctx, tx, jobID, "")
	return out, true, err
}

// importTallies reads the counts by skip code and by quarantine reason of
// a job (unitID "") or one of its units; each map is nil when empty.
func importTallies(ctx context.Context, tx SendingTx, jobID, unitID string) (map[tenant.ImportSkipCode]int64, map[tenant.QuarantineReason]int64, error) {
	var skipped map[tenant.ImportSkipCode]int64
	var held map[tenant.QuarantineReason]int64
	err := tx.Query(ctx, `SELECT kind, code, count FROM import_tallies WHERE job_id = ? AND unit_id = ? AND count > 0`,
		[]any{jobID, unitID}, func(r Row) error {
			var kind, code string
			var n int64
			if err := r.Scan(&kind, &code, &n); err != nil {
				return err
			}
			if kind == tallySkip {
				if skipped == nil {
					skipped = map[tenant.ImportSkipCode]int64{}
				}
				skipped[tenant.ImportSkipCode(code)] = n
				return nil
			}
			if held == nil {
				held = map[tenant.QuarantineReason]int64{}
			}
			held[tenant.QuarantineReason(code)] = n
			return nil
		})
	return skipped, held, err
}

// The kinds of import_tallies' rows.
const (
	tallySkip        = "skip"
	tallyQuarantined = "quarantined"
)

// fillImport gives an import job what its import row holds: Import, and its
// counts as a job's (tenant.ImportCounts.JobCounts). A job of another kind
// is left as it is.
func fillImport(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	if j.Kind != tenant.JobImport {
		return nil
	}
	r, found, err := importJobInTx(ctx, tx, j.ID, "")
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "an import job has no import row")
	}
	spec := r.spec
	j.Import = &spec
	j.Counts = spec.Counts.JobCounts(spec.Listed)
	return nil
}

// CreateImportJob implements tenant.Imports.
func (k Sending) CreateImportJob(ctx context.Context, j tenant.NewImportJob, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := j.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	keyColumns, err := JSONText(nonNil(j.Map.KeyColumns))
	if err != nil {
		return tenant.Job{}, err
	}
	keyParts, err := JSONText(nonNil(j.Map.KeyParts))
	if err != nil {
		return tenant.Job{}, err
	}
	var mapCSV any
	if len(j.MapCSV) > 0 {
		mapCSV = j.MapCSV
	}
	var out tenant.Job
	err = k.Host.Write(ctx, "the import job cannot be made", func(tx SendingTx) error {
		// Two jobs of one receiver are compared under the tenant's lock of
		// configuration, so two made at once cannot both pass the overlap
		// rule.
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		has, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = ?`+k.D.Share, j.Receiver)
		if err != nil {
			return err
		}
		if !has {
			return tenant.Precondition("the tenant has no receiver named " + j.Receiver)
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, j.ID)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		overlap := false
		if err := tx.Query(ctx, `SELECT ij.path FROM import_jobs AS ij JOIN jobs AS j ON j.id = ij.job_id
			WHERE ij.receiver = ? AND `+k.D.NotEnded, []any{j.Receiver}, func(r Row) error {
			var p string
			if err := r.Scan(&p); err != nil {
				return err
			}
			overlap = overlap || importpath.Overlap(p, j.Path)
			return nil
		}); err != nil {
			return err
		}
		if overlap {
			return tenant.Precondition("another import job of this receiver, queued or running, reads some of the same files")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
			receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
			retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
			scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
			failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
			VALUES (?, 'import', '', '', '', '', '', ?, NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
			j.ID, j.Receiver, k.D.Bool(false), audit.KeyID, k.D.Time(j.Created), k.D.Time(j.Created), k.D.Time(j.Created),
			k.D.Bool(false), k.D.Bool(true), k.D.Bool(true)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO import_jobs (job_id, receiver, path, map_file, map_csv, map_key_columns,
			map_value_column, map_key_parts, map_gives, failures_kept, files_listed, files_not_listed, listed, units, units_walked,
			next_unit, next_file, fail_reason, settling_since, found, skipped, handed_in, archived, already_held, quarantined, lost,
			failed, discarded, processing, bytes_handed_in, label_differs, retention_start)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, 1, 0, 1, 1, '', NULL, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, ?)`,
			j.ID, j.Receiver, j.Path, j.MapFile, mapCSV, keyColumns, j.Map.ValueColumn, keyParts, j.Map.Gives, j.FailuresKept,
			k.D.Bool(false), k.nullTime(j.RetentionStart)); err != nil {
			return err
		}
		if err := k.insertImportUnit(ctx, tx, j.ID, 0, j.Path, true, j.Created); err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, j.ID, "")
		if err != nil {
			return err
		}
		out = r.job
		e, err := tenant.JobStepEvent("", r.job, j.Created)
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// nonNil returns s, or an empty slice for nil, so a list is kept as "[]".
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// insertImportUnit writes unit number n of a job, queued, with its folder,
// and queues its unit of work, due at now.
func (k Sending) insertImportUnit(ctx context.Context, tx SendingTx, jobID string, n int64, folder string, listing bool, now time.Time) error {
	unitID := tenant.ImportUnitID(n)
	workID := tenant.ImportUnitWorkID(jobID, unitID)
	if _, err := tx.Exec(ctx, `INSERT INTO import_units (job_id, unit_id, work_id, seq, folder_key, listing, state, reason,
		cursor_key, walk_ended, found, skipped, handed_in, archived, already_held, quarantined, lost, failed, discarded,
		processing, bytes_handed_in, label_differs)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', '', NULL, NULL, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)`,
		jobID, unitID, workID, n, tenant.ImportPathKey(folder), k.D.Bool(listing)); err != nil {
		return err
	}
	_, _, err := tx.Enqueue(ctx, tenant.Work{ID: workID, Kind: tenant.ImportWorkKind, Subject: jobID,
		UniqueKey: tenant.ImportUnitKey(jobID, unitID), Due: now, MaxAttempts: tenant.JobUnitAttempts}, now)
	return err
}

// importJobOf reads an import job for a read of it: NotFound when the tenant
// has no job of that id, a precondition failure for another kind.
func importJobOf(ctx context.Context, tx SendingTx, id string) (tenant.Job, error) {
	r, err := jobInTx(ctx, tx, id, "")
	if err != nil {
		return tenant.Job{}, err
	}
	if r.job.Kind != tenant.JobImport {
		return tenant.Job{}, tenant.Precondition("the job is not an import job")
	}
	return r.job, nil
}

// ImportJob implements tenant.Imports.
func (k Sending) ImportJob(ctx context.Context, id string) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Read(ctx, "the import job cannot be read", func(tx SendingTx) error {
		j, err := importJobOf(ctx, tx, id)
		if err != nil {
			return err
		}
		var csv []byte
		if _, err := tx.QueryRow(ctx, `SELECT map_csv FROM import_jobs WHERE job_id = ?`, []any{id}, &csv); err != nil {
			return err
		}
		if len(csv) > 0 {
			j.Import.MapCSV = csv
		}
		out = j
		return nil
	})
	return out, err
}

// importUnitColumns are a unit's columns, with its unit of work's attempt.
const importUnitColumns = `u.job_id, u.unit_id, u.work_id, u.seq, u.folder_key, u.listing, u.state, u.reason, u.cursor_key,
	u.walk_ended, u.found, u.skipped, u.handed_in, u.archived, u.already_held, u.quarantined, u.lost, u.failed, u.discarded,
	u.processing, u.bytes_handed_in, u.label_differs, w.attempt`

// importUnitRow is a unit as its row holds it.
type importUnitRow struct {
	jobID, workID string
	seq           int64
	// cursorSet says the unit has judged an entry. No entry has the empty
	// path, so an empty cursor before the first is told apart by it alone.
	cursorSet   bool
	skipped     int64
	quarantined int64
	unit        tenant.ImportUnit
}

// scanImportUnit reads one row of importUnitColumns.
func scanImportUnit(row Row) (importUnitRow, error) {
	var r importUnitRow
	u := &r.unit
	c := &u.Counts
	var folder, cursor []byte
	var state string
	var walkEnded DBTime
	err := row.Scan(&r.jobID, &u.ID, &r.workID, &r.seq, &folder, &u.Listing, &state, &u.Reason, &cursor, &walkEnded,
		&c.Found, &r.skipped, &c.HandedIn, &c.Archived, &c.AlreadyHeld, &r.quarantined, &c.Lost, &c.Failed, &c.Discarded,
		&c.Processing, &c.BytesHandedIn, &c.LabelDiffers, &u.Attempts)
	u.Folder, u.State, u.WalkEnded = tenant.ImportPathOfKey(folder), tenant.ImportUnitState(state), walkEnded.T
	r.cursorSet = cursor != nil
	u.Cursor = tenant.ImportPathOfKey(cursor)
	c.Units = 1
	if u.State.WalkEnded() {
		c.UnitsWalked = 1
	}
	return r, err
}

// importUnitOf reads the import unit whose unit of work is workID, with
// the lock given on its row; false when the unit of work is no import
// unit.
func (k Sending) importUnitOf(ctx context.Context, tx SendingTx, workID, lock string) (importUnitRow, bool, error) {
	var out importUnitRow
	found := false
	err := tx.Query(ctx, `SELECT `+importUnitColumns+` FROM import_units AS u JOIN work AS w ON w.id = u.work_id
		WHERE u.work_id = ?`+lock, []any{workID}, func(r Row) error {
		found = true
		var err error
		out, err = scanImportUnit(r)
		return err
	})
	return out, found, err
}

// withTallies gives a unit its counts by code and reason.
func withTallies(ctx context.Context, tx SendingTx, r *importUnitRow) error {
	var err error
	r.unit.Counts.Skipped, r.unit.Counts.Quarantined, err = importTallies(ctx, tx, r.jobID, r.unit.ID)
	return err
}

// ListImportUnits implements tenant.Imports.
func (k Sending) ListImportUnits(ctx context.Context, jobID string, filter tenant.ImportUnitFilter, page master.Page) ([]tenant.ImportUnit, string, error) {
	if err := master.ValidateID("job id", jobID); err != nil {
		return nil, "", err
	}
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	states := make([]string, len(filter.States))
	for i, s := range filter.States {
		states[i] = string(s)
	}
	binding := k.Host.Binding(jobID, strings.Join(states, ","))
	after, err := seqAfter(page, ListImportUnits, binding)
	if err != nil {
		return nil, "", err
	}
	where := []string{"u.job_id = ?"}
	args := []any{jobID}
	if after >= 0 {
		where = append(where, "u.folder_key > (SELECT folder_key FROM import_units WHERE job_id = ? AND seq = ?)")
		args = append(args, jobID, after)
	}
	if len(states) > 0 {
		where = append(where, "u.state IN ("+marks(len(states))+")")
		for _, s := range states {
			args = append(args, s)
		}
	}
	query := `SELECT ` + importUnitColumns + ` FROM import_units AS u JOIN work AS w ON w.id = u.work_id WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY u.folder_key LIMIT ?` // #nosec G202 -- every fragment is a constant above; values are parameters.
	args = append(args, page.Limit+1)
	var rows []importUnitRow
	err = k.Host.Read(ctx, "the import job's units cannot be listed", func(tx SendingTx) error {
		rows = nil
		if _, err := importJobOf(ctx, tx, jobID); err != nil {
			return err
		}
		if err := tx.Query(ctx, query, args, func(r Row) error {
			u, err := scanImportUnit(r)
			rows = append(rows, u)
			return err
		}); err != nil {
			return err
		}
		for i := range rows {
			if i == page.Limit {
				break
			}
			if err := withTallies(ctx, tx, &rows[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	rows, last, more := Cut(rows, page.Limit)
	out := make([]tenant.ImportUnit, len(rows))
	for i, r := range rows {
		out[i] = r.unit
	}
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListImportUnits, binding, strconv.FormatInt(last.seq, 10)), nil
}

// seqAfter checks a list's page and returns the number of the row its
// cursor resumes after, or -1 for the first page.
func seqAfter(page master.Page, list, binding string) (int64, error) {
	if page.Cursor == "" {
		return -1, nil
	}
	f, err := DecodeCursor(page.Cursor, list, binding, 1)
	if err != nil {
		return 0, err
	}
	n, err := DecodeCursorInt(f[0])
	if err != nil || n < 0 {
		return 0, errBadCursor()
	}
	return n, nil
}

// marks returns n parameters separated by commas.
func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// importFileColumns are a listed file's columns.
const importFileColumns = `path_key, kind, seq, unit_id, skip_code, quarantine_id, quarantine_reason, reception_id, size, detail, at`

// ListImportFiles implements tenant.Imports.
func (k Sending) ListImportFiles(ctx context.Context, jobID string, filter tenant.ImportFileFilter, page master.Page) ([]tenant.ImportFile, string, error) {
	if err := master.ValidateID("job id", jobID); err != nil {
		return nil, "", err
	}
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	kinds := make([]string, len(filter.Kinds))
	for i, s := range filter.Kinds {
		kinds[i] = string(s)
	}
	binding := k.Host.Binding(jobID, strings.Join(kinds, ","), filter.Unit)
	after, err := seqAfter(page, ListImportFiles, binding)
	if err != nil {
		return nil, "", err
	}
	where := []string{"job_id = ?", "hidden = ?"}
	args := []any{jobID, k.D.Bool(false)}
	if filter.Unit != "" {
		where, args = append(where, "unit_id = ?"), append(args, filter.Unit)
	}
	if after >= 0 {
		where = append(where, "(path_key, kind) > (SELECT path_key, kind FROM import_files WHERE job_id = ? AND seq = ?)")
		args = append(args, jobID, after)
	}
	if len(kinds) > 0 {
		where = append(where, "kind IN ("+marks(len(kinds))+")")
		for _, s := range kinds {
			args = append(args, s)
		}
	}
	query := `SELECT ` + importFileColumns + ` FROM import_files WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY path_key, kind LIMIT ?` // #nosec G202 -- every fragment is a constant above; values are parameters.
	args = append(args, page.Limit+1)
	type fileRow struct {
		seq  int64
		file tenant.ImportFile
	}
	var rows []fileRow
	err = k.Host.Read(ctx, "the import job's files cannot be listed", func(tx SendingTx) error {
		rows = nil
		if _, err := importJobOf(ctx, tx, jobID); err != nil {
			return err
		}
		return tx.Query(ctx, query, args, func(r Row) error {
			var f fileRow
			var key []byte
			var kind, code, reason string
			var at DBTime
			err := r.Scan(&key, &kind, &f.seq, &f.file.Unit, &code, &f.file.QuarantinedFile, &reason, &f.file.Reception,
				&f.file.Size, &f.file.Detail, &at)
			f.file.Path, f.file.Kind, f.file.SkipCode = tenant.ImportPathOfKey(key), tenant.ImportFileKind(kind), tenant.ImportSkipCode(code)
			f.file.QuarantineReason, f.file.Time = tenant.QuarantineReason(reason), at.T
			rows = append(rows, f)
			return err
		})
	})
	if err != nil {
		return nil, "", err
	}
	rows, last, more := Cut(rows, page.Limit)
	out := make([]tenant.ImportFile, len(rows))
	for i, r := range rows {
		out[i] = r.file
	}
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListImportFiles, binding, strconv.FormatInt(last.seq, 10)), nil
}
