package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The steps of a correction after its prepare: each one transaction that
// reads the correction's row locked, checks that its state allows the
// step, and answers a correction already at the step's end as it stands.

// step runs fn on the correction id, read locked, in one write transaction
// that locks the configuration first, and answers the correction as it is
// after.
func (k Sending) step(ctx context.Context, id, message string, fn func(tx SendingTx, c *tenant.Correction) error) (tenant.Correction, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return tenant.Correction{}, err
	}
	var out tenant.Correction
	err := k.Host.Write(ctx, message, func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		c, err := k.correctionIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if err := fn(tx, &c); err != nil {
			return err
		}
		out, err = k.correctionIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// stepRow is step for a change that answers nothing: the row is read
// locked without what fillCorrection adds (every object its purge
// recorded), so a step taken once per object of a purge of fifty thousand
// costs at its last object what it cost at its first.
func (k Sending) stepRow(ctx context.Context, id, message string, fn func(tx SendingTx, c *tenant.Correction) error) error {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return err
	}
	return k.Host.Write(ctx, message, func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		var c tenant.Correction
		found := false
		if err := tx.Query(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE id = ?`+k.D.Lock, []any{id}, func(r Row) error {
			var err error
			c, err = scanCorrection(r)
			found = true
			return err
		}); err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no correction with this id")
		}
		return fn(tx, &c)
	})
}

// setState moves a correction to state, with its end time when it is an
// end.
func (k Sending) setState(ctx context.Context, tx SendingTx, c *tenant.Correction, state corr.State, now time.Time, why string) error {
	var ended any
	if state == corr.StatePurged || state == corr.StateUndone || state == corr.StateAbandoned {
		ended = k.D.Time(now)
	}
	// The line a prepared replace could write back is kept while it is
	// prepared only.
	if _, err := tx.Exec(ctx, `UPDATE corrections SET state = ?, ended_at = ?, state_reason = ?, restore_line = NULL WHERE id = ?`,
		string(state), ended, why, c.ID); err != nil {
		return err
	}
	c.State = state
	return nil
}

// changedEvent writes the EventCatalogChanged of c reaching its state.
func changedEvent(ctx context.Context, tx SendingTx, c tenant.Correction, now time.Time) error {
	e, err := tenant.CatalogChangedEvent(c, now)
	if err != nil {
		return err
	}
	return tx.WriteEvents(ctx, e)
}

// CommitCorrection implements tenant.Corrections.
func (k Sending) CommitCorrection(ctx context.Context, cc tenant.CorrectionCommit) (tenant.Correction, error) {
	if err := master.ValidateTime("commit time", cc.Now); err != nil {
		return tenant.Correction{}, err
	}
	if err := tenant.ValidateGrace(cc.Grace); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, cc.ID, "the correction cannot be published", func(tx SendingTx, c *tenant.Correction) error {
		if c.State.Published() {
			return nil
		}
		if c.State != corr.StatePrepared {
			return tenant.Precondition("the correction is " + string(c.State) + ", not prepared")
		}
		var err error
		switch c.Kind {
		case corr.KindDelete:
			var r targetRows
			if r, err = resolveTarget(ctx, tx, c.Target, true); err == nil {
				err = k.hideTarget(ctx, tx, c.ID, r, cc.Snapshots)
			}
		case corr.KindRelabel:
			err = k.switchLabels(ctx, tx, *c, cc.Manifests, cc.Snapshots)
		case corr.KindReplace:
			err = k.publishReplace(ctx, tx, *c, cc)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE corrections SET committed_at = ?, purge_after = ? WHERE id = ?`, k.D.Time(cc.Now),
			k.D.Time(cc.Now.Add(cc.Grace)), c.ID); err != nil {
			return err
		}
		if err := k.setState(ctx, tx, c, corr.StateCommitted, cc.Now, ""); err != nil {
			return err
		}
		return changedEvent(ctx, tx, *c, cc.Now)
	})
}

