package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The swap of a switch (tenant.Switching): what moves with each of two
// storages' places in SwapStorages' one transaction, and the replica's rows
// it records in step.

// placeColumns are the columns of a storage that say where its files are
// and what is true of that place, which a swap moves with the place: its
// type, its directory or its bucket with the credentials and the purge's
// versions, its capacity, and a directory's declaration of encryption.
const placeColumns = `type, dir_root, dir_path, s3_endpoint, s3_region, s3_bucket, s3_prefix, s3_path_style, s3_access_key_id,
	s3_secret, s3_keep_versions, capacity_bytes, dir_encrypted`

// place is one storage's placeColumns.
type place struct {
	typ, root, path, endpoint, region, bucket, prefix, keyID string
	pathStyle, keepVersions, encrypted                       bool
	secret                                                   []byte
	capacity                                                 int64
}

// placeOf reads a storage's place in tx.
func placeOf(ctx context.Context, tx SendingTx, name string) (place, error) {
	var p place
	_, err := tx.QueryRow(ctx, `SELECT `+placeColumns+` FROM storages WHERE name = ?`, []any{name}, &p.typ, &p.root, &p.path,
		&p.endpoint, &p.region, &p.bucket, &p.prefix, &p.pathStyle, &p.keyID, &p.secret, &p.keepVersions, &p.capacity, &p.encrypted)
	return p, err
}

// setPlace writes place p onto the storage named.
func (k Sending) setPlace(ctx context.Context, tx SendingTx, name string, p place, now time.Time) error {
	var secret any
	if len(p.secret) > 0 {
		secret = p.secret
	}
	_, err := tx.Exec(ctx, `UPDATE storages SET type = ?, dir_root = ?, dir_path = ?, s3_endpoint = ?, s3_region = ?, s3_bucket = ?,
		s3_prefix = ?, s3_path_style = ?, s3_access_key_id = ?, s3_secret = ?, s3_keep_versions = ?, capacity_bytes = ?,
		dir_encrypted = ?, updated = ? WHERE name = ?`, p.typ, p.root, p.path, p.endpoint, p.region, p.bucket, p.prefix,
		k.D.Bool(p.pathStyle), p.keyID, secret, k.D.Bool(p.keepVersions), p.capacity, k.D.Bool(p.encrypted), k.D.Time(now), name)
	return err
}

// resealed returns place p as it is written under its new name: an S3
// place carries the secret sealed for that name, which the caller gave
// (sealed), since the seal of its old row is bound to its old name and does
// not open under the new one; a directory place has none to carry. An S3
// place without its secret sealed again, or a secret given for a place that
// is not S3, is refused.
func resealed(p place, sealed []byte) (place, error) {
	switch {
	case p.typ == string(tenant.StorageS3) && len(sealed) == 0:
		return p, tenant.Precondition(tenant.RefusalPlaceNotResealed)
	case p.typ != string(tenant.StorageS3) && len(sealed) > 0:
		return p, tenant.Precondition("a sealed secret access key was given for a place that is not an S3 bucket")
	}
	p.secret = sealed
	return p, nil
}

