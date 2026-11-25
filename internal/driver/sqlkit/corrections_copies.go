package sqlkit

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a correction does on the replicas of its scans' storages
// (tenant.Corrections, "Every copy"): the copy set, the records owed to
// replicas (correction_records_owed), the copies a detach abandoned
// (correction_copies_abandoned), and the state reason a purge waits with.

// CorrectionCopies implements tenant.Corrections.
func (k Sending) CorrectionCopies(ctx context.Context, id string) ([]tenant.CorrectionCopy, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	var out []tenant.CorrectionCopy
	err := k.Host.Read(ctx, "the correction's copies cannot be read", func(tx SendingTx) error {
		out = nil
		found, err := exists(ctx, tx, `SELECT 1 FROM corrections WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no correction with this id")
		}
		err = tx.Query(ctx, `SELECT name, replica_of FROM storages WHERE replica_of <> '' AND replica_of IN
			(SELECT storage FROM correction_scans WHERE correction_id = ?) ORDER BY replica_of, name`, []any{id}, func(r Row) error {
			var c tenant.CorrectionCopy
			err := r.Scan(&c.Replica, &c.Primary)
			out = append(out, c)
			return err
		})
		if err != nil {
			return err
		}
		for i := range out {
			c := &out[i]
			err := tx.Query(ctx, `SELECT cs.series_uid FROM correction_scans AS cs
				JOIN replica_scans AS r ON r.replica = ? AND r.series_uid = cs.series_uid AND r.state = 'in_step'
				JOIN scans AS s ON s.series_uid = cs.series_uid
				WHERE cs.correction_id = ? AND cs.storage = ? AND `+sameManifest+` ORDER BY cs.series_uid`,
				[]any{c.Replica, id, c.Primary}, func(r Row) error {
					var s string
					err := r.Scan(&s)
					c.InStep = append(c.InStep, s)
					return err
				})
			if err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// oweAllRecords owes the record of the correction id to every replica of
// its scans' storages, in the transaction that publishes it.
func (k Sending) oweAllRecords(ctx context.Context, tx SendingTx, id string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO correction_records_owed (correction_id, storage, owed_at)
		SELECT ?, name, ? FROM storages WHERE replica_of <> '' AND replica_of IN
		(SELECT storage FROM correction_scans WHERE correction_id = ?) ON CONFLICT DO NOTHING`, id, k.D.Time(now), id)
	return err
}

// OweRecord implements tenant.Corrections.
func (k Sending) OweRecord(ctx context.Context, id, replica string, now time.Time) error {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	return k.stepRow(ctx, id, "the correction's record owed cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		_, err := tx.Exec(ctx, `INSERT INTO correction_records_owed (correction_id, storage, owed_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, id, replica, k.D.Time(now))
		return err
	})
}

// MarkRecordWritten implements tenant.Corrections.
func (k Sending) MarkRecordWritten(ctx context.Context, id, replica string, state corr.State) error {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return err
	}
	return k.stepRow(ctx, id, "the correction's record written cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		// The row read locked: a step that moved the correction on since the
		// record was made commits before this or after, and either way the
		// record of the new state stays owed.
		if c.State != state {
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM correction_records_owed WHERE correction_id = ? AND storage = ?`, id, replica)
		return err
	})
}

// RecordsOwed implements tenant.Corrections.
func (k Sending) RecordsOwed(ctx context.Context, after tenant.RecordOwed, limit int) ([]tenant.RecordOwed, error) {
	if err := tenant.ValidateRecordsOwed(after, limit); err != nil {
		return nil, err
	}
	var out []tenant.RecordOwed
	err := k.Host.Read(ctx, "the records owed cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT correction_id, storage, owed_at FROM correction_records_owed
			WHERE correction_id > ? OR (correction_id = ? AND storage > ?) ORDER BY correction_id, storage LIMIT ?`,
			[]any{after.Correction, after.Correction, after.Replica, limit}, func(r Row) error {
				var o tenant.RecordOwed
				var since DBTime
				err := r.Scan(&o.Correction, &o.Replica, &since)
				o.Since = since.T
				out = append(out, o)
				return err
			})
	})
	return out, err
}

