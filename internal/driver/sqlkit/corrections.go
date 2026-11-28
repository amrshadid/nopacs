package sqlkit

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's corrections (tenant.Corrections) on the tables of the
// corrections' migration: the rows of corrections and of the scans each
// touches, the catalog's rows a committed delete hides (the hidden_
// tables, each the columns of its catalog table and the correction's id),
// what names deleted data (deleted_targets), the revisions no longer
// current (instance_revisions), relabels, the objects each correction owns
// (correction_keys), each object a purge removed (correction_objects), and
// the clock a purge trusts (correction_clock). One implementation, run in
// each driver's
// transactions; a transaction that moves catalog rows locks the
// configuration first, so two corrections never interleave their moves.

// ListCorrections is the corrections' list's name, the first field of its
// cursor.
const ListCorrections = "tenant-corrections"

// correctionColumns are a correction's columns as the store reads them.
const correctionColumns = `id, kind, level, state, records_state, origin, subject_label, session_label, scan_label, instance_uid,
	to_label, quarantine_id, reason, key_id, created, committed_at, purge_after, ended_at, state_reason, old_revision,
	new_revision, old_sha256, new_sha256, received_sha256, new_size, instance_key, instance_size, restore_line,
	arrivals_allowed, undo_reason, unresolved, subjects, sessions, scans, instances, bytes, objects_copied, objects_superseded`

// correctionSelect is what a read selects of a correction: its columns as
// a row is written with them (correctionColumns, which a restore writes
// too), then those a later change gave a default.
const correctionSelect = correctionColumns + `, swept, retention_rule, retention_due, override_retention, override_reason,
	copies_in_flight_until, rewrite_identity, identity_refused`

// scanCorrection reads one row of correctionSelect.
func scanCorrection(row Row) (tenant.Correction, error) {
	var c tenant.Correction
	var kind, state, records string
	var created, committed, purgeAfter, ended, due, inFlight DBTime
	var oldSum, newSum, received []byte
	err := row.Scan(&c.ID, &kind, &c.Level, &state, &records, &c.Origin, &c.Target.Subject, &c.Target.Session, &c.Target.Scan,
		&c.Target.Instance, &c.ToLabel, &c.Quarantine, &c.Reason, &c.KeyID, &created, &committed, &purgeAfter, &ended,
		&c.StateReason, &c.OldRevision, &c.NewRevision, &oldSum, &newSum, &received, &c.NewSize, &c.InstanceKey,
		&c.InstanceSize, &c.RestoreLine, &c.ArrivalsAllowed, &c.UndoReason, &c.Unresolved, &c.Counts.Subjects,
		&c.Counts.Sessions, &c.Counts.Scans, &c.Counts.Instances, &c.Counts.Bytes, &c.Counts.ObjectsCopied,
		&c.Counts.ObjectsSuperseded, &c.Swept, &c.RetentionRule, &due, &c.OverrideRetention, &c.OverrideReason, &inFlight,
		&c.RewriteIdentity, &c.IdentityRefused)
	c.Kind, c.State, c.RecordsState = corr.Kind(kind), corr.State(state), corr.State(records)
	c.Created, c.CommitTime, c.PurgeAfter, c.Ended, c.RetentionDue = created.T, committed.T, purgeAfter.T, ended.T, due.T
	c.CopiesInFlightUntil = inFlight.T
	c.OldSHA256, c.NewSHA256, c.ReceivedSHA256 = ToSHA256(oldSum), ToSHA256(newSum), ToSHA256(received)
	return c, err
}

