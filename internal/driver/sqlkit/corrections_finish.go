package sqlkit

import (
	"context"

	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The end of a purge: what the correction's rows become once its objects
// are gone.

// FinishPurge implements tenant.Corrections.
func (k Sending) FinishPurge(ctx context.Context, f tenant.PurgeFinish) (tenant.Correction, error) {
	if err := master.ValidateTime("time", f.Now); err != nil {
		return tenant.Correction{}, err
	}
	if f.Audit != nil {
		if err := f.Audit.Validate(); err != nil {
			return tenant.Correction{}, err
		}
	}
	return k.step(ctx, f.ID, "the correction's purge cannot be ended", func(tx SendingTx, c *tenant.Correction) error {
		if c.State == corr.StatePurged {
			return nil
		}
		if c.State != corr.StatePurging {
			return tenant.Precondition("the correction is " + string(c.State) + ", not purging")
		}
		if c.Kind == corr.KindDelete {
			if err := k.dropDeleted(ctx, tx, *c, f.Manifest, f.ManifestBefore); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE instance_revisions SET purged = ? WHERE correction_id = ?`, k.D.Bool(true),
			c.ID); err != nil {
			return err
		}
		// What it did not settle (a target a stop left between its purge
		// and its record) was asked of its storage; it owns nothing more.
		if _, err := tx.Exec(ctx, `UPDATE correction_keys SET outcome = 'purged' WHERE correction_id = ? AND outcome = ''`,
			c.ID); err != nil {
			return err
		}
		if err := k.setState(ctx, tx, c, corr.StatePurged, f.Now, ""); err != nil {
			return err
		}
		if err := changedEvent(ctx, tx, *c, f.Now); err != nil {
			return err
		}
		if f.Audit == nil {
			return nil
		}
		return tx.Audited(ctx, *f.Audit, nil)
	})
}

// dropDeleted drops a purged delete's hidden rows for good, keeping each
// instance's revision and checksum, and the damage marks of its instances;
// an instance delete's scan takes the SHA-256 of its manifest written
// without it, while the catalog still holds the one it was made from. Each
// statement is one over the delete's own rows, whatever their number, and
// touches no row another correction or the catalog owns: a revision
// another correction recorded is left as it is, and a mark of a UID the
// catalog holds again stays.
func (k Sending) dropDeleted(ctx context.Context, tx SendingTx, c tenant.Correction, manifest, before tenant.SHA256) error {
	// The layout's key is revision 1; a key the catalog held whole is a
	// later revision, whose number its key carries, read one by one (a
	// replaced instance is rare).
	if _, err := tx.Exec(ctx, `INSERT INTO instance_revisions (sop_instance_uid, revision, sha256, size, storage, storage_key,
		correction_id, fate, purged)
		SELECT h.sop_instance_uid, 1, coalesce(h.archived_sha256, h.sha256), h.size,
			coalesce((SELECT storage FROM hidden_scans WHERE correction_id = h.correction_id AND id = h.scan_id),
				(SELECT storage FROM scans WHERE id = h.scan_id), ''), '', h.correction_id, 'deleted', ?
		FROM hidden_instances AS h WHERE h.correction_id = ? AND h.storage_key = ''
		ON CONFLICT (sop_instance_uid, revision) DO UPDATE SET purged = excluded.purged
		WHERE instance_revisions.correction_id = excluded.correction_id`, k.D.Bool(true), c.ID); err != nil {
		return err
	}
	type row struct {
		uid, key, storage string
		sum               []byte
		size              int64
	}
	var rows []row
	err := tx.Query(ctx, `SELECT h.sop_instance_uid, h.storage_key, coalesce(h.archived_sha256, h.sha256), h.size,
		coalesce((SELECT storage FROM hidden_scans WHERE correction_id = h.correction_id AND id = h.scan_id),
			(SELECT storage FROM scans WHERE id = h.scan_id), '')
		FROM hidden_instances AS h WHERE h.correction_id = ? AND h.storage_key <> '' ORDER BY h.sop_instance_uid`, []any{c.ID},
		func(r Row) error {
			var x row
			err := r.Scan(&x.uid, &x.key, &x.sum, &x.size, &x.storage)
			rows = append(rows, x)
			return err
		})
	if err != nil {
		return err
	}
	for _, x := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO instance_revisions (sop_instance_uid, revision, sha256, size, storage, storage_key,
			correction_id, fate, purged) VALUES (?, ?, ?, ?, ?, ?, ?, 'deleted', ?) ON CONFLICT (sop_instance_uid, revision)
			DO UPDATE SET purged = excluded.purged WHERE instance_revisions.correction_id = excluded.correction_id`,
			x.uid, tenant.RevisionOfKey(x.key), x.sum, x.size, x.storage, x.key, c.ID, k.D.Bool(true)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM instance_damage WHERE sop_instance_uid IN
		(SELECT h.sop_instance_uid FROM hidden_instances AS h WHERE h.correction_id = ?
			AND NOT EXISTS (SELECT 1 FROM instances AS i WHERE i.sop_instance_uid = h.sop_instance_uid))`, c.ID); err != nil {
		return err
	}
	if c.Level == corr.LevelInstance && !manifest.IsZero() {
		q, args := `UPDATE scans SET manifest_sha256 = ? WHERE id = (SELECT scan_id FROM hidden_instances WHERE correction_id = ?)`,
			[]any{manifest[:], c.ID}
		if !before.IsZero() {
			q, args = q+` AND (manifest_sha256 = ? OR manifest_sha256 IS NULL)`, append(args, before[:])
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return err
		}
	}
	return dropHidden(ctx, tx, c.ID)
}
