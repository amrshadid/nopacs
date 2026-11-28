package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// An edit's rows (tenant.Edits): each instance it gives a new revision
// (correction_edits), and the catalog values of each scan before it
// (correction_edit_scans), which its undo gives back.

// editTargetSQL selects the instances of a target's scans, scope appended.
const editTargetSQL = `SELECT s.storage, s.subject_label, s.session_label, s.label, s.study_uid, s.series_uid, s.receiver,
	i.sop_instance_uid, i.storage_key, i.size, i.sha256, i.archived_sha256, coalesce(pv.name, ''), coalesce(pv.version, 0)
	FROM instances AS i JOIN scans AS s ON s.id = i.scan_id LEFT JOIN profile_versions AS pv ON pv.id = i.profile_version_id
	WHERE `

// EditTargets implements tenant.Edits.
func (k Sending) EditTargets(ctx context.Context, t tenant.CorrectionTarget, limit int) ([]tenant.EditTarget, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxEditInstances+1 {
		return nil, fault.New(fault.Invalid, "a limit of an edit's instances is 1 to one past the most an edit changes")
	}
	var out []tenant.EditTarget
	err := k.Host.Read(ctx, "the edit's instances cannot be read", func(tx SendingTx) error {
		r, err := resolveTarget(ctx, tx, t, false)
		if err != nil {
			return err
		}
		out, err = editTargetsIn(ctx, tx, t, r, limit)
		return err
	})
	return out, err
}

// editTargetsIn reads at most limit instances of r's scans (one, for an
// instance's target), in order of storage, series and SOP Instance UID.
func editTargetsIn(ctx context.Context, tx SendingTx, t tenant.CorrectionTarget, r targetRows, limit int) ([]tenant.EditTarget, error) {
	q, args := editTargetSQL+r.scope, []any{r.scopeArg}
	if t.Instance != "" {
		q, args = q+` AND i.sop_instance_uid = ?`, append(args, t.Instance)
	}
	q += ` ORDER BY s.storage, s.series_uid, i.sop_instance_uid LIMIT ?`
	args = append(args, limit)
	var out []tenant.EditTarget
	// #nosec G202 -- the scope is a constant of resolveTarget; values are parameters.
	err := tx.Query(ctx, q, args, func(row Row) error {
		var e tenant.EditTarget
		var raw string
		var received, archived []byte
		if err := row.Scan(&e.Storage, &e.Path.Subject, &e.Path.Session, &e.Path.Scan, &e.StudyUID, &e.SeriesUID, &e.Receiver, &e.UID,
			&raw, &e.Size, &received, &archived, &e.Profile.Name, &e.Profile.Version); err != nil {
			return err
		}
		e.SHA256 = ToSHA256(archived)
		if e.SHA256.IsZero() {
			e.SHA256 = ToSHA256(received)
		}
		e.Revision = tenant.RevisionOfKey(raw)
		key, err := instanceKey(raw, e.Path, e.UID)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "an instance's key cannot be made")
		}
		e.Key = key
		out = append(out, e)
		return nil
	})
	return out, err
}

