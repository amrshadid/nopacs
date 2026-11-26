package sqlkit

import (
	"context"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// How a delete hides the catalog's rows and an undo shows them again: each
// row moved whole between its catalog table and the hidden_ table of the
// same columns, which keeps the correction's id. Every column of the
// catalog table is copied, by the names the database lists for it, so a
// column a later migration adds to both moves with the rest.

// columnList returns the columns of a catalog table, comma separated.
func (k Sending) columnList(ctx context.Context, tx SendingTx, table string) (string, []string, error) {
	cols, err := k.Shadow.Columns(ctx, tx, table)
	if err != nil {
		return "", nil, err
	}
	return strings.Join(cols, ", "), cols, nil
}

// hideRows moves the rows of table that where selects into its hidden_
// table under the correction id, and returns how many it moved. table
// and where are constants of this file.
func (k Sending) hideRows(ctx context.Context, tx SendingTx, id, table, where string, args ...any) (int64, error) {
	list, _, err := k.columnList(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	// #nosec G202 -- table, the columns the database lists and where are constants; values are parameters.
	if _, err := tx.Exec(ctx, `INSERT INTO hidden_`+table+` (correction_id, `+list+`) SELECT ?, `+list+` FROM `+table+
		` WHERE `+where, append([]any{id}, args...)...); err != nil {
		return 0, err
	}
	return tx.Exec(ctx, `DELETE FROM `+table+` WHERE `+where, args...) // #nosec G202 -- as above.
}

// ownInstances records, as objects the correction owns, the object of each
// instance of r's scans as the catalog holds it now, under the scans'
// labels now: one statement, whatever the number of instances.
func ownInstances(ctx context.Context, tx SendingTx, id string, r targetRows) error {
	scope, arg := r.scanScope()
	// #nosec G202 -- the scope is a constant of resolveTarget; values are parameters.
	_, err := tx.Exec(ctx, `INSERT INTO correction_keys (correction_id, ord, storage, subject_label, session_label, scan_label,
		sop_instance_uid, storage_key, size, sha256, outcome)
		SELECT ?, 1, s.storage, s.subject_label, s.session_label, s.label, i.sop_instance_uid, i.storage_key, i.size,
			coalesce(i.archived_sha256, i.sha256), ''
		FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.scan_id IN `+scope+` ON CONFLICT DO NOTHING`, id, arg)
	return err
}

// ownSnapshots records, as objects the correction owns, each scan's
// metadata and manifest under its labels now, with the SHA-256 the caller
// read of them before the publish; one the caller did not read (or that
// was not there) is not the correction's to remove.
func ownSnapshots(ctx context.Context, tx SendingTx, id string, r targetRows, snapshots map[string]tenant.ScanSnapshot) error {
	for _, s := range r.scans {
		snap := snapshots[s.series]
		for _, x := range []struct {
			ord  int
			sum  tenant.SHA256
			size int64
		}{{tenant.TargetMetadata, snap.Metadata, snap.MetadataSize}, {tenant.TargetManifest, snap.Manifest, snap.ManifestSize}} {
			if x.sum.IsZero() {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO correction_keys (correction_id, ord, storage, subject_label, session_label, scan_label,
				sop_instance_uid, storage_key, size, sha256, outcome) VALUES (?, ?, ?, ?, ?, ?, '', '', ?, ?, '') ON CONFLICT DO NOTHING`,
				id, x.ord, s.storage, s.path.Subject, s.path.Session, s.path.Scan, x.size, x.sum[:]); err != nil {
				return err
			}
		}
	}
	return nil
}

// hideTarget moves what a delete of r hides out of the catalog, and moves
// the running totals of what stays; the objects it owns are fixed first.
func (k Sending) hideTarget(ctx context.Context, tx SendingTx, id string, r targetRows, snapshots map[string]tenant.ScanSnapshot) error {
	if r.instance != nil {
		return k.hideInstance(ctx, tx, id, r)
	}
	if err := ownInstances(ctx, tx, id, r); err != nil {
		return err
	}
	if err := ownSnapshots(ctx, tx, id, r, snapshots); err != nil {
		return err
	}
	scope, arg := r.scanScope()
	in := `scan_id IN ` + scope
	instances, err := k.hideRows(ctx, tx, id, "instances", in, arg)
	if err != nil {
		return err
	}
	if _, err := k.hideRows(ctx, tx, id, "reception_scans", in, arg); err != nil {
		return err
	}
	if _, err := k.hideRows(ctx, tx, id, "scans", `id IN `+scope, arg); err != nil {
		return err
	}
	for _, s := range r.sessionsGone {
		if _, err := k.hideRows(ctx, tx, id, "session_modalities", `session_id = ?`, s); err != nil {
			return err
		}
		if _, err := k.hideRows(ctx, tx, id, "sessions", `id = ?`, s); err != nil {
			return err
		}
	}
	subjects := int64(0)
	if r.subjectGone {
		if subjects, err = k.hideRows(ctx, tx, id, "subjects", `id = ?`, r.subjectID); err != nil {
			return err
		}
	}
	if err := k.recountAfter(ctx, tx, r); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_totals SET subjects = subjects - ?, sessions = sessions - ?, scans = scans - ?,
		instances = instances - ?`, subjects, len(r.sessionsGone), len(r.scans), instances); err != nil {
		return err
	}
	for storage, bytes := range bytesByStorage(r) {
		if err := moveStorageBytes(ctx, tx, storage, -bytes); err != nil {
			return err
		}
	}
	return nil
}

// bytesByStorage sums r's scans' bytes by storage.
func bytesByStorage(r targetRows) map[string]int64 {
	out := map[string]int64{}
	for _, s := range r.scans {
		out[s.storage] += s.bytes
	}
	return out
}

// moveStorageBytes adds delta to a storage's running total, which a
// storage holding nothing does not have.
func moveStorageBytes(ctx context.Context, tx SendingTx, storage string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO storage_totals (storage, bytes) VALUES (?, ?) ON CONFLICT (storage)
		DO UPDATE SET bytes = storage_totals.bytes + excluded.bytes`, storage, max(delta, 0)); err != nil {
		return err
	}
	if delta < 0 {
		if _, err := tx.Exec(ctx, `UPDATE storage_totals SET bytes = bytes + ? WHERE storage = ?`, delta, storage); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM storage_totals WHERE storage = ? AND bytes = 0`, storage)
	return err
}

// hideInstance hides one instance of a scan that stays.
func (k Sending) hideInstance(ctx context.Context, tx SendingTx, id string, r targetRows) error {
	in := r.instance
	s := r.scans[0]
	if _, err := tx.Exec(ctx, `INSERT INTO correction_keys (correction_id, ord, storage, subject_label, session_label, scan_label,
		sop_instance_uid, storage_key, size, sha256, outcome) VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, '') ON CONFLICT DO NOTHING`,
		id, s.storage, s.path.Subject, s.path.Session, s.path.Scan, in.uid, in.raw, in.size, in.archived[:]); err != nil {
		return err
	}
	n, err := k.hideRows(ctx, tx, id, "instances", `scan_id = ? AND sop_instance_uid = ?`, in.scanID, in.uid)
	if err != nil {
		return err
	}
	if n != 1 {
		return fault.New(fault.Internal, "the instance a delete names is not held")
	}
	if _, err := tx.Exec(ctx, `UPDATE reception_scans SET instances = instances - 1, bytes = bytes - ? WHERE reception_id = ?
		AND scan_id = ?`, in.size, in.receptionID, in.scanID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reception_scans WHERE reception_id = ? AND scan_id = ? AND instances = 0`,
		in.receptionID, in.scanID); err != nil {
		return err
	}
	if err := k.recountAfter(ctx, tx, r); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_totals SET instances = instances - 1`); err != nil {
		return err
	}
	return moveStorageBytes(ctx, tx, r.scans[0].storage, -in.size)
}

// recountAfter gives the scan of an instance delete, and the sessions and
// subject of r that stay, their counts from the rows beneath them.
func (k Sending) recountAfter(ctx context.Context, tx SendingTx, r targetRows) error {
	if r.instance != nil {
		if err := recountScan(ctx, tx, r.instance.scanID); err != nil {
			return err
		}
	}
	gone := map[int64]bool{}
	for _, s := range r.sessionsGone {
		gone[s] = true
	}
	seen := map[int64]bool{}
	for _, s := range r.scans {
		if gone[s.sessionID] || seen[s.sessionID] {
			continue
		}
		seen[s.sessionID] = true
		if err := recountSession(ctx, tx, s.sessionID); err != nil {
			return err
		}
	}
	if r.subjectGone {
		return nil
	}
	return recountSubject(ctx, tx, r.subjectID)
}

// recountScan gives a scan its counts from its instances.
func recountScan(ctx context.Context, tx SendingTx, id int64) error {
	_, err := tx.Exec(ctx, `UPDATE scans SET instances = (SELECT count(*) FROM instances WHERE scan_id = ?),
		bytes = (SELECT coalesce(sum(size), 0) FROM instances WHERE scan_id = ?) WHERE id = ?`, id, id, id)
	return err
}

// recountSession gives a session its counts, times and modalities from its
// scans, as a rebuild derives them from the rows beneath (its first
// scan's creation, its last scan's update), so a catalog after a
// correction is the one a rebuild makes from the storage.
func recountSession(ctx context.Context, tx SendingTx, id int64) error {
	if _, err := tx.Exec(ctx, `UPDATE sessions SET scan_count = (SELECT count(*) FROM scans WHERE session_id = ?),
		instance_count = (SELECT coalesce(sum(instances), 0) FROM scans WHERE session_id = ?),
		bytes = (SELECT coalesce(sum(bytes), 0) FROM scans WHERE session_id = ?),
		created = (SELECT min(created) FROM scans WHERE session_id = ?),
		updated = (SELECT max(updated) FROM scans WHERE session_id = ?) WHERE id = ?`, id, id, id, id, id, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_modalities WHERE session_id = ?`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO session_modalities (session_id, modality, created)
		SELECT DISTINCT s.session_id, s.modality, se.created FROM scans AS s JOIN sessions AS se ON se.id = s.session_id
		WHERE s.session_id = ? AND s.modality <> ''`, id)
	return err
}

// recountSubject gives a subject its counts and times from its sessions,
// as a rebuild derives them.
func recountSubject(ctx context.Context, tx SendingTx, id int64) error {
	_, err := tx.Exec(ctx, `UPDATE subjects SET session_count = (SELECT count(*) FROM sessions WHERE subject_id = ?),
		instance_count = (SELECT coalesce(sum(instance_count), 0) FROM sessions WHERE subject_id = ?),
		bytes = (SELECT coalesce(sum(bytes), 0) FROM sessions WHERE subject_id = ?),
		created = (SELECT min(created) FROM sessions WHERE subject_id = ?),
		updated = (SELECT max(updated) FROM sessions WHERE subject_id = ?) WHERE id = ?`, id, id, id, id, id, id)
	return err
}

// restoreParent puts back one hidden row of subjects, sessions or scans:
// with its id when no row has taken it since, else with the next id the
// table and the correction's hidden rows have not used, the rows beneath
// it in the hidden tables then pointed at it. It returns the id it has.
func (k Sending) restoreParent(ctx context.Context, tx SendingTx, id, table string, oldID int64, children [][2]string) (int64, error) {
	list, _, err := k.columnList(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	taken, err := exists(ctx, tx, `SELECT 1 FROM `+table+` WHERE id = ?`, oldID) // #nosec G202 -- table is a constant.
	if err != nil {
		return 0, err
	}
	newID := oldID
	if taken {
		if _, err := tx.QueryRow(ctx, `SELECT max(m) + 1 FROM (SELECT max(id) AS m FROM `+table+` UNION ALL SELECT max(id) FROM hidden_`+
			table+` WHERE correction_id = ?) AS ids`, []any{id}, &newID); err != nil { // #nosec G202 -- table is a constant.
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE hidden_`+table+` SET id = ? WHERE correction_id = ? AND id = ?`, newID, id, oldID); err != nil {
			return 0, err
		}
		for _, c := range children {
			// #nosec G202 -- the children's tables and columns are constants.
			if _, err := tx.Exec(ctx, `UPDATE hidden_`+c[0]+` SET `+c[1]+` = ? WHERE correction_id = ? AND `+c[1]+` = ?`,
				newID, id, oldID); err != nil {
				return 0, err
			}
		}
	}
	// #nosec G202 -- table, the columns the database lists and the clause are constants; values are parameters.
	_, err = tx.Exec(ctx, `INSERT INTO `+table+` (`+list+`)`+k.D.Overriding+` SELECT `+list+` FROM hidden_`+table+
		` WHERE correction_id = ? AND id = ?`, id, newID)
	return newID, err
}

// hiddenIDs returns the ids of a hidden table's rows of the correction.
func hiddenIDs(ctx context.Context, tx SendingTx, id, table string) ([]int64, error) {
	var out []int64
	// #nosec G202 -- table is a constant.
	err := tx.Query(ctx, `SELECT id FROM hidden_`+table+` WHERE correction_id = ? ORDER BY id`, []any{id}, func(r Row) error {
		var v int64
		err := r.Scan(&v)
		out = append(out, v)
		return err
	})
	return out, err
}

// checkFree refuses an undo when a row of the catalog has taken a key of
// one the delete hid since: a label at its level, a study, a series or an
// instance's UID.
func checkFree(ctx context.Context, tx SendingTx, id string) error {
	for _, q := range []string{
		`SELECT 1 FROM hidden_subjects AS h JOIN subjects AS l ON l.folded = h.folded WHERE h.correction_id = ?`,
		`SELECT 1 FROM hidden_sessions AS h JOIN sessions AS l ON l.study_uid = h.study_uid WHERE h.correction_id = ?`,
		`SELECT 1 FROM hidden_sessions AS h JOIN sessions AS l ON l.subject_folded = h.subject_folded AND l.folded = h.folded
			WHERE h.correction_id = ?`,
		`SELECT 1 FROM hidden_scans AS h JOIN scans AS l ON l.series_uid = h.series_uid WHERE h.correction_id = ?`,
		`SELECT 1 FROM hidden_scans AS h JOIN scans AS l ON l.subject_folded = h.subject_folded AND l.session_folded = h.session_folded
			AND l.folded = h.folded WHERE h.correction_id = ?`,
		`SELECT 1 FROM hidden_instances AS h JOIN instances AS l ON l.sop_instance_uid = h.sop_instance_uid WHERE h.correction_id = ?`,
	} {
		held, err := exists(ctx, tx, q, id)
		if err != nil {
			return err
		}
		if held {
			return tenant.Precondition("the catalog has taken a label or a UID of the deleted data since; it cannot be shown again")
		}
	}
	return nil
}

// showTarget puts back every row a delete hid, and moves the running
// totals back; it returns the counts it put back.
func (k Sending) showTarget(ctx context.Context, tx SendingTx, c tenant.Correction) error {
	id := c.ID
	if err := checkFree(ctx, tx, id); err != nil {
		return err
	}
	subjects, err := hiddenIDs(ctx, tx, id, "subjects")
	if err != nil {
		return err
	}
	for _, s := range subjects {
		if _, err := k.restoreParent(ctx, tx, id, "subjects", s, [][2]string{{"sessions", "subject_id"}}); err != nil {
			return err
		}
	}
	sessions, err := hiddenIDs(ctx, tx, id, "sessions")
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if _, err := k.restoreParent(ctx, tx, id, "sessions", s, [][2]string{{"scans", "session_id"},
			{"session_modalities", "session_id"}}); err != nil {
			return err
		}
	}
	scans, err := hiddenIDs(ctx, tx, id, "scans")
	if err != nil {
		return err
	}
	for _, s := range scans {
		if _, err := k.restoreParent(ctx, tx, id, "scans", s, [][2]string{{"instances", "scan_id"}, {"reception_scans", "scan_id"}}); err != nil {
			return err
		}
	}
	for _, table := range []string{"instances", "session_modalities", "reception_scans"} {
		list, _, err := k.columnList(ctx, tx, table)
		if err != nil {
			return err
		}
		// #nosec G202 -- table and the columns the database lists are constants; values are parameters.
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (`+list+`) SELECT `+list+` FROM hidden_`+table+` WHERE correction_id = ?`,
			id); err != nil {
			return err
		}
	}
	if len(scans) == 0 {
		// One instance of a scan that stayed: its reception's count of the
		// scan back.
		if _, err := tx.Exec(ctx, `INSERT INTO reception_scans (reception_id, scan_id, instances, bytes)
			SELECT reception_id, scan_id, count(*), sum(size) FROM hidden_instances WHERE correction_id = ? GROUP BY reception_id, scan_id
			ON CONFLICT (reception_id, scan_id) DO UPDATE SET instances = reception_scans.instances + excluded.instances,
			bytes = reception_scans.bytes + excluded.bytes`, id); err != nil {
			return err
		}
	}
	if err := k.recountShown(ctx, tx, id, len(scans) == 0); err != nil {
		return err
	}
	var instances int64
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM hidden_instances WHERE correction_id = ?`, []any{id}, &instances); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_totals SET subjects = subjects + ?, sessions = sessions + ?, scans = scans + ?,
		instances = instances + ?`, len(subjects), len(sessions), len(scans), instances); err != nil {
		return err
	}
	byStorage := map[string]int64{}
	err = tx.Query(ctx, `SELECT s.storage, h.size FROM hidden_instances AS h JOIN scans AS s ON s.id = h.scan_id
		WHERE h.correction_id = ?`, []any{id}, func(r Row) error {
		var storage string
		var size int64
		err := r.Scan(&storage, &size)
		byStorage[storage] += size
		return err
	})
	if err != nil {
		return err
	}
	for storage, bytes := range byStorage {
		if err := moveStorageBytes(ctx, tx, storage, bytes); err != nil {
			return err
		}
	}
	// The objects are the catalog's again; an undone delete owns none.
	if _, err := tx.Exec(ctx, `DELETE FROM correction_keys WHERE correction_id = ?`, id); err != nil {
		return err
	}
	return dropHidden(ctx, tx, id)
}

// recountShown gives the rows above what an undo put back their counts:
// the scan of one instance, and the sessions and subject of the scans put
// back.
func (k Sending) recountShown(ctx context.Context, tx SendingTx, id string, instanceOnly bool) error {
	var scanIDs, sessionIDs, subjectIDs []int64
	q := `SELECT DISTINCT s.id, s.session_id, se.subject_id FROM hidden_instances AS h JOIN scans AS s ON s.id = h.scan_id
		JOIN sessions AS se ON se.id = s.session_id WHERE h.correction_id = ?`
	err := tx.Query(ctx, q, []any{id}, func(r Row) error {
		var a, b, c int64
		err := r.Scan(&a, &b, &c)
		scanIDs, sessionIDs, subjectIDs = append(scanIDs, a), append(sessionIDs, b), append(subjectIDs, c)
		return err
	})
	if err != nil {
		return err
	}
	seen := map[[2]int64]bool{}
	for i := range scanIDs {
		if instanceOnly {
			if err := recountScan(ctx, tx, scanIDs[i]); err != nil {
				return err
			}
		}
		if !seen[[2]int64{0, sessionIDs[i]}] {
			seen[[2]int64{0, sessionIDs[i]}] = true
			if err := recountSession(ctx, tx, sessionIDs[i]); err != nil {
				return err
			}
		}
	}
	for _, s := range subjectIDs {
		if !seen[[2]int64{1, s}] {
			seen[[2]int64{1, s}] = true
			if err := recountSubject(ctx, tx, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropHidden removes every hidden row of a correction.
func dropHidden(ctx context.Context, tx SendingTx, id string) error {
	if err := dropHiddenScanRowsInTx(ctx, tx, id); err != nil {
		return err
	}
	for _, table := range []string{"instances", "reception_scans", "scans", "session_modalities", "sessions", "subjects"} {
		// #nosec G202 -- table is a constant.
		if _, err := tx.Exec(ctx, `DELETE FROM hidden_`+table+` WHERE correction_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}
