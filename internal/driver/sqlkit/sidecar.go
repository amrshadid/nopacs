package sqlkit

import (
	"context"
	"time"

	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The digest of each scan's metadata file (CatalogScan.SidecarSHA256), on
// the scan_sidecars table: written by the transaction that writes the
// manifest's digest it was written with, and read only while the scan's
// manifest digest is still that one, so a writer that changes the manifest
// without telling the metadata file's digest leaves the file unchecked
// rather than checked against a digest of another file.
//
// A writer that puts a new file before it can record its digest drops the
// row first (ForgetSidecar, StartPurge), so between the put and the record
// the file is unchecked, never checked against the file it replaced: a
// read turning to a replica, or a scrub, would otherwise take a good copy
// for a damaged one. While an instance delete's purge rewrites a scan's
// files no digest is kept for it at all (purgeRewritesSQL): the purge puts
// the new metadata file on every copy before the catalog learns of it.

// sidecarSQL reads the digest of a scan's metadata file that holds with
// the manifest digest given: scan_sidecars' key.
const sidecarSQL = `SELECT sha256 FROM scan_sidecars WHERE series_uid = ? AND manifest_sha256 = ?`

// purgeRewritesSQL finds an instance delete purging that touches the scan
// of a series, whose purge rewrites the scan's metadata file and manifest:
// corrections_by_state's range of the corrections purging (few), each
// joined to correction_scans by its key.
const purgeRewritesSQL = `SELECT 1 FROM corrections AS c JOIN correction_scans AS cs ON cs.correction_id = c.id
	AND cs.series_uid = ? WHERE c.state = 'purging' AND c.kind = 'delete' AND c.level = 'instance' LIMIT 1`

// preparedRewritesSQL finds a replace, an edit or a relabel prepared, or an
// edit being undone, that touches the scan of a series: a replace and an
// edit write the scan's metadata file again and release the scan's lock
// before their publish keeps that file's digest (an edit's undo before the
// undo's publish), and a relabel copies the file to its new keys before
// its publish keeps the copy's. A file another writer puts between the two
// would be named by a digest of another file once the publish commits.
const preparedRewritesSQL = `SELECT 1 FROM corrections AS c JOIN correction_scans AS cs ON cs.correction_id = c.id
	AND cs.series_uid = ? WHERE (c.state = 'prepared' AND c.kind IN ('replace', 'relabel', 'edit')
	OR c.state = 'committed' AND c.kind = 'edit' AND c.state_reason = '` + tenant.StateReasonUndoing + `') LIMIT 1`

// SetSidecarInTx keeps, in the caller's transaction, sidecar as the digest
// of the metadata file of the scan of series written with the manifest of
// digest manifest; a zero sidecar keeps none for the scan. A zero manifest
// changes nothing: the scan keeps its manifest, and so its digest. While an
// instance delete's purge of the scan is under way it keeps none either:
// the purge rewrites the file after this.
func SetSidecarInTx(ctx context.Context, tx SendingTx, series string, manifest, sidecar tenant.SHA256) error {
	if manifest.IsZero() {
		return nil
	}
	if !sidecar.IsZero() {
		purging, err := exists(ctx, tx, purgeRewritesSQL, series)
		if err != nil {
			return err
		}
		if purging {
			sidecar = tenant.SHA256{}
		}
	}
	if sidecar.IsZero() {
		_, err := tx.Exec(ctx, `DELETE FROM scan_sidecars WHERE series_uid = ?`, series)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO scan_sidecars (series_uid, manifest_sha256, sha256) VALUES (?, ?, ?)
		ON CONFLICT (series_uid) DO UPDATE SET manifest_sha256 = excluded.manifest_sha256, sha256 = excluded.sha256`,
		series, manifest[:], sidecar[:])
	return err
}

// KeepRolledBack implements tenant.Corrections.
func (k Sending) KeepRolledBack(ctx context.Context, id, series string, manifest, metadata tenant.SHA256) error {
	if err := tenant.ValidateSeriesUID(series); err != nil {
		return err
	}
	if manifest.IsZero() {
		return fault.New(fault.Invalid, "the digest of the manifest a rollback wrote is given")
	}
	return k.stepRow(ctx, id, "the digests of a scan a rollback wrote cannot be kept", func(tx SendingTx, c *tenant.Correction) error {
		if c.State != corr.StatePrepared || c.Kind != corr.KindReplace && c.Kind != corr.KindEdit {
			return tenant.Precondition("the correction is not a prepared replace or edit")
		}
		ours, err := exists(ctx, tx, `SELECT 1 FROM correction_scans WHERE correction_id = ? AND series_uid = ?`, id, series)
		if err != nil {
			return err
		}
		if !ours {
			return tenant.Precondition("the scan is not one of the correction's")
		}
		if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE series_uid = ?`, manifest[:], series); err != nil {
			return err
		}
		return SetSidecarInTx(ctx, tx, series, manifest, metadata)
	})
}

