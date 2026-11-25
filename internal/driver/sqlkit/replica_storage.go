package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The storages' rows as both drivers read them, and the rules of replicas
// every change of a storage keeps (tenant.CheckAttach and the others), on
// the storages table of each driver's tenant baseline schema. A store reads
// the facts here, in the change's transaction, and the port decides.

// StorageColumns are a storage's columns as a read returns them: the secret
// only as whether one is set, so no read can carry it out, and the name's
// switch generation, which outlives the storage, from storage_generations.
const StorageColumns = `name, type, dir_root, dir_path, s3_endpoint, s3_region, s3_bucket, s3_prefix,
	s3_path_style, s3_access_key_id, s3_secret IS NOT NULL, s3_keep_versions, capacity_bytes, created, updated,
	replica_of, tier, s3_recall_days, s3_recall_tier, dir_encrypted,
	coalesce((SELECT generation FROM storage_generations AS g WHERE g.storage = storages.name), 0)`

// ScanStorage reads one row of StorageColumns.
func ScanStorage(row Row) (tenant.Storage, error) {
	var s tenant.Storage
	var typ, tier, speed string
	var created, updated DBTime
	err := row.Scan(&s.Name, &typ, &s.Directory.Root, &s.Directory.Path, &s.S3.Endpoint, &s.S3.Region, &s.S3.Bucket,
		&s.S3.Prefix, &s.S3.PathStyle, &s.S3.AccessKeyID, &s.S3.SecretSet, &s.S3.KeepVersions, &s.CapacityBytes, &created, &updated,
		&s.ReplicaOf, &tier, &s.S3.RecallDays, &speed, &s.EncryptedAtRestDeclared, &s.SwitchGeneration)
	s.Type, s.Tier, s.S3.RecallSpeed = tenant.StorageType(typ), tenant.StorageTier(tier), tenant.RecallSpeed(speed)
	s.Created, s.Updated = created.T, updated.T
	return s, err
}

// storageIn reads a storage in tx, and whether there is one.
func storageIn(ctx context.Context, tx SendingTx, name, lock string) (tenant.Storage, bool, error) {
	var out tenant.Storage
	found := false
	err := tx.Query(ctx, `SELECT `+StorageColumns+` FROM storages WHERE name = ?`+lock, []any{name}, func(r Row) error {
		var err error
		out, err = ScanStorage(r)
		found = true
		return err
	})
	return out, found, err
}

// replicasOf returns the names of a primary's replicas, in order: the
// partial index storages_by_primary.
func replicasOf(ctx context.Context, tx SendingTx, primary string) ([]string, error) {
	var out []string
	err := tx.Query(ctx, `SELECT name FROM storages WHERE replica_of = ? AND replica_of <> '' ORDER BY name`, []any{primary},
		func(r Row) error {
			var n string
			err := r.Scan(&n)
			out = append(out, n)
			return err
		})
	return out, err
}

// switchRunning reports whether a switch of the storage named, as primary
// or as replica, is accepted or swapped.
func switchRunning(ctx context.Context, tx SendingTx, storage string) (bool, error) {
	return exists(ctx, tx, `SELECT 1 FROM storage_switches WHERE state IN ('accepted', 'swapped')
		AND (primary_name = ? OR replica_name = ?)`, storage, storage)
}

// receiverNaming returns the receiver that, or one of whose routes, archives
// to the storage named, or "".
func receiverNaming(ctx context.Context, tx SendingTx, storage string) (string, error) {
	var name string
	found, err := tx.QueryRow(ctx, `SELECT name FROM receivers WHERE storage = ? ORDER BY name LIMIT 1`, []any{storage}, &name)
	if err != nil || found {
		return name, err
	}
	return ReceiverRoutingTo(ctx, tx, storage)
}

