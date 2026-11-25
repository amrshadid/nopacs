package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The replicas' rows (tenant.Replication) as the transactions that publish a
// scan's change write them, on the replica_scans, replica_damage and
// replica_totals tables of each driver's tenant baseline schema. Every
// change of a row moves its replica's counts in the same transaction
// (addTotals), or counts them again from the rows (recountTotals) where a
// change touches many.

// replicaRowColumns are a row's columns as the store reads them, with its
// damaged objects counted.
const replicaRowColumns = `replica, series_uid, state, manifest_sha256, queued_at, due, verified_at, backfill, attempt, last_error,
	seq, claimed_seq, (SELECT count(*) FROM replica_damage AS d WHERE d.replica = replica_scans.replica
	AND d.series_uid = replica_scans.series_uid)`

// replicaRow is a row as the store reads it, with what only the store
// needs.
type replicaRow struct {
	tenant.ReplicaScan
	seq, claimed int64
}

// scanReplicaRow reads one row of replicaRowColumns.
func scanReplicaRow(r Row) (replicaRow, error) {
	var out replicaRow
	var state string
	var manifest []byte
	var queued, due, verified DBTime
	err := r.Scan(&out.Replica, &out.SeriesUID, &state, &manifest, &queued, &due, &verified, &out.Backfill, &out.Attempt,
		&out.LastError, &out.seq, &out.claimed, &out.Damaged)
	out.State, out.ManifestSHA256 = tenant.ReplicaScanState(state), ToSHA256(manifest)
	out.Queued, out.Due, out.Verified = queued.T, due.T, verified.T
	return out, err
}

// replicaRowIn reads a replica's row of a series in tx, with the lock
// given, and whether there is one.
func replicaRowIn(ctx context.Context, tx SendingTx, replica, series, lock string) (replicaRow, bool, error) {
	var out replicaRow
	found := false
	err := tx.Query(ctx, `SELECT `+replicaRowColumns+` FROM replica_scans WHERE replica = ? AND series_uid = ?`+lock,
		[]any{replica, series}, func(r Row) error {
			var err error
			out, err = scanReplicaRow(r)
			found = true
			return err
		})
	return out, found, err
}

// totals are changes of a replica's counts.
type totals struct {
	pending, copying, inStep, damaged, objects, backfill int64
}

// move counts one row leaving state from and reaching state to; the empty
// state is no row.
func (t *totals) move(from, to tenant.ReplicaScanState) {
	for _, s := range []struct {
		state tenant.ReplicaScanState
		by    int64
	}{{from, -1}, {to, 1}} {
		switch s.state {
		case tenant.ReplicaPending:
			t.pending += s.by
		case tenant.ReplicaCopying:
			t.copying += s.by
		case tenant.ReplicaInStep:
			t.inStep += s.by
		case tenant.ReplicaDamaged:
			t.damaged += s.by
		}
	}
}

// addTotals adds t to the replica's counts, and records its backfill done
// at now when t ends what it owed, or not done when t owes more.
func (k Sending) addTotals(ctx context.Context, tx SendingTx, replica string, t totals, now time.Time) error {
	if t == (totals{}) {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE replica_totals SET pending = pending + ?, copying = copying + ?, in_step = in_step + ?,
		damaged = damaged + ?, objects_damaged = objects_damaged + ?, backfill = backfill + ?,
		backfill_done_at = CASE WHEN backfill + ? = 0 AND backfill > 0 THEN ? WHEN backfill + ? > 0 THEN NULL
		ELSE backfill_done_at END WHERE replica = ?`,
		t.pending, t.copying, t.inStep, t.damaged, t.objects, t.backfill, t.backfill, k.D.Time(now), t.backfill, replica)
	return err
}

// recountTotals counts the replica's counts again from its rows ("" for
// every replica), its backfill done at now when it owes nothing and was
// not done already.
func (k Sending) recountTotals(ctx context.Context, tx SendingTx, replica string, now time.Time) error {
	const count = `(SELECT count(*) FROM replica_scans AS x WHERE x.replica = replica_totals.replica AND `
	q := `UPDATE replica_totals SET pending = ` + count + `x.state = 'pending'), copying = ` + count + `x.state = 'copying'),
		in_step = ` + count + `x.state = 'in_step'), damaged = ` + count + `x.state = 'damaged'),
		objects_damaged = (SELECT count(*) FROM replica_damage AS d WHERE d.replica = replica_totals.replica),
		backfill = ` + count + `x.backfill = ?),
		backfill_done_at = CASE WHEN EXISTS (SELECT 1 FROM replica_scans AS x WHERE x.replica = replica_totals.replica
		AND x.backfill = ?) THEN NULL ELSE coalesce(backfill_done_at, ?) END`
	args := []any{k.D.Bool(true), k.D.Bool(true), k.D.Time(now)}
	if replica != "" {
		q += ` WHERE replica = ?`
		args = append(args, replica)
	}
	_, err := tx.Exec(ctx, q, args...)
	return err
}

// beginReplica begins what replica holds as a replica of primary: its
// counts, and every scan of the primary pending for it as its backfill. It
// answers the scans queued.
func (k Sending) beginReplica(ctx context.Context, tx SendingTx, replica, primary string, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO replica_totals (replica, pending, copying, in_step, damaged, objects_damaged, backfill,
		attached_at, backfill_done_at, last_fault, last_fault_at) VALUES (?, 0, 0, 0, 0, 0, 0, ?, NULL, '', NULL)`,
		replica, k.D.Time(now)); err != nil {
		return 0, err
	}
	n, err := k.queueAll(ctx, tx, replica, primary, now)
	if err != nil {
		return 0, err
	}
	return n, k.recountTotals(ctx, tx, replica, now)
}

