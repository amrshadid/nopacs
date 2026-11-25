package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A replica's damage (tenant.Replication): the objects of a replica a scrub
// or a read found missing or wrong, on the replica_damage table, each
// recorded with its scan's row and counts in one transaction.

// RecordReplicaDamage implements tenant.Replication.
func (k Sending) RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	var made bool
	err := k.Host.Write(ctx, "the replica's damage cannot be recorded", func(tx SendingTx) error {
		made = false
		if d.Fence != nil {
			if err := tx.CheckFence(ctx, *d.Fence); err != nil {
				return err
			}
		}
		row, found, err := replicaRowIn(ctx, tx, d.Replica, d.SeriesUID, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return tenant.Precondition("the replica holds no row of this scan; a scan never copied holds nothing damaged")
		}
		dm := d.Damage
		held, err := exists(ctx, tx, `SELECT 1 FROM replica_damage WHERE replica = ? AND series_uid = ? AND sop_instance_uid = ?`,
			d.Replica, d.SeriesUID, dm.SOPInstanceUID)
		if err != nil {
			return err
		}
		if held {
			_, err = tx.Exec(ctx, `UPDATE replica_damage SET revision = ?, found_size = ?, found_sha256 = ?, found_at = ?
				WHERE replica = ? AND series_uid = ? AND sop_instance_uid = ?`, dm.Revision, nullInt(dm.FoundSize),
				nullSHA(dm.FoundSHA256), k.D.Time(dm.Found), d.Replica, d.SeriesUID, dm.SOPInstanceUID)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO replica_damage (replica, series_uid, sop_instance_uid, revision, found_size, found_sha256,
				found_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, d.Replica, d.SeriesUID, dm.SOPInstanceUID, dm.Revision, nullInt(dm.FoundSize),
				nullSHA(dm.FoundSHA256), k.D.Time(dm.Found))
		}
		if err != nil {
			return err
		}
		made = !held
		next := row.State
		if next != tenant.ReplicaCopying {
			next = tenant.ReplicaDamaged
		}
		if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = ?, seq = seq + 1, due = ?, queued_at = coalesce(queued_at, ?)
			WHERE replica = ? AND series_uid = ?`, string(next), k.D.Time(dm.Found), k.D.Time(dm.Found), d.Replica, d.SeriesUID); err != nil {
			return err
		}
		t := totals{}
		if made {
			t.objects = 1
		}
		t.move(row.State, next)
		return k.addTotals(ctx, tx, d.Replica, t, dm.Found)
	})
	if err != nil {
		return false, err
	}
	return made, nil
}

// ClearReplicaDamage implements tenant.Replication.
func (k Sending) ClearReplicaDamage(ctx context.Context, fence *tenant.WorkFence, replica, seriesUID, sopUID string, now time.Time) (bool, error) {
	if fence != nil {
		if err := fence.Validate(); err != nil {
			return false, err
		}
	}
	if err := tenant.ValidateName("storage", replica); err != nil {
		return false, err
	}
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return false, err
	}
	if sopUID != "" {
		if err := tenant.ValidateInstanceUID(sopUID); err != nil {
			return false, err
		}
	}
	if err := master.ValidateTime("time", now); err != nil {
		return false, err
	}
	var cleared bool
	err := k.Host.Write(ctx, "the replica's damage cannot be cleared", func(tx SendingTx) error {
		cleared = false
		if fence != nil {
			if err := tx.CheckFence(ctx, *fence); err != nil {
				return err
			}
		}
		row, found, err := replicaRowIn(ctx, tx, replica, seriesUID, k.D.Lock)
		if err != nil || !found {
			return err
		}
		n, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE replica = ? AND series_uid = ? AND sop_instance_uid = ?`,
			replica, seriesUID, sopUID)
		if err != nil || n == 0 {
			return err
		}
		cleared = true
		t := totals{objects: -1}
		if row.Damaged == 1 && row.State == tenant.ReplicaDamaged {
			next := tenant.ReplicaPending
			s, held, err := scanOfSeries(ctx, tx, seriesUID)
			if err != nil {
				return err
			}
			if held && !row.Verified.IsZero() && ToSHA256(s.manifest) == row.ManifestSHA256 {
				next = tenant.ReplicaInStep
			}
			q, args := `UPDATE replica_scans SET state = 'in_step', queued_at = NULL WHERE replica = ? AND series_uid = ?`,
				[]any{replica, seriesUID}
			if next != tenant.ReplicaInStep {
				q, args = `UPDATE replica_scans SET state = 'pending', queued_at = coalesce(queued_at, ?) WHERE replica = ?
					AND series_uid = ?`, []any{k.D.Time(now), replica, seriesUID}
			}
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				return err
			}
			t.move(tenant.ReplicaDamaged, next)
		}
		return k.addTotals(ctx, tx, replica, t, now)
	})
	if err != nil {
		return false, err
	}
	return cleared, nil
}