// prepareEdit records an edit: its target held by no hold and no other
// correction but a committed move (checkEditOverlap), no reception of it
// open, and every instance of it the one the edit read (its revision and
// checksum), none left out.
func (k Sending) prepareEdit(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	issued, ok := tenant.ConfirmIssued(p.Confirm)
	if !ok || p.Now.Sub(issued) > tenant.MaxConfirmAge || issued.After(p.Now.Add(time.Minute)) {
		return tenant.Precondition("the confirm token is not one of an edit's preview made within the hour")
	}
	if err := checkEditOverlap(ctx, tx, p.Target.Path(), p.ID); err != nil {
		return err
	}
	holds, err := activeHolds(ctx, tx)
	if err != nil {
		return err
	}
	if id := holds.coveringPath(p.Target.Path()); id != "" {
		return fault.Wrap(fault.Precondition, &tenant.HeldRefusal{Hold: id}, "a hold covers the data the edit would change (HELD)")
	}
	r, err := resolveTarget(ctx, tx, p.Target, false)
	if err != nil {
		return err
	}
	if err := k.checkReceptions(ctx, tx, r, p.Target.Subject); err != nil {
		return err
	}
	now, err := editTargetsIn(ctx, tx, p.Target, r, tenant.MaxEditInstances+1)
	if err != nil {
		return err
	}
	if len(now) != len(p.Edits) {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the target holds other instances than the edit read")
	}
	byUID := make(map[string]tenant.EditTarget, len(now))
	for _, e := range now {
		byUID[e.UID] = e
	}
	var bytes int64
	scans := map[string]bool{}
	for _, e := range p.Edits {
		cur, ok := byUID[e.UID]
		if !ok || cur.SeriesUID != e.SeriesUID || cur.Revision != e.OldRevision || cur.SHA256 != e.OldSHA256 {
			return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "an instance changed since the edit read it")
		}
		if cur.Revision >= layout.MaxRevision {
			return tenant.Precondition("an instance of the edit has reached its last revision")
		}
		bytes += cur.Size
		scans[cur.SeriesUID] = true
	}
	next := make(map[string]int, len(p.Edits))
	for _, e := range p.Edits {
		n, err := nextRevision(ctx, tx, e.UID, e.OldRevision)
		if err != nil {
			return err
		}
		next[e.UID] = n
	}
	kept := r.scans[:0:0]
	for _, s := range r.scans {
		if scans[s.series] {
			kept = append(kept, s)
		}
	}
	r.scans = kept
	c := tenant.CorrectionCounts{Scans: int64(len(kept)), Instances: int64(len(p.Edits)), Bytes: bytes}
	if err := k.insertCorrection(ctx, tx, p, p.Target.Level(), r, c, samePath, nil); err != nil {
		return err
	}
	for _, e := range p.Edits {
		cur := byUID[e.UID]
		if _, err := tx.Exec(ctx, `INSERT INTO correction_edits (correction_id, sop_instance_uid, series_uid, old_revision, new_revision,
			old_sha256, new_sha256, old_key, old_size, new_size, restore_line) VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, 0, ?)`,
			p.ID, e.UID, e.SeriesUID, e.OldRevision, next[e.UID], e.OldSHA256[:], cur.Key, cur.Size, e.RestoreLine); err != nil {
			return err
		}
	}
	for _, s := range kept {
		if _, err := tx.Exec(ctx, `INSERT INTO correction_edit_scans (correction_id, series_uid, modality, series_description, series_number)
			SELECT ?, series_uid, modality, series_description, series_number FROM scans WHERE id = ?`, p.ID, s.id); err != nil {
			return err
		}
	}
	return nil
}

// nextRevision returns the revision a correction gives an instance now at
// revision cur: one above every revision the tenant ever numbered for it
// (those superseded or deleted, those an undone edit made and its sweep
// may not have removed yet, and those an edit or a replace that did not
// end wrote), so a revision's key names one file only. An undone edit's
// revision reused would be removed, live, by that edit's late sweep.
func nextRevision(ctx context.Context, tx SendingTx, uid string, cur int) (int, error) {
	var top *int64
	if _, err := tx.QueryRow(ctx, `SELECT max(r) FROM (
		SELECT max(revision) AS r FROM instance_revisions WHERE sop_instance_uid = ?
		UNION ALL SELECT max(new_revision) AS r FROM correction_edits WHERE sop_instance_uid = ?
		UNION ALL SELECT max(new_revision) AS r FROM corrections WHERE kind = 'replace' AND instance_uid = ?) AS numbered`,
		[]any{uid, uid, uid}, &top); err != nil {
		return 0, err
	}
	n := cur
	if top != nil && int(*top) > n {
		n = int(*top)
	}
	if n+1 > layout.MaxRevision {
		return 0, tenant.Precondition("an instance has reached its last revision")
	}
	return n + 1, nil
}

// CorrectionEdits implements tenant.Edits.
func (k Sending) CorrectionEdits(ctx context.Context, id string) ([]tenant.EditedInstance, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	var out []tenant.EditedInstance
	err := k.Host.Read(ctx, "the edit's instances cannot be read", func(tx SendingTx) error {
		var err error
		out, err = correctionEditsIn(ctx, tx, id)
		return err
	})
	return out, err
}

