package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// instanceRowSQL reads what the catalog holds or a reception claims under a
// UID: the held instance with its scan's study and series
// (instances_by_uid, then the scans' primary key), or the claim
// (instance_claims' primary key). A UID is in one of the two at most.
const instanceRowSQL = `SELECT s.study_uid, s.series_uid, i.sha256, i.reception_id, 1 FROM instances AS i
	JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid = ?1
	UNION ALL SELECT study_uid, series_uid, sha256, reception_id, 0 FROM instance_claims WHERE sop_instance_uid = ?1`

// scanInstanceRow reads one row of instanceRowSQL.
func scanInstanceRow(row interface{ Scan(...any) error }) (tenant.InstanceRow, error) {
	var r tenant.InstanceRow
	var sum []byte
	err := row.Scan(&r.StudyUID, &r.SeriesUID, &sum, &r.By, &r.Held)
	r.SHA256 = sqlkit.ToSHA256(sum)
	return r, err
}

// instanceRowInTx reads the instance row under uid with the transaction's
// statement, or nil when there is none.
func instanceRowInTx(ctx context.Context, stmt *sql.Stmt, uid string) (*tenant.InstanceRow, error) {
	r, err := scanInstanceRow(stmt.QueryRowContext(ctx, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// lockArchiving locks the row of a reception that has been identified and
// not ended, in the statement that checks it, and returns its Study Instance
// UID as received and its receiver. A step that ends it (and releases its claims) is then
// ordered with a claim or a commit: on a database with several writers the
// second waits for the first. It refuses a reception not there (NotFound) or
// not archiving (a precondition).
func lockArchiving(ctx context.Context, tx *sql.Tx, receptionID string) (study, receiver string, err error) {
	err = tx.QueryRowContext(ctx, `UPDATE receptions SET step_at = step_at WHERE id = ? AND ended = 0 AND storage <> ''
		RETURNING study_uid, receiver`, receptionID).Scan(&study, &receiver)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return study, receiver, err
	}
	has, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = ?`, receptionID)
	if err != nil {
		return "", "", err
	}
	if !has {
		return "", "", fault.New(fault.NotFound, "the tenant has no reception with this id")
	}
	return "", "", tenant.Precondition("the reception is not archiving: it has not been identified, or it has ended")
}

// lockStorage takes a storage's row in the transaction, by an update that
// changes nothing, so a commit or an identify that names the storage and its
// removal or move are ordered: on a database with several writers the later
// waits for the earlier, then sees what it wrote. A storage not there is a
// precondition failure.
func lockStorage(ctx context.Context, tx *sql.Tx, name string) error {
	res, err := tx.ExecContext(ctx, `UPDATE storages SET updated = updated WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	return tenant.Precondition("the tenant has no storage named " + name)
}

// releaseClaims removes the claims a reception did not confirm. A held
// instance is never touched.
func releaseClaims(ctx context.Context, tx *sql.Tx, receptionID string) error {
	_, err := tx.ExecContext(ctx, releaseClaimedSQL, receptionID)
	return err
}

// endReception does what a step to an end does besides writing the
// reception's row: it releases the claims the reception did not confirm,
// gives the import's files still processing in it their outcome
// (tenant.Importing) and, for an end that is not a failure, removes its
// received files (tenant.Pipeline's ReceivedFiles says why), in the step's
// transaction, at now. It returns the events of the import jobs that
// ended, which the step writes last (writeImportEvents).
func endReception(ctx context.Context, tx *sql.Tx, receptionID string, state reception.State, now time.Time) ([]tenant.EventRecord, error) {
	if err := releaseClaims(ctx, tx, receptionID); err != nil {
		return nil, err
	}
	events, err := importInTx(tx, func(k sqlkit.Sending, x sqlkit.SendingTx) ([]tenant.EventRecord, error) {
		return k.ImportEnded(ctx, x, receptionID, state, now)
	})
	if err != nil {
		return nil, err
	}
	if !tenant.KeepsReceivedFiles(state) {
		_, err := tx.ExecContext(ctx, `DELETE FROM received_files WHERE reception_id = ?`, receptionID)
		return events, err
	}
	return events, nil
}

// ClaimInstances implements tenant.Pipeline. The files are claimed in sorted
// order of UID, then checksum, and answered in the caller's order. Each must
// be one of the reception's received files, as received. Each answer is
// tenant.DecideClaim's on the row read; an insert that meets a row made
// meanwhile (on a database with several writers, by a transaction it waited
// for) reads it again and decides again.
func (t *Tenant) ClaimInstances(ctx context.Context, receptionID string, files []tenant.InstanceClaim, now time.Time, fence *tenant.WorkFence) ([]tenant.Duplicate, error) {
	if err := tenant.ValidateClaimBatch(receptionID, files, now); err != nil {
		return nil, err
	}
	if fence != nil {
		if err := fence.Validate(); err != nil {
			return nil, err
		}
	}
	order := tenant.SortClaims(files)
	var out []tenant.Duplicate
	err := t.write(ctx, "the instances cannot be claimed", func(tx *sql.Tx, s *tenantStatements) error {
		if fence != nil {
			if err := checkFence(ctx, tx, *fence); err != nil {
				return err
			}
		}
		if _, _, err := lockArchiving(ctx, tx, receptionID); err != nil {
			return err
		}
		read, insert := tx.StmtContext(ctx, s.instanceRow), tx.StmtContext(ctx, s.insertClaim)
		received := tx.StmtContext(ctx, s.receivedFile)
		answers := make([]tenant.Duplicate, len(files))
		for _, i := range order {
			c := files[i]
			var study, series string
			err := received.QueryRowContext(ctx, receptionID, c.SOPInstanceUID, c.SHA256[:]).Scan(&study, &series)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (study != c.StudyUID || series != c.SeriesUID)) {
				return tenant.Precondition("a claim names a file the reception did not receive, or not as it received it")
			}
			if err != nil {
				return err
			}
			for try := 0; ; try++ {
				row, err := instanceRowInTx(ctx, read, c.SOPInstanceUID)
				if err != nil {
					return err
				}
				d, action := tenant.DecideClaim(receptionID, c, row)
				if action == tenant.ClaimInsert {
					res, err := insert.ExecContext(ctx, c.SOPInstanceUID, c.StudyUID, c.SeriesUID, c.SHA256[:], receptionID, micros(now))
					if err != nil {
						return err
					}
					n, err := res.RowsAffected()
					if err != nil {
						return err
					}
					if n == 0 {
						if try < 3 {
							continue
						}
						return fault.New(fault.Unavailable, "the instance's row kept changing under the claim; try again")
					}
				}
				answers[i] = d
				break
			}
		}
		out = answers
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CheckDuplicates implements tenant.Pipeline: a read of each UID's instance
// row, judged by tenant.JudgeRow.
func (t *Tenant) CheckDuplicates(ctx context.Context, files []tenant.DuplicateQuery) ([]tenant.Duplicate, error) {
	if err := tenant.ValidateDuplicateBatch(files); err != nil {
		return nil, err
	}
	s, err := t.ready(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tenant.Duplicate, len(files))
	if len(files) == 0 {
		return out, nil
	}
	uids := make([]string, len(files))
	for i, f := range files {
		uids[i] = f.SOPInstanceUID
	}
	list, err := sqlkit.JSONText(uids)
	if err != nil {
		return nil, err
	}
	type found struct {
		uid string
		row tenant.InstanceRow
	}
	rows, err := s.duplicates.QueryContext(ctx, list)
	all, err := collect(rows, err, func(row interface{ Scan(...any) error }) (found, error) {
		var f found
		var uid string
		r, err := scanInstanceRow(scanPrefixed{row: row, first: &uid})
		f.uid, f.row = uid, r
		return f, err
	})
	if err != nil {
		return nil, t.fail(ctx, err, "the duplicate check cannot be read")
	}
	byUID := make(map[string]*tenant.InstanceRow, len(all))
	for i := range all {
		byUID[all[i].uid] = &all[i].row
	}
	for i, f := range files {
		out[i] = tenant.JudgeRow(f, byUID[f.SOPInstanceUID])
	}
	return out, nil
}

// scanPrefixed reads a row whose first column goes to first and the rest to
// the destinations its caller gives, so a row with a key before the columns
// of another statement is read by that statement's scanner.
type scanPrefixed struct {
	row   interface{ Scan(...any) error }
	first *string
}

// Scan reads the row.
func (p scanPrefixed) Scan(dst ...any) error { return p.row.Scan(append([]any{p.first}, dst...)...) }

// duplicatesSQL reads the rows of a batch's SOP Instance UIDs as
// instanceRowSQL does, given as one JSON array (json_each), so one prepared
// statement serves a batch of any size; each UID is a lookup of
// instances_by_uid and of the claims' primary key.
const duplicatesSQL = `SELECT i.sop_instance_uid, s.study_uid, s.series_uid, i.sha256, i.reception_id, 1 FROM instances AS i
	JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid IN (SELECT value FROM json_each(?1))
	UNION ALL SELECT sop_instance_uid, study_uid, series_uid, sha256, reception_id, 0 FROM instance_claims
	WHERE sop_instance_uid IN (SELECT value FROM json_each(?1))`

// CatalogLabels implements tenant.Pipeline.
func (t *Tenant) CatalogLabels(ctx context.Context, studyUID string, seriesUIDs []string) (tenant.CatalogLabels, error) {
	if err := tenant.ValidateSeriesLookup(studyUID, seriesUIDs); err != nil {
		return tenant.CatalogLabels{}, err
	}
	var out tenant.CatalogLabels
	sessions, err := readRows(ctx, t, "the catalog's labels cannot be read", sessionLabelsSQL, []any{studyUID},
		func(row interface{ Scan(...any) error }) (tenant.SessionLabels, error) {
			var l tenant.SessionLabels
			var route string
			if err := row.Scan(&l.SubjectLabel, &l.SessionLabel, &l.Storage, &route); err != nil {
				return l, err
			}
			var err error
			l.Route, err = sqlkit.ReadReceptionRoute(route)
			return l, err
		})
	if err != nil {
		return out, err
	}
	if len(sessions) == 1 {
		out.Session = &sessions[0]
	}
	if len(seriesUIDs) == 0 {
		return out, nil
	}
	list, err := sqlkit.JSONText(seriesUIDs)
	if err != nil {
		return out, err
	}
	type scanLabel struct{ series, label string }
	scans, err := readRows(ctx, t, "the catalog's labels cannot be read", scanLabelsSQL, []any{list},
		func(row interface{ Scan(...any) error }) (scanLabel, error) {
			var l scanLabel
			err := row.Scan(&l.series, &l.label)
			return l, err
		})
	if err != nil {
		return out, err
	}
	out.Scans = make(map[string]string, len(scans))
	for _, s := range scans {
		out.Scans[s.series] = s.label
	}
	return out, nil
}

// CatalogTotals implements tenant.Pipeline.
func (t *Tenant) CatalogTotals(ctx context.Context) (tenant.CatalogCounts, error) {
	return readOne(ctx, t, "the catalog has no totals", "the catalog's totals cannot be read",
		`SELECT subjects, sessions, scans, instances FROM catalog_totals WHERE one = 1`, nil,
		func(row interface{ Scan(...any) error }) (tenant.CatalogCounts, error) {
			var c tenant.CatalogCounts
			err := row.Scan(&c.Subjects, &c.Sessions, &c.Scans, &c.Instances)
			return c, err
		})
}

// ReceptionScans implements tenant.Pipeline.
func (t *Tenant) ReceptionScans(ctx context.Context, receptionID string) ([]tenant.ReceptionScan, bool, error) {
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return nil, false, err
	}
	out, err := readRows(ctx, t, "the reception's scans cannot be read", receptionScansSQL,
		[]any{receptionID, tenant.MaxReceptionScans + 1}, func(row interface{ Scan(...any) error }) (tenant.ReceptionScan, error) {
			var s tenant.ReceptionScan
			err := row.Scan(&s.Label, &s.SeriesUID, &s.Instances, &s.Bytes)
			return s, err
		})
	if err != nil {
		return nil, false, err
	}
	if len(out) > tenant.MaxReceptionScans {
		return out[:tenant.MaxReceptionScans], true, nil
	}
	return out, false, nil
}

// scanInstancesSQL pages the instances held in the scan of a series: the
// scans' unique index on series_uid, then the instances' primary key, in
// order of SOP Instance UID. The storage and the labels the key is made
// from are the scan's (tenant.InstanceKey).
const scanInstancesSQL = `SELECT i.sop_instance_uid, i.reception_id, s.storage, i.storage_key, s.subject_label, s.session_label, s.label,
	i.byte_offset, i.byte_length, i.size, i.sha256, coalesce(i.archived_sha256, i.sha256), i.held_at
	FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
	WHERE s.series_uid = ? AND i.sop_instance_uid > ? ORDER BY i.sop_instance_uid LIMIT ?`

// ScanInstances implements tenant.ScanContents.
func (t *Tenant) ScanInstances(ctx context.Context, seriesUID string, page master.Page) ([]tenant.ScanInstance, string, error) {
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.binding(seriesUID)
	after := ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listScanInstances, binding, 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	out, err := readRows(ctx, t, "the scan's instances cannot be listed", scanInstancesSQL,
		[]any{seriesUID, after, page.Limit + 1}, func(row interface{ Scan(...any) error }) (tenant.ScanInstance, error) {
			var in tenant.ScanInstance
			var sum, archived []byte
			var held int64
			var stored string
			var p label.Path
			err := row.Scan(&in.SOPInstanceUID, &in.ReceptionID, &in.Storage, &stored, &p.Subject, &p.Session, &p.Scan,
				&in.Offset, &in.Length, &in.Size, &sum, &archived, &held)
			if err != nil {
				return in, err
			}
			in.SHA256, in.ArchivedSHA256, in.Held = sqlkit.ToSHA256(sum), sqlkit.ToSHA256(archived), fromMicros(held)
			in.Key, err = tenant.InstanceKey(p, in.SOPInstanceUID, stored)
			return in, err
		})
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listScanInstances, binding, last.SOPInstanceUID), nil
}