// checkUnchanged refuses the publish of a relabel or replace whose scans
// hold other instances than when it was prepared: what it copied or wrote
// would miss them.
func checkUnchanged(ctx context.Context, tx SendingTx, c tenant.Correction) error {
	changed, err := exists(ctx, tx, `SELECT 1 FROM correction_scans AS cs LEFT JOIN scans AS s ON s.series_uid = cs.series_uid
		WHERE cs.correction_id = ? AND (s.id IS NULL OR s.instances <> cs.instances)`, c.ID)
	if err != nil {
		return err
	}
	if changed {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the target's scans changed while the correction was prepared")
	}
	return nil
}

// switchLabels publishes a relabel: every label of the target at once, and
// the keys the catalog holds whole under the old labels. What it owns (the
// old tree, as the catalog and the snapshots name it) is fixed first.
func (k Sending) switchLabels(ctx context.Context, tx SendingTx, c tenant.Correction, manifests map[string]tenant.SHA256,
	snapshots map[string]tenant.ScanSnapshot) error {
	if err := checkUnchanged(ctx, tx, c); err != nil {
		return err
	}
	r, err := resolveTarget(ctx, tx, c.Target, false)
	if err != nil {
		return err
	}
	var copied int64
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM correction_scans WHERE correction_id = ?`, []any{c.ID}, &copied); err != nil {
		return err
	}
	if copied != int64(len(r.scans)) {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "a scan was added to the target while the relabel copied it")
	}
	level, to, folded := c.Level, c.ToLabel, label.Fold(c.ToLabel)
	if err := checkLabelFree(ctx, tx, r, level, to); err != nil {
		return err
	}
	if err := ownInstances(ctx, tx, c.ID, r); err != nil {
		return err
	}
	if err := ownSnapshots(ctx, tx, c.ID, r, snapshots); err != nil {
		return err
	}
	var stmts []struct {
		q    string
		args []any
	}
	add := func(q string, args ...any) {
		stmts = append(stmts, struct {
			q    string
			args []any
		}{q, args})
	}
	switch level {
	case corr.LevelSubject:
		add(`UPDATE subjects SET label = ?, folded = ? WHERE id = ?`, to, folded, r.subjectID)
		add(`UPDATE sessions SET subject_label = ?, subject_folded = ? WHERE subject_id = ?`, to, folded, r.subjectID)
		add(`UPDATE scans SET subject_label = ?, subject_folded = ? WHERE session_id IN (SELECT id FROM sessions WHERE subject_id = ?)`,
			to, folded, r.subjectID)
	case corr.LevelSession:
		add(`UPDATE sessions SET label = ?, folded = ? WHERE id = ?`, to, folded, r.sessionID)
		add(`UPDATE scans SET session_label = ?, session_folded = ? WHERE session_id = ?`, to, folded, r.sessionID)
	default:
		add(`UPDATE scans SET label = ?, folded = ? WHERE id = ?`, to, folded, r.scans[0].id)
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.q, s.args...); err != nil {
			return err
		}
	}
	for _, s := range r.scans {
		if m, ok := manifests[s.series]; ok && !m.IsZero() {
			if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, m[:], s.id); err != nil {
				return err
			}
		}
		if err := rekeyRevisions(ctx, tx, s.id, relabeledPath(s.path, level, to)); err != nil {
			return err
		}
	}
	return nil
}

// rekeyRevisions gives the instances of a scan whose key the catalog holds
// whole (a revision past the first) the same key under the scan's new
// labels.
func rekeyRevisions(ctx context.Context, tx SendingTx, scanID int64, to label.Path) error {
	type row struct{ uid, key string }
	var rows []row
	err := tx.Query(ctx, `SELECT sop_instance_uid, storage_key FROM instances WHERE scan_id = ? AND storage_key <> ''`, []any{scanID},
		func(r Row) error {
			var x row
			err := r.Scan(&x.uid, &x.key)
			rows = append(rows, x)
			return err
		})
	if err != nil {
		return err
	}
	for _, x := range rows {
		lk, err := layout.Parse(x.key)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "an instance's key is not one of the layout")
		}
		lk.Path = to
		key, err := lk.String()
		if err != nil {
			return fault.Wrap(fault.Internal, err, "an instance's key cannot be made")
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET storage_key = ? WHERE scan_id = ? AND sop_instance_uid = ?`, key, scanID, x.uid); err != nil {
			return err
		}
	}
	return nil
}