// correctionEditsIn reads an edit's instances, in order of storage, series
// and SOP Instance UID.
func correctionEditsIn(ctx context.Context, tx SendingTx, id string) ([]tenant.EditedInstance, error) {
	var out []tenant.EditedInstance
	err := tx.Query(ctx, `SELECT e.sop_instance_uid, e.series_uid, e.old_revision, e.new_revision, e.old_sha256, e.new_sha256, e.old_key,
		e.old_size, e.new_size, e.restore_line, e.new_line, cs.storage, cs.subject_label, cs.session_label, cs.scan_label,
		coalesce(v.modality, ''), coalesce(v.series_description, ''), v.series_number,
		coalesce(s.modality, ''), coalesce(s.series_description, ''), s.series_number
		FROM correction_edits AS e JOIN correction_scans AS cs ON cs.correction_id = e.correction_id AND cs.series_uid = e.series_uid
		LEFT JOIN correction_edit_scans AS v ON v.correction_id = e.correction_id AND v.series_uid = e.series_uid
		LEFT JOIN scans AS s ON s.series_uid = e.series_uid
		WHERE e.correction_id = ? ORDER BY cs.storage, e.series_uid, e.sop_instance_uid`, []any{id}, func(r Row) error {
		var e tenant.EditedInstance
		var oldSum, newSum []byte
		var number, now *int64
		if err := r.Scan(&e.UID, &e.SeriesUID, &e.OldRevision, &e.NewRevision, &oldSum, &newSum, &e.OldKey, &e.OldSize, &e.NewSize,
			&e.RestoreLine, &e.NewLine, &e.Storage, &e.Path.Subject, &e.Path.Session, &e.Path.Scan, &e.Before.Modality,
			&e.Before.SeriesDescription, &number, &e.After.Modality, &e.After.SeriesDescription, &now); err != nil {
			return err
		}
		if number != nil {
			e.Before.SeriesNumber, e.Before.HasSeriesNumber = int32(*number), true //nolint:gosec // G115: a column of 32 bits.
		}
		if now != nil {
			e.After.SeriesNumber, e.After.HasSeriesNumber = int32(*now), true //nolint:gosec // G115: a column of 32 bits.
		}
		e.OldSHA256, e.NewSHA256 = ToSHA256(oldSum), ToSHA256(newSum)
		out = append(out, e)
		return nil
	})
	return out, err
}

// editRow is one instance of an edit with the catalog's row of it now.
type editRow struct {
	tenant.EditedInstance
	scanID, sessionID int64
	reception         string
	key               string
	received          tenant.SHA256
	archived          tenant.SHA256
	size              int64
}

// editRowsIn reads an edit's instances with the catalog's rows of them now;
// an instance the catalog no longer holds is the target changed.
func editRowsIn(ctx context.Context, tx SendingTx, id string) ([]editRow, error) {
	edits, err := correctionEditsIn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	out := make([]editRow, 0, len(edits))
	for _, e := range edits {
		row := editRow{EditedInstance: e}
		var received, archived []byte
		found, err := tx.QueryRow(ctx, `SELECT i.scan_id, s.session_id, i.reception_id, i.storage_key, i.sha256, i.archived_sha256, i.size
			FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE s.series_uid = ? AND i.sop_instance_uid = ?`,
			[]any{e.SeriesUID, e.UID}, &row.scanID, &row.sessionID, &row.reception, &row.key, &received, &archived, &row.size)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "an instance of the edit is no longer held")
		}
		row.received, row.archived = ToSHA256(received), ToSHA256(archived)
		if row.archived.IsZero() {
			row.archived = row.received
		}
		out = append(out, row)
	}
	return out, nil
}

