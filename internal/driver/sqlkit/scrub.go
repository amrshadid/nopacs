package sqlkit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's scrubs (tenant.Scrubbing) on the tables of the scrub's
// migration: what a scrub job reads (scrub_jobs), the findings
// (scrub_findings) and the marks of damaged instances (instance_damage).
// A finding's key holds a SOP Instance UID as received, so it is kept in
// these rows only: no error here holds one, and a list's cursor holds a
// finding's id.

// ListScrubFindings is the findings' list's name, the first field of its
// cursor.
const ListScrubFindings = "tenant-scrub-findings"

// findingColumns are a finding's columns as the store reads them.
const findingColumns = `id, kind, storage, object_key, subject_label, session_label, scan_label, sop_instance_uid, revision,
	expected_size, found_size, expected_sha256, found_sha256, job_id, found_at, last_job, last_seen, cleared_at, clear_job, replica_of`

// The statements of scrubs that read tables which grow, each a range or a
// lookup of the index named beside it.
const (
	// openFindingSQL reads the finding that holds of an object and kind:
	// scrub_findings_open.
	openFindingSQL = `SELECT ` + findingColumns + ` FROM scrub_findings WHERE storage = ? AND object_key = ? AND kind = ?
		AND cleared_at IS NULL`
	// findingByIDSQL reads a finding by its id: the key.
	findingByIDSQL = `SELECT ` + findingColumns + ` FROM scrub_findings WHERE id = ?`
	// scanFindingsSQL reads a page of the findings that hold of a scan's
	// files, after a key and kind: a range of scrub_findings_of_scan.
	scanFindingsSQL = `SELECT ` + findingColumns + ` FROM scrub_findings WHERE storage = ? AND subject_label = ?
		AND session_label = ? AND scan_label = ? AND cleared_at IS NULL AND kind NOT IN ('missing', 'damaged')
		AND (object_key, kind) > (?, ?) ORDER BY object_key, kind LIMIT ?`
	// markingFindingSQL reads the first other finding that holds of an
	// instance and marks it: scrub_findings_marking. A finding of a
	// replica's copy never marks the instance.
	markingFindingSQL = `SELECT id FROM scrub_findings WHERE sop_instance_uid = ? AND cleared_at IS NULL
		AND kind IN ('missing', 'damaged') AND replica_of = '' ORDER BY id LIMIT 1`
	// lastScrubSQL reads the scrub of a whole storage made last (a scrub of
	// one scan names its scan): scrub_jobs_by_storage, whose rows are few
	// (one per scrub), each joined to its job by the key.
	lastScrubSQL = `SELECT s.job_id FROM scrub_jobs AS s JOIN jobs AS j ON j.id = s.job_id WHERE s.storage = ?
		AND j.scan_label = '' ORDER BY j.created DESC, j.id DESC LIMIT 1`
	// openFindingCountsSQL counts the findings that hold by kind: the
	// partial index scrub_findings_open, which holds those alone.
	openFindingCountsSQL = `SELECT kind, count(*) FROM scrub_findings WHERE cleared_at IS NULL GROUP BY kind`
	// scrubJobsInProgressSQL reads the storages of the scrub jobs not
	// ended, before its condition on ended: few rows of jobs, then
	// scrub_jobs' key.
	scrubJobsInProgressSQL = `SELECT s.storage, j.scan_label FROM jobs AS j JOIN scrub_jobs AS s ON s.job_id = j.id
		WHERE j.kind = 'scrub' AND `
	// damageSQL reads the marks of instances: instance_damage's key.
	damageSQL = `SELECT sop_instance_uid, finding_id FROM instance_damage WHERE sop_instance_uid IN (`
)