// publishReplace publishes a replace: the instance's next revision in use,
// the revision it supersedes kept, its damage mark cleared, the quarantined
// file released.
func (k Sending) publishReplace(ctx context.Context, tx SendingTx, c tenant.Correction, cc tenant.CorrectionCommit) error {
	if err := checkUnchanged(ctx, tx, c); err != nil {
		return err
	}
	r, err := resolveTarget(ctx, tx, c.Target, false)
	if err != nil {
		return err
	}
	in, s := r.instance, r.scans[0]
	if in.revision != c.OldRevision || in.archived != c.OldSHA256 {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the instance changed while the replace was prepared")
	}
	key, err := layout.RevisionKey(s.path, in.uid, c.NewRevision)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
	}
	var reception string
	var qsize int64
	var kept bool
	found, err := tx.QueryRow(ctx, `SELECT reception_id, size, content_kept FROM quarantine WHERE id = ? AND state = 'held'`,
		[]any{c.Quarantine}, &reception, &qsize, &kept)
	if err != nil {
		return err
	}
	if !found {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the quarantined file is no longer held")
	}
	var archived any
	if c.NewSHA256 != c.ReceivedSHA256 {
		archived = c.NewSHA256[:]
	}
	// The revision it supersedes is the one object it owns.
	if _, err := tx.Exec(ctx, `INSERT INTO correction_keys (correction_id, ord, storage, subject_label, session_label, scan_label,
		sop_instance_uid, storage_key, size, sha256, outcome) VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, '') ON CONFLICT DO NOTHING`,
		c.ID, s.storage, s.path.Subject, s.path.Session, s.path.Scan, in.uid, in.raw, in.size, in.archived[:]); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO instance_revisions (sop_instance_uid, revision, sha256, size, storage, storage_key,
		correction_id, fate, purged) VALUES (?, ?, ?, ?, ?, ?, ?, 'superseded', ?) ON CONFLICT (sop_instance_uid, revision)
		DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, storage = excluded.storage, storage_key = excluded.storage_key,
		correction_id = excluded.correction_id, fate = excluded.fate, purged = excluded.purged`,
		in.uid, in.revision, in.archived[:], in.size, s.storage, in.key, c.ID, k.D.Bool(false)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET storage_key = ?, sha256 = ?, archived_sha256 = ?, size = ?, byte_offset = 0,
		byte_length = ?, reception_id = ? WHERE scan_id = ? AND sop_instance_uid = ?`, key, c.ReceivedSHA256[:], archived, c.NewSize,
		c.NewSize, reception, s.id, in.uid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE reception_scans SET instances = instances - 1, bytes = bytes - ? WHERE reception_id = ?
		AND scan_id = ?`, in.size, in.receptionID, s.id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reception_scans WHERE reception_id = ? AND scan_id = ? AND instances = 0`, in.receptionID,
		s.id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reception_scans (reception_id, scan_id, instances, bytes) VALUES (?, ?, 1, ?)
		ON CONFLICT (reception_id, scan_id) DO UPDATE SET instances = reception_scans.instances + 1,
		bytes = reception_scans.bytes + excluded.bytes`, reception, s.id, c.NewSize); err != nil {
		return err
	}
	if m, ok := cc.Manifests[s.series]; ok && !m.IsZero() {
		if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, m[:], s.id); err != nil {
			return err
		}
	}
	r.instance = &targetInstance{scanID: s.id}
	if err := k.recountAfter(ctx, tx, r); err != nil {
		return err
	}
	if err := moveStorageBytes(ctx, tx, s.storage, c.NewSize-in.size); err != nil {
		return err
	}
	if _, err := ClearDamageInTx(ctx, tx, in.uid); err != nil {
		return err
	}
	held := int64(0)
	if kept {
		held = qsize
	}
	if _, err := tx.Exec(ctx, `UPDATE quarantine SET state = 'released', decided_at = ?, release_mode = 'overwrite' WHERE id = ?`,
		k.D.Time(cc.Now), c.Quarantine); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE quarantine_totals SET files = files - 1, bytes = bytes - ?`, held)
	return err
}