// DropRecordOwed implements tenant.Corrections.
func (k Sending) DropRecordOwed(ctx context.Context, id, storage string) (bool, error) {
	if err := tenant.ValidateName("storage", storage); err != nil {
		return false, err
	}
	var dropped bool
	err := k.stepRow(ctx, id, "the correction's record owed cannot be dropped", func(tx SendingTx, c *tenant.Correction) error {
		// Decided in the transaction, under the configuration lock an
		// attach takes too: a storage attached since keeps what it is owed.
		n, err := tx.Exec(ctx, `DELETE FROM correction_records_owed WHERE correction_id = ? AND storage = ? AND NOT EXISTS
			(SELECT 1 FROM storages AS s JOIN correction_scans AS cs ON cs.storage = s.replica_of
				WHERE s.name = ? AND s.replica_of <> '' AND cs.correction_id = ?)`, id, storage, storage, id)
		dropped = n > 0
		return err
	})
	return dropped, err
}

// PurgedTargets implements tenant.Corrections.
func (k Sending) PurgedTargets(ctx context.Context, id string, after tenant.PurgeTarget, limit int) ([]tenant.PurgeTarget, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxPurgeTargets {
		return nil, fault.New(fault.Invalid, "a limit of purge targets is 1 to 1000")
	}
	var out []tenant.PurgeTarget
	err := k.Host.Read(ctx, "the correction's purged objects cannot be read", func(tx SendingTx) error {
		out = nil
		if err := checkNoSwitch(ctx, tx, id); err != nil {
			return err
		}
		// The order is the primary key's, which the row value after reads
		// on from.
		return tx.Query(ctx, `SELECT ord, storage, subject_label, session_label, scan_label, sop_instance_uid, storage_key, size, sha256
			FROM correction_keys WHERE correction_id = ? AND outcome = 'purged'
			AND (ord, storage, subject_label, session_label, scan_label, sop_instance_uid) > (?, ?, ?, ?, ?, ?)
			ORDER BY ord, storage, subject_label, session_label, scan_label, sop_instance_uid LIMIT ?`,
			[]any{id, after.Ord, after.Storage, after.Path.Subject, after.Path.Session, after.Path.Scan, after.UID, limit},
			func(r Row) error {
				var t tenant.PurgeTarget
				var sum []byte
				if err := r.Scan(&t.Ord, &t.Storage, &t.Path.Subject, &t.Path.Session, &t.Path.Scan, &t.UID, &t.StorageKey, &t.Size,
					&sum); err != nil {
					return err
				}
				t.SHA256 = ToSHA256(sum)
				out = append(out, t)
				return nil
			})
	})
	return out, err
}

// NotePurge implements tenant.Corrections.
func (k Sending) NotePurge(ctx context.Context, id, why string) error {
	if err := tenant.ValidatePurgeNote(why); err != nil {
		return err
	}
	return k.stepRow(ctx, id, "the purge's state reason cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.State != corr.StatePurging || c.StateReason == why {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE corrections SET state_reason = ? WHERE id = ?`, why, id)
		return err
	})
}