// checkScrubJob refuses a scrub of a storage the tenant does not have, and
// one while another scrub of the same storage, or of every storage, is
// queued or running. A scrub of one scan queued or running never refuses a
// whole scrub: the two run together, each paced by the storage's rate, so
// the scrub a read asked for cannot hold back the storage's scheduled one.
// A scrub of one scan is still refused while any scrub of its storage
// runs, which reads the scan's requests as well. It serializes with the
// tenant's changes of configuration, so two scrubs made at once on several
// writers cannot both pass.
func (k Sending) checkScrubJob(ctx context.Context, tx SendingTx, storage string, oneScan bool) error {
	if err := tx.LockConfig(ctx); err != nil {
		return err
	}
	if storage != "" {
		has, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = ?`, storage)
		if err != nil {
			return err
		}
		if !has {
			return tenant.Precondition("the tenant has no storage named " + storage)
		}
	}
	busy := false
	err := tx.Query(ctx, scrubJobsInProgressSQL+"j.ended = ?", []any{k.D.Bool(false)}, func(r Row) error {
		var s, scan string
		if err := r.Scan(&s, &scan); err != nil {
			return err
		}
		if scan != "" && !oneScan {
			return nil
		}
		if s == "" || storage == "" || s == storage {
			busy = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("another scrub of the same storage is queued or running")
	}
	return nil
}

// fillJob gives a job what the row of its kind holds: an import job's
// (fillImport), a scrub job's (fillScrub); a job of another kind is left as
// it is.
func fillJob(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	switch j.Kind {
	case tenant.JobScrub:
		return fillScrub(ctx, tx, j)
	case tenant.JobRebuild:
		return fillRebuild(ctx, tx, j)
	case tenant.JobRepair:
		return fillRepair(ctx, tx, j)
	}
	return fillImport(ctx, tx, j)
}

// fillScrub gives a scrub job what its scrub row holds: its storage and
// whether the server made it.
func fillScrub(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	found, err := tx.QueryRow(ctx, `SELECT storage, scheduled FROM scrub_jobs WHERE job_id = ?`, []any{j.ID}, &j.Storage, &j.Scheduled)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "a scrub job has no scrub row")
	}
	return nil
}

// scanFinding reads one row of findingColumns.
func scanFinding(row Row) (tenant.ScrubFinding, error) {
	var f tenant.ScrubFinding
	var kind string
	var expected, found []byte
	var foundAt, lastSeen, cleared DBTime
	err := row.Scan(&f.ID, &kind, &f.Storage, &f.Key, &f.Path.Subject, &f.Path.Session, &f.Path.Scan, &f.SOPInstanceUID,
		&f.Revision, &f.ExpectedSize, &f.FoundSize, &expected, &found, &f.Job, &foundAt, &f.LastJob, &lastSeen, &cleared,
		&f.ClearJob, &f.ReplicaOf)
	f.Kind = tenant.ScrubFindingKind(kind)
	f.ExpectedSHA256, f.FoundSHA256 = ToSHA256(expected), ToSHA256(found)
	f.Found, f.LastSeen, f.Cleared = foundAt.T, lastSeen.T, cleared.T
	return f, err
}

// findingIn reads one finding with the statement and its arguments, and
// reports whether there was one.
func findingIn(ctx context.Context, tx SendingTx, query string, args ...any) (tenant.ScrubFinding, bool, error) {
	var out tenant.ScrubFinding
	found := false
	err := tx.Query(ctx, query, args, func(r Row) error {
		var err error
		out, err = scanFinding(r)
		found = true
		return err
	})
	return out, found, err
}

// scrubJobOfUnit returns the scrub job whose unit holds the fence, locked,
// after checking the fence: a scan's unit, or the job's listing unit. A
// unit of another job, or of none, is a precondition failure.
func (k Sending) scrubJobOfUnit(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (jobRow, error) {
	if err := tx.CheckFence(ctx, fence); err != nil {
		return jobRow{}, err
	}
	scan, isScan, err := jobScanOf(ctx, tx, fence.ID, "")
	if err != nil {
		return jobRow{}, err
	}
	jobID := scan.jobID
	if !isScan {
		if jobID, err = listingJobOf(ctx, tx, fence.ID); err != nil {
			return jobRow{}, err
		}
		if jobID == "" {
			return jobRow{}, errNotJobUnit()
		}
	}
	r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
	if err != nil {
		return jobRow{}, err
	}
	if r.job.Kind != tenant.JobScrub {
		return jobRow{}, tenant.Precondition("the unit of work is not one of a scrub job's")
	}
	return r, nil
}

// nullSHA returns a checksum as its column keeps it: NULL for none.
func nullSHA(s tenant.SHA256) any {
	if s.IsZero() {
		return nil
	}
	return s[:]
}

// nullInt returns a size as its column keeps it: NULL for none.
func nullInt(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

// RecordScrubFinding implements tenant.Scrubbing.
func (k Sending) RecordScrubFinding(ctx context.Context, rec tenant.ScrubFindingRecord) (tenant.ScrubFinding, bool, error) {
	if err := rec.Validate(); err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	var out tenant.ScrubFinding
	var made bool
	err := k.Host.Write(ctx, "the scrub's finding cannot be recorded", func(tx SendingTx) error {
		r, err := k.scrubJobOfUnit(ctx, tx, rec.Fence)
		if err != nil {
			return err
		}
		out, made, err = k.recordFindingInTx(ctx, tx, r, rec)
		return err
	})
	if err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	return out, made, nil
}

// recordFindingInTx records the finding of rec by the scrub job of r, whose
// unit's fence was checked, in the caller's transaction, as
// RecordScrubFinding says, and returns it as it is after and whether this
// call made it. A finding of a storage that is a replica names its primary
// (replica_of) and never marks its instance: the instance's reads keep
// going to the primary.
func (k Sending) recordFindingInTx(ctx context.Context, tx SendingTx, r jobRow, rec tenant.ScrubFindingRecord) (tenant.ScrubFinding, bool, error) {
	f := rec.Finding
	job := r.job.ID
	tallies := map[string]int64{}
	out, held, err := findingIn(ctx, tx, openFindingSQL+k.D.Lock, f.Storage, f.Key, string(f.Kind))
	if err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	made := false
	if held {
		if out.LastJob == job {
			return out, false, nil // seen by this job already: a write sent again
		}
		if _, err := tx.Exec(ctx, `UPDATE scrub_findings SET last_job = ?, last_seen = ? WHERE id = ?`,
			job, k.D.Time(rec.Now), out.ID); err != nil {
			return tenant.ScrubFinding{}, false, err
		}
		out.LastJob, out.LastSeen = job, rec.Now
		tallies[tenant.ScrubTally(f.Kind)] = 1
	} else {
		var replicaOf string
		if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{f.Storage}, &replicaOf); err != nil {
			return tenant.ScrubFinding{}, false, err
		}
		insert := func(id string) (int64, error) {
			return tx.Exec(ctx, `INSERT INTO scrub_findings (`+findingColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?) ON CONFLICT (id) DO NOTHING`,
				id, string(f.Kind), f.Storage, f.Key, f.Path.Subject, f.Path.Session, f.Path.Scan, f.SOPInstanceUID,
				f.Revision, nullInt(f.ExpectedSize), nullInt(f.FoundSize), nullSHA(f.ExpectedSHA256), nullSHA(f.FoundSHA256),
				job, k.D.Time(rec.Now), job, k.D.Time(rec.Now), replicaOf)
		}
		id := f.ID
		n, err := insert(id)
		if err == nil && n == 0 {
			// The id names a finding no longer holding (the open one
			// was looked for above): this job cleared it in a later
			// attempt and finds the object wrong once more.
			id = tenant.ScrubFindingIDAgain(f.ID, rec.Fence)
			n, err = insert(id)
		}
		if err != nil {
			return tenant.ScrubFinding{}, false, err
		}
		if n == 0 {
			return tenant.ScrubFinding{}, false, fault.New(fault.Conflict, "a finding with this id exists and no longer holds")
		}
		if out, _, err = findingIn(ctx, tx, findingByIDSQL, id); err != nil {
			return tenant.ScrubFinding{}, false, err
		}
		made = true
		tallies[tenant.ScrubTally(f.Kind)] = 1
		if f.Kind == tenant.FindingOrphan {
			tallies[tenant.TallyOrphanObjects] = 1
		}
		e, err := tenant.ScrubFindingEvent(out, false, job, rec.Now)
		if err != nil {
			return tenant.ScrubFinding{}, false, err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return tenant.ScrubFinding{}, false, err
		}
	}
	if out.Kind.Marks() && out.SOPInstanceUID != "" && out.ReplicaOf == "" {
		if _, err := tx.Exec(ctx, `INSERT INTO instance_damage (sop_instance_uid, finding_id, marked_at) VALUES (?, ?, ?)
			ON CONFLICT (sop_instance_uid) DO NOTHING`, out.SOPInstanceUID, out.ID, k.D.Time(rec.Now)); err != nil {
			return tenant.ScrubFinding{}, false, err
		}
	}
	if r.job.State.TakesProgress() {
		return out, made, k.addTallies(ctx, tx, job, tallies)
	}
	return out, made, nil
}

