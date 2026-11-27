package sqlkit

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a restore asks of a tenant's database (tenant.Restoring), on the
// corrections and storage_owners tables of each driver's tenant baseline
// schema (migrations/tenant/0001_baseline.sql).

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
			state, _ := rec.Alone()
			return k.adoptScans(ctx, tx, rec, storage, state)
		case row.kind != rec.Kind:
			// The backup's row wins; the record is answered as one that
			// disagrees with it, and changes nothing.
			out = tenant.AdoptedDisagrees
			return nil
		case row.kind == corr.KindDelete && row.state == corr.StateCommitted && rec.State == corr.StateUndone:
			// An undo after the backup: the engine writes a record undone
			// only once the database's undo has committed, so the verified
			// record proves it, whatever made the row (a request, or an
			// earlier restore from a record). The row is undone as an undo
			// does it, what it hid shown again; kept committed, its purge
			// would remove data the tenant took the delete of back.
			out = tenant.AdoptedMoved
			c, err := k.correctionIn(ctx, tx, rec.ID, "")
			if err != nil {
				return err
			}
			if err := k.undoDeleteIn(ctx, tx, &c, reasonUndoneByRecord, tenant.StateReasonFromRecord, now); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE corrections SET records_state = ? WHERE id = ?`, string(rec.State), rec.ID)
			return err
		case row.stateReason == tenant.StateReasonUnresolved:
			out = tenant.AdoptedScans
			return k.adoptScans(ctx, tx, rec, storage, row.state)
		case row.state == corr.StatePrepared && (rec.State.After(row.state, rec.Kind) ||
			rec.Kind == corr.KindDelete && rec.State == corr.StatePrepared):
			// The backup was taken before the correction's request was
			// answered, and its record, which verified, says how it went:
			// the row knows no outcome, so it takes the record's, by the
			// rule a rebuild reads a record with no row by. A row left
			// prepared would be rolled back at the next start, against
			// what the storages hold (a replace's new revision removed,
			// which the rebuilt catalog names). A delete whose record is
			// still prepared may have been published and answered with
			// its record's rewrite failed: it is taken as committed, and
			// unresolved, as a record with no row is.
			out = tenant.AdoptedMoved
			return k.moveAdopted(ctx, tx, rec, storage, now)
		case rec.State.After(row.state, rec.Kind):
			// A row the backup holds committed or further stays as it is
			// for a record of its purge: the purge began past its point of
			// no return, and the row's own purge, which finds what it
			// removed gone, ends it. Its records are
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

// reasonUndoneByRecord is the undo reason of a delete a restore undid as
// its record says: a record holds only its reason's digest.
const reasonUndoneByRecord = "undone after the backup, as its record says"

// moveAdopted moves a row the backup holds prepared to the state its
// record is read in alone (record.Correction.Alone): committed and
// unresolved, its times the record's, naming a delete's data, a replace
// or an edit published with what its publish writes (publishReplace,
// publishEdit); or abandoned, a delete naming nothing.
func (k Sending) moveAdopted(ctx context.Context, tx SendingTx, rec corr.Correction, storage string, now time.Time) error {
	state, unresolved := rec.Alone()
	if rec.Kind == corr.KindReplace && state == corr.StateCommitted {
		if err := k.publishMovedReplace(ctx, tx, rec.ID, storage, now); err != nil {
			return err
		}
	}
	if rec.Kind == corr.KindEdit && state == corr.StateCommitted {
		if err := k.publishMovedEdit(ctx, tx, rec); err != nil {
			return err
		}
	}
	var committed, purgeAfter, endedAt any
	if rec.State.Published() || rec.State == corr.StateUndone {
		committed, purgeAfter = k.D.Time(rec.CommittedAt), k.D.Time(rec.PurgeAfter)
	} else if state == corr.StateCommitted {
		committed, purgeAfter = k.D.Time(rec.Created), k.D.Time(rec.Created)
	}
	if ended(state) {
		endedAt = k.D.Time(now)
	}
	if _, err := tx.Exec(ctx, `UPDATE corrections SET state = ?, records_state = ?, committed_at = ?, purge_after = ?, ended_at = ?,
		state_reason = ?, unresolved = ?, restore_line = NULL WHERE id = ?`, string(state), string(rec.State), committed, purgeAfter,
		endedAt, tenant.StateReasonFromRecord, k.D.Bool(unresolved), rec.ID); err != nil {
		return err
	}
	if rec.Kind == corr.KindDelete && state != corr.StateCommitted {
		_, err := tx.Exec(ctx, `DELETE FROM deleted_targets WHERE correction_id = ?`, rec.ID)
		return err
	}
	return k.adoptScans(ctx, tx, rec, storage, state)
}

// publishMovedReplace writes, for a replace the backup holds prepared that
// a restore moves to committed, what its publish writes outside the
// catalog (publishReplace), from what its prepare recorded: the
// superseded revision it owns, which its purge removes; the revision in
// the instance's history; its quarantined file released. The backup holds
// no catalog: the restore's rebuild reads the new revision from the
// storage's manifest.
func (k Sending) publishMovedReplace(ctx context.Context, tx SendingTx, id, storage string, now time.Time) error {
	c, err := k.correctionIn(ctx, tx, id, "")
	if err != nil {
		return err
	}
	p, uid := c.Target.Path(), c.Target.Instance
	raw := c.InstanceKey
	if plain, err := layout.InstanceKey(p, uid); err == nil && plain == raw {
		raw = ""
	}
	if err := k.ownSuperseded(ctx, tx, id, storage, p, uid, c.InstanceKey, raw, c.InstanceSize, c.OldSHA256, c.OldRevision); err != nil {
		return err
	}
	if _, err := ClearDamageInTx(ctx, tx, uid); err != nil {
		return err
	}
	q, found, err := heldQuarantine(ctx, tx, c.Quarantine)
	if err != nil || !found {
		return err
	}
	return k.releaseOverwritten(ctx, tx, c.Quarantine, q, now)
}

// OwnAdoptedReplace implements tenant.Restoring: a replace made committed
// from its record alone gets what its publish writes outside the catalog
// (ownSuperseded, releaseOverwritten), from the record: the superseded
// revision's key from the scan's labels, the UID and the old revision, its
// checksum the record's, its size the storage's (sizeOf; a revision gone
// from the storage owns nothing); the quarantined file the one still held
// of that UID and the new revision's checksum, when the backup holds it.
func (k Sending) OwnAdoptedReplace(ctx context.Context, rec corr.Correction, storage string,
	sizeOf func(ctx context.Context, key string) (int64, error), now time.Time) error {
	if rec.Kind != corr.KindReplace || rec.Replace == nil {
		return nil
	}
	if state, _ := rec.Alone(); state != corr.StateCommitted {
		return nil
	}
	s := rec.Scans[0]
	p := label.Path{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
	uid := rec.Replace.SOPInstanceUID
	plain, err := layout.InstanceKey(p, uid)
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "a replace's instance key cannot be made")
	}
	key, raw := plain, ""
	if rec.Replace.OldRevision > 1 {
		if key, err = layout.RevisionKey(p, uid, rec.Replace.OldRevision); err != nil {
			return fault.Wrap(fault.Invalid, err, "a replace's revision key cannot be made")
		}
		raw = key
	}
	var old, sum tenant.SHA256
	if b, err := hex.DecodeString(rec.Replace.OldSHA256); err == nil && len(b) == len(old) {
		copy(old[:], b)
	}
	if b, err := hex.DecodeString(rec.Replace.NewSHA256); err == nil && len(b) == len(sum) {
		copy(sum[:], b)
	}
	size, err := sizeOf(ctx, key)
	if fault.KindOf(err) == fault.NotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return k.Host.Write(ctx, "the replace cannot be given what its publish writes", func(tx SendingTx) error {
		if err := k.ownSuperseded(ctx, tx, rec.ID, storage, p, uid, key, raw, size, old, rec.Replace.OldRevision); err != nil {
			return err
		}
		if _, err := ClearDamageInTx(ctx, tx, uid); err != nil {
			return err
		}
		var qid string
		found, err := tx.QueryRow(ctx, `SELECT id FROM quarantine WHERE state = 'held' AND sop_instance_uid = ? AND sha256 = ? ORDER BY id`,
			[]any{uid, sum[:]}, &qid)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE corrections SET instance_key = ?, instance_size = ?, quarantine_id = ? WHERE id = ?`,
			key, size, qid, rec.ID); err != nil {
			return err
		}
		if !found {
			return nil
		}
		q, _, err := heldQuarantine(ctx, tx, qid)
		if err != nil {
			return err
		}
		return k.releaseOverwritten(ctx, tx, qid, q, now)
	})
}