// dropPurgeSidecarsInTx drops, in the caller's transaction, the digests of
// the metadata files of the scans an instance delete's purge of correction
// id rewrites: those files are unchecked from now until the scan's next
// commit, or the pass that records a file's digest, rather than checked
// against the file the purge replaces on every copy.
func dropPurgeSidecarsInTx(ctx context.Context, tx SendingTx, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM scan_sidecars WHERE series_uid IN (SELECT cs.series_uid FROM correction_scans AS cs
		JOIN corrections AS c ON c.id = cs.correction_id WHERE c.id = ? AND c.kind = 'delete' AND c.level = 'instance')`, id)
	return err
}

// sidecarOf returns the digest of the metadata file of the scan of series
// that holds while its manifest digest is manifest, zero for none.
func sidecarOf(ctx context.Context, tx SendingTx, series string, manifest []byte) (tenant.SHA256, error) {
	if len(manifest) == 0 {
		return tenant.SHA256{}, nil
	}
	var sum []byte
	if _, err := tx.QueryRow(ctx, sidecarSQL, []any{series, manifest}, &sum); err != nil {
		return tenant.SHA256{}, err
	}
	return ToSHA256(sum), nil
}

// ForgetSidecar implements tenant.SidecarRewrites.
func (k Sending) ForgetSidecar(ctx context.Context, seriesUID, storage string) (bool, error) {
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return false, err
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return false, err
	}
	var dropped bool
	err := k.Host.Write(ctx, "the scan's metadata digest cannot be dropped", func(tx SendingTx) error {
		dropped = false
		// The writer puts its file after this answers: refused while a
		// replace or relabel of the scan is prepared, it puts nothing, and
		// tries again once the publish (which keeps the digest of the file
		// it wrote) or the abandon has ended the correction.
		if busy, err := exists(ctx, tx, preparedRewritesSQL, seriesUID); err != nil || busy {
			if err != nil {
				return err
			}
			return fault.New(fault.Unavailable, "a correction of the scan is under way; its metadata file is rewritten after it")
		}
		n, err := tx.Exec(ctx, `DELETE FROM scan_sidecars WHERE series_uid = ?
			AND EXISTS (SELECT 1 FROM scans WHERE series_uid = ? AND storage = ?)`, seriesUID, seriesUID, storage)
		dropped = n > 0
		return err
	})
	if err != nil {
		return false, err
	}
	return dropped, nil
}

// RecordSidecarRewritten implements tenant.SidecarRewrites.
func (k Sending) RecordSidecarRewritten(ctx context.Context, seriesUID, storage string, sidecar tenant.SHA256, now time.Time) (bool, error) {
	if err := tenant.ValidateSeriesUID(seriesUID); err != nil {
		return false, err
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return false, err
	}
	if sidecar.IsZero() {
		return false, fault.New(fault.Invalid, "a metadata file rewritten has a digest")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return false, err
	}
	var recorded bool
	err := k.Host.Write(ctx, "the scan's metadata digest cannot be recorded", func(tx SendingTx) error {
		recorded = false
		var on string
		var manifest []byte
		// Read under the lock a commit of the scan takes, so the manifest
		// digest the metadata file is kept with is the one a commit
		// running beside this left.
		found, err := tx.QueryRow(ctx, `SELECT storage, manifest_sha256 FROM scans WHERE series_uid = ?`+k.D.Lock, []any{seriesUID},
			&on, &manifest)
		if err != nil || !found || on != storage || len(manifest) == 0 {
			return err
		}
		// A purge rewriting the scan's files puts its own metadata file on
		// every copy next; a digest kept now would name the file it replaces.
		if purging, err := exists(ctx, tx, purgeRewritesSQL, seriesUID); err != nil || purging {
			return err
		}
		// A replace or relabel prepared keeps, at its publish, the digest of
		// the file it wrote: one kept now would name a file it writes over,
		// or be replaced by its digest while this file stays.
		if busy, err := exists(ctx, tx, preparedRewritesSQL, seriesUID); err != nil || busy {
			return err
		}
		if err := SetSidecarInTx(ctx, tx, seriesUID, ToSHA256(manifest), sidecar); err != nil {
			return err
		}
		recorded = true
		// The replicas copy the file again: a replica in step holds the old
		// one, and a copy claimed before this counts it as a change.
		replicas, err := replicasOf(ctx, tx, storage)
		if err != nil {
			return err
		}
		for _, r := range replicas {
			if has, err := exists(ctx, tx, `SELECT 1 FROM replica_scans WHERE replica = ? AND series_uid = ?`, r, seriesUID); err != nil {
				return err
			} else if !has {
				continue
			}
			if err := k.queueScan(ctx, tx, r, seriesUID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return recorded, nil
}
