package sqlkit

import (
	"context"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// When each scan was last read whole on each storage (tenant.Scrubbing's
// VerificationRecorder and VerificationSource), on the scrub_verified
// table: keyed by the storage's name and the scan's Series Instance UID, and
// read beside the scans of the storage, so a coverage counts what the
// catalog holds now and a row whose scan is gone is never read.

// verificationsSQL reads a page of a storage's scans after one, each with
// what is recorded of its read on the storage named: a range of the scans'
// index on (storage, id), each joined to scrub_verified by its key.
const verificationsSQL = `SELECT s.id, s.instances, s.created, v.verified_at, v.presence_at FROM scans AS s
	LEFT JOIN scrub_verified AS v ON v.storage = ? AND v.series_uid = s.series_uid
	WHERE s.storage = ? AND s.id > ? ORDER BY s.id LIMIT ?`

// ForgetVerifiedInTx drops, in the caller's transaction, what is recorded
// of the reads of the scan of series on storage and on each of its
// replicas: a commit that gives a scan more instances calls it, since no
// read recorded covered them, so the scan counts as unverified until a
// scrub reads it whole again. Each row is found by its key.
func ForgetVerifiedInTx(ctx context.Context, tx SendingTx, storage, series string) error {
	_, err := tx.Exec(ctx, `DELETE FROM scrub_verified WHERE series_uid = ?
		AND storage IN (SELECT name FROM storages WHERE name = ? OR replica_of = ?)`, series, storage, storage)
	return err
}

// dropHiddenScanRowsInTx drops, in the caller's transaction, the metadata
// digests and the records of reads of the scans a correction of id hid and
// that the catalog no longer holds, as its hidden rows are dropped for
// good: rows keyed by a Series Instance UID would otherwise outlive their
// scan, and be taken as a new scan's of the same series. Each row is found
// by its key.
func dropHiddenScanRowsInTx(ctx context.Context, tx SendingTx, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM scan_sidecars WHERE series_uid IN (SELECT h.series_uid FROM hidden_scans AS h
		WHERE h.correction_id = ? AND NOT EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = h.series_uid))`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM scrub_verified WHERE (storage, series_uid) IN (SELECT st.name, h.series_uid
		FROM hidden_scans AS h JOIN storages AS st ON st.name = h.storage OR st.replica_of = h.storage
		WHERE h.correction_id = ? AND NOT EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = h.series_uid))`, id)
	return err
}

// DropGoneScanRowsInTx drops, in the caller's transaction, the metadata
// digests and the records of reads whose scan the catalog holds neither
// live nor hidden by a correction: what a rebuild that replaces the
// catalog leaves of scans it did not find again.
func DropGoneScanRowsInTx(ctx context.Context, tx SendingTx) error {
	for _, table := range []string{"scan_sidecars", "scrub_verified"} {
		// #nosec G202 -- table is a constant.
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE NOT EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = `+table+
			`.series_uid) AND NOT EXISTS (SELECT 1 FROM hidden_scans AS h WHERE h.series_uid = `+table+`.series_uid)`); err != nil {
			return err
		}
	}
	return nil
}

// DropStorageVerifiedInTx drops, in the caller's transaction, what is
// recorded of the reads of every scan on the storage named, as the storage
// is removed: a storage added again under the name holds none of them.
func DropStorageVerifiedInTx(ctx context.Context, tx SendingTx, storage string) error {
	_, err := tx.Exec(ctx, `DELETE FROM scrub_verified WHERE storage = ?`, storage)
	return err
}

// RecordScanVerified implements tenant.VerificationRecorder.
func (k Sending) RecordScanVerified(ctx context.Context, v tenant.ScanVerified) error {
	if err := v.Validate(); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the scan's verification cannot be recorded", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, v.Fence); err != nil {
			return err
		}
		p := v.Path
		var series string
		found, err := tx.QueryRow(ctx, `SELECT series_uid FROM scans WHERE subject_folded = ? AND session_folded = ? AND folded = ?
			AND subject_label = ? AND session_label = ? AND label = ?`, []any{label.Fold(p.Subject), label.Fold(p.Session),
			label.Fold(p.Scan), p.Subject, p.Session, p.Scan}, &series)
		if err != nil || !found {
			return err // a scan gone from the catalog has nothing to cover
		}
		q := `INSERT INTO scrub_verified (storage, series_uid, verified_at, presence_at) VALUES (?, ?, ?, NULL)
			ON CONFLICT (storage, series_uid) DO UPDATE SET verified_at = excluded.verified_at`
		if v.ByPresence {
			q = `INSERT INTO scrub_verified (storage, series_uid, verified_at, presence_at) VALUES (?, ?, NULL, ?)
				ON CONFLICT (storage, series_uid) DO UPDATE SET presence_at = excluded.presence_at`
		}
		_, err = tx.Exec(ctx, q, v.Storage, series, k.D.Time(v.At))
		return err
	})
}

// ScanVerifications implements tenant.VerificationSource.
func (k Sending) ScanVerifications(ctx context.Context, storage, after string, limit int) ([]tenant.ScanVerification, string, error) {
	from, err := tenant.ValidateVerificationPage(storage, after, limit)
	if err != nil {
		return nil, "", err
	}
	var out []tenant.ScanVerification
	var last int64
	err = k.Host.Read(ctx, "the scans' verifications cannot be read", func(tx SendingTx) error {
		out, last = nil, 0
		var replicaOf string
		found, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{storage}, &replicaOf)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + storage)
		}
		held := storage
		if replicaOf != "" {
			held = replicaOf // a replica holds its primary's scans
		}
		return tx.Query(ctx, verificationsSQL, []any{storage, held, from, limit}, func(r Row) error {
			var s tenant.ScanVerification
			var created, verified, presence DBTime
			err := r.Scan(&last, &s.Objects, &created, &verified, &presence)
			s.Held, s.Verified, s.Presence = created.T, verified.T, presence.T
			out = append(out, s)
			return err
		})
	})
	if err != nil {
		return nil, "", err
	}
	if len(out) < limit {
		return out, "", nil
	}
	return out, strconv.FormatInt(last, 10), nil
}
