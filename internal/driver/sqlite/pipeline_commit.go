package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// catalogID finds a row by its key with query, or reports none.
func catalogID(ctx context.Context, tx *sql.Tx, query string, args []any, dst ...any) (bool, error) {
	err := tx.QueryRowContext(ctx, query, args...).Scan(dst...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// findOrMake runs insert, which does nothing when a row with its key is
// there, then read, which finds the row by its key, until read finds one;
// it reports whether this call made it. On a database with several writers
// an insert that meets a row being made waits for that transaction, so the
// read then finds the row it committed. A few rounds suffice: a row once
// made stays.
func findOrMake(ctx context.Context, tx *sql.Tx, insert string, insertArgs []any, read func() (bool, error)) (bool, error) {
	for range 4 {
		found, err := read()
		if err != nil || found {
			return false, err
		}
		res, err := tx.ExecContext(ctx, insert, insertArgs...)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			if err != nil {
				return false, err
			}
			found, err := read()
			if err == nil && !found {
				err = errors.New("a catalog row just made cannot be read back")
			}
			return err == nil, err
		}
	}
	return false, fault.New(fault.Unavailable, "the catalog's row kept changing under the commit; try again")
}

// sessionRow is the session a commit found or made, with what it made on
// the way.
type sessionRow struct {
	// id and subjectID are the session's and its subject's ids; created
	// is when the session was made, in microseconds.
	id, subjectID, created int64
	// newSubjects and newSessions count the rows this commit made.
	newSubjects, newSessions int64
}

// commitSession finds or makes the commit's session, and its subject, made
// through the receiver of the commit's reception. Each row is found or made
// by findOrMake, so two commits at once of two studies of one subject share
// the subject, and a label another row took meanwhile is classified as the
// conflict it is.
func commitSession(ctx context.Context, tx *sql.Tx, c tenant.ScanCommit, receiver string) (sessionRow, error) {
	var row sessionRow
	var sessionLabel, storage, subjectLabel string
	readSession := func() (bool, error) {
		return catalogID(ctx, tx, `SELECT id, subject_id, created, label, storage, subject_label FROM sessions WHERE study_uid = ?`,
			[]any{c.StudyUID}, &row.id, &row.subjectID, &row.created, &sessionLabel, &storage, &subjectLabel)
	}
	found, err := readSession()
	if err != nil {
		return row, err
	}
	if !found {
		var held string
		var subjectID int64
		made, err := findOrMake(ctx, tx, `INSERT INTO subjects (label, folded, created, updated) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			[]any{c.SubjectLabel, label.Fold(c.SubjectLabel), micros(c.At), micros(c.At)}, func() (bool, error) {
				return catalogID(ctx, tx, `SELECT id, label FROM subjects WHERE folded = ?`, []any{label.Fold(c.SubjectLabel)}, &subjectID, &held)
			})
		if err != nil {
			return row, err
		}
		if held != c.SubjectLabel {
			return row, tenant.LabelConflictError("subject", tenant.FaultCaseCollision)
		}
		row.newSubjects = int64(boolInt(made))
		// The session is made unless its study's row or its label is
		// there; which one, the reads after say.
		// The session keeps what the routing chose for the reception that
		// makes it, so every later arrival goes by the same choice.
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions (study_uid, subject_id, label, folded, storage, created,
			subject_label, subject_folded, study_date, receiver, updated, route)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT route FROM receptions WHERE id = ?)) ON CONFLICT DO NOTHING`,
			c.StudyUID, subjectID, c.SessionLabel, label.Fold(c.SessionLabel),
			c.Storage, micros(c.At), c.SubjectLabel, label.Fold(c.SubjectLabel), c.Values.StudyDate, receiver, micros(c.At), c.ReceptionID)
		if err != nil {
			return row, err
		}
		if row.newSessions, err = res.RowsAffected(); err != nil {
			return row, err
		}
		if found, err = readSession(); err != nil {
			return row, err
		}
		if !found {
			// Not the study's row: its label is held at its subject.
			if _, err := catalogID(ctx, tx, `SELECT label FROM sessions WHERE subject_id = ? AND folded = ?`,
				[]any{subjectID, label.Fold(c.SessionLabel)}, &held); err != nil {
				return row, err
			}
			if held == c.SessionLabel {
				return row, tenant.LabelConflictError("session", tenant.FaultHeldByAnotherSession)
			}
			return row, tenant.LabelConflictError("session", tenant.FaultCaseCollision)
		}
	}
	switch {
	case subjectLabel != c.SubjectLabel:
		return row, tenant.LabelConflictError("subject", tenant.FaultTwoSubjectsForOneStudy)
	case sessionLabel != c.SessionLabel:
		return row, tenant.Precondition("the session already has its label; an existing session's labels are found with CatalogLabels")
	case storage != c.Storage:
		return row, tenant.Precondition("the session's files are on another storage, where every arrival of it is archived")
	}
	return row, nil
}

// commitScanRow finds or makes the commit's scan under its session and
// returns its id and whether it made it, as commitSession does.
func commitScanRow(ctx context.Context, tx *sql.Tx, c tenant.ScanCommit, sessionID int64, receiver string) (int64, bool, error) {
	var scanID, scanSession int64
	var held string
	readScan := func() (bool, error) {
		return catalogID(ctx, tx, `SELECT id, session_id, label FROM scans WHERE series_uid = ?`, []any{c.SeriesUID}, &scanID, &scanSession, &held)
	}
	v := c.Values
	res, err := tx.ExecContext(ctx, `INSERT INTO scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
		subject_label, subject_folded, session_label, session_folded, storage, modality, series_description, description_folded,
		series_number, receiver, study_uid)
		VALUES (?, ?, ?, ?, 0, 0, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		sessionID, c.SeriesUID, c.ScanLabel, label.Fold(c.ScanLabel), micros(c.At), micros(c.At),
		c.SubjectLabel, label.Fold(c.SubjectLabel), c.SessionLabel, label.Fold(c.SessionLabel), c.Storage,
		v.Modality, v.SeriesDescription, tenant.FoldDescription(v.SeriesDescription), seriesNumber(v), receiver, c.StudyUID)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	found, err := readScan()
	if err != nil {
		return 0, false, err
	}
	switch {
	case !found:
		// Not the series's row: its label is held in the session.
		if _, err := catalogID(ctx, tx, `SELECT label FROM scans WHERE session_id = ? AND folded = ?`,
			[]any{sessionID, label.Fold(c.ScanLabel)}, &held); err != nil {
			return 0, false, err
		}
		if held == c.ScanLabel {
			return 0, false, tenant.LabelConflictError("scan", tenant.FaultHeldByAnotherScan)
		}
		return 0, false, tenant.LabelConflictError("scan", tenant.FaultCaseCollision)
	case scanSession != sessionID:
		return 0, false, fault.Wrap(fault.Conflict, tenant.ErrSeriesElsewhere, "the series belongs to a scan of another session")
	case held != c.ScanLabel:
		return 0, false, tenant.Precondition("the scan already has its label; an existing scan's label is found with CatalogLabels")
	}
	return scanID, n == 1, nil
}

// takeClaimSQL removes an instance's claim by the reception, in the
// statement that checks the claim is still this reception's, in the
// commit's study and series, and returns the checksum it was received with,
// which the held row keeps.
const takeClaimSQL = `DELETE FROM instance_claims WHERE sop_instance_uid = ? AND reception_id = ? AND study_uid = ? AND series_uid = ?
	RETURNING sha256`

// insertHeldSQL writes a held instance, in its scan. Its key is the
// layout's (tenant.InstanceKey), so none is kept; its archived checksum is
// kept only when it is not the received one; it records no profile
// version, as an instance archived as received.
const insertHeldSQL = `INSERT INTO instances (scan_id, sop_instance_uid, reception_id, sha256, archived_sha256, storage_key,
	byte_offset, byte_length, size, held_at) VALUES (?1, ?2, ?3, ?4, CASE WHEN ?5 = ?4 THEN NULL ELSE ?5 END, '', ?6, ?7, ?8, ?9)`

// insertDeidentifiedSQL is insertHeldSQL for an instance a profile version
// de-identified: it records the version's id. An instance archived as
// received is written by insertHeldSQL, the row it was before the catalog
// had the column.
const insertDeidentifiedSQL = `INSERT INTO instances (scan_id, sop_instance_uid, reception_id, sha256, archived_sha256, storage_key,
	byte_offset, byte_length, size, held_at, profile_version_id)
	VALUES (?1, ?2, ?3, ?4, CASE WHEN ?5 = ?4 THEN NULL ELSE ?5 END, '', ?6, ?7, ?8, ?9, ?10)`

// CommitScan implements tenant.Pipeline.
func (t *Tenant) CommitScan(ctx context.Context, c tenant.ScanCommit) (tenant.Scan, error) {
	if err := c.Validate(); err != nil {
		return tenant.Scan{}, err
	}
	var out tenant.Scan
	err := t.write(ctx, "the scan cannot be committed", func(tx *sql.Tx, _ *tenantStatements) error {
		if c.Fence != nil {
			if err := checkFence(ctx, tx, *c.Fence); err != nil {
				return err
			}
		}
		study, receiver, err := lockArchiving(ctx, tx, c.ReceptionID)
		if err != nil {
			return err
		}
		if study != c.StudyUID {
			return tenant.Precondition("a commit's study is its reception's")
		}
		if err := lockStorage(ctx, tx, c.Storage); err != nil {
			return err
		}
		if err := sqlkit.CheckCorrectionHolds(ctx, sendingTx{tx: tx}, c); err != nil {
			return err
		}
		session, err := commitSession(ctx, tx, c, receiver)
		if err != nil {
			return err
		}
		scanID, newScan, err := commitScanRow(ctx, tx, c, session.id, receiver)
		if err != nil {
			return err
		}
		profiles, err := sqlkit.ProfileVersionIDs(ctx, sendingTx{tx: tx}, c.Instances)
		if err != nil {
			return err
		}
		var newInstances, newBytes int64
		var confirmed []sqlkit.ArchivedFile
		for _, in := range c.Instances {
			var by string
			var archived []byte
			found, err := catalogID(ctx, tx, `SELECT reception_id, coalesce(archived_sha256, sha256) FROM instances
				WHERE sop_instance_uid = ?`, []any{in.SOPInstanceUID}, &by, &archived)
			if err != nil {
				return err
			}
			if found && by == c.ReceptionID && sqlkit.ToSHA256(archived) == in.ArchivedSHA256 {
				continue // confirmed by this reception before: a commit written again
			}
			if found {
				return tenant.Precondition("an instance confirmed is not claimed by this reception")
			}
			var sum []byte
			err = tx.QueryRowContext(ctx, takeClaimSQL, in.SOPInstanceUID, c.ReceptionID, c.StudyUID, c.SeriesUID).Scan(&sum)
			if errors.Is(err, sql.ErrNoRows) {
				mine, err := exists(ctx, tx, `SELECT 1 FROM instance_claims WHERE sop_instance_uid = ? AND reception_id = ?`,
					in.SOPInstanceUID, c.ReceptionID)
				if err != nil {
					return err
				}
				if mine {
					return tenant.Precondition("an instance confirmed was claimed in another study or series")
				}
				return tenant.Precondition("an instance confirmed is not claimed by this reception")
			}
			if err != nil {
				return err
			}
			query, args := insertHeldSQL, []any{scanID, in.SOPInstanceUID, c.ReceptionID, sum, in.ArchivedSHA256[:],
				in.Offset, in.Length, in.Size, micros(c.At)}
			if id, ok := profiles[in.Profile]; ok {
				query, args = insertDeidentifiedSQL, append(args, id)
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			newInstances++
			newBytes += in.Size
			confirmed = append(confirmed, sqlkit.ArchivedFile{UID: in.SOPInstanceUID, Sum: sqlkit.ToSHA256(sum)})
		}
		// The retention start an import gave, read before the outcome of its
		// files decides their arrivals.
		started, err := (sqlkit.Sending{D: sendingDialect}).ImportRetentionStart(ctx, sendingTx{tx: tx}, c.ReceptionID, newScan)
		if err != nil {
			return err
		}
		// The outcome of an import's files among those confirmed, in this
		// transaction (tenant.Importing).
		importEvents, err := importInTx(tx, func(k sqlkit.Sending, x sqlkit.SendingTx) ([]tenant.EventRecord, error) {
			return k.ImportArchived(ctx, x, c.ReceptionID, confirmed, c.At)
		})
		if err != nil {
			return err
		}
		if newInstances > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO reception_scans (reception_id, scan_id, instances, bytes) VALUES (?, ?, ?, ?)
				ON CONFLICT (reception_id, scan_id) DO UPDATE SET instances = instances + excluded.instances, bytes = bytes + excluded.bytes`,
				c.ReceptionID, scanID, newInstances, newBytes); err != nil {
				return err
			}
		}
		var manifest any
		if !c.ManifestSHA256.IsZero() {
			manifest = c.ManifestSHA256[:]
		}
		if err := addToCatalog(ctx, tx, c, session, scanID, newScan, newInstances, newBytes, manifest); err != nil {
			return err
		}
		// A repair holding the scan puts an object of it meanwhile, maybe its
		// manifest over the one this commit names: the step is retried once
		// the repair ends (one writer at a time here, so this sees every
		// repair recorded before).
		if !newScan {
			if err := (sqlkit.Sending{D: sendingDialect}).CheckRepairHold(ctx, sendingTx{tx: tx}, c.SeriesUID, c.At); err != nil {
				return err
			}
		}
		// A scan given more instances holds objects no recorded read covered.
		if !newScan && newInstances > 0 {
			if err := sqlkit.ForgetVerifiedInTx(ctx, sendingTx{tx: tx}, c.Storage, c.SeriesUID); err != nil {
				return err
			}
		}
		// The metadata file's digest, kept with the manifest's it was
		// written with.
		if err := sqlkit.SetSidecarInTx(ctx, sendingTx{tx: tx}, c.SeriesUID, c.ManifestSHA256, c.SidecarSHA256); err != nil {
			return err
		}
		// The scan's copies on the storage's replicas, queued in the
		// commit's transaction so a crash after it forgets none.
		if err := (sqlkit.Sending{D: sendingDialect}).QueueReplicaCopies(ctx, sendingTx{tx: tx}, c.Storage, c.SeriesUID, c.At); err != nil {
			return err
		}
		// A new scan joins its session's retention: its times are stored
		// again in the commit's transaction (tenant.Retention).
		if err := (sqlkit.Sending{D: sendingDialect}).CommitRetention(ctx, sendingTx{tx: tx}, c, newScan, started); err != nil {
			return err
		}
		// A held target's new scan owes its holds' records to its storage.
		if err := (sqlkit.Sending{D: sendingDialect}).CommitHolds(ctx, sendingTx{tx: tx}, c, newScan); err != nil {
			return err
		}
		if newInstances > 0 {
			if err := (sqlkit.Sending{D: sendingDialect}).LowerCreated(ctx, sendingTx{tx: tx}, c.At, scanID, session.id,
				session.subjectID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE receptions SET files_archived = files_archived + ?, bytes_archived = bytes_archived + ?
			WHERE id = ?`, newInstances, newBytes, c.ReceptionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_totals SET subjects = subjects + ?, sessions = sessions + ?,
			scans = scans + ?, instances = instances + ? WHERE one = 1`,
			session.newSubjects, session.newSessions, boolInt(newScan), newInstances); err != nil {
			return err
		}
		if newBytes > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO storage_totals (storage, bytes) VALUES (?, ?)
				ON CONFLICT (storage) DO UPDATE SET bytes = bytes + excluded.bytes`, c.Storage, newBytes); err != nil {
				return err
			}
		}
		var sum []byte
		out = tenant.Scan{SubjectLabel: c.SubjectLabel, SessionLabel: c.SessionLabel, Label: c.ScanLabel,
			StudyUID: c.StudyUID, SeriesUID: c.SeriesUID, Storage: c.Storage}
		if err := tx.QueryRowContext(ctx, `SELECT instances, bytes, manifest_sha256 FROM scans WHERE id = ?`, scanID).
			Scan(&out.Instances, &out.Bytes, &sum); err != nil {
			return err
		}
		out.ManifestSHA256 = sqlkit.ToSHA256(sum)
		// The events of the import jobs the commit ended, last.
		return writeImportEvents(ctx, tx, importEvents)
	})
	if err != nil {
		return tenant.Scan{}, err
	}
	return out, nil
}

// addToCatalog adds a commit's instances to the running totals of its scan,
// its session and its subject, and gives the scan and session the values
// they hold empty, in that order: on a database with several writers every
// commit locks the rows it changes in this one order (scan, session, the
// session's modalities, subject, then the catalog's and the storage's
// totals), so two commits never wait for each other in a cycle. A row's
// update time moves only when instances are added.
func addToCatalog(ctx context.Context, tx *sql.Tx, c tenant.ScanCommit, session sessionRow, scanID int64, newScan bool,
	newInstances, newBytes int64, manifest any) error {
	v := c.Values
	var modality string
	if err := tx.QueryRowContext(ctx, `UPDATE scans SET instances = instances + ?, bytes = bytes + ?,
		manifest_sha256 = coalesce(?, manifest_sha256), updated = CASE WHEN ? > 0 THEN max(updated, ?) ELSE updated END,
		modality = CASE WHEN modality = '' THEN ? ELSE modality END,
		description_folded = CASE WHEN series_description = '' THEN ? ELSE description_folded END,
		series_description = CASE WHEN series_description = '' THEN ? ELSE series_description END,
		series_number = coalesce(series_number, ?)
		WHERE id = ? RETURNING modality`,
		newInstances, newBytes, manifest, newInstances, micros(c.At), v.Modality, tenant.FoldDescription(v.SeriesDescription), v.SeriesDescription,
		seriesNumber(v), scanID).Scan(&modality); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET scan_count = scan_count + ?, instance_count = instance_count + ?,
		bytes = bytes + ?, updated = CASE WHEN ? > 0 THEN max(updated, ?) ELSE updated END,
		study_date = CASE WHEN study_date = '' THEN ? ELSE study_date END WHERE id = ?`,
		boolInt(newScan), newInstances, newBytes, newInstances, micros(c.At), v.StudyDate, session.id); err != nil {
		return err
	}
	if modality != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_modalities (session_id, modality, created) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, session.id, modality, session.created); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE subjects SET session_count = session_count + ?, instance_count = instance_count + ?,
		bytes = bytes + ?, updated = CASE WHEN ? > 0 THEN max(updated, ?) ELSE updated END WHERE id = ?`,
		session.newSessions, newInstances, newBytes, newInstances, micros(c.At), session.subjectID)
	return err
}

// FillScanValues implements tenant.ScanValueFill, by addToCatalog with no
// instance added, so the rule of the values a row keeps is the commit's
// own, and the rows are locked in its order.
func (t *Tenant) FillScanValues(ctx context.Context, seriesUID string, v tenant.ScanValues) error {
	if err := tenant.ValidateFillScanValues(seriesUID, v); err != nil {
		return err
	}
	return t.write(ctx, "the scan's values cannot be filled in", func(tx *sql.Tx, _ *tenantStatements) error {
		var scanID int64
		var session sessionRow
		found, err := catalogID(ctx, tx, `SELECT sc.id, se.id, se.subject_id, se.created FROM scans AS sc
			JOIN sessions AS se ON se.id = sc.session_id WHERE sc.series_uid = ?`, []any{seriesUID},
			&scanID, &session.id, &session.subjectID, &session.created)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(fault.NotFound, "no scan holds the series")
		}
		return addToCatalog(ctx, tx, tenant.ScanCommit{Values: v}, session, scanID, false, 0, 0, nil)
	})
}

// seriesNumber returns a scan's series number as a column holds it: NULL
// when its files held none.
func seriesNumber(v tenant.ScanValues) any {
	if !v.HasSeriesNumber {
		return nil
	}
	return v.SeriesNumber
}