// ClearScrubFinding implements tenant.Scrubbing.
func (k Sending) ClearScrubFinding(ctx context.Context, fence tenant.WorkFence, id string, now time.Time) (tenant.ScrubFinding, bool, error) {
	if err := fence.Validate(); err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	if err := master.ValidateID("finding id", id); err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	var out tenant.ScrubFinding
	var cleared bool
	err := k.Host.Write(ctx, "the scrub's finding cannot be cleared", func(tx SendingTx) error {
		out, cleared = tenant.ScrubFinding{}, false
		r, err := k.scrubJobOfUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		f, found, err := findingIn(ctx, tx, findingByIDSQL+k.D.Lock, id)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no scrub finding with this id")
		}
		out = f
		if !f.Holds() {
			return nil
		}
		if out, err = k.clearInTx(ctx, tx, r.job.ID, f, now); err != nil {
			return err
		}
		cleared = true
		if r.job.State.TakesProgress() {
			return k.addTallies(ctx, tx, r.job.ID, map[string]int64{tenant.TallyCleared: 1})
		}
		return nil
	})
	if err != nil {
		return tenant.ScrubFinding{}, false, err
	}
	return out, cleared, nil
}

// clearInTx clears the finding f, which holds, by job: its row, the mark
// it made, and its event. It returns the finding as it is after.
func (k Sending) clearInTx(ctx context.Context, tx SendingTx, job string, f tenant.ScrubFinding, now time.Time) (tenant.ScrubFinding, error) {
	if _, err := tx.Exec(ctx, `UPDATE scrub_findings SET cleared_at = ?, clear_job = ? WHERE id = ? AND cleared_at IS NULL`,
		k.D.Time(now), job, f.ID); err != nil {
		return f, err
	}
	f.Cleared, f.ClearJob = now, job
	if f.SOPInstanceUID != "" {
		if err := k.unmark(ctx, tx, f.SOPInstanceUID, f.ID, now); err != nil {
			return f, err
		}
	}
	e, err := tenant.ScrubFindingEvent(f, true, job, now)
	if err != nil {
		return f, err
	}
	return f, tx.WriteEvents(ctx, e)
}