// fillCopies gives c the replicas a detach abandoned and those still owed
// its record.
func fillCopies(ctx context.Context, tx SendingTx, c *tenant.Correction) error {
	c.CopiesAbandoned, c.RecordsOwed = nil, nil
	for _, x := range []struct {
		query string
		to    *[]string
	}{
		{`SELECT storage FROM correction_copies_abandoned WHERE correction_id = ? ORDER BY storage`, &c.CopiesAbandoned},
		{`SELECT storage FROM correction_records_owed WHERE correction_id = ? ORDER BY storage`, &c.RecordsOwed},
	} {
		err := tx.Query(ctx, x.query, []any{c.ID}, func(r Row) error {
			var s string
			err := r.Scan(&s)
			*x.to = append(*x.to, s)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// abandonCopies records, in the transaction of a detach with abandon, that
// each correction of ids no longer reaches the replica: the replica named
// in the correction's copies abandoned with the reason, and each object the
// correction owns that the replica's removal of is not recorded (an object
// not settled, nor done on the replica) recorded unknown there, at the
// replica's generation and naming its primary, both read before the
// detach clears them. What the replica was owed is the detach's to drop.
func (k Sending) abandonCopies(ctx context.Context, tx SendingTx, replica tenant.Storage, ids []string, reason string,
	now time.Time) error {
	var generation int64
	if _, err := tx.QueryRow(ctx, `SELECT generation FROM storage_generations WHERE storage = ?`, []any{replica.Name},
		&generation); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO correction_copies_abandoned (correction_id, storage, reason, abandoned_at)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, id, replica.Name, reason, k.D.Time(now)); err != nil {
			return err
		}
		var targets []tenant.PurgeTarget
		var dones [][]tenant.CopyDone
		err := tx.Query(ctx, `SELECT ord, storage, subject_label, session_label, scan_label, sop_instance_uid, storage_key, size,
			sha256, copies_done FROM correction_keys WHERE correction_id = ? AND outcome = '' AND storage = ?
			ORDER BY ord, subject_label, session_label, scan_label, sop_instance_uid`, []any{id, replica.ReplicaOf}, func(r Row) error {
			var t tenant.PurgeTarget
			var sum []byte
			var done string
			if err := r.Scan(&t.Ord, &t.Storage, &t.Path.Subject, &t.Path.Session, &t.Path.Scan, &t.UID, &t.StorageKey, &t.Size,
				&sum, &done); err != nil {
				return err
			}
			t.SHA256 = ToSHA256(sum)
			targets, dones = append(targets, t), append(dones, tenant.ParseCopiesDone(done))
			return nil
		})
		if err != nil {
			return err
		}
		for i, t := range targets {
			// Removed there only when recorded at the replica's generation
			// now: at another, the name held another place.
			if slices.Contains(dones[i], tenant.CopyDone{Storage: replica.Name, Generation: generation}) {
				continue
			}
			key, err := t.Key()
			if err != nil {
				return err
			}
			if err := k.insertUnknown(ctx, tx, id, replica, generation, key, t.Size, t.SHA256, now); err != nil {
				return err
			}
		}
		if err := k.abandonUnseeded(ctx, tx, id, replica, generation, now); err != nil {
			return err
		}
	}
	return nil
}

// abandonUnseeded records unknown on the replica, for a delete whose purge
// has not yet fixed the objects of a scan of its on the replica's primary
// (one known only from its record, after a restore, whose objects its purge
// reads from the scan's manifest), that scan's manifest and metadata keys:
// what the replica holds of it is not known here, but it is not claimed
// removed, and the manifest names the rest.
func (k Sending) abandonUnseeded(ctx context.Context, tx SendingTx, id string, replica tenant.Storage, generation int64,
	now time.Time) error {
	var kind string
	if _, err := tx.QueryRow(ctx, `SELECT kind FROM corrections WHERE id = ?`, []any{id}, &kind); err != nil {
		return err
	}
	if kind != string(corr.KindDelete) {
		return nil
	}
	var paths []label.Path
	err := tx.Query(ctx, `SELECT cs.subject_label, cs.session_label, cs.scan_label FROM correction_scans AS cs
		WHERE cs.correction_id = ? AND cs.storage = ? AND NOT EXISTS (SELECT 1 FROM correction_keys AS ck
			WHERE ck.correction_id = cs.correction_id AND ck.storage = cs.storage AND ck.subject_label = cs.subject_label
			AND ck.session_label = cs.session_label AND ck.scan_label = cs.scan_label)
		ORDER BY cs.subject_label, cs.session_label, cs.scan_label`, []any{id, replica.ReplicaOf}, func(r Row) error {
		var p label.Path
		err := r.Scan(&p.Subject, &p.Session, &p.Scan)
		paths = append(paths, p)
		return err
	})
	if err != nil {
		return err
	}
	for _, p := range paths {
		for _, keyOf := range []func(label.Path) (string, error){layout.ManifestKey, layout.MetadataKey} {
			key, err := keyOf(p)
			if err != nil {
				return fault.Wrap(fault.Internal, err, "a scan's key cannot be made")
			}
			if err := k.insertUnknown(ctx, tx, id, replica, generation, key, 0, tenant.SHA256{}, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// insertUnknown records an object of the correction id unknown on a
// replica a detach abandons, at its generation and naming its primary.
func (k Sending) insertUnknown(ctx context.Context, tx SendingTx, id string, replica tenant.Storage, generation int64, key string,
	size int64, sha tenant.SHA256, now time.Time) error {
	var sum any
	if !sha.IsZero() {
		sum = sha[:]
	}
	_, err := tx.Exec(ctx, `INSERT INTO correction_objects (correction_id, storage, generation, replica_of, object_key,
		version, size, sha256, disposition, retain_until, purged_at) VALUES (?, ?, ?, ?, ?, '', ?, ?, 'unknown', NULL, ?)
		ON CONFLICT DO NOTHING`, id, replica.Name, generation, replica.ReplicaOf, key, size, sum, k.D.Time(now))
	return err
}