// swapPlaces swaps what belongs to the two storages' places: their place
// columns, each S3 place with the secret sealed again for the name it comes
// to (sw), their owner records' generations, and the open scrub findings
// of the primary, whose marks on instances are lifted; and raises both
// names' switch generations. The replica has no open finding (the swap
// checked), so only the primary's move.
func (k Sending) swapPlaces(ctx context.Context, tx SendingTx, primary, replica string, sw tenant.SwitchSwap) error {
	now := sw.Now
	a, err := placeOf(ctx, tx, primary)
	if err != nil {
		return err
	}
	b, err := placeOf(ctx, tx, replica)
	if err != nil {
		return err
	}
	if b, err = resealed(b, sw.PrimarySecret); err != nil {
		return err
	}
	if a, err = resealed(a, sw.ReplicaSecret); err != nil {
		return err
	}
	if err := k.setPlace(ctx, tx, primary, b, now); err != nil {
		return err
	}
	if err := k.setPlace(ctx, tx, replica, a, now); err != nil {
		return err
	}
	type owner struct {
		found      bool
		generation int64
		written    DBTime
	}
	owners := map[string]*owner{primary: {}, replica: {}}
	for _, name := range []string{primary, replica} {
		o := owners[name]
		if o.found, err = tx.QueryRow(ctx, `SELECT generation, written FROM storage_owners WHERE storage = ?`+k.D.Lock, []any{name},
			&o.generation, &o.written); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM storage_owners WHERE storage = ?`, name); err != nil {
			return err
		}
	}
	for _, x := range [][2]string{{primary, replica}, {replica, primary}} {
		if o := owners[x[1]]; o.found {
			if _, err := tx.Exec(ctx, `INSERT INTO storage_owners (storage, generation, written) VALUES (?, ?, ?)`, x[0], o.generation,
				k.D.Time(o.written.T)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{primary, replica} {
		if _, err := tx.Exec(ctx, `INSERT INTO storage_generations (storage, generation) VALUES (?, 1) ON CONFLICT (storage)
			DO UPDATE SET generation = storage_generations.generation + 1`, name); err != nil {
			return err
		}
	}
	var moved []string
	if err := tx.Query(ctx, `SELECT id FROM scrub_findings WHERE storage = ? AND cleared_at IS NULL ORDER BY id`, []any{primary},
		func(r Row) error {
			var id string
			err := r.Scan(&id)
			moved = append(moved, id)
			return err
		}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE scrub_findings SET storage = ? WHERE storage = ? AND cleared_at IS NULL`, replica, primary); err != nil {
		return err
	}
	for _, id := range moved {
		if _, err := tx.Exec(ctx, `DELETE FROM instance_damage WHERE finding_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// recordInStep records every scan of the primary in step on the replica,
// whose name now names the place that held the primary's bytes, at the
// catalog's manifest digest; but the scans that the findings moved there
// name, which are damaged on it, each object named recorded as its damage
// so the replicator copies it again from the primary's new place. Its
// counts are then counted again.
func (k Sending) recordInStep(ctx context.Context, tx SendingTx, primary, replica string, now time.Time) error {
	for _, q := range []string{`DELETE FROM replica_damage WHERE replica = ?`, `DELETE FROM replica_scans WHERE replica = ?`} {
		if _, err := tx.Exec(ctx, q, replica); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO replica_scans (replica, series_uid, state, manifest_sha256, queued_at, due, seq, claimed_seq,
		attempt, owner, lease_until, verified_at, backfill, last_error)
		SELECT ?, series_uid, 'in_step', manifest_sha256, NULL, ?, 1, 0, 0, '', NULL, ?, ?, '' FROM scans WHERE storage = ?`,
		replica, k.D.Time(now), k.D.Time(now), k.D.Bool(false), primary); err != nil {
		return err
	}
	type damage struct {
		series, uid string
		revision    int
		size        *int64
		sum         []byte
	}
	var found []damage
	err := tx.Query(ctx, `SELECT coalesce(si.series_uid, sl.series_uid, ''), f.sop_instance_uid, f.revision, f.found_size, f.found_sha256
		FROM scrub_findings AS f
		LEFT JOIN instances AS i ON f.sop_instance_uid <> '' AND i.sop_instance_uid = f.sop_instance_uid
		LEFT JOIN scans AS si ON si.id = i.scan_id AND si.storage = ?
		LEFT JOIN scans AS sl ON f.sop_instance_uid = '' AND sl.storage = ? AND sl.subject_label = f.subject_label
		AND sl.session_label = f.session_label AND sl.label = f.scan_label
		WHERE f.storage = ? AND f.cleared_at IS NULL AND f.kind <> 'orphan' ORDER BY f.id`, []any{primary, primary, replica},
		func(r Row) error {
			var d damage
			err := r.Scan(&d.series, &d.uid, &d.revision, &d.size, &d.sum)
			if d.series != "" {
				found = append(found, d)
			}
			return err
		})
	if err != nil {
		return err
	}
	for _, d := range found {
		if _, err := tx.Exec(ctx, `INSERT INTO replica_damage (replica, series_uid, sop_instance_uid, revision, found_size, found_sha256,
			found_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (replica, series_uid, sop_instance_uid) DO NOTHING`, replica, d.series,
			d.uid, d.revision, nullInt(d.size), d.sum, k.D.Time(now)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE replica_scans SET state = 'damaged', queued_at = ?, due = ? WHERE replica = ?
			AND series_uid = ? AND state = 'in_step'`, k.D.Time(now), k.D.Time(now), replica, d.series); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE replica_totals SET last_fault = '', last_fault_at = NULL WHERE replica = ?`, replica); err != nil {
		return err
	}
	return k.recountTotals(ctx, tx, replica, now)
}
