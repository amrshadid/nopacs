package sqlkit

import (
	"context"
	"encoding/hex"
	"time"

	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a restore asks of a tenant's database (tenant.Restoring), on the
// tables of 0022_corrections.sql and 0023_storage_owners.sql.

// StorageOwners implements tenant.Restoring.
func (k Sending) StorageOwners(ctx context.Context) ([]tenant.StorageOwner, error) {
	var out []tenant.StorageOwner
	err := k.Host.Read(ctx, "the storages' owners cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT storage, generation, written FROM storage_owners ORDER BY storage`, nil, func(r Row) error {
			var o tenant.StorageOwner
			var w DBTime
			if err := r.Scan(&o.Storage, &o.Generation, &w); err != nil {
				return err
			}
			o.Written = w.T
			out = append(out, o)
			return nil
		})
	})
	return out, err
}

// SetStorageOwner implements tenant.Restoring.
func (k Sending) SetStorageOwner(ctx context.Context, storage string, generation int64, now time.Time) error {
	if err := tenant.ValidateName("storage", storage); err != nil {
		return err
	}
	if generation < 1 || generation > corr.MaxGeneration {
		return fault.New(fault.Invalid, "an owner generation is 1 to 2^53-1")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the storage's owner cannot be recorded", func(tx SendingTx) error {
		var held int64
		found, err := tx.QueryRow(ctx, `SELECT generation FROM storage_owners WHERE storage = ?`+k.D.Lock, []any{storage}, &held)
		if err != nil {
			return err
		}
		if found && held > generation {
			return tenant.Precondition("the database holds a later owner generation for this storage")
		}
		if found {
			_, err = tx.Exec(ctx, `UPDATE storage_owners SET generation = ?, written = ? WHERE storage = ?`, generation, k.D.Time(now), storage)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO storage_owners (storage, generation, written) VALUES (?, ?, ?)`, storage, generation, k.D.Time(now))
		return err
	})
}

// adoptedRow is what a correction's row says when a record is adopted.
type adoptedRow struct {
	state       corr.State
	kind        corr.Kind
	unresolved  bool
	stateReason string
}

// AdoptCorrection implements tenant.Restoring.
func (k Sending) AdoptCorrection(ctx context.Context, rec corr.Correction, storage string, now time.Time) (tenant.Adopted, error) {
	if err := rec.Validate(); err != nil {
		return tenant.AdoptedNone, fault.Wrap(fault.Invalid, err, "the correction record is not valid")
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return tenant.AdoptedNone, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.AdoptedNone, err
	}
	var out tenant.Adopted
	err := k.Host.Write(ctx, "the correction cannot be brought up to its record", func(tx SendingTx) error {
		out = tenant.AdoptedNone
		var row adoptedRow
		var kind, state string
		var unresolved bool
		found, err := tx.QueryRow(ctx, `SELECT kind, state, unresolved, state_reason FROM corrections WHERE id = ?`+k.D.Lock,
			[]any{rec.ID}, &kind, &state, &unresolved, &row.stateReason)
		if err != nil {
			return err
		}
		row.kind, row.state, row.unresolved = corr.Kind(kind), corr.State(state), unresolved
		switch {
		case !found:
			out = tenant.AdoptedAdded
			if err := k.insertAdopted(ctx, tx, rec, now); err != nil {
				return err
			}
			return k.adoptScans(ctx, tx, rec, storage, adoptedState(rec))
		case row.kind != rec.Kind:
			// The backup's row wins; the record is answered as one that
			// disagrees with it, and changes nothing.
			out = tenant.AdoptedDisagrees
			return nil
		case row.unresolved && row.stateReason == tenant.StateReasonUnresolved:
			out = tenant.AdoptedScans
			return k.adoptScans(ctx, tx, rec, storage, row.state)
		case rec.State.After(row.state, rec.Kind):
			// A record is not authenticated: the row stays as the backup
			// holds it, so a record cannot end a delete (dropping what it
			// hid, or showing its data again) or purge it. Its records are
			// marked as written with the record's state, so a start writes
			// them again from the row.
			out = tenant.AdoptedDisagrees
			_, err := tx.Exec(ctx, `UPDATE corrections SET records_state = ? WHERE id = ?`, string(rec.State), rec.ID)
			return err
		}
		return nil
	})
	return out, err
}

// adoptedState is the state a row made from rec takes. A record is not
// authenticated, so it never makes a row in an end it cannot come back
// from: a delete, whatever its record says, is committed and unresolved,
// holding its data for an admin's decision (an undo or a purge), so a
// record cannot purge data or end a delete made after the backup; a
// prepared or abandoned relabel or replace is abandoned, since no row is
// left to finish or undo its writes; one published is committed and
// unresolved, never purged.
func adoptedState(rec corr.Correction) corr.State {
	switch {
	case rec.Kind == corr.KindDelete:
		return corr.StateCommitted
	case rec.State == corr.StatePrepared || rec.State == corr.StateAbandoned:
		return corr.StateAbandoned
	}
	return corr.StateCommitted
}

// ended reports whether s is an end of the state machine.
func ended(s corr.State) bool {
	return s == corr.StatePurged || s == corr.StateUndone || s == corr.StateAbandoned
}

// insertAdopted writes the row of a correction known only from its record.
func (k Sending) insertAdopted(ctx context.Context, tx SendingTx, rec corr.Correction, now time.Time) error {
	state := adoptedState(rec)
	first := rec.Scans[0]
	level := rec.Level
	subject, session, scan, instance, to := first.Subject, "", "", "", ""
	oldRev, newRev := 0, 0
	var oldSum, newSum any
	switch rec.Kind {
	case corr.KindRelabel:
		level = rec.Relabel.Level
		to = rec.Relabel.To
	case corr.KindReplace:
		level = corr.LevelInstance
		instance = rec.Replace.SOPInstanceUID
		oldRev, newRev = rec.Replace.OldRevision, rec.Replace.NewRevision
		if b, err := hex.DecodeString(rec.Replace.OldSHA256); err == nil {
			oldSum = b
		}
		if b, err := hex.DecodeString(rec.Replace.NewSHA256); err == nil {
			newSum = b
		}
	default:
		if level == corr.LevelInstance && len(rec.Instances) > 0 {
			instance = rec.Instances[0]
		}
	}
	if level != corr.LevelSubject {
		session = first.Session
	}
	if level == corr.LevelScan || level == corr.LevelInstance {
		scan = first.Scan
	}
	var committed, purgeAfter, endedAt any
	switch {
	case rec.State.Published() || rec.State == corr.StateUndone:
		committed, purgeAfter = k.D.Time(rec.CommittedAt), k.D.Time(rec.PurgeAfter)
	case state == corr.StateCommitted:
		// A delete found prepared or abandoned: published now by the
		// restore, its purge left to an admin, since it is unresolved.
		committed, purgeAfter = k.D.Time(rec.Created), k.D.Time(rec.Created)
	}
	if ended(state) {
		endedAt = k.D.Time(now)
	}
	unresolved := state == corr.StateCommitted
	subjects, sessions := map[string]bool{}, map[string]bool{}
	for _, s := range rec.Scans {
		subjects[s.Subject] = true
		sessions[s.StudyUID] = true
	}
	_, err := tx.Exec(ctx, `INSERT INTO corrections (`+correctionColumns+`, confirm_token)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, '', 0, NULL, ?, '', ?, ?, ?, ?, 0, 0, 0, 0, NULL)`,
		rec.ID, string(rec.Kind), level, string(state), string(rec.State), tenant.OriginPerson, subject, session, scan, instance, to,
		k.D.Time(rec.Created), committed, purgeAfter, endedAt, tenant.StateReasonUnresolved, oldRev, newRev, oldSum, newSum,
		k.D.Bool(false), k.D.Bool(unresolved), len(subjects), len(sessions), len(rec.Scans))
	if err != nil {
		return err
	}
	if rec.Kind == corr.KindRelabel {
		_, err = tx.Exec(ctx, `INSERT INTO relabels (correction_id, level, old_subject, old_session, old_scan, new_label)
			VALUES (?, ?, ?, ?, ?, ?)`, rec.ID, level, subject, session, scan, to)
	}
	return err
}

// adoptScans writes the scans of rec on one storage into its row, and for a
// delete in state what names its data. A scan already written is left.
func (k Sending) adoptScans(ctx context.Context, tx SendingTx, rec corr.Correction, storage string, state corr.State) error {
	for _, s := range rec.Scans {
		np := corr.Scan{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
		if rec.Kind == corr.KindRelabel {
			switch rec.Relabel.Level {
			case corr.LevelSubject:
				np.Subject = rec.Relabel.To
			case corr.LevelSession:
				np.Session = rec.Relabel.To
			default:
				np.Scan = rec.Relabel.To
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO correction_scans (correction_id, series_uid, study_uid, storage, subject_label,
			session_label, scan_label, new_subject, new_session, new_scan, instances, bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)
			ON CONFLICT DO NOTHING`, rec.ID, s.SeriesUID, s.StudyUID, storage, s.Subject, s.Session, s.Scan, np.Subject, np.Session,
			np.Scan); err != nil {
			return err
		}
	}
	if rec.Kind != corr.KindDelete || state == corr.StateUndone || state == corr.StateAbandoned {
		return nil
	}
	add := func(kind, value string) error {
		_, err := tx.Exec(ctx, `INSERT INTO deleted_targets (kind, value, correction_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			kind, value, rec.ID)
		return err
	}
	if rec.Level == corr.LevelInstance {
		for _, u := range rec.Instances {
			if err := add("instance", u); err != nil {
				return err
			}
		}
		return nil
	}
	for _, s := range rec.Scans {
		if rec.Level == corr.LevelSubject {
			if err := add("subject", s.Subject); err != nil {
				return err
			}
		}
		if rec.Level != corr.LevelScan {
			if err := add("study", s.StudyUID); err != nil {
				return err
			}
		}
		if err := add("series", s.SeriesUID); err != nil {
			return err
		}
	}
	return nil
}

// relinkStatements point the rows a committed delete hid at the catalog's
// rows they hang from, by name, after a restore rebuilt the catalog under
// new ids: a hidden row whose parent the same delete hid keeps its id (the
// parent comes back with it at an undo); one whose parent stayed in the
// catalog (a session's subject, a scan's session, an instance's scan, a
// session's modalities and a reception's counts of a scan that stayed)
// takes the id that parent has now. The modalities go first, while the
// hidden scans still hold the old session ids that name their study.
var relinkStatements = []string{
	`UPDATE hidden_session_modalities SET session_id = (SELECT s.id FROM sessions AS s WHERE s.study_uid =
		(SELECT min(h.study_uid) FROM hidden_scans AS h WHERE h.correction_id = hidden_session_modalities.correction_id
			AND h.session_id = hidden_session_modalities.session_id))
	WHERE session_id NOT IN (SELECT h.id FROM hidden_sessions AS h WHERE h.correction_id = hidden_session_modalities.correction_id)
		AND EXISTS (SELECT 1 FROM sessions AS s WHERE s.study_uid = (SELECT min(h.study_uid) FROM hidden_scans AS h
			WHERE h.correction_id = hidden_session_modalities.correction_id AND h.session_id = hidden_session_modalities.session_id))`,
	`UPDATE hidden_instances SET scan_id = (SELECT s.id FROM scans AS s WHERE s.series_uid =
		(SELECT min(c.series_uid) FROM correction_scans AS c WHERE c.correction_id = hidden_instances.correction_id))
	WHERE scan_id NOT IN (SELECT h.id FROM hidden_scans AS h WHERE h.correction_id = hidden_instances.correction_id)
		AND (SELECT count(*) FROM correction_scans AS c WHERE c.correction_id = hidden_instances.correction_id) = 1
		AND EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = (SELECT min(c.series_uid) FROM correction_scans AS c
			WHERE c.correction_id = hidden_instances.correction_id))`,
	`UPDATE hidden_reception_scans SET scan_id = (SELECT s.id FROM scans AS s WHERE s.series_uid =
		(SELECT min(c.series_uid) FROM correction_scans AS c WHERE c.correction_id = hidden_reception_scans.correction_id))
	WHERE scan_id NOT IN (SELECT h.id FROM hidden_scans AS h WHERE h.correction_id = hidden_reception_scans.correction_id)
		AND (SELECT count(*) FROM correction_scans AS c WHERE c.correction_id = hidden_reception_scans.correction_id) = 1
		AND EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = (SELECT min(c.series_uid) FROM correction_scans AS c
			WHERE c.correction_id = hidden_reception_scans.correction_id))`,
	`UPDATE hidden_scans SET session_id = (SELECT s.id FROM sessions AS s WHERE s.study_uid = hidden_scans.study_uid)
	WHERE session_id NOT IN (SELECT h.id FROM hidden_sessions AS h WHERE h.correction_id = hidden_scans.correction_id)
		AND EXISTS (SELECT 1 FROM sessions AS s WHERE s.study_uid = hidden_scans.study_uid)`,
	`UPDATE hidden_sessions SET subject_id = (SELECT s.id FROM subjects AS s WHERE s.folded = hidden_sessions.subject_folded)
	WHERE subject_id NOT IN (SELECT h.id FROM hidden_subjects AS h WHERE h.correction_id = hidden_sessions.correction_id)
		AND EXISTS (SELECT 1 FROM subjects AS s WHERE s.folded = hidden_sessions.subject_folded)`,
}

// RelinkHidden implements tenant.Restoring.
func (k Sending) RelinkHidden(ctx context.Context) (int64, error) {
	var n int64
	err := k.Host.Write(ctx, "the rows hidden by deletes cannot be linked to the rebuilt catalog", func(tx SendingTx) error {
		n = 0
		for _, q := range relinkStatements {
			changed, err := tx.Exec(ctx, q)
			if err != nil {
				return err
			}
			n += changed
		}
		return nil
	})
	return n, err
}

// MarkQuarantineLost implements tenant.Restoring: every record held whose
// bytes were kept becomes incoming lost, with no details and no bytes kept
// (the reason's own rules: tenant.QuarantinedFile.Validate), and the
// quarantine's totals stop counting the bytes kept for them.
func (k Sending) MarkQuarantineLost(ctx context.Context) (int64, error) {
	var n int64
	err := k.Host.Write(ctx, "the quarantined files whose bytes are gone cannot be marked", func(tx SendingTx) error {
		var bytes int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*), CAST(coalesce(sum(size), 0) AS BIGINT) FROM quarantine WHERE state = ? AND content_kept = ?`,
			[]any{string(tenant.QuarantineHeld), k.D.Bool(true)}, &n, &bytes); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE quarantine SET reason = ?, details = '{}', content_kept = ?, incoming = '', kept_sha256 = NULL
			WHERE state = ? AND content_kept = ?`, string(tenant.QuarantineIncomingLost), k.D.Bool(false), string(tenant.QuarantineHeld),
			k.D.Bool(true)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE quarantine_totals SET bytes = CASE WHEN bytes > ? THEN bytes - ? ELSE 0 END`, bytes, bytes)
		return err
	})
	return n, err
}
