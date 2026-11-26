package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A replica's damage (tenant.Replication): the objects of a replica a scrub
// or a read found missing or wrong, on the replica_damage table, each
// recorded with its scan's row and counts in one transaction. Damage a
// scrub found is also a scrub finding on the replica's name, whose id the
// damage row keeps (finding_id; on the scan's row, the metadata file's in
// sidecar_finding_id beside the manifest's), so a list of findings shows
// it; those findings are cleared in the transaction that removes the
// damage, whoever removes it (a scrub that read it good, the replicator's
// verified copy, a detach).

// RecordReplicaDamage implements tenant.Replication.
func (k Sending) RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	var made bool
	err := k.Host.Write(ctx, "the replica's damage cannot be recorded", func(tx SendingTx) error {
		made = false
		var scrub jobRow
		if d.Fence != nil {
			if d.Finding != nil {
				r, err := k.scrubJobOfUnit(ctx, tx, *d.Fence)
				if err != nil {
					return err
				}
				scrub = r
			} else if err := tx.CheckFence(ctx, *d.Fence); err != nil {
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
		var finding, sidecarFinding string
		held, err := tx.QueryRow(ctx, `SELECT finding_id, sidecar_finding_id FROM replica_damage WHERE replica = ? AND series_uid = ?
			AND sop_instance_uid = ?`, []any{d.Replica, d.SeriesUID, dm.SOPInstanceUID}, &finding, &sidecarFinding)
		if err != nil {
			return err
		}
		if f := d.Finding; f != nil {
			// Every finding of a replica's damage names its primary, which
			// lets the copy that mends it clear it with no job.
			var primary string
			if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{d.Replica}, &primary); err != nil {
				return err
			}
			if primary == "" {
				return tenant.Precondition("the storage " + d.Replica + " is not a replica")
			}
			// The id takes the damage's time too: the same unit may find the
			// object damaged again after a copy mended it and cleared the
			// finding, and each such finding is one of its own.
			id := tenant.ScrubFindingID(scrub.job.ID, d.Replica, f.Key+"\x00"+dm.Found.UTC().Format(time.RFC3339Nano), f.Kind)
			out, _, err := k.recordFindingInTx(ctx, tx, scrub, tenant.ScrubFindingRecord{Fence: *d.Fence, Now: dm.Found,
				Finding: tenant.ScrubFinding{ID: id, Kind: f.Kind,
					Storage: d.Replica, Key: f.Key, Path: f.Path, SOPInstanceUID: dm.SOPInstanceUID, Revision: dm.Revision,
					ExpectedSize: f.ExpectedSize, FoundSize: dm.FoundSize, ExpectedSHA256: f.ExpectedSHA256, FoundSHA256: dm.FoundSHA256}})
			if err != nil {
				return err
			}
			// The scan's row ('') is the damage of its manifest and of its
			// metadata file at once: each keeps its own finding, so the end
			// of the damage clears both.
			if dm.SOPInstanceUID == "" && f.Kind == tenant.FindingDisagreeing {
				sidecarFinding = out.ID
			} else {
				finding = out.ID
			}
		}
		if held {
			_, err = tx.Exec(ctx, `UPDATE replica_damage SET revision = ?, found_size = ?, found_sha256 = ?, found_at = ?, finding_id = ?,
				sidecar_finding_id = ? WHERE replica = ? AND series_uid = ? AND sop_instance_uid = ?`, dm.Revision, nullInt(dm.FoundSize),
				nullSHA(dm.FoundSHA256), k.D.Time(dm.Found), finding, sidecarFinding, d.Replica, d.SeriesUID, dm.SOPInstanceUID)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO replica_damage (replica, series_uid, sop_instance_uid, revision, found_size, found_sha256,
				found_at, finding_id, sidecar_finding_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.Replica, d.SeriesUID, dm.SOPInstanceUID,
				dm.Revision, nullInt(dm.FoundSize), nullSHA(dm.FoundSHA256), k.D.Time(dm.Found), finding, sidecarFinding)
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
		job := ""
		if fence != nil {
			if err := tx.CheckFence(ctx, *fence); err != nil {
				return err
			}
			var err error
			if job, err = scrubJobOfFence(ctx, tx, *fence); err != nil {
				return err
			}
		}
		row, found, err := replicaRowIn(ctx, tx, replica, seriesUID, k.D.Lock)
		if err != nil || !found {
			return err
		}
		where, args := `replica = ? AND series_uid = ? AND sop_instance_uid = ?`, []any{replica, seriesUID, sopUID}
		if err := k.clearDamageFindings(ctx, tx, where, args, job, now); err != nil {
			return err
		}
		n, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE `+where, args...) // #nosec G202 -- where is a constant above; values are parameters.
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

// ReplicaDamaged implements tenant.Replication.
func (k Sending) ReplicaDamaged(ctx context.Context, replica, seriesUID string) (map[string]string, error) {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return nil, err
	}
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return nil, err
	}
	out := map[string]string{}
	err := k.Host.Read(ctx, "the replica's damage cannot be read", func(tx SendingTx) error {
		clear(out)
		// The scan's row answers the manifest's finding, or the metadata
		// file's when it has only that one.
		return tx.Query(ctx, `SELECT sop_instance_uid, CASE WHEN finding_id <> '' THEN finding_id ELSE sidecar_finding_id END
			FROM replica_damage WHERE replica = ? AND series_uid = ?`,
			[]any{replica, seriesUID}, func(r Row) error {
				var uid, finding string
				err := r.Scan(&uid, &finding)
				out[uid] = finding
				return err
			})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scrubJobOfFence returns the id of the scrub job whose unit holds the
// fence, already checked, or "" when the unit is no scrub job's.
func scrubJobOfFence(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (string, error) {
	scan, isScan, err := jobScanOf(ctx, tx, fence.ID, "")
	if err != nil {
		return "", err
	}
	job := scan.jobID
	if !isScan {
		if job, err = listingJobOf(ctx, tx, fence.ID); err != nil || job == "" {
			return "", err
		}
	}
	var kind string
	found, err := tx.QueryRow(ctx, `SELECT kind FROM jobs WHERE id = ?`, []any{job}, &kind)
	if err != nil || !found || kind != string(tenant.JobScrub) {
		return "", err
	}
	return job, nil
}

// clearDamageFindings clears, in the caller's transaction, the scrub
// findings that hold and that the replica_damage rows where selects were
// recorded with, before those rows are removed: by job when it is a scrub's
// (with each finding's event), with no job when the replicator's verified
// copy or a detach removes the damage. where is a constant of its caller;
// values are parameters.
func (k Sending) clearDamageFindings(ctx context.Context, tx SendingTx, where string, args []any, job string, now time.Time) error {
	var ids []string
	// #nosec G202 -- where is a constant of the caller; values are parameters.
	err := tx.Query(ctx, `SELECT finding_id, sidecar_finding_id FROM replica_damage WHERE (finding_id <> '' OR sidecar_finding_id <> '')
		AND `+where, args, func(r Row) error {
		var id, sidecar string
		err := r.Scan(&id, &sidecar)
		for _, x := range []string{id, sidecar} {
			if x != "" {
				ids = append(ids, x)
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		f, found, err := findingIn(ctx, tx, findingByIDSQL+k.D.Lock, id)
		if err != nil {
			return err
		}
		if !found || !f.Holds() {
			continue
		}
		if job != "" {
			if _, err := k.clearInTx(ctx, tx, job, f, now); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE scrub_findings SET cleared_at = ? WHERE id = ? AND cleared_at IS NULL`, k.D.Time(now), id); err != nil {
			return err
		}
	}
	return nil
}