// queueAll gives every scan of primary that has no row of replica's one,
// pending and owed to its backfill, and answers how many. A scan a delete
// hid is not in the catalog's scans, so it is never queued.
func (k Sending) queueAll(ctx context.Context, tx SendingTx, replica, primary string, now time.Time) (int64, error) {
	return tx.Exec(ctx, `INSERT INTO replica_scans (replica, series_uid, state, manifest_sha256, queued_at, due, seq, claimed_seq,
		attempt, owner, lease_until, verified_at, backfill, last_error)
		SELECT ?, s.series_uid, 'pending', NULL, ?, ?, 1, 0, 0, '', NULL, NULL, ?, '' FROM scans AS s
		WHERE s.storage = ? AND NOT EXISTS (SELECT 1 FROM replica_scans AS x WHERE x.replica = ? AND x.series_uid = s.series_uid)`,
		replica, k.D.Time(now), k.D.Time(now), k.D.Bool(true), primary, replica)
}

// QueueReplicaCopies is what the transaction that publishes a change of the
// scan of seriesUID on storage does for its replicas: it refuses, as a
// precondition failure, a storage that is a replica or of the archive tier
// (nothing is archived to one first), and makes the scan pending for each
// replica of the storage, or counts the change on a row pending, copying
// or damaged already, so a copy claimed before it never ends in step. One
// lookup of storages by name and one of its replicas by the partial index
// storages_by_primary, which holds the replicas alone: a tenant without a
// replica pays the two.
//
// The caller's transaction must already hold the storage's row against
// every change (a commit reads it so, before it writes the scan), which an
// attach's or a new replica's lock of its primary's row (Lock) waits for,
// and which waits for that lock: on a database with several writers at
// read committed, an attach whose backfill could not see the scan, not yet
// committed, would otherwise commit beside a commit whose read of the
// replicas could not see the attach, and the scan would be owed to no
// replica. Whichever takes the row second reads, once the first commits,
// what the first wrote.
func (k Sending) QueueReplicaCopies(ctx context.Context, tx SendingTx, storage, seriesUID string, now time.Time) error {
	if err := CheckArchiveTarget(ctx, tx, "", storage); err != nil {
		return err
	}
	replicas, err := replicasOf(ctx, tx, storage)
	if err != nil {
		return err
	}
	for _, r := range replicas {
		if err := k.queueScan(ctx, tx, r, seriesUID, now); err != nil {
			return err
		}
	}
	return nil
}

