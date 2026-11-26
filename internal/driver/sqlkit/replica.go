package sqlkit

import (
	"context"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's replicas (tenant.Replication) on a SQL database: attaching
// and detaching them, their status, and the copier's claims and writes,
// each claim fenced by its row's attempt as a unit of work is by its own.

// ListReplicas is the replicas' list's name, the first field of its cursor.
const ListReplicas = "tenant-replicas"

// replicaOf reads the storage named, which must be a replica: NotFound for
// a storage the tenant does not have, a precondition failure for one that
// is not a replica.
func replicaOf(ctx context.Context, tx SendingTx, name, lock string) (tenant.Storage, error) {
	s, found, err := storageIn(ctx, tx, name, lock)
	if err != nil {
		return s, err
	}
	if !found {
		return s, notFound("the tenant has no storage named " + name)
	}
	if s.ReplicaOf == "" {
		return s, tenant.Precondition("the storage " + name + " is not a replica")
	}
	return s, nil
}

// AttachReplica implements tenant.Replication.
func (k Sending) AttachReplica(ctx context.Context, replica, primary string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Storage, int64, bool, error) {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return tenant.Storage{}, 0, false, err
	}
	if err := tenant.ValidateName("storage", primary); err != nil {
		return tenant.Storage{}, 0, false, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Storage{}, 0, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, 0, false, err
	}
	var out tenant.Storage
	var queued int64
	var attached bool
	err := k.Host.Write(ctx, "the replica cannot be attached", func(tx SendingTx) error {
		queued, attached = 0, false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		r, found, err := storageIn(ctx, tx, replica, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + replica)
		}
		p, found, err := storageIn(ctx, tx, primary, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return tenant.Precondition("the tenant has no storage named " + primary + " to be the primary")
		}
		if r.ReplicaOf != primary {
			f, err := k.attachFacts(ctx, tx, r, p)
			if err != nil {
				return err
			}
			if err := tenant.CheckAttach(f); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE storages SET replica_of = ?, updated = ? WHERE name = ?`, primary, k.D.Time(now),
				replica); err != nil {
				return err
			}
			if queued, err = k.beginReplica(ctx, tx, replica, primary, now); err != nil {
				return err
			}
			if err := holdsAfterAttach(ctx, tx); err != nil {
				return err
			}
			attached = true
		}
		if out, _, err = storageIn(ctx, tx, replica, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, 0, false, err
	}
	return out, queued, attached, nil
}

// DetachReplica implements tenant.Replication.
func (k Sending) DetachReplica(ctx context.Context, d tenant.ReplicaDetach, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, []string, error) {
	if err := d.Validate(); err != nil {
		return tenant.Storage{}, nil, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, nil, err
	}
	var out tenant.Storage
	var abandoned []string
	err := k.Host.Write(ctx, "the replica cannot be detached", func(tx SendingTx) error {
		abandoned = nil
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		r, found, err := storageIn(ctx, tx, d.Replica, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + d.Replica)
		}
		switching := false
		if r.ReplicaOf != "" {
			if switching, err = switchRunning(ctx, tx, r.ReplicaOf); err != nil {
				return err
			}
			err = tx.Query(ctx, `SELECT DISTINCT c.id FROM corrections AS c JOIN correction_scans AS cs ON cs.correction_id = c.id
				WHERE c.state IN ('committed', 'purging') AND cs.storage = ? ORDER BY c.id`, []any{r.ReplicaOf}, func(row Row) error {
				var id string
				err := row.Scan(&id)
				abandoned = append(abandoned, id)
				return err
			})
			if err != nil {
				return err
			}
		}
		if err := tenant.CheckDetach(r, len(abandoned), d.Abandon, switching); err != nil {
			return err
		}
		// Each correction that counted the replica no longer reaches it:
		// its objects not removed there are recorded unknown, and it names
		// the replica among its copies abandoned. What the replica was owed
		// goes with it: nothing writes to a storage that is not a copy.
		if err := k.abandonCopies(ctx, tx, r, abandoned, d.Reason, d.Now); err != nil {
			return err
		}
		if err := k.clearDamageFindings(ctx, tx, `replica = ?`, []any{d.Replica}, "", d.Now); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM replica_damage WHERE replica = ?`, `DELETE FROM replica_scans WHERE replica = ?`,
			`DELETE FROM replica_totals WHERE replica = ?`, `DELETE FROM correction_records_owed WHERE storage = ?`} {
			if _, err := tx.Exec(ctx, q, d.Replica); err != nil {
				return err
			}
		}
		if err := DropHoldRecordsInTx(ctx, tx, d.Replica); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE storages SET replica_of = '', updated = ? WHERE name = ?`, k.D.Time(d.Now), d.Replica); err != nil {
			return err
		}
		if out, _, err = storageIn(ctx, tx, d.Replica, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, nil, err
	}
	return out, abandoned, nil
}

// BackfillReplica implements tenant.Replication.
func (k Sending) BackfillReplica(ctx context.Context, replica string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (int64, error) {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return 0, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return 0, err
	}
	if err := audit.Validate(); err != nil {
		return 0, err
	}
	var marked int64
	err := k.Host.Write(ctx, "the replica's backfill cannot be queued", func(tx SendingTx) error {
		marked = 0
		r, err := replicaOf(ctx, tx, replica, k.D.Lock)
		if err != nil {
			return err
		}
		added, err := k.queueAll(ctx, tx, replica, r.ReplicaOf, now)
		if err != nil {
			return err
		}
		again, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'pending', queued_at = ?, due = ?, seq = seq + 1, backfill = ?
			WHERE replica = ? AND state = 'in_step'`, k.D.Time(now), k.D.Time(now), k.D.Bool(true), replica)
		if err != nil {
			return err
		}
		owed, err := tx.Exec(ctx, `UPDATE replica_scans SET backfill = ? WHERE replica = ? AND backfill = ?`, k.D.Bool(true),
			replica, k.D.Bool(false))
		if err != nil {
			return err
		}
		marked = added + again + owed
		// The backfill is a new one: it is done when it owes nothing more.
		if _, err := tx.Exec(ctx, `UPDATE replica_totals SET backfill_done_at = NULL WHERE replica = ?`, replica); err != nil {
			return err
		}
		if err := k.recountTotals(ctx, tx, replica, now); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return 0, err
	}
	return marked, nil
}