// AbandonCorrection implements tenant.Corrections.
func (k Sending) AbandonCorrection(ctx context.Context, id, why string, now time.Time) (tenant.Correction, error) {
	if err := tenant.ValidateStateReason(why); err != nil {
		return tenant.Correction{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, id, "the correction cannot be abandoned", func(tx SendingTx, c *tenant.Correction) error {
		return k.abandonIn(ctx, tx, c, why, now)
	})
}

// abandonIn abandons a prepared correction, or answers one abandoned.
func (k Sending) abandonIn(ctx context.Context, tx SendingTx, c *tenant.Correction, why string, now time.Time) error {
	if c.State == corr.StateAbandoned {
		return nil
	}
	if c.State != corr.StatePrepared {
		return tenant.Precondition("the correction is " + string(c.State) + ", not prepared")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM deleted_targets WHERE correction_id = ?`, c.ID); err != nil {
		return err
	}
	return k.setState(ctx, tx, c, corr.StateAbandoned, now, why)
}

// UndoCorrection implements tenant.Corrections.
func (k Sending) UndoCorrection(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, error) {
	if err := tenant.ValidateUndoReason(reason); err != nil {
		return tenant.Correction{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Correction{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, id, "the correction cannot be undone", func(tx SendingTx, c *tenant.Correction) error {
		switch {
		case c.State == corr.StateUndone || (c.Kind == corr.KindRelabel && c.State == corr.StateAbandoned):
		case c.Kind == corr.KindRelabel && c.State == corr.StatePrepared:
			if err := k.abandonIn(ctx, tx, c, tenant.StateReasonStopped, now); err != nil {
				return err
			}
		case c.Kind == corr.KindDelete && c.State == corr.StateCommitted:
			if err := k.checkNoRebuild(ctx, tx); err != nil {
				return err
			}
			if err := k.showTarget(ctx, tx, *c); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM deleted_targets WHERE correction_id = ?`, c.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE corrections SET undo_reason = ?, unresolved = ? WHERE id = ?`, reason, k.D.Bool(false),
				c.ID); err != nil {
				return err
			}
			if err := k.setState(ctx, tx, c, corr.StateUndone, now, ""); err != nil {
				return err
			}
			if err := changedEvent(ctx, tx, *c, now); err != nil {
				return err
			}
		default:
			return tenant.Precondition("the correction is neither a committed delete nor a prepared relabel")
		}
		return tx.Audited(ctx, audit, guard)
	})
}

// StartPurge implements tenant.Corrections.
func (k Sending) StartPurge(ctx context.Context, id string, now time.Time, audit *master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Correction{}, err
	}
	if audit != nil {
		if err := audit.Validate(); err != nil {
			return tenant.Correction{}, err
		}
	}
	return k.step(ctx, id, "the correction's purge cannot be started", func(tx SendingTx, c *tenant.Correction) error {
		switch c.State {
		case corr.StatePurging, corr.StatePurged:
		case corr.StateCommitted:
			if audit == nil && (c.Unresolved || c.PurgeAfter.After(now)) {
				return tenant.Precondition("the correction's grace period has not ended")
			}
			if err := k.checkClockIn(ctx, tx, now, audit == nil); err != nil {
				return err
			}
			if audit != nil {
				if err := k.checkNoRebuild(ctx, tx); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE corrections SET unresolved = ? WHERE id = ?`, k.D.Bool(false), c.ID); err != nil {
				return err
			}
			if err := k.setState(ctx, tx, c, corr.StatePurging, now, ""); err != nil {
				return err
			}
		default:
			return tenant.Precondition("the correction is " + string(c.State) + ", not committed")
		}
		if audit == nil {
			return nil
		}
		return tx.Audited(ctx, *audit, guard)
	})
}