// queueScan makes the scan of series pending for replica, or counts the
// change on its row.
func (k Sending) queueScan(ctx context.Context, tx SendingTx, replica, series string, now time.Time) error {
	n, err := tx.Exec(ctx, `INSERT INTO replica_scans (replica, series_uid, state, manifest_sha256, queued_at, due, seq, claimed_seq,
		attempt, owner, lease_until, verified_at, backfill, last_error) VALUES (?, ?, 'pending', NULL, ?, ?, 1, 0, 0, '', NULL, NULL, ?, '')
		ON CONFLICT (replica, series_uid) DO NOTHING`, replica, series, k.D.Time(now), k.D.Time(now), k.D.Bool(false))
	if err != nil {
		return err
	}
	if n == 1 {
		return k.addTotals(ctx, tx, replica, totals{pending: 1}, now)
	}
	row, found, err := replicaRowIn(ctx, tx, replica, series, k.D.Lock)
	if err != nil || !found {
		return err
	}
	if row.State != tenant.ReplicaInStep {
		_, err := tx.Exec(ctx, `UPDATE replica_scans SET seq = seq + 1 WHERE replica = ? AND series_uid = ?`, replica, series)
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'pending', queued_at = ?, due = ?, seq = seq + 1
		WHERE replica = ? AND series_uid = ?`, k.D.Time(now), k.D.Time(now), replica, series); err != nil {
		return err
	}
	var t totals
	t.move(tenant.ReplicaInStep, tenant.ReplicaPending)
	return k.addTotals(ctx, tx, replica, t, now)
}

// dropScan removes every replica's row of the scan of series, and its
// damage, with their counts: a scan the catalog no longer holds is nothing a
// replica is asked to hold.
func (k Sending) dropScan(ctx context.Context, tx SendingTx, series string, now time.Time) error {
	var rows []replicaRow
	err := tx.Query(ctx, `SELECT `+replicaRowColumns+` FROM replica_scans WHERE series_uid = ?`+k.D.Lock, []any{series},
		func(r Row) error {
			row, err := scanReplicaRow(r)
			rows = append(rows, row)
			return err
		})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := k.dropRow(ctx, tx, row, now); err != nil {
			return err
		}
	}
	return nil
}

// dropRow removes one row and its damage, with their counts.
func (k Sending) dropRow(ctx context.Context, tx SendingTx, row replicaRow, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE replica = ? AND series_uid = ?`, row.Replica, row.SeriesUID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM replica_scans WHERE replica = ? AND series_uid = ?`, row.Replica, row.SeriesUID); err != nil {
		return err
	}
	t := totals{objects: -row.Damaged}
	t.move(row.State, "")
	if row.Backfill {
		t.backfill = -1
	}
	return k.addTotals(ctx, tx, row.Replica, t, now)
}

// QueueCorrectionCopies is what the transaction that publishes or undoes a
// correction does for the replicas: each scan the correction touches that
// the catalog still holds is queued again for the replicas of its storage
// (a relabel's new keys, a replace's new revision, an instance deleted from
// it), and each it hid whole is dropped from every replica's rows.
func (k Sending) QueueCorrectionCopies(ctx context.Context, tx SendingTx, correction string, now time.Time) error {
	var series []string
	err := tx.Query(ctx, `SELECT series_uid FROM correction_scans WHERE correction_id = ? ORDER BY series_uid`, []any{correction},
		func(r Row) error {
			var s string
			err := r.Scan(&s)
			series = append(series, s)
			return err
		})
	if err != nil {
		return err
	}
	for _, s := range series {
		var storage string
		found, err := tx.QueryRow(ctx, `SELECT storage FROM scans WHERE series_uid = ?`, []any{s}, &storage)
		if err != nil {
			return err
		}
		if !found {
			if err := k.dropScan(ctx, tx, s, now); err != nil {
				return err
			}
			continue
		}
		replicas, err := replicasOf(ctx, tx, storage)
		if err != nil {
			return err
		}
		for _, r := range replicas {
			if err := k.queueScan(ctx, tx, r, s, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// sameManifest is the condition that a row's manifest digest is its scan's
// in the catalog (s), NULL equal to NULL: a scan committed without one is
// in step on a replica that holds it without one.
const sameManifest = `(r.manifest_sha256 = s.manifest_sha256 OR (r.manifest_sha256 IS NULL AND s.manifest_sha256 IS NULL))`

// ResyncReplicas makes the replicas' rows agree with the catalog in use, in
// the caller's transaction, after a rebuild made it again
// (tenant.Replication, "Rebuilds and restores"): a row whose scan is not on
// its replica's primary is dropped with its damage, a row in step at
// another digest than its scan's manifest is pending, and a scan of a
// primary with replicas that has no row of one of them gets a pending row;
// then every replica's counts are counted again.
func (k Sending) ResyncReplicas(ctx context.Context, tx SendingTx, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM replica_scans WHERE NOT EXISTS (SELECT 1 FROM scans AS s
		JOIN storages AS p ON p.replica_of = s.storage WHERE s.series_uid = replica_scans.series_uid
		AND p.name = replica_scans.replica)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE NOT EXISTS (SELECT 1 FROM replica_scans AS x
		WHERE x.replica = replica_damage.replica AND x.series_uid = replica_damage.series_uid)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'pending', queued_at = ?, due = ?, seq = seq + 1
		WHERE state = 'in_step' AND NOT EXISTS (SELECT 1 FROM scans AS s WHERE s.series_uid = replica_scans.series_uid
		AND (s.manifest_sha256 = replica_scans.manifest_sha256
		OR (s.manifest_sha256 IS NULL AND replica_scans.manifest_sha256 IS NULL)))`, k.D.Time(now), k.D.Time(now)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO replica_scans (replica, series_uid, state, manifest_sha256, queued_at, due, seq,
		claimed_seq, attempt, owner, lease_until, verified_at, backfill, last_error)
		SELECT p.name, s.series_uid, 'pending', NULL, ?, ?, 1, 0, 0, '', NULL, NULL, ?, '' FROM storages AS p
		JOIN scans AS s ON s.storage = p.replica_of WHERE p.replica_of <> '' AND NOT EXISTS (SELECT 1 FROM replica_scans AS x
		WHERE x.replica = p.name AND x.series_uid = s.series_uid)`, k.D.Time(now), k.D.Time(now), k.D.Bool(false)); err != nil {
		return err
	}
	return k.recountTotals(ctx, tx, "", now)
}