// storageHoldsData says why a storage holds data, or "": an instance, a
// session, a reception not ended that names it (a reception keeps ended as
// a job does, so the dialect's condition of a job not ended reads it).
func (k Sending) storageHoldsData(ctx context.Context, tx SendingTx, storage string) (string, error) {
	if held, err := exists(ctx, tx, `SELECT 1 FROM scans WHERE storage = ? AND instances > 0`, storage); err != nil || held {
		return "it holds archived files", err
	}
	if named, err := exists(ctx, tx, `SELECT 1 FROM sessions WHERE storage = ?`, storage); err != nil || named {
		return "a session's files are on it", err
	}
	if used, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE storage = ? AND `+k.D.NotEnded, storage); err != nil || used {
		return "a reception in progress archives to it", err
	}
	return "", nil
}

// attachFacts reads what CheckAttach decides on for replica r and primary p.
func (k Sending) attachFacts(ctx context.Context, tx SendingTx, r, p tenant.Storage) (tenant.AttachFacts, error) {
	f := tenant.AttachFacts{Replica: r, Primary: p}
	others, err := replicasOf(ctx, tx, p.Name)
	if err != nil {
		return f, err
	}
	for _, o := range others {
		if o != r.Name {
			f.PrimaryReplicas++
		}
	}
	own, err := replicasOf(ctx, tx, r.Name)
	if err != nil {
		return f, err
	}
	f.ReplicaHasReplicas = len(own) > 0
	if f.ReplicaInUse, err = k.storageHoldsData(ctx, tx, r.Name); err != nil {
		return f, err
	}
	if f.ReplicaNamedBy, err = receiverNaming(ctx, tx, r.Name); err != nil {
		return f, err
	}
	f.Switching, err = switchRunning(ctx, tx, p.Name)
	return f, err
}

// CheckNewStorage applies the rules of replicas to a storage AddStorage is
// about to write, in its transaction after the tenant's configuration lock:
// the archive tier only for a replica, and a replica's primary under
// CheckAttach. The caller calls AfterNewStorage once the row is written.
func (k Sending) CheckNewStorage(ctx context.Context, tx SendingTx, s tenant.Storage) error {
	if err := tenant.CheckArchiveTier(s); err != nil {
		return err
	}
	if s.ReplicaOf == "" {
		return nil
	}
	p, found, err := storageIn(ctx, tx, s.ReplicaOf, k.D.Lock)
	if err != nil {
		return err
	}
	if !found {
		return tenant.Precondition("the tenant has no storage named " + s.ReplicaOf + " to be the primary")
	}
	f, err := k.attachFacts(ctx, tx, s, p)
	if err != nil {
		return err
	}
	return tenant.CheckAttach(f)
}

// AfterNewStorage begins what a storage added as a replica holds: its
// counts, and every scan of its primary pending for it as its backfill.
func (k Sending) AfterNewStorage(ctx context.Context, tx SendingTx, s tenant.Storage, now time.Time) error {
	if s.ReplicaOf == "" {
		return nil
	}
	_, err := k.beginReplica(ctx, tx, s.Name, s.ReplicaOf, now)
	return err
}

// CheckStorageChange applies the rules of replicas to a change of a storage
// from stored to next (tenant.CheckStorageChange); locationChanged says it
// moves where the files are.
func (k Sending) CheckStorageChange(ctx context.Context, tx SendingTx, stored, next tenant.Storage, locationChanged bool) error {
	own, err := replicasOf(ctx, tx, stored.Name)
	if err != nil {
		return err
	}
	return tenant.CheckStorageChange(stored, next, locationChanged, len(own) > 0)
}

// CheckStorageRemoval applies the rules of replicas to the removal of a
// storage (tenant.CheckStorageRemoval).
func (k Sending) CheckStorageRemoval(ctx context.Context, tx SendingTx, stored tenant.Storage) error {
	own, err := replicasOf(ctx, tx, stored.Name)
	if err != nil {
		return err
	}
	return tenant.CheckStorageRemoval(stored, len(own) > 0)
}

// CheckArchiveTarget refuses, as a precondition failure, a storage data
// would be archived to first (a receiver's, a route's, a release's) that
// the tenant does not have, that is a replica, or that is of the archive
// tier (tenant.CheckArchivedTo); what, when not empty, is the field that
// names it, for the message.
func CheckArchiveTarget(ctx context.Context, tx SendingTx, what, name string) error {
	var replicaOf, tier string
	found, err := tx.QueryRow(ctx, `SELECT replica_of, tier FROM storages WHERE name = ?`, []any{name}, &replicaOf, &tier)
	if err != nil {
		return err
	}
	if !found {
		return tenant.Precondition(tenant.Prefixed(what, "the tenant has no storage named "+name))
	}
	return tenant.CheckArchivedTo(what, name, replicaOf, tenant.StorageTier(tier))
}
