package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A scan's commit into the catalog (CommitScan): the subject, session and
// scan found or made once whoever commits at once, the reception's claims
// confirmed, and the counts added.

// findOne finds a row by its key with query, or reports none.
func findOne(ctx context.Context, tx pgx.Tx, query string, args []any, dst ...any) (bool, error) {
	err := tx.QueryRow(ctx, query, args...).Scan(dst...)
	if isNoRow(err) {
		return false, nil
	}
	return err == nil, err
}

// findOrMake runs read, which finds a row by its key, and when it finds none
// runs insert, which does nothing when a row with its key is there, then
// reads again, until read finds one; it reports whether this call made it.
// An insert that meets a row another transaction is making waits for that
// transaction and then does nothing; the read after it is a statement of its
// own, so it sees the row that transaction committed. A row once made stays,
// so a few rounds suffice.
func findOrMake(ctx context.Context, tx pgx.Tx, insert string, insertArgs []any, read func() (bool, error)) (bool, error) {
	for range 4 {
		found, err := read()
		if err != nil || found {
			return false, err
		}
		n, err := rowsChanged(tx.Exec(ctx, insert, insertArgs...))
		if err != nil {
			return false, err
		}
		if n == 1 {
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
	// id and subjectID are the session's and its subject's ids.
	id, subjectID int64
	// created is when the session was made.
	created time.Time
	// newSubjects and newSessions count the rows this commit made.
	newSubjects, newSessions int64
}

// commitSession finds or makes the commit's session, and its subject, made
// through the receiver of the commit's reception. Each row is found or made
// as findOrMake says, so two commits at once of two studies of one subject
// share the subject, and a label another row took meanwhile is classified
// as the conflict it is.
func commitSession(ctx context.Context, tx pgx.Tx, c tenant.ScanCommit, receiver string) (sessionRow, error) {
	var row sessionRow
	var sessionLabel, storage, subjectLabel string
	readSession := func() (bool, error) {
		return findOne(ctx, tx, `SELECT id, subject_id, created, label, storage, subject_label FROM sessions WHERE study_uid = $1`,
			[]any{c.StudyUID}, &row.id, &row.subjectID, &row.created, &sessionLabel, &storage, &subjectLabel)
	}
	found, err := readSession()
	if err != nil {
		return row, err
	}
	if !found {
		var held string
		var subjectID int64
		folded := label.Fold(c.SubjectLabel)
		made, err := findOrMake(ctx, tx, `INSERT INTO subjects (label, folded, created, updated) VALUES ($1, $2, $3, $3) ON CONFLICT DO NOTHING`,
			[]any{c.SubjectLabel, folded, micros(c.At)}, func() (bool, error) {
				return findOne(ctx, tx, `SELECT id, label FROM subjects WHERE folded = $1`, []any{folded}, &subjectID, &held)
			})
		if err != nil {
			return row, err
		}
		if held != c.SubjectLabel {
			return row, tenant.LabelConflictError("subject", tenant.FaultCaseCollision)
		}
		if made {
			row.newSubjects = 1
		}
		// The session is made unless its study's row or its label is there;
		// which one, the reads after say.
		// The session keeps what the routing chose for the reception that
		// makes it, so every later arrival goes by the same choice.
		if row.newSessions, err = rowsChanged(tx.Exec(ctx, `INSERT INTO sessions (study_uid, subject_id, label, folded, storage, created,
			subject_label, subject_folded, study_date, receiver, updated, route)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $6, (SELECT route FROM receptions WHERE id = $11)) ON CONFLICT DO NOTHING`,
			c.StudyUID, subjectID, c.SessionLabel,
			label.Fold(c.SessionLabel), c.Storage, micros(c.At), c.SubjectLabel, folded, c.Values.StudyDate, receiver, c.ReceptionID)); err != nil {
			return row, err
		}
		if found, err = readSession(); err != nil {
			return row, err
		}
		if !found {
			// Not the study's row: its label is held at its subject.
			if _, err := findOne(ctx, tx, `SELECT label FROM sessions WHERE subject_id = $1 AND folded = $2`,
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
func commitScanRow(ctx context.Context, tx pgx.Tx, c tenant.ScanCommit, sessionID int64, receiver string) (int64, bool, error) {
	var scanID, scanSession int64
	var held string
	v := c.Values
	n, err := rowsChanged(tx.Exec(ctx, `INSERT INTO scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
		subject_label, subject_folded, session_label, session_folded, storage, modality, series_description, description_folded,
		series_number, receiver, study_uid)
		VALUES ($1, $2, $3, $4, 0, 0, NULL, $5, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) ON CONFLICT DO NOTHING`,
		sessionID, c.SeriesUID, c.ScanLabel, label.Fold(c.ScanLabel), micros(c.At),
		c.SubjectLabel, label.Fold(c.SubjectLabel), c.SessionLabel, label.Fold(c.SessionLabel), c.Storage,
		v.Modality, v.SeriesDescription, tenant.FoldDescription(v.SeriesDescription), seriesNumber(v), receiver, c.StudyUID))
	if err != nil {
		return 0, false, err
	}
	found, err := findOne(ctx, tx, `SELECT id, session_id, label FROM scans WHERE series_uid = $1`, []any{c.SeriesUID}, &scanID, &scanSession, &held)
	if err != nil {
		return 0, false, err
	}
	switch {
	case !found:
		// Not the series's row: its label is held in the session.
		if _, err := findOne(ctx, tx, `SELECT label FROM scans WHERE session_id = $1 AND folded = $2`,
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

// confirmSQL moves a set of the reception's claims into held instances in
// one statement: it removes each claim in the statement that checks the
// claim is still this reception's, in the commit's study and series, and
// inserts the held row with the checksum the claim was received with. The
// key is the layout's (tenant.InstanceKey), so none is kept; the archived
// checksum is kept only when it is not the received one. It records no
// profile version: it writes the instances archived as received, the row
// they were before the catalog had the column. The UIDs confirmed are
// returned.
const confirmSQL = `WITH taken AS (
	DELETE FROM instance_claims AS c WHERE c.sop_instance_uid = ANY($3::text[]) AND c.reception_id = $8
		AND c.study_uid = $9 AND c.series_uid = $10
	RETURNING c.sop_instance_uid, c.sha256)
	INSERT INTO instances (sop_instance_uid, scan_id, reception_id, sha256, archived_sha256, storage_key, byte_offset, byte_length,
		size, held_at)
	SELECT u.uid, $1, $8, t.sha256, nullif(u.sha, t.sha256), '', u.off, u.len, u.size, $2
	FROM unnest($3::text[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bytea[]) WITH ORDINALITY AS u(uid, off, len, size, sha, n)
	JOIN taken AS t ON t.sop_instance_uid = u.uid ORDER BY u.n
	RETURNING sop_instance_uid`

// confirmDeidentifiedSQL is confirmSQL for a commit any of whose instances
// a profile version de-identified: each records its version's id, given in
// $11, 0 there and NULL in the row for one archived as received, and
// whether a pixel rule of it accepted an image at risk of burned-in text,
// in $12.
const confirmDeidentifiedSQL = `WITH taken AS (
	DELETE FROM instance_claims AS c WHERE c.sop_instance_uid = ANY($3::text[]) AND c.reception_id = $8
		AND c.study_uid = $9 AND c.series_uid = $10
	RETURNING c.sop_instance_uid, c.sha256)
	INSERT INTO instances (sop_instance_uid, scan_id, reception_id, sha256, archived_sha256, storage_key, byte_offset, byte_length,
		size, held_at, profile_version_id, pixel_risk_accepted)
	SELECT u.uid, $1, $8, t.sha256, nullif(u.sha, t.sha256), '', u.off, u.len, u.size, $2, nullif(u.pv, 0), u.pr
	FROM unnest($3::text[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bytea[], $11::bigint[], $12::boolean[])
		WITH ORDINALITY AS u(uid, off, len, size, sha, pv, pr, n)
	JOIN taken AS t ON t.sop_instance_uid = u.uid ORDER BY u.n
	RETURNING sop_instance_uid`

// CommitScan implements tenant.Pipeline. Its locks, in the package's order:
// the unit of work (the fence), the reception FOR NO KEY UPDATE, the
// storage FOR SHARE, the catalog's subject, session and scan as findOrMake
// meets them, the reception's own claims, then the rows whose totals it
// adds to (addToCatalog: the scan, the session, the session's modalities,
// the subject), and the catalog's totals last.
func (t *Tenant) CommitScan(ctx context.Context, c tenant.ScanCommit) (tenant.Scan, error) {
	if err := c.Validate(); err != nil {
		return tenant.Scan{}, err
	}
	uids := make([]string, len(c.Instances))
	for i, in := range c.Instances {
		uids[i] = in.SOPInstanceUID
	}
	var out tenant.Scan
	err := t.pipe(ctx, "the scan cannot be committed", func(tx pgx.Tx) error {
		if c.Fence != nil {
			if err := checkFence(ctx, tx, *c.Fence); err != nil {
				return err
			}
		}
		study, receiver, err := lockArchiving(ctx, tx, c.ReceptionID, "FOR NO KEY UPDATE")
		if err != nil {
			return err
		}
		if study != c.StudyUID {
			return tenant.Precondition("a commit's study is its reception's")
		}
		if err := lockStorageShared(ctx, tx, c.Storage); err != nil {
			return err
		}
		if err := sqlkit.CheckCorrectionHolds(ctx, sendingTx{tx: tx, t: t}, c); err != nil {
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
		profiles, err := sqlkit.ProfileVersionIDs(ctx, sendingTx{tx: tx, t: t}, c.Instances)
		if err != nil {
			return err
		}
		// This reception's rows cannot change under the commit: a claim of
		// the reception holds its row FOR SHARE, and a step that releases
		// its claims FOR NO KEY UPDATE, both of which wait for this one.
		type held struct {
			held, mine bool
			archived   tenant.SHA256
		}
		rows, err := tx.Query(ctx, `SELECT sop_instance_uid, true, reception_id = $2, coalesce(archived_sha256, sha256) FROM instances
			WHERE sop_instance_uid = ANY($1::text[])
			UNION ALL SELECT sop_instance_uid, false, reception_id = $2, sha256 FROM instance_claims
			WHERE sop_instance_uid = ANY($1::text[])`, uids, c.ReceptionID)
		got, err := collect(rows, err, func(row scanner) (struct {
			uid string
			h   held
		}, error) {
			var r struct {
				uid string
				h   held
			}
			var archived []byte
			err := row.Scan(&r.uid, &r.h.held, &r.h.mine, &archived)
			r.h.archived = sqlkit.ToSHA256(archived)
			return r, err
		})
		if err != nil {
			return err
		}
		byUID := make(map[string]held, len(got))
		for _, r := range got {
			byUID[r.uid] = r.h
		}
		var offs, lens, sizes, pvs []int64
		var risks []bool
		var sums [][]byte
		var confirm []string
		// Each instance confirmed with its received checksum, the claim's,
		// the key of the received file an import's arrival names.
		var confirmed []sqlkit.ArchivedFile
		var newBytes int64
		for _, in := range c.Instances {
			h, found := byUID[in.SOPInstanceUID]
			if found && h.held && h.mine && h.archived == in.ArchivedSHA256 {
				continue // confirmed by this reception before: a commit written again
			}
			if !found || h.held || !h.mine {
				return tenant.Precondition("an instance confirmed is not claimed by this reception")
			}
			confirm = append(confirm, in.SOPInstanceUID)
			confirmed = append(confirmed, sqlkit.ArchivedFile{UID: in.SOPInstanceUID, Sum: h.archived})
			offs, lens, sizes = append(offs, in.Offset), append(lens, in.Length), append(sizes, in.Size)
			sums = append(sums, sumBytes(in.ArchivedSHA256))
			pvs = append(pvs, profiles[in.Profile])
			risks = append(risks, in.PixelRiskAccepted)
			newBytes += in.Size
		}
		var newInstances int64
		if len(confirm) > 0 {
			query, args := confirmSQL, []any{scanID, micros(c.At), confirm, offs, lens, sizes, sums,
				c.ReceptionID, c.StudyUID, c.SeriesUID}
			if len(profiles) > 0 {
				query, args = confirmDeidentifiedSQL, append(args, pvs, risks)
			}
			rows, err := tx.Query(ctx, query, args...)
			done, err := collect(rows, err, func(row scanner) (string, error) {
				var u string
				err := row.Scan(&u)
				return u, err
			})
			if err != nil {
				return err
			}
			if len(done) != len(confirm) {
				return tenant.Precondition("an instance confirmed was claimed in another study or series")
			}
			newInstances = int64(len(done))
			if _, err := tx.Exec(ctx, `INSERT INTO reception_scans (reception_id, scan_id, instances, bytes) VALUES ($1, $2, $3, $4)
				ON CONFLICT (reception_id, scan_id) DO UPDATE SET instances = reception_scans.instances + excluded.instances,
				bytes = reception_scans.bytes + excluded.bytes`, c.ReceptionID, scanID, newInstances, newBytes); err != nil {
				return err
			}
		}
		// The retention start an import gave, read before the outcome of its
		// files decides their arrivals.
		started, err := importSending().ImportRetentionStart(ctx, sendingTx{tx: tx}, c.ReceptionID, newScan)
		if err != nil {
			return err
		}
		// The outcome of an import's files among those confirmed, in this
		// transaction (tenant.Importing); its events are written last.
		importEvents, err := importSending().ImportArchived(ctx, sendingTx{tx: tx}, c.ReceptionID, confirmed, c.At)
		if err != nil {
			return err
		}
		if err := addToCatalog(ctx, tx, c, session, scanID, newScan, newInstances, newBytes); err != nil {
			return err
		}
		// Looked for once the scan's row is locked (addToCatalog updated it):
		// a repair recorded meanwhile locks that row too, so this commit
		// either waited for it and sees its hold, or the repair waits for
		// this commit and reads the scan as it leaves it.
		if !newScan {
			if err := (sqlkit.Sending{D: sendingDialect}).CheckRepairHold(ctx, sendingTx{tx: tx, t: t}, c.SeriesUID, c.At); err != nil {
				return err
			}
		}
		// A scan given more instances holds objects no recorded read covered.
		if !newScan && newInstances > 0 {
			if err := sqlkit.ForgetVerifiedInTx(ctx, sendingTx{tx: tx, t: t}, c.Storage, c.SeriesUID); err != nil {
				return err
			}
		}
		// The metadata file's digest, kept with the manifest's it was
		// written with.
		if err := sqlkit.SetSidecarInTx(ctx, sendingTx{tx: tx}, c.SeriesUID, c.ManifestSHA256, c.SidecarSHA256); err != nil {
			return err
		}
		// The scan's copies on the storage's replicas, queued in the
		// commit's transaction so a crash after it forgets none; their rows
		// are locked after the scan's, as a correction's publish locks them.
		if err := importSending().QueueReplicaCopies(ctx, sendingTx{tx: tx}, c.Storage, c.SeriesUID, c.At); err != nil {
			return err
		}
		// A new scan joins its session's retention: its times are stored
		// again in the commit's transaction (tenant.Retention).
		if err := importSending().CommitRetention(ctx, sendingTx{tx: tx}, c, newScan, started); err != nil {
			return err
		}
		// A held target's new scan owes its holds' records to its storage.
		if err := importSending().CommitHolds(ctx, sendingTx{tx: tx}, c, newScan); err != nil {
			return err
		}
		if newInstances > 0 {
			if err := importSending().LowerCreated(ctx, sendingTx{tx: tx}, c.At, scanID, session.id, session.subjectID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE receptions SET files_archived = files_archived + $1, bytes_archived = bytes_archived + $2
			WHERE id = $3`, newInstances, newBytes, c.ReceptionID); err != nil {
			return err
		}
		scans := int64(0)
		if newScan {
			scans = 1
		}
		if session.newSubjects+session.newSessions+scans+newInstances > 0 {
			if _, err := tx.Exec(ctx, `UPDATE catalog_totals SET subjects = subjects + $1, sessions = sessions + $2,
				scans = scans + $3, instances = instances + $4 WHERE only_row`,
				session.newSubjects, session.newSessions, scans, newInstances); err != nil {
				return err
			}
		}
		// The storage's total, a row of its own (storage_totals), last
		// with the catalog's, among the totals the package locks last.
		if newBytes > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO storage_totals (storage, bytes) VALUES ($1, $2)
				ON CONFLICT (storage) DO UPDATE SET bytes = storage_totals.bytes + excluded.bytes`, c.Storage, newBytes); err != nil {
				return err
			}
		}
		var sum []byte
		out = tenant.Scan{SubjectLabel: c.SubjectLabel, SessionLabel: c.SessionLabel, Label: c.ScanLabel,
			StudyUID: c.StudyUID, SeriesUID: c.SeriesUID, Storage: c.Storage}
		if err := tx.QueryRow(ctx, `SELECT instances, bytes, manifest_sha256 FROM scans WHERE id = $1`, scanID).
			Scan(&out.Instances, &out.Bytes, &sum); err != nil {
			return err
		}
		out.ManifestSHA256 = sqlkit.ToSHA256(sum)
		return writeEvents(ctx, tx, importEvents...)
	})
	if err != nil {
		return tenant.Scan{}, err
	}
	return out, nil
}

// addToCatalog adds a commit's instances to the running totals of its scan,
// its session and its subject, and gives the scan and session the values
// they hold empty, locking each row in this one order (scan, session, the
// session's modalities, subject), which every commit keeps, so two commits
// never wait for each other in a cycle: two commits into one session wait
// at the session's row before either inserts a modality, whose unique key
// would otherwise make the second wait for the first while holding what
// the first needs. A row's update time moves only when instances are
// added.
func addToCatalog(ctx context.Context, tx pgx.Tx, c tenant.ScanCommit, session sessionRow, scanID int64, newScan bool,
	newInstances, newBytes int64) error {
	v := c.Values
	var modality string
	if err := tx.QueryRow(ctx, `UPDATE scans SET instances = instances + $1, bytes = bytes + $2,
		manifest_sha256 = coalesce($3, manifest_sha256), updated = CASE WHEN $1 > 0 THEN greatest(updated, $4) ELSE updated END,
		modality = CASE WHEN modality = '' THEN $5 ELSE modality END,
		description_folded = CASE WHEN series_description = '' THEN $6 ELSE description_folded END,
		series_description = CASE WHEN series_description = '' THEN $7 ELSE series_description END,
		series_number = coalesce(series_number, $8)
		WHERE id = $9 RETURNING modality`,
		newInstances, newBytes, nullSum(c.ManifestSHA256), micros(c.At), v.Modality, tenant.FoldDescription(v.SeriesDescription),
		v.SeriesDescription, seriesNumber(v), scanID).Scan(&modality); err != nil {
		return err
	}
	newScans := int64(0)
	if newScan {
		newScans = 1
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET scan_count = scan_count + $1, instance_count = instance_count + $2,
		bytes = bytes + $3, updated = CASE WHEN $2 > 0 THEN greatest(updated, $4) ELSE updated END,
		study_date = CASE WHEN study_date = '' THEN $5 ELSE study_date END WHERE id = $6`,
		newScans, newInstances, newBytes, micros(c.At), v.StudyDate, session.id); err != nil {
		return err
	}
	if modality != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO session_modalities (session_id, modality, created) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, session.id, modality, session.created); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE subjects SET session_count = session_count + $1, instance_count = instance_count + $2,
		bytes = bytes + $3, updated = CASE WHEN $2 > 0 THEN greatest(updated, $4) ELSE updated END WHERE id = $5`,
		session.newSessions, newInstances, newBytes, micros(c.At), session.subjectID)
	return err
}

// FillScanValues implements tenant.ScanValueFill, by addToCatalog with no
// instance added, so the rule of the values a row keeps is the commit's
// own, and the rows are locked in its order.
func (t *Tenant) FillScanValues(ctx context.Context, seriesUID string, v tenant.ScanValues) error {
	if err := tenant.ValidateFillScanValues(seriesUID, v); err != nil {
		return err
	}
	return t.pipe(ctx, "the scan's values cannot be filled in", func(tx pgx.Tx) error {
		var scanID int64
		var session sessionRow
		found, err := findOne(ctx, tx, `SELECT sc.id, se.id, se.subject_id, se.created FROM scans AS sc
			JOIN sessions AS se ON se.id = sc.session_id WHERE sc.series_uid = $1`, []any{seriesUID},
			&scanID, &session.id, &session.subjectID, &session.created)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(fault.NotFound, "no scan holds the series")
		}
		return addToCatalog(ctx, tx, tenant.ScanCommit{Values: v}, session, scanID, false, 0, 0)
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
