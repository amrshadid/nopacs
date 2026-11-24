package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// claimTries bounds the rounds of one claim. A round whose insert of a UID
// does nothing met a row another transaction committed meanwhile (it waited
// for that transaction), and the next round reads it; a row is removed again
// only when its reception ends, so a UID settles within a few rounds.
const claimTries = 8

// lockArchiving locks the row of a reception that has been identified and
// not ended, in the statement that checks it, with the lock given, and
// returns its Study Instance UID as received and its receiver. A step that
// ends it (and
// releases its claims) takes the row FOR NO KEY UPDATE, so it and a claim
// (FOR SHARE: claims of one reception do not wait for each other) or a
// commit (FOR NO KEY UPDATE: it adds to the reception's counts) are
// ordered; under read committed a lock that waited reads the row as the
// other left it and tests the condition again. It refuses a reception not
// there (NotFound) or not archiving (a precondition).
func lockArchiving(ctx context.Context, tx pgx.Tx, receptionID, lock string) (study, receiver string, err error) {
	err = tx.QueryRow(ctx, `SELECT study_uid, receiver FROM receptions WHERE id = $1 AND NOT ended AND storage <> '' `+lock,
		receptionID).Scan(&study, &receiver)
	if err == nil || !isNoRow(err) {
		return study, receiver, err
	}
	has, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = $1`, receptionID)
	if err != nil {
		return "", "", err
	}
	if !has {
		return "", "", errNoReception()
	}
	return "", "", tenant.Precondition("the reception is not archiving: it has not been identified, or it has ended")
}

// scanInstanceRow reads one row of instanceRowsSQL.
func scanInstanceRow(row scanner) (string, tenant.InstanceRow, error) {
	var uid string
	var r tenant.InstanceRow
	var sum []byte
	err := row.Scan(&uid, &r.StudyUID, &r.SeriesUID, &sum, &r.By, &r.Held)
	r.SHA256 = sqlkit.ToSHA256(sum)
	return uid, r, err
}

// instanceRows reads the instance rows under uids, by UID.
func instanceRows(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, uids []string) (map[string]*tenant.InstanceRow, error) {

	type found struct {
		uid string
		row tenant.InstanceRow
	}
	rows, err := q.Query(ctx, instanceRowsSQL, uids)
	all, err := collect(rows, err, func(row scanner) (found, error) {
		uid, r, err := scanInstanceRow(row)
		return found{uid, r}, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*tenant.InstanceRow, len(all))
	for i := range all {
		out[all[i].uid] = &all[i].row
	}
	return out, nil
}

// insertClaimsSQL writes a set of claims in one statement, in the order of
// the arrays, which the caller gives sorted: Postgres inserts the rows of an
// INSERT ... SELECT in the order the SELECT yields them, and a row that
// meets another transaction's row not yet committed waits for it there, so
// two claims that share UIDs wait for each other in UID order and never in a
// cycle. A UID that meets a row is skipped (ON CONFLICT DO NOTHING); the
// UIDs written are returned.
const insertClaimsSQL = `INSERT INTO instance_claims (sop_instance_uid, study_uid, series_uid, sha256, reception_id, claimed_at)
	SELECT u.uid, u.study, u.series, u.sha, $1, $2
	FROM unnest($3::text[], $4::text[], $5::text[], $6::bytea[]) AS u(uid, study, series, sha)
	ON CONFLICT DO NOTHING RETURNING sop_instance_uid`

// heldAfterClaimSQL finds, among the UIDs a claim just wrote, those a
// commit of another reception's claim moved into instances meanwhile: the
// claim's insert waited for that commit, which removed the claim it met;
// this statement, begun after, sees the held row. One probe of the
// instances' primary key for each UID; the claims found are removed by
// dropClaimsSQL, which a claim almost never needs.
const heldAfterClaimSQL = `SELECT sop_instance_uid FROM instances WHERE sop_instance_uid = ANY($1::text[])`

// dropClaimsSQL removes a reception's claims of the UIDs given.
const dropClaimsSQL = `DELETE FROM instance_claims WHERE sop_instance_uid = ANY($1::text[]) AND reception_id = $2`

// ClaimInstances implements tenant.Pipeline. Every file must be one of the
// reception's received files, as received (one read of the received files'
// key for the batch). The instance rows of the batch are read, each file is
// decided by tenant.DecideClaim in sorted order of UID, then checksum, and
// the UIDs to claim are inserted as one sorted set. A UID whose insert met a
// row committed meanwhile is read again, in a statement of its own (one that
// began before the other transaction committed would not see its row), and
// decided again; the others are settled.
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
	uids, sums := make([]string, len(files)), make([][]byte, len(files))
	var distinct []string
	for k, i := range order {
		uids[k], sums[k] = files[i].SOPInstanceUID, sumBytes(files[i].SHA256)
		if k == 0 || uids[k] != uids[k-1] {
			distinct = append(distinct, uids[k])
		}
	}
	var out []tenant.Duplicate
	err := t.pipe(ctx, "the instances cannot be claimed", func(tx pgx.Tx) error {
		if fence != nil {
			if err := checkFence(ctx, tx, *fence); err != nil {
				return err
			}
		}
		if _, _, err := lockArchiving(ctx, tx, receptionID, "FOR SHARE"); err != nil {
			return err
		}
		type asReceived struct{ study, series string }
		rows, err := tx.Query(ctx, receivedOfClaimSQL, receptionID, uids, sums)
		received, err := collect(rows, err, func(row scanner) (struct {
			k fileKey
			v asReceived
		}, error) {
			var r struct {
				k fileKey
				v asReceived
			}
			var sum []byte
			err := row.Scan(&r.k.uid, &sum, &r.v.study, &r.v.series)
			r.k.sum = sqlkit.ToSHA256(sum)
			return r, err
		})
		if err != nil {
			return err
		}
		byKey := make(map[fileKey]asReceived, len(received))
		for _, r := range received {
			byKey[r.k] = r.v
		}
		for _, c := range files {
			got, ok := byKey[fileKey{c.SOPInstanceUID, c.SHA256}]
			if !ok || got.study != c.StudyUID || got.series != c.SeriesUID {
				return tenant.Precondition("a claim names a file the reception did not receive, or not as it received it")
			}
		}
		view, err := instanceRows(ctx, tx, distinct)
		if err != nil {
			return err
		}
		answers := make([]tenant.Duplicate, len(files))
		settled := make(map[string]bool, len(distinct))
		for try := 0; ; try++ {
			if try == claimTries {
				return fault.New(fault.Unavailable, "the instances' rows kept changing under the claim; try again")
			}
			var insUIDs, insStudies, insSeries []string
			var insSums [][]byte
			for _, i := range order {
				c := files[i]
				if settled[c.SOPInstanceUID] {
					continue
				}
				d, action := tenant.DecideClaim(receptionID, c, view[c.SOPInstanceUID])
				answers[i] = d
				if action == tenant.ClaimInsert {
					insUIDs, insStudies = append(insUIDs, c.SOPInstanceUID), append(insStudies, c.StudyUID)
					insSeries, insSums = append(insSeries, c.SeriesUID), append(insSums, sumBytes(c.SHA256))
					// A second file of the batch under this UID is decided
					// against this claim: the lower checksum is kept.
					view[c.SOPInstanceUID] = &tenant.InstanceRow{StudyUID: c.StudyUID, SeriesUID: c.SeriesUID, SHA256: c.SHA256, By: receptionID}
				}
			}
			var lost []string
			if len(insUIDs) > 0 {
				var err error
				lost, err = writeClaims(ctx, tx, receptionID, now, insUIDs, insStudies, insSeries, insSums)
				if err != nil {
					return err
				}
			}
			for _, u := range distinct {
				settled[u] = true
			}
			if len(lost) == 0 {
				break
			}
			again, err := instanceRows(ctx, tx, lost)
			if err != nil {
				return err
			}
			for _, u := range lost {
				settled[u] = false
				view[u] = again[u]
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

// writeClaims inserts claims for the UIDs given and returns those it did not
// keep: the ones another claim won while this one waited for its lock, and
// the ones a commit of another reception moved into instances meanwhile
// (that commit deleted the claim this one is written over, and was not yet
// visible when the batch was judged). A UID is claimed or held, never both;
// no constraint ties the two tables, so the probe after the insert is what
// keeps a claim from sitting beside the instance that took its place.
func writeClaims(ctx context.Context, tx pgx.Tx, receptionID string, now time.Time, uids, studies, series []string, sums [][]byte) ([]string, error) {
	rows, err := tx.Query(ctx, insertClaimsSQL, receptionID, micros(now), uids, studies, series, sums)
	written, err := collect(rows, err, func(row scanner) (string, error) {
		var u string
		err := row.Scan(&u)
		return u, err
	})
	if err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(written))
	for _, u := range written {
		done[u] = true
	}
	var lost []string
	for _, u := range uids {
		if !done[u] {
			lost = append(lost, u)
		}
	}
	if len(written) == 0 {
		return lost, nil
	}
	rows, err = tx.Query(ctx, heldAfterClaimSQL, written)
	moved, err := collect(rows, err, func(row scanner) (string, error) {
		var u string
		err := row.Scan(&u)
		return u, err
	})
	if err != nil {
		return nil, err
	}
	if len(moved) > 0 {
		if _, err := tx.Exec(ctx, dropClaimsSQL, moved, receptionID); err != nil {
			return nil, err
		}
		lost = append(lost, moved...)
	}
	return lost, nil
}

// CheckDuplicates implements tenant.Pipeline: one read of the batch's UIDs'
// instance rows, each judged by tenant.JudgeRow.
func (t *Tenant) CheckDuplicates(ctx context.Context, files []tenant.DuplicateQuery) ([]tenant.Duplicate, error) {
	if err := tenant.ValidateDuplicateBatch(files); err != nil {
		return nil, err
	}
	if err := t.open(); err != nil {
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
	byUID, err := instanceRows(ctx, t.pool, uids)
	if err != nil {
		return nil, t.fail(ctx, err, "the duplicate check cannot be read")
	}
	for i, f := range files {
		out[i] = tenant.JudgeRow(f, byUID[f.SOPInstanceUID])
	}
	return out, nil
}

// CatalogLabels implements tenant.Pipeline.
func (t *Tenant) CatalogLabels(ctx context.Context, studyUID string, seriesUIDs []string) (tenant.CatalogLabels, error) {
	if err := tenant.ValidateSeriesLookup(studyUID, seriesUIDs); err != nil {
		return tenant.CatalogLabels{}, err
	}
	var out tenant.CatalogLabels
	sessions, err := readRows(ctx, t, "the catalog's labels cannot be read", sessionLabelsSQL, []any{studyUID},
		func(row scanner) (tenant.SessionLabels, error) {
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
	type scanLabel struct{ series, label string }
	scans, err := readRows(ctx, t, "the catalog's labels cannot be read", scanLabelsSQL, []any{seriesUIDs},
		func(row scanner) (scanLabel, error) {
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
		`SELECT subjects, sessions, scans, instances FROM catalog_totals WHERE only_row`, nil,
		func(row scanner) (tenant.CatalogCounts, error) {
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
		[]any{receptionID, tenant.MaxReceptionScans + 1}, func(row scanner) (tenant.ReceptionScan, error) {
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
// scans' unique key on series_uid, then instances_by_scan, in order of SOP
// Instance UID. The storage and the labels the key is made from are the
// scan's (tenant.InstanceKey).
const scanInstancesSQL = `SELECT i.sop_instance_uid, i.reception_id, s.storage, i.storage_key, s.subject_label, s.session_label, s.label,
	i.byte_offset, i.byte_length, i.size, i.sha256, coalesce(i.archived_sha256, i.sha256), i.held_at
	FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
	WHERE s.series_uid = $1 AND i.sop_instance_uid > $2 ORDER BY i.sop_instance_uid LIMIT $3`

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
		[]any{seriesUID, after, page.Limit + 1}, func(row scanner) (tenant.ScanInstance, error) {
			var in tenant.ScanInstance
			var sum, archived []byte
			var held time.Time
			var stored string
			var p label.Path
			err := row.Scan(&in.SOPInstanceUID, &in.ReceptionID, &in.Storage, &stored, &p.Subject, &p.Session, &p.Scan,
				&in.Offset, &in.Length, &in.Size, &sum, &archived, &held)
			if err != nil {
				return in, err
			}
			in.SHA256, in.ArchivedSHA256, in.Held = sqlkit.ToSHA256(sum), sqlkit.ToSHA256(archived), held.UTC()
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