// HoldCommittedDeletes implements tenant.Restoring.
func (k Sending) HoldCommittedDeletes(ctx context.Context) (int64, error) {
	var n int64
	err := k.Host.Write(ctx, "the deletes a restore keeps cannot be held", func(tx SendingTx) error {
		var err error
		n, err = tx.Exec(ctx, `UPDATE corrections SET unresolved = ?, state_reason = ? WHERE kind = 'delete' AND state = 'committed'
			AND unresolved = ?`, k.D.Bool(true), tenant.StateReasonRestoreHeld, k.D.Bool(false))
		return err
	})
	return n, err
}

// unhiddenDeletes selects the deletes committed that hide no row: those a
// restore made committed from their records, whose data the backup's
// catalog-less load hid nowhere.
const unhiddenDeletes = `SELECT id FROM corrections AS c WHERE kind = 'delete' AND state = 'committed'
	AND NOT EXISTS (SELECT 1 FROM hidden_scans AS h WHERE h.correction_id = c.id)
	AND NOT EXISTS (SELECT 1 FROM hidden_instances AS h WHERE h.correction_id = c.id) ORDER BY id`

// UnhiddenDeletes implements tenant.Restoring.
func (k Sending) UnhiddenDeletes(ctx context.Context) ([]string, error) {
	var out []string
	err := k.Host.Read(ctx, "the deletes that hide nothing cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, unhiddenDeletes, nil, func(r Row) error {
			var id string
			err := r.Scan(&id)
			out = append(out, id)
			return err
		})
	})
	return out, err
}