// replicaStatusColumns are a replica's status as ListReplicas reads it: the
// replica and its primary's places (for SharesBucket), its counts, its
// oldest row not in step (replica_scans_lagging) and its recalls not ended
// (recalls_by_state).
const replicaStatusColumns = `r.name, r.replica_of, r.tier, r.type, r.s3_endpoint, r.s3_bucket,
	coalesce(p.type, ''), coalesce(p.s3_endpoint, ''), coalesce(p.s3_bucket, ''),
	t.pending, t.copying, t.in_step, t.damaged, t.objects_damaged, t.backfill, t.attached_at, t.backfill_done_at,
	t.last_fault, t.last_fault_at,
	(SELECT min(q.queued_at) FROM replica_scans AS q WHERE q.replica = r.name AND q.queued_at IS NOT NULL),
	(SELECT count(*) FROM recalls AS c WHERE c.state IN ('requested', 'in_progress') AND c.storage = r.name)`

// scanReplicaStatus reads one row of replicaStatusColumns.
func scanReplicaStatus(row Row) (tenant.ReplicaStatus, error) {
	var s tenant.ReplicaStatus
	var tier string
	var rt, pt tenant.StorageType
	var rType, rEnd, rBucket, pType, pEnd, pBucket string
	var attached, done, faultAt, oldest DBTime
	err := row.Scan(&s.Replica, &s.Primary, &tier, &rType, &rEnd, &rBucket, &pType, &pEnd, &pBucket, &s.Pending, &s.Copying,
		&s.InStep, &s.Damaged, &s.ObjectsDamaged, &s.BackfillPending, &attached, &done, &s.LastFault, &faultAt, &oldest,
		&s.RecallsInProgress)
	rt, pt = tenant.StorageType(rType), tenant.StorageType(pType)
	s.Tier = tenant.StorageTier(tier)
	s.SharesBucket = tenant.SharesBucket(tenant.Storage{Type: rt, S3: tenant.S3Storage{Endpoint: rEnd, Bucket: rBucket}},
		tenant.Storage{Type: pt, S3: tenant.S3Storage{Endpoint: pEnd, Bucket: pBucket}})
	s.Attached, s.BackfillDone, s.LastFaultTime, s.OldestQueued = attached.T, done.T, faultAt.T, oldest.T
	return s, err
}