// correctionIn reads one correction, locked with lock, and its storages,
// dispositions and objects removed; NotFound when there is none.
func (k Sending) correctionIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.Correction, error) {
	var out tenant.Correction
	found := false
	err := tx.Query(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE id = ?`+lock, []any{id}, func(r Row) error {
		var err error
		out, err = scanCorrection(r)
		found = true
		return err
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no correction with this id")
	}
	if err := k.fillCorrection(ctx, tx, &out); err != nil {
		return out, err
	}
	if out.Kind == corr.KindEdit {
		if out.Edits, err = correctionEditsIn(ctx, tx, id); err != nil {
			return out, err
		}
	}
	out.Holds = nil
	return out, tx.Query(ctx, `SELECT kind, value FROM deleted_targets WHERE correction_id = ? ORDER BY kind, value`, []any{id},
		func(r Row) error {
			var v tenant.HeldValue
			err := r.Scan(&v.Kind, &v.Value)
			out.Holds = append(out.Holds, v)
			return err
		})
}

// fillCorrection gives c its storages and what its purge recorded.
func (k Sending) fillCorrection(ctx context.Context, tx SendingTx, c *tenant.Correction) error {
	c.Storages = nil
	err := tx.Query(ctx, `SELECT DISTINCT storage FROM correction_scans WHERE correction_id = ? ORDER BY storage`, []any{c.ID},
		func(r Row) error {
			var s string
			err := r.Scan(&s)
			c.Storages = append(c.Storages, s)
			return err
		})
	if err != nil {
		return err
	}
	if _, err := tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(CASE WHEN outcome IN ('kept_shared', 'kept_changed') THEN 1 ELSE 0 END), 0)
		FROM correction_keys WHERE correction_id = ?`, []any{c.ID}, &c.Counts.ObjectsOwned, &c.Counts.ObjectsKept); err != nil {
		return err
	}
	c.Dispositions, c.Counts.ObjectsRemoved, c.Counts.BytesRemoved = nil, 0, 0
	// An object is the rows of one key, one per version a versioned
	// bucket held: it is removed only when every version is, and counted
	// by the strongest claim against that (retained, then unknown, then
	// hidden).
	var key string
	var worst tenant.Disposition
	var size int64
	flush := func() {
		if key == "" {
			return
		}
		d := &c.Dispositions[len(c.Dispositions)-1]
		switch worst {
		case tenant.DispositionRemoved:
			d.Removed++
			c.Counts.ObjectsRemoved++
			c.Counts.BytesRemoved += size
		case tenant.DispositionHidden:
			d.Hidden++
		case tenant.DispositionRetained:
			d.Retained++
		default:
			d.Unknown++
		}
	}
	rank := map[tenant.Disposition]int{tenant.DispositionRemoved: 0, tenant.DispositionHidden: 1, tenant.DispositionUnknown: 2,
		tenant.DispositionRetained: 3}
	// One disposition per storage and switch generation: a purge that spans
	// a switch says what each place did.
	err = tx.Query(ctx, `SELECT storage, generation, replica_of, object_key, disposition, version, size, retain_until
		FROM correction_objects WHERE correction_id = ? ORDER BY storage, generation, object_key, version`, []any{c.ID}, func(r Row) error {
		var storage, replicaOf, objectKey, disposition, version string
		var generation, n int64
		var until DBTime
		if err := r.Scan(&storage, &generation, &replicaOf, &objectKey, &disposition, &version, &n, &until); err != nil {
			return err
		}
		if n := len(c.Dispositions); n == 0 || c.Dispositions[n-1].Storage != storage || c.Dispositions[n-1].SwitchGeneration != generation {
			flush()
			key = ""
			c.Dispositions = append(c.Dispositions, tenant.StorageDisposition{Storage: storage, SwitchGeneration: generation,
				ReplicaOf: replicaOf})
		}
		if objectKey != key {
			flush()
			key, worst, size = objectKey, tenant.DispositionRemoved, n
		}
		d := &c.Dispositions[len(c.Dispositions)-1]
		if dd := tenant.Disposition(disposition); rank[dd] > rank[worst] {
			worst = dd
		}
		if version != "" && tenant.Disposition(disposition) == tenant.DispositionRemoved {
			d.VersionsRemoved++
		}
		if until.T.After(d.RetainUntil) {
			d.RetainUntil = until.T
		}
		return nil
	})
	flush()
	if err != nil {
		return err
	}
	return fillCopies(ctx, tx, c)
}