// HideDeletes implements tenant.Restoring: each delete hides what the
// rebuilt catalog holds of its target, as its publish does (hideTarget),
// owning its scans' manifest and metadata as their snapshots say, so an
// undo shows it again from what was hidden and its purge removes what a
// purge of a delete removes. A target the catalog does not hold hides
// nothing.
func (k Sending) HideDeletes(ctx context.Context, deletes []tenant.DeleteToHide) (tenant.Hidden, error) {
	var out tenant.Hidden
	for _, d := range deletes {
		id := d.ID
		if err := tenant.ValidateCorrectionID(id); err != nil {
			return out, err
		}
		err := k.Host.Write(ctx, "a delete cannot hide what the rebuilt catalog holds of it", func(tx SendingTx) error {
			c, err := k.correctionIn(ctx, tx, id, k.D.Lock)
			if err != nil {
				return err
			}
			if c.Kind != corr.KindDelete || c.State != corr.StateCommitted {
				return nil
			}
			r, err := resolveTarget(ctx, tx, c.Target, true)
			if fault.KindOf(err) == fault.NotFound {
				// The rebuild cataloged the delete's data to be hidden here:
				// one that holds what it names under other labels would be
				// left shown, so the restore fails rather than show it.
				q, arg := `SELECT 1 FROM scans AS s JOIN correction_scans AS c ON c.series_uid = s.series_uid WHERE c.correction_id = ?`, any(id)
				if c.Target.Instance != "" {
					q, arg = `SELECT 1 FROM instances WHERE sop_instance_uid = ?`, c.Target.Instance
				}
				held, err := exists(ctx, tx, q, arg)
				if err != nil || !held {
					return err
				}
				return fault.New(fault.Precondition, "the delete "+id+" names data the rebuilt catalog holds under other labels; it cannot be hidden")
			}
			if err != nil {
				return err
			}
			if err := k.hideTarget(ctx, tx, id, r, d.Snapshots); err != nil {
				return err
			}
			// As the delete's publish does: a scan hidden whole leaves the
			// replicas' rows, one an instance delete changed is queued
			// again. The restore's step carries no time; a replica's
			// backfill is recorded done at the clock's.
			if err := k.QueueCorrectionCopies(ctx, tx, id, time.Now().UTC().Truncate(time.Microsecond), nil); err != nil {
				return err
			}
			var whole, part int64
			if _, err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM hidden_scans WHERE correction_id = ?),
				(SELECT count(DISTINCT scan_id) FROM hidden_instances WHERE correction_id = ?
					AND scan_id NOT IN (SELECT id FROM hidden_scans WHERE correction_id = ?))`, []any{id, id, id}, &whole, &part); err != nil {
				return err
			}
			if c.Unresolved {
				out.Unresolved++
			}
			out.Scans += whole + part
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// ended reports whether s is an end of the state machine.
func ended(s corr.State) bool {
	return s == corr.StatePurged || s == corr.StateUndone || s == corr.StateAbandoned
}

// insertAdopted writes the row of a correction known only from its record.
func (k Sending) insertAdopted(ctx context.Context, tx SendingTx, rec corr.Correction, now time.Time) error {
	state, unresolved := rec.Alone()
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
	case corr.KindEdit:
		// An edit of one instance names it in its target, as its prepare
		// does.
		if level == corr.LevelInstance && len(rec.Edits) > 0 {
			instance = rec.Edits[0].SOPInstanceUID
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
		// A delete found prepared: published now by the restore, its purge
		// left to an admin, since it is unresolved.
		committed, purgeAfter = k.D.Time(rec.Created), k.D.Time(rec.Created)
	}
	if ended(state) {
		endedAt = k.D.Time(now)
	}
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
	if rec.Kind == corr.KindEdit {
		return adoptEdits(ctx, tx, rec)
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