// ListReplicas implements tenant.Replication.
func (k Sending) ListReplicas(ctx context.Context, primary string, page master.Page) ([]tenant.ReplicaStatus, string, error) {
	if primary != "" {
		if err := tenant.ValidateName("storage", primary); err != nil {
			return nil, "", err
		}
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(primary)
	after := []string{"", ""}
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListReplicas, binding, 2)
		if err != nil {
			return nil, "", err
		}
		after = f
	}
	where := []string{"r.replica_of <> ''", "(r.replica_of, r.name) > (?, ?)"}
	args := []any{after[0], after[1]}
	if primary != "" {
		where, args = append(where, "r.replica_of = ?"), append(args, primary)
	}
	// #nosec G202 -- every fragment is a constant; values are parameters.
	query := `SELECT ` + replicaStatusColumns + ` FROM storages AS r JOIN replica_totals AS t ON t.replica = r.name
		LEFT JOIN storages AS p ON p.name = r.replica_of WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY r.replica_of, r.name LIMIT ?`
	args = append(args, page.Limit+1)
	var out []tenant.ReplicaStatus
	err := k.Host.Read(ctx, "the replicas cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, query, args, func(r Row) error {
			s, err := scanReplicaStatus(r)
			out = append(out, s)
			return err
		})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListReplicas, binding, last.Primary, last.Replica), nil
}

// ReplicaScan implements tenant.Replication.
func (k Sending) ReplicaScan(ctx context.Context, replica, seriesUID string) (tenant.ReplicaScan, error) {
	if err := tenant.ValidateName("storage", replica); err != nil {
		return tenant.ReplicaScan{}, err
	}
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return tenant.ReplicaScan{}, err
	}
	var out tenant.ReplicaScan
	err := k.Host.Read(ctx, "the replica's scan cannot be read", func(tx SendingTx) error {
		row, found, err := replicaRowIn(ctx, tx, replica, seriesUID, "")
		if err != nil {
			return err
		}
		if !found {
			return notFound("the replica holds no row of this scan")
		}
		out = row.ReplicaScan
		return nil
	})
	return out, err
}