// ClearObjectFindings implements tenant.Scrubbing.
func (k Sending) ClearObjectFindings(ctx context.Context, fence tenant.WorkFence, storage, key string, kinds []tenant.ScrubFindingKind,
	now time.Time) (int, error) {
	if err := fence.Validate(); err != nil {
		return 0, err
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return 0, err
	}
	if err := tenant.ValidateFindingKey(key); err != nil {
		return 0, err
	}
	if err := tenant.ValidateObjectKinds(kinds); err != nil {
		return 0, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return 0, err
	}
	var n int
	err := k.Host.Write(ctx, "the scrub's findings cannot be cleared", func(tx SendingTx) error {
		n = 0
		r, err := k.scrubJobOfUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		for _, kind := range kinds {
			f, held, err := findingIn(ctx, tx, openFindingSQL+k.D.Lock, storage, key, string(kind))
			if err != nil {
				return err
			}
			if !held {
				continue
			}
			if _, err := k.clearInTx(ctx, tx, r.job.ID, f, now); err != nil {
				return err
			}
			n++
		}
		if n > 0 && r.job.State.TakesProgress() {
			return k.addTallies(ctx, tx, r.job.ID, map[string]int64{tenant.TallyCleared: int64(n)})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// AddScrubTallies implements tenant.Scrubbing.
func (k Sending) AddScrubTallies(ctx context.Context, fence tenant.WorkFence, tallies map[string]int64) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	if err := tenant.ValidateScrubTallies(tallies); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the scrub's tallies cannot be written", func(tx SendingTx) error {
		r, err := k.scrubJobOfUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		if !r.job.State.TakesProgress() {
			return nil
		}
		return k.addTallies(ctx, tx, r.job.ID, tallies)
	})
}

// LastScrub implements tenant.Scrubbing.
func (k Sending) LastScrub(ctx context.Context, storage string) (tenant.Job, bool, error) {
	if err := tenant.ValidateName("storage", storage); err != nil {
		return tenant.Job{}, false, err
	}
	var out tenant.Job
	var found bool
	err := k.Host.Read(ctx, "the last scrub cannot be read", func(tx SendingTx) error {
		out, found = tenant.Job{}, false
		for _, s := range []string{storage, ""} {
			var id string
			has, err := tx.QueryRow(ctx, lastScrubSQL, []any{s}, &id)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			r, err := jobInTx(ctx, tx, id, "")
			if err != nil {
				return err
			}
			if err := fillJob(ctx, tx, &r.job); err != nil {
				return err
			}
			if !found || r.job.Created.After(out.Created) || (r.job.Created.Equal(out.Created) && r.job.ID > out.ID) {
				out, found = r.job, true
			}
		}
		return nil
	})
	if err != nil {
		return tenant.Job{}, false, err
	}
	return out, found, nil
}

// unmark removes the mark finding id made on the instance, and marks it
// again with another finding of the instance that still holds and marks,
// when there is one: an instance stays marked while any such finding holds.
func (k Sending) unmark(ctx context.Context, tx SendingTx, uid, id string, now time.Time) error {
	n, err := tx.Exec(ctx, `DELETE FROM instance_damage WHERE sop_instance_uid = ? AND finding_id = ?`, uid, id)
	if err != nil || n == 0 {
		return err
	}
	var other string
	found, err := tx.QueryRow(ctx, markingFindingSQL, []any{uid}, &other)
	if err != nil || !found {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO instance_damage (sop_instance_uid, finding_id, marked_at) VALUES (?, ?, ?)`,
		uid, other, k.D.Time(now))
	return err
}

// ClearDamageInTx removes the mark of the instance, whichever finding made
// it, in the caller's transaction: for a change that makes the instance's
// bytes whole again (a replace from a file sent again), which writes its own
// record of why. It reports whether the instance was marked.
func ClearDamageInTx(ctx context.Context, tx SendingTx, uid string) (bool, error) {
	n, err := tx.Exec(ctx, `DELETE FROM instance_damage WHERE sop_instance_uid = ?`, uid)
	return n > 0, err
}

// ScanScrubFindings implements tenant.Scrubbing.
func (k Sending) ScanScrubFindings(ctx context.Context, storage string, path label.Path, afterKey string,
	afterKind tenant.ScrubFindingKind) ([]tenant.ScrubFinding, error) {
	if err := tenant.ValidateName("storage", storage); err != nil {
		return nil, err
	}
	if err := tenant.ValidateScanPath(path); err != nil {
		return nil, err
	}
	if afterKey != "" || afterKind != "" {
		if err := tenant.ValidateFindingKey(afterKey); err != nil {
			return nil, err
		}
		if !afterKind.Valid() {
			return nil, fault.New(fault.Invalid, "a finding's kind is one of missing, orphan, damaged, disagreeing, manifest_damaged")
		}
	}
	var out []tenant.ScrubFinding
	err := k.Host.Read(ctx, "the scan's scrub findings cannot be read", func(tx SendingTx) error {
		out = nil
		args := []any{storage, path.Subject, path.Session, path.Scan, afterKey, string(afterKind), tenant.MaxScanFindings}
		return tx.Query(ctx, scanFindingsSQL, args,
			func(r Row) error {
				f, err := scanFinding(r)
				out = append(out, f)
				return err
			})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// findingBinding is what a list's cursor is bound to: the tenant and the
// filter.
func (k Sending) findingBinding(f tenant.ScrubFindingFilter) string {
	kinds := make([]string, len(f.Kinds))
	for i, kind := range f.Kinds {
		kinds[i] = string(kind)
	}
	return k.Host.Binding(strings.Join(kinds, ","), f.Storage, strconv.FormatBool(f.IncludeCleared), f.Path.Subject, f.Path.Session,
		f.Path.Scan)
}

// FindingsQuery returns the statement and arguments of one page of
// ListScrubFindings after the (storage, key, kind, id) given, nil for the
// first page: a range of scrub_findings_by_key, every filter a condition on
// its rows. Every fragment is a constant; values are parameters.
func (d SendingDialect) FindingsQuery(f tenant.ScrubFindingFilter, after []string, limit int) (string, []any) {
	var where []string
	var args []any
	if f.Storage != "" {
		where, args = append(where, "storage = ?"), append(args, f.Storage)
	}
	if f.Path != (label.Path{}) {
		where = append(where, "subject_label = ? AND session_label = ? AND scan_label = ?")
		args = append(args, f.Path.Subject, f.Path.Session, f.Path.Scan)
	}
	if !f.IncludeCleared {
		where = append(where, "cleared_at IS NULL")
	}
	if len(f.Kinds) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(f.Kinds)), ", ")
		where = append(where, "kind IN ("+marks+")")
		for _, kind := range f.Kinds {
			args = append(args, string(kind))
		}
	}
	if after != nil {
		where = append(where, "(storage, object_key, kind, id) > (?, ?, ?, ?)")
		args = append(args, after[0], after[1], after[2], after[3])
	}
	q := `SELECT ` + findingColumns + ` FROM scrub_findings`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
	}
	return q + ` ORDER BY storage, object_key, kind, id LIMIT ?`, append(args, limit)
}

// ListScrubFindings implements tenant.Scrubbing.
func (k Sending) ListScrubFindings(ctx context.Context, filter tenant.ScrubFindingFilter, page master.Page) ([]tenant.ScrubFinding, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.findingBinding(filter)
	afterID := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListScrubFindings, binding, 1)
		if err != nil {
			return nil, "", err
		}
		if err := master.ValidateID("finding id", f[0]); err != nil {
			return nil, "", errBadCursor()
		}
		afterID = f[0]
	}
	var out []tenant.ScrubFinding
	err := k.Host.Read(ctx, "the scrub findings cannot be listed", func(tx SendingTx) error {
		out = nil
		var after []string
		if afterID != "" {
			// The cursor holds the last finding's id, never its key; the
			// key is read back here, in the page's snapshot. A finding is
			// never removed, so the id always names one.
			last, found, err := findingIn(ctx, tx, findingByIDSQL, afterID)
			if err != nil {
				return err
			}
			if !found {
				return errBadCursor()
			}
			after = []string{last.Storage, last.Key, string(last.Kind), last.ID}
		}
		query, args := k.D.FindingsQuery(filter, after, page.Limit+1)
		return tx.Query(ctx, query, args, func(r Row) error {
			f, err := scanFinding(r)
			out = append(out, f)
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
	return out, EncodeCursor(ListScrubFindings, binding, last.ID), nil
}

// OpenScrubFindings implements tenant.Scrubbing.
func (k Sending) OpenScrubFindings(ctx context.Context) (map[tenant.ScrubFindingKind]int64, error) {
	out := map[tenant.ScrubFindingKind]int64{}
	err := k.Host.Read(ctx, "the scrub findings cannot be counted", func(tx SendingTx) error {
		clear(out)
		return tx.Query(ctx, openFindingCountsSQL, nil, func(r Row) error {
			var kind string
			var n int64
			err := r.Scan(&kind, &n)
			out[tenant.ScrubFindingKind(kind)] = n
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DamagedInstances implements tenant.Scrubbing.
func (k Sending) DamagedInstances(ctx context.Context, uids []string) (map[string]string, error) {
	if err := tenant.ValidateDamageLookup(uids); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(uids) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(uids)), ", ") + ")"
	args := make([]any, len(uids))
	for i, u := range uids {
		args[i] = u
	}
	err := k.Host.Read(ctx, "the marks of damaged instances cannot be read", func(tx SendingTx) error {
		clear(out)
		// #nosec G202 -- damageSQL is a constant and marks only placeholders; values are parameters.
		return tx.Query(ctx, damageSQL+marks, args, func(r Row) error {
			var uid, finding string
			err := r.Scan(&uid, &finding)
			out[uid] = finding
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