// setInstanceBytes points an instance's row at a revision: its key (raw,
// "" for the layout's), its archived checksum (none kept when it is the
// received one) and size, and moves its reception's, scan's and storage's
// bytes by the change.
func (k Sending) setInstanceBytes(ctx context.Context, tx SendingTx, row editRow, storage, raw string, sum tenant.SHA256, size int64) error {
	var archived any
	if sum != row.received {
		archived = sum[:]
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET storage_key = ?, archived_sha256 = ?, size = ?, byte_offset = 0, byte_length = ?
		WHERE scan_id = ? AND sop_instance_uid = ?`, raw, archived, size, size, row.scanID, row.UID); err != nil {
		return err
	}
	delta := size - row.size
	if _, err := tx.Exec(ctx, `UPDATE reception_scans SET bytes = bytes + ? WHERE reception_id = ? AND scan_id = ?`, delta, row.reception,
		row.scanID); err != nil {
		return err
	}
	return moveStorageBytes(ctx, tx, storage, delta)
}

// publishEdit publishes an edit: each instance at its new revision, the
// revision it supersedes kept and owned by the edit's purge, each scan's
// manifest digest and the catalog values the edit set, the counts moved.
func (k Sending) publishEdit(ctx context.Context, tx SendingTx, c tenant.Correction, cc tenant.CorrectionCommit) error {
	if err := checkUnchanged(ctx, tx, c); err != nil {
		return err
	}
	rows, err := editRowsIn(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	written := make(map[string]tenant.EditWritten, len(cc.Edits))
	for _, w := range cc.Edits {
		written[w.UID] = w
	}
	if len(written) != len(rows) {
		return fault.New(fault.Internal, "an edit's publish names the new revision of each of its instances")
	}
	for _, row := range rows {
		w, ok := written[row.UID]
		if !ok || w.SHA256.IsZero() || w.Size <= 0 {
			return fault.New(fault.Internal, "an edit's publish names the new revision of each of its instances")
		}
		if tenant.RevisionOfKey(row.key) != row.OldRevision || row.archived != row.OldSHA256 {
			return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "an instance changed while the edit was prepared")
		}
		key, err := layout.RevisionKey(row.Path, row.UID, row.NewRevision)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
		}
		if err := k.ownSuperseded(ctx, tx, c.ID, row.Storage, row.Path, row.UID, row.OldKey, row.key, row.OldSize, row.OldSHA256,
			row.OldRevision); err != nil {
			return err
		}
		if err := k.setInstanceBytes(ctx, tx, row, row.Storage, key, w.SHA256, w.Size); err != nil {
			return err
		}
		var line any
		if len(w.Line) > 0 {
			line = w.Line
		}
		if _, err := tx.Exec(ctx, `UPDATE correction_edits SET new_sha256 = ?, new_size = ?, new_line = ? WHERE correction_id = ? AND sop_instance_uid = ?`,
			w.SHA256[:], w.Size, line, c.ID, row.UID); err != nil {
			return err
		}
	}
	for series, v := range cc.Values {
		if err := setEditValues(ctx, tx, series, v); err != nil {
			return err
		}
	}
	for series, row := range cc.Search {
		if err := ApplyEditSearch(ctx, tx, c.ID, series, row); err != nil {
			return err
		}
	}
	return k.afterEditRows(ctx, tx, c, rows, cc.Manifests)
}

// setEditValues gives a scan the catalog values an edit names.
func setEditValues(ctx context.Context, tx SendingTx, series string, v tenant.EditValues) error {
	if v.SetDescription {
		if _, err := tx.Exec(ctx, `UPDATE scans SET series_description = ?, description_folded = ? WHERE series_uid = ?`, v.SeriesDescription,
			tenant.FoldDescription(v.SeriesDescription), series); err != nil {
			return err
		}
	}
	if v.SetModality {
		if _, err := tx.Exec(ctx, `UPDATE scans SET modality = ? WHERE series_uid = ?`, v.Modality, series); err != nil {
			return err
		}
	}
	if v.SetSeriesNumber {
		var n any
		if v.HasSeriesNumber {
			n = v.SeriesNumber
		}
		if _, err := tx.Exec(ctx, `UPDATE scans SET series_number = ? WHERE series_uid = ?`, n, series); err != nil {
			return err
		}
	}
	return nil
}

// afterEditRows gives each scan of an edit's rows the manifest digest
// given and its counts from its instances, then its sessions and subject
// theirs (their modalities among them).
func (k Sending) afterEditRows(ctx context.Context, tx SendingTx, c tenant.Correction, rows []editRow, manifests map[string]tenant.SHA256) error {
	scans, sessions := map[int64]string{}, map[int64]bool{}
	for _, row := range rows {
		scans[row.scanID] = row.SeriesUID
		sessions[row.sessionID] = true
	}
	for id, series := range scans {
		if m, ok := manifests[series]; ok && !m.IsZero() {
			if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, m[:], id); err != nil {
				return err
			}
		}
		if err := recountScan(ctx, tx, id); err != nil {
			return err
		}
	}
	for id := range sessions {
		if err := recountSession(ctx, tx, id); err != nil {
			return err
		}
	}
	var subject int64
	found, err := tx.QueryRow(ctx, `SELECT id FROM subjects WHERE folded = ? AND label = ?`, []any{label.Fold(c.Target.Subject),
		c.Target.Subject}, &subject)
	if err != nil || !found {
		return err
	}
	return recountSubject(ctx, tx, subject)
}

// checkEditMayUndo refuses a step of an edit's undo while a rebuild runs,
// a repair holds one of its scans, a switch of one of its storages runs, or
// an active hold covers it: the undo writes the scans' manifests and
// metadata again and removes the edit's new revisions.
func (k Sending) checkEditMayUndo(ctx context.Context, tx SendingTx, c tenant.Correction, now time.Time) error {
	if c.Kind != corr.KindEdit {
		return tenant.Precondition("the correction is not an edit")
	}
	if err := k.checkNoRebuild(ctx, tx); err != nil {
		return err
	}
	if err := k.checkCorrectionNotHeld(ctx, tx, c.ID, now); err != nil {
		return err
	}
	if err := checkNoSwitch(ctx, tx, c.ID); err != nil {
		return err
	}
	held, err := correctionHeldIn(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	if held != "" {
		return fault.Wrap(fault.Precondition, &tenant.HeldRefusal{Hold: held}, "a hold covers the revisions the undo would remove (HELD)")
	}
	return nil
}