// InStepReplicas implements tenant.Replication.
func (k Sending) InStepReplicas(ctx context.Context, seriesUIDs []string) (map[string][]string, error) {
	if err := tenant.ValidateReplicaLookup(seriesUIDs); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	if len(seriesUIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(seriesUIDs)), ", ")
	args := make([]any, len(seriesUIDs))
	for i, u := range seriesUIDs {
		args[i] = u
	}
	// #nosec G202 -- marks are placeholders only; values are parameters.
	query := `SELECT r.series_uid, r.replica FROM replica_scans AS r JOIN scans AS s ON s.series_uid = r.series_uid
		JOIN storages AS p ON p.name = r.replica AND p.replica_of = s.storage
		WHERE r.series_uid IN (` + marks + `) AND r.state = 'in_step' AND ` + sameManifest + ` ORDER BY r.series_uid, r.replica`
	err := k.Host.Read(ctx, "the replicas in step cannot be read", func(tx SendingTx) error {
		clear(out)
		return tx.Query(ctx, query, args, func(r Row) error {
			var series, replica string
			err := r.Scan(&series, &replica)
			out[series] = append(out[series], replica)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claimedScan is what the catalog says of a claimed row's scan.
type claimedScan struct {
	study, storage string
	path           label.Path
	manifest       []byte
}

// scanOfSeries reads the catalog's scan of a series, and whether it holds
// one.
func scanOfSeries(ctx context.Context, tx SendingTx, series string) (claimedScan, bool, error) {
	var s claimedScan
	found, err := tx.QueryRow(ctx, `SELECT study_uid, storage, subject_label, session_label, label, manifest_sha256 FROM scans
		WHERE series_uid = ?`, []any{series}, &s.study, &s.storage, &s.path.Subject, &s.path.Session, &s.path.Scan, &s.manifest)
	return s, found, err
}

// The claim's two reads, each in the order of its index and cut to its
// limit, so a claim reads its rows from the index in order and sorts none,
// however long the backlog: replica_scans_due, the rows due earliest first;
// replica_scans_leased, the leases ended.
const (
	replicaDueSQL = `SELECT replica, series_uid FROM replica_scans WHERE state IN ('pending', 'damaged') AND due <= ?
		ORDER BY due, replica, series_uid LIMIT ?`
	replicaLeaseEndedSQL = `SELECT replica, series_uid FROM replica_scans WHERE state = 'copying' AND lease_until <= ?
		ORDER BY lease_until, replica, series_uid LIMIT ?`
)

// ClaimReplicaScans implements tenant.Replication.
func (k Sending) ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error) {
	if err := tenant.ValidateReplicaClaim(owner, now, lease, limit); err != nil {
		return nil, err
	}
	var out []tenant.ReplicaClaim
	err := k.Host.Write(ctx, "the replicas' scans cannot be claimed", func(tx SendingTx) error {
		out = nil
		type key struct{ replica, series string }
		var keys []key
		collect := func(r Row) error {
			var x key
			err := r.Scan(&x.replica, &x.series)
			keys = append(keys, x)
			return err
		}
		if err := tx.Query(ctx, replicaDueSQL+k.D.SkipLocked, []any{k.D.Time(now), limit}, collect); err != nil {
			return err
		}
		if len(keys) < limit {
			if err := tx.Query(ctx, replicaLeaseEndedSQL+k.D.SkipLocked, []any{k.D.Time(now), limit - len(keys)}, collect); err != nil {
				return err
			}
		}
		for _, x := range keys {
			c, ok, err := k.claimOne(ctx, tx, x.replica, x.series, owner, now, lease)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, c)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claimOne leases one row the claim selected, or drops it when the catalog
// no longer holds its scan on the replica's primary; false when it was
// dropped.
func (k Sending) claimOne(ctx context.Context, tx SendingTx, replica, series, owner string, now time.Time,
	lease time.Duration) (tenant.ReplicaClaim, bool, error) {
	row, found, err := replicaRowIn(ctx, tx, replica, series, "")
	if err != nil || !found {
		return tenant.ReplicaClaim{}, false, err
	}
	var primary string
	if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{replica}, &primary); err != nil {
		return tenant.ReplicaClaim{}, false, err
	}
	s, held, err := scanOfSeries(ctx, tx, series)
	if err != nil {
		return tenant.ReplicaClaim{}, false, err
	}
	if !held || primary == "" || s.storage != primary {
		return tenant.ReplicaClaim{}, false, k.dropRow(ctx, tx, row, now)
	}
	if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'copying', claimed_seq = seq, attempt = attempt + 1, owner = ?,
		lease_until = ? WHERE replica = ? AND series_uid = ?`, owner, k.D.Time(now.Add(lease)), replica, series); err != nil {
		return tenant.ReplicaClaim{}, false, err
	}
	var t totals
	t.move(row.State, tenant.ReplicaCopying)
	if err := k.addTotals(ctx, tx, replica, t, now); err != nil {
		return tenant.ReplicaClaim{}, false, err
	}
	c := tenant.ReplicaClaim{Fence: tenant.ReplicaFence{Replica: replica, SeriesUID: series, Attempt: row.Attempt + 1},
		Primary: primary, StudyUID: s.study, Path: s.path, ManifestSHA256: ToSHA256(s.manifest), Backfill: row.Backfill}
	if c.SidecarSHA256, err = sidecarOf(ctx, tx, series, s.manifest); err != nil {
		return c, true, err
	}
	err = tx.Query(ctx, `SELECT sop_instance_uid, revision, found_size, found_sha256, found_at FROM replica_damage
		WHERE replica = ? AND series_uid = ? ORDER BY sop_instance_uid LIMIT ?`, []any{replica, series, tenant.MaxReplicaDamage + 1},
		func(r Row) error {
			var d tenant.ReplicaDamage
			var sum []byte
			var at DBTime
			if err := r.Scan(&d.SOPInstanceUID, &d.Revision, &d.FoundSize, &sum, &at); err != nil {
				return err
			}
			d.FoundSHA256, d.Found = ToSHA256(sum), at.T
			c.Damaged = append(c.Damaged, d)
			return nil
		})
	if err != nil {
		return c, true, err
	}
	if len(c.Damaged) > tenant.MaxReplicaDamage {
		c.Damaged, c.RecopyAll = c.Damaged[:tenant.MaxReplicaDamage], true
	}
	// An instance a purging correction hides is not copied: the purge took
	// its copies when it began, and StartPurge counted a change on every
	// row of its scans being copied, so a claim made before ends again.
	err = tx.Query(ctx, `SELECT h.sop_instance_uid FROM hidden_instances AS h JOIN corrections AS c ON c.id = h.correction_id
		JOIN scans AS s ON s.id = h.scan_id WHERE c.state = 'purging' AND s.series_uid = ? ORDER BY h.sop_instance_uid`,
		[]any{series}, func(r Row) error {
			var uid string
			err := r.Scan(&uid)
			c.Purging = append(c.Purging, uid)
			return err
		})
	return c, true, err
}

// lockFence reads, locked, the row a claim's fence names, refusing with
// ErrLeaseLost one that is not there, not copying, or of another attempt.
func (k Sending) lockFence(ctx context.Context, tx SendingTx, f tenant.ReplicaFence) (replicaRow, error) {
	row, found, err := replicaRowIn(ctx, tx, f.Replica, f.SeriesUID, k.D.Lock)
	if err != nil {
		return row, err
	}
	if !found || row.State != tenant.ReplicaCopying || row.Attempt != f.Attempt {
		return row, tenant.LeaseLost()
	}
	return row, nil
}

// RenewReplicaScan implements tenant.Replication.
func (k Sending) RenewReplicaScan(ctx context.Context, fence tenant.ReplicaFence, now time.Time, lease time.Duration) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	if err := tenant.ValidateReplicaClaim("renew", now, lease, 1); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the replica's lease cannot be renewed", func(tx SendingTx) error {
		if _, err := k.lockFence(ctx, tx, fence); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE replica_scans SET lease_until = ? WHERE replica = ? AND series_uid = ?`,
			k.D.Time(now.Add(lease)), fence.Replica, fence.SeriesUID)
		return err
	})
}

// afterCopy is the state a row leaves copying for when its copy did not
// end it in step: damaged while damage is left, pending otherwise.
func afterCopy(row replicaRow) tenant.ReplicaScanState {
	if row.Damaged > 0 {
		return tenant.ReplicaDamaged
	}
	return tenant.ReplicaPending
}

// EndReplicaScan implements tenant.Replication.
func (k Sending) EndReplicaScan(ctx context.Context, fence tenant.ReplicaFence, manifest tenant.SHA256, now time.Time) (tenant.ReplicaScanState, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return "", err
	}
	var out tenant.ReplicaScanState
	err := k.Host.Write(ctx, "the replica's copy cannot be recorded", func(tx SendingTx) error {
		out = ""
		row, err := k.lockFence(ctx, tx, fence)
		if err != nil {
			return err
		}
		var primary string
		if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{fence.Replica}, &primary); err != nil {
			return err
		}
		s, held, err := scanOfSeries(ctx, tx, fence.SeriesUID)
		if err != nil {
			return err
		}
		if !held || s.storage != primary {
			return k.dropRow(ctx, tx, row, now)
		}
		var t totals
		if row.seq == row.claimed && ToSHA256(s.manifest) == manifest {
			// The copy put every damaged object again, verified: the
			// findings recorded with the damage go with it.
			if err := k.clearDamageFindings(ctx, tx, `replica = ? AND series_uid = ?`, []any{fence.Replica, fence.SeriesUID}, "",
				now); err != nil {
				return err
			}
			n, err := tx.Exec(ctx, `DELETE FROM replica_damage WHERE replica = ? AND series_uid = ?`, fence.Replica, fence.SeriesUID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'in_step', manifest_sha256 = ?, queued_at = NULL,
				lease_until = NULL, owner = '', verified_at = ?, backfill = ?, last_error = '' WHERE replica = ? AND series_uid = ?`,
				nullSHA(manifest), k.D.Time(now), k.D.Bool(false), fence.Replica, fence.SeriesUID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE replica_totals SET last_fault = '', last_fault_at = NULL WHERE replica = ?`,
				fence.Replica); err != nil {
				return err
			}
			out, t.objects = tenant.ReplicaInStep, -n
			if row.Backfill {
				t.backfill = -1
			}
		} else {
			out = afterCopy(row)
			if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = ?, lease_until = NULL, owner = '', due = ?
				WHERE replica = ? AND series_uid = ?`, string(out), k.D.Time(now), fence.Replica, fence.SeriesUID); err != nil {
				return err
			}
		}
		t.move(tenant.ReplicaCopying, out)
		return k.addTotals(ctx, tx, fence.Replica, t, now)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// FailReplicaScan implements tenant.Replication.
func (k Sending) FailReplicaScan(ctx context.Context, f tenant.ReplicaFailure) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the replica's failed copy cannot be recorded", func(tx SendingTx) error {
		row, err := k.lockFence(ctx, tx, f.Fence)
		if err != nil {
			return err
		}
		next := afterCopy(row)
		if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = ?, lease_until = NULL, owner = '', due = ?, last_error = ?
			WHERE replica = ? AND series_uid = ?`, string(next), k.D.Time(f.Retry), f.Reason, f.Fence.Replica, f.Fence.SeriesUID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE replica_totals SET last_fault = ?, last_fault_at = ? WHERE replica = ?`, f.Fault,
			k.D.Time(f.Now), f.Fence.Replica); err != nil {
			return err
		}
		var t totals
		t.move(tenant.ReplicaCopying, next)
		return k.addTotals(ctx, tx, f.Fence.Replica, t, f.Now)
	})
}