// RecordPurgedObject implements tenant.Corrections.
func (k Sending) RecordPurgedObject(ctx context.Context, o tenant.PurgedObject) error {
	if err := o.Validate(); err != nil {
		return err
	}
	err := k.stepRow(ctx, o.ID, "the purged object cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.State == corr.StatePurged {
			// A later pass over a version the storage retained: what it did
			// now replaces the retention, and nothing else is recorded.
			_, err := tx.Exec(ctx, `UPDATE correction_objects SET disposition = ?, retain_until = ?, purged_at = ?
				WHERE correction_id = ? AND storage = ? AND object_key = ? AND version = ? AND disposition = 'retained'`,
				string(o.Disposition), k.nullTime(o.RetainUntil), k.D.Time(o.Now), o.ID, o.Storage, o.Key, o.Version)
			return err
		}
		if c.State != corr.StatePurging {
			return tenant.Precondition("the correction is " + string(c.State) + ", not purging")
		}
		return k.insertPurged(ctx, tx, o)
	})
	return err
}

// RecordCopied implements tenant.Corrections.
func (k Sending) RecordCopied(ctx context.Context, id string, copied int64) error {
	if copied < 0 {
		return fault.New(fault.Invalid, "a count of objects copied is not negative")
	}
	err := k.stepRow(ctx, id, "the relabel's progress cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind != corr.KindRelabel || c.State != corr.StatePrepared {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE corrections SET objects_copied = ? WHERE id = ?`, copied, id)
		return err
	})
	return err
}

// MarkRecords implements tenant.Corrections.
func (k Sending) MarkRecords(ctx context.Context, id string, state corr.State) error {
	if !state.Valid() {
		return fault.New(fault.Invalid, "a correction's state is one of the six")
	}
	err := k.stepRow(ctx, id, "the correction's records cannot be marked", func(tx SendingTx, c *tenant.Correction) error {
		_, err := tx.Exec(ctx, `UPDATE corrections SET records_state = ? WHERE id = ?`, string(state), id)
		return err
	})
	return err
}

// AllowArrivals implements tenant.Corrections.
func (k Sending) AllowArrivals(ctx context.Context, id string, value tenant.HeldValue, reason string, now time.Time,
	audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	if err := tenant.ValidateUndoReason(reason); err != nil {
		return tenant.Correction{}, err
	}
	if err := value.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Correction{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, id, "the delete's arrivals cannot be allowed", func(tx SendingTx, c *tenant.Correction) error {
		switch {
		case c.Kind != corr.KindDelete || c.State == corr.StateUndone || c.State == corr.StateAbandoned:
			return tenant.Precondition("the correction is not a delete that holds")
		case c.State != corr.StatePurged:
			// Until its purge has removed every object it owns, data that
			// arrived again at its labels would sit where the purge works.
			return tenant.Precondition("a delete's arrivals are allowed once it is purged")
		}
		n, err := tx.Exec(ctx, `DELETE FROM deleted_targets WHERE correction_id = ? AND kind = ? AND value = ?`, id, value.Kind, value.Value)
		if err != nil {
			return err
		}
		if n == 0 {
			return tenant.Precondition("the delete does not hold this value")
		}
		left, err := exists(ctx, tx, `SELECT 1 FROM deleted_targets WHERE correction_id = ?`, id)
		if err != nil {
			return err
		}
		if !left {
			if _, err := tx.Exec(ctx, `UPDATE corrections SET arrivals_allowed = ? WHERE id = ?`, k.D.Bool(true), id); err != nil {
				return err
			}
		}
		return tx.Audited(ctx, audit, guard)
	})
}