// Correction implements tenant.Corrections.
func (k Sending) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return tenant.Correction{}, err
	}
	var out tenant.Correction
	err := k.Host.Read(ctx, "the correction cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.correctionIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// CorrectionScans implements tenant.Corrections.
func (k Sending) CorrectionScans(ctx context.Context, id string) ([]tenant.CorrectionScan, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	var out []tenant.CorrectionScan
	err := k.Host.Read(ctx, "the correction's scans cannot be read", func(tx SendingTx) error {
		var err error
		out, err = correctionScansIn(ctx, tx, id)
		return err
	})
	return out, err
}

// correctionScansIn reads a correction's scans.
func correctionScansIn(ctx context.Context, tx SendingTx, id string) ([]tenant.CorrectionScan, error) {
	var out []tenant.CorrectionScan
	err := tx.Query(ctx, `SELECT storage, subject_label, session_label, scan_label, new_subject, new_session, new_scan, study_uid,
		series_uid, instances, bytes FROM correction_scans WHERE correction_id = ? ORDER BY storage, series_uid`, []any{id},
		func(r Row) error {
			var s tenant.CorrectionScan
			err := r.Scan(&s.Storage, &s.Path.Subject, &s.Path.Session, &s.Path.Scan, &s.NewPath.Subject, &s.NewPath.Session,
				&s.NewPath.Scan, &s.StudyUID, &s.SeriesUID, &s.Instances, &s.Bytes)
			out = append(out, s)
			return err
		})
	return out, err
}

// correctionBinding is what a list's cursor is bound to.
func (k Sending) correctionBinding(f tenant.CorrectionFilter) string {
	var kinds, states []string
	for _, x := range f.Kinds {
		kinds = append(kinds, string(x))
	}
	for _, x := range f.States {
		states = append(states, string(x))
	}
	slices.Sort(kinds)
	slices.Sort(states)
	return k.Host.Binding(strings.Join(kinds, ","), strings.Join(states, ","))
}

// ListCorrections implements tenant.Corrections.
func (k Sending) ListCorrections(ctx context.Context, filter tenant.CorrectionFilter, page master.Page) ([]tenant.Correction, string, error) {
	if err := tenant.ValidateCorrectionFilter(filter); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.correctionBinding(filter)
	before := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListCorrections, binding, 1)
		if err != nil {
			return nil, "", err
		}
		if tenant.ValidateCorrectionID(f[0]) != nil {
			return nil, "", errBadCursor()
		}
		before = f[0]
	}
	var where []string
	var args []any
	if len(filter.Kinds) > 0 {
		where = append(where, "kind IN ("+marks(len(filter.Kinds))+")")
		for _, x := range filter.Kinds {
			args = append(args, string(x))
		}
	}
	if len(filter.States) > 0 {
		where = append(where, "state IN ("+marks(len(filter.States))+")")
		for _, x := range filter.States {
			args = append(args, string(x))
		}
	}
	if before != "" {
		where, args = append(where, "id < ?"), append(args, before)
	}
	q := `SELECT ` + correctionSelect + ` FROM corrections`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant; values are parameters.
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, page.Limit+1)
	var out []tenant.Correction
	err := k.Host.Read(ctx, "the corrections cannot be listed", func(tx SendingTx) error {
		out = nil
		if err := tx.Query(ctx, q, args, func(r Row) error {
			c, err := scanCorrection(r)
			out = append(out, c)
			return err
		}); err != nil {
			return err
		}
		for i := range out {
			if err := k.fillCorrection(ctx, tx, &out[i]); err != nil {
				return err
			}
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
	return out, EncodeCursor(ListCorrections, binding, last.ID), nil
}

// PendingCorrections implements tenant.Corrections.
func (k Sending) PendingCorrections(ctx context.Context, after string, limit int) ([]tenant.Correction, error) {
	if after != "" {
		if err := tenant.ValidateCorrectionID(after); err != nil {
			return nil, err
		}
	}
	if limit < 1 || limit > 1000 {
		return nil, fault.New(fault.Invalid, "a limit is 1 to 1000")
	}
	return k.readCorrections(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE id > ?
		AND (state IN ('prepared', 'committed', 'purging') OR records_state <> state
			OR (kind = 'relabel' AND state = 'abandoned' AND swept = ?) OR (kind = 'edit' AND state = 'undone' AND swept = ?)
			OR (kind = 'move' AND state IN ('abandoned', 'undone') AND swept = ?))
		ORDER BY id LIMIT ?`, after, k.D.Bool(false), k.D.Bool(false), k.D.Bool(false), limit)
}

// DueCorrections implements tenant.Corrections.
func (k Sending) DueCorrections(ctx context.Context, now time.Time, limit int) ([]tenant.Correction, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, fault.New(fault.Invalid, "a limit is 1 to 1000")
	}
	// The purging ones come after: a purge that failed or stopped goes on
	// at the next pass, not only at the next start. An edit being undone is
	// not due: its undo is finished first (StartPurge refuses it).
	due, err := k.readCorrections(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE state = 'committed' AND unresolved = ?
		AND purge_after <= ? AND NOT (kind = 'edit' AND state_reason = ?) ORDER BY purge_after, id LIMIT ?`, k.D.Bool(false), k.D.Time(now),
		tenant.StateReasonUndoing, limit)
	if err != nil || len(due) >= limit {
		return due, err
	}
	purging, err := k.readCorrections(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE state = 'purging' ORDER BY id LIMIT ?`,
		limit-len(due))
	return append(due, purging...), err
}

// readCorrections reads the corrections a statement selects, filled.
func (k Sending) readCorrections(ctx context.Context, q string, args ...any) ([]tenant.Correction, error) {
	var out []tenant.Correction
	err := k.Host.Read(ctx, "the corrections cannot be read", func(tx SendingTx) error {
		out = nil
		if err := tx.Query(ctx, q, args, func(r Row) error {
			c, err := scanCorrection(r)
			out = append(out, c)
			return err
		}); err != nil {
			return err
		}
		for i := range out {
			if err := k.fillCorrection(ctx, tx, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// nullTime returns a time as a nullable column keeps it: NULL for zero.
func (k Sending) nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return k.D.Time(t)
}

// overlaps reports whether two paths name scans in common: one is a prefix
// of the other, level by level, an empty label covering everything below.
// Labels are compared case folded, as a label is unique at its level: two
// that differ in case alone are one directory on a file system that
// ignores case.
func overlaps(a, b label.Path) bool {
	if label.Fold(a.Subject) != label.Fold(b.Subject) {
		return false
	}
	if a.Session == "" || b.Session == "" || label.Fold(a.Session) != label.Fold(b.Session) {
		return a.Session == "" || b.Session == ""
	}
	return a.Scan == "" || b.Scan == "" || label.Fold(a.Scan) == label.Fold(b.Scan)
}

// checkOverlap refuses a correction while another not ended covers any of
// the scans of path, under its old labels or a relabel's new ones. An
// undone edit whose new revisions are not yet removed from every copy
// (swept) has not ended either: its sweep still removes objects under the
// scans' labels. (A SQLite integer and a Postgres boolean both read as a
// truth value under NOT.) A relabel or a move whose copies are not yet
// swept does not hold its new labels: its sweep leaves the copies of any
// scan another correction holds the new path of (SweepableScans).
func checkOverlap(ctx context.Context, tx SendingTx, path label.Path, except string) error {
	return overlapIn(ctx, tx, path, except, false)
}

// checkEditOverlap is checkOverlap for an edit, which alone may overlap a
// committed move: the identity a move rewrites is written by an edit of
// what it moved, made once it is published (its new revisions under the
// destination, never in the move's old tree), and the move's undo is
// refused while any later correction of its scans stands.
func checkEditOverlap(ctx context.Context, tx SendingTx, path label.Path, except string) error {
	return overlapIn(ctx, tx, path, except, true)
}

// overlapIn is checkOverlap, leaving out committed moves when movesAllowed.
func overlapIn(ctx context.Context, tx SendingTx, path label.Path, except string, movesAllowed bool) error {
	busy := false
	folded := label.Fold(path.Subject)
	q := `SELECT cs.subject_label, cs.session_label, cs.scan_label, cs.new_subject, cs.new_session, cs.new_scan
		FROM correction_scans AS cs JOIN corrections AS c ON c.id = cs.correction_id
		WHERE (c.state IN ('prepared', 'committed', 'purging') OR c.kind = 'edit' AND c.state = 'undone' AND NOT c.swept) AND c.id <> ?
		AND (lower(cs.subject_label) = ? OR lower(cs.new_subject) = ?)`
	if movesAllowed {
		q += ` AND NOT (c.kind = 'move' AND c.state = 'committed')`
	}
	err := tx.Query(ctx, q, []any{except, folded, folded}, func(r Row) error {
		var old, nw label.Path
		if err := r.Scan(&old.Subject, &old.Session, &old.Scan, &nw.Subject, &nw.Session, &nw.Scan); err != nil {
			return err
		}
		if overlaps(path, old) || overlaps(path, nw) {
			busy = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if busy {
		return tenant.OverlapError()
	}
	return nil
}

// rebuildNotEnded selects a rebuild job of either mode not ended, the
// dialect's condition on jobs.ended appended.
const rebuildNotEnded = `SELECT 1 FROM rebuild_jobs AS r JOIN jobs ON jobs.id = r.job_id WHERE `

// checkNoRebuild refuses a correction's step while a rebuild of the tenant
// is queued or running: its shadow would miss the change, or read it torn.
// Every caller is in a transaction that holds the tenant's configuration
// lock (step, stepRow and PrepareCorrection take it first, before the
// correction's row), which CreateJob of a rebuild takes too, so on a database with several
// writers the check and the making of a rebuild never pass each other.
func (k Sending) checkNoRebuild(ctx context.Context, tx SendingTx) error {
	busy, err := exists(ctx, tx, rebuildNotEnded+k.D.NotEnded)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("a rebuild of the tenant is running")
	}
	return nil
}
