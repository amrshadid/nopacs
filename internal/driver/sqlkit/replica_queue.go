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
	AND d.series_uid = replica_scans.series_uid), lease_until`

// replicaRow is a row as the store reads it, with what only the store
// needs.
type replicaRow struct {
	tenant.ReplicaScan
	seq, claimed int64
	// leaseUntil is the due of a copying row's lease.
	leaseUntil time.Time
}

// scanReplicaRow reads one row of replicaRowColumns.
func scanReplicaRow(r Row) (replicaRow, error) {
	var out replicaRow
	var state string
	var manifest []byte
	var queued, due, verified, lease DBTime
	err := r.Scan(&out.Replica, &out.SeriesUID, &state, &manifest, &queued, &due, &verified, &out.Backfill, &out.Attempt,
		&out.LastError, &out.seq, &out.claimed, &out.Damaged, &lease)
	out.State, out.ManifestSHA256 = tenant.ReplicaScanState(state), ToSHA256(manifest)
	out.Queued, out.Due, out.Verified, out.leaseUntil = queued.T, due.T, verified.T, lease.T
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
	if err != nil || t.backfill == 0 {
		return err
	}
	return k.followReplicateJobs(ctx, tx, replica, now)
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
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return err
	}
	return k.followReplicateJobs(ctx, tx, replica, now)
}

// beginReplica begins what replica holds as a replica of primary: its
// counts, every scan of the primary pending for it as its backfill, and
// the record owed to it of every committed or purging correction with a
// scan on the primary, which the outbox writes and a switch to it waits
// for: a rebuild from the replica alone would otherwise not know a delete
// published before the attach, and show what it hid. It answers the scans
// queued.
func (k Sending) beginReplica(ctx context.Context, tx SendingTx, replica, primary string, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO replica_totals (replica, pending, copying, in_step, damaged, objects_damaged, backfill,
		attached_at, backfill_done_at, last_fault, last_fault_at) VALUES (?, 0, 0, 0, 0, 0, 0, ?, NULL, '', NULL)`,
		replica, k.D.Time(now)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO correction_records_owed (correction_id, storage, owed_at)
		SELECT c.id, ?, ? FROM corrections AS c WHERE c.state IN ('committed', 'purging')
		AND EXISTS (SELECT 1 FROM correction_scans AS cs WHERE cs.correction_id = c.id AND cs.storage = ?) ON CONFLICT DO NOTHING`,
		replica, k.D.Time(now), primary); err != nil {
		return 0, err
	}
	n, err := k.queueAll(ctx, tx, replica, primary, now)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		// The backfill followed as the server's own job, so a person sees
		// when the copy of what the primary held is done.
		if err := k.insertReplicateJob(ctx, tx, tenant.ReplicateJobID(replica, now), "", tenant.ReplicateJob{Replica: replica,
			Primary: primary, Scheduled: true}, n, now); err != nil {
			return 0, err
		}
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
		// A row waiting out a purge (EndReplicaScan: no failure, due
		// later) is due at once: the change is a new arrival to copy, or
		// the purge's end giving the scan its new manifest. A row waiting
		// after a failure keeps its retry time.
		_, err := tx.Exec(ctx, `UPDATE replica_scans SET seq = seq + 1,
			due = CASE WHEN state IN ('pending', 'damaged') AND last_error = '' AND due > ? THEN ? ELSE due END
			WHERE replica = ? AND series_uid = ?`, k.D.Time(now), k.D.Time(now), replica, series)
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
// replica is asked to hold. A copy of a row it drops loses its lease (its
// next renewal or write finds no row), and dropScan answers the latest due
// among the leases of the rows that were being copied, zero for none: a put
// of such a copy may land until then (tenant.ReplicaPutGrace after it).
func (k Sending) dropScan(ctx context.Context, tx SendingTx, series string, now time.Time) (time.Time, error) {
	var rows []replicaRow
	err := tx.Query(ctx, `SELECT `+replicaRowColumns+` FROM replica_scans WHERE series_uid = ?`+k.D.Lock, []any{series},
		func(r Row) error {
			row, err := scanReplicaRow(r)
			rows = append(rows, row)
			return err
		})
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, row := range rows {
		if row.State == tenant.ReplicaCopying && row.leaseUntil.After(latest) {
			latest = row.leaseUntil
		}
		if err := k.dropRow(ctx, tx, row, now); err != nil {
			return time.Time{}, err
		}
	}
	return latest, nil
}

// endCopies ends the lease of every replica's row being copied of the
// scans a correction touches: the row's attempt moves on, so its copier's
// next renewal or write is tenant.ErrLeaseLost and it stops; the row is due
// again now (damaged while damage is left), its change counted, and the
// next claim copies it as the catalog says then. It answers the latest due
// among the leases it ended, zero for none.
func (k Sending) endCopies(ctx context.Context, tx SendingTx, correction string, now time.Time) (time.Time, error) {
	var rows []replicaRow
	err := tx.Query(ctx, `SELECT `+replicaRowColumns+` FROM replica_scans WHERE state = 'copying'
		AND series_uid IN (SELECT series_uid FROM correction_scans WHERE correction_id = ?)
		ORDER BY replica, series_uid`+k.D.Lock, []any{correction}, func(r Row) error {
		row, err := scanReplicaRow(r)
		rows = append(rows, row)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, row := range rows {
		if row.leaseUntil.After(latest) {
			latest = row.leaseUntil
		}
		next := afterCopy(row)
		if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = ?, seq = seq + 1, attempt = attempt + 1, owner = '',
			lease_until = NULL, due = ?, queued_at = coalesce(queued_at, ?) WHERE replica = ? AND series_uid = ?`, string(next),
			k.D.Time(now), k.D.Time(now), row.Replica, row.SeriesUID); err != nil {
			return time.Time{}, err
		}
		var t totals
		t.move(tenant.ReplicaCopying, next)
		if err := k.addTotals(ctx, tx, row.Replica, t, now); err != nil {
			return time.Time{}, err
		}
	}
	return latest, nil
}

// keepCopiesInFlight keeps, on the correction of id, the last moment a put
// of a copy ended with its latest lease due at due can land
// (tenant.CopiesInFlightUntil), unless a later one is kept; a zero due keeps
// nothing.
func (k Sending) keepCopiesInFlight(ctx context.Context, tx SendingTx, id string, due time.Time) error {
	until := tenant.CopiesInFlightUntil(due)
	if until.IsZero() {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE corrections SET copies_in_flight_until = ? WHERE id = ?
		AND (copies_in_flight_until IS NULL OR copies_in_flight_until < ?)`, k.D.Time(until), id, k.D.Time(until))
	return err
}

// dropRow removes one row and its damage, with their counts.
func (k Sending) dropRow(ctx context.Context, tx SendingTx, row replicaRow, now time.Time) error {
	if err := k.clearDamageFindings(ctx, tx, `replica = ? AND series_uid = ?`, []any{row.Replica, row.SeriesUID}, "", now); err != nil {
		return err
	}
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
// it), and each it hid whole is dropped from every replica's rows. kept
// are, by replica then series, the manifests a relabel wrote under the new
// labels on a replica in step (tenant.CorrectionCommit.ReplicaManifests):
// such a row, in step still and at that digest the scan's manifest now, is
// moved to it in step rather than queued, since the replica holds the new
// keys verified before the publish, and a read may turn to it at once.
func (k Sending) QueueCorrectionCopies(ctx context.Context, tx SendingTx, correction string, now time.Time,
	kept map[string]map[string]tenant.SHA256) error {
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
	var latest time.Time
	for _, s := range series {
		var storage string
		var digest []byte
		found, err := tx.QueryRow(ctx, `SELECT storage, manifest_sha256 FROM scans WHERE series_uid = ?`, []any{s}, &storage, &digest)
		if err != nil {
			return err
		}
		if !found {
			due, err := k.dropScan(ctx, tx, s, now)
			if err != nil {
				return err
			}
			if due.After(latest) {
				latest = due
			}
			continue
		}
		replicas, err := replicasOf(ctx, tx, storage)
		if err != nil {
			return err
		}
		for _, r := range replicas {
			if sum, ok := kept[r][s]; ok && len(digest) > 0 && ToSHA256(digest) == sum {
				moved, err := tx.Exec(ctx, `UPDATE replica_scans SET manifest_sha256 = ?, verified_at = ?
					WHERE replica = ? AND series_uid = ? AND state = 'in_step'`, sum[:], k.D.Time(now), r, s)
				if err != nil {
					return err
				}
				if moved == 1 {
					continue
				}
			}
			if err := k.queueScan(ctx, tx, r, s, now); err != nil {
				return err
			}
		}
	}
	// A copy of a scan hidden whole may still be putting its objects: the
	// correction's purge waits until none can land.
	return k.keepCopiesInFlight(ctx, tx, correction, latest)
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
	gone := `NOT EXISTS (SELECT 1 FROM replica_scans AS x WHERE x.replica = replica_damage.replica
		AND x.series_uid = replica_damage.series_uid)`
	if err := k.clearDamageFindings(ctx, tx, gone, nil, "", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE `+gone); err != nil { // #nosec G202 -- gone is a constant above.
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
