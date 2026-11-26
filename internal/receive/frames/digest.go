package frames

import (
	"context"
	"crypto/sha256"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The catalog keeps the SHA-256 of each scan's metadata file
// (tenant.CatalogScan.SidecarSHA256), against which reads and scrubs check
// every copy. A pass that rewrites a file drops the digest before its put
// (forgetDigest) and records the new one after (recordRewrite): a record
// that fails then leaves the file unchecked, never checked against the file
// it replaced, which would make every copy read as changed and keep the
// replicas from ever being in step. A file a pass leaves as it is gets its
// digest recorded when the catalog keeps none, or when the storage's own
// SHA-256 of it shows another (settleDigest): so a record lost after a put
// is made by the next pass, and a scan archived before digests were kept
// gets one.

// DigestCatalog is what a pass asks of the catalog about a metadata file's
// digest: the scan as it stands, and the records of the file.
type DigestCatalog interface {
	// CatalogScan is tenant.ScanReads.CatalogScan.
	CatalogScan(ctx context.Context, path label.Path) (tenant.CatalogScan, error)
	tenant.SidecarRewrites
}

// forgetDigest drops the digest the catalog keeps of the metadata file of
// the scan of series on storage, before a pass puts a new file there.
func forgetDigest(ctx context.Context, rec tenant.SidecarRewrites, series, storage string) error {
	if rec == nil {
		return nil
	}
	if _, err := rec.ForgetSidecar(ctx, series, storage); err != nil {
		return err
	}
	return nil
}

// settleDigest records, for a scan whose metadata file at key the pass
// leaves as it is, the file's digest when the catalog keeps none, or keeps
// one the storage's own SHA-256 of the file (where the storage keeps one)
// shows is of another file. A file the storage keeps no SHA-256 of and the
// catalog has a digest for is not read: the scrub reads every file whole.
// The file is read, and the catalog read again, admitted by the gate of
// writes to the storage and under the scan's lock, as a rewrite is, so no
// change of the scan comes between the read and the record.
func (r *runner) settleDigest(ctx context.Context, cat DigestCatalog, storage string, path label.Path, key string) error {
	if cat == nil {
		return nil
	}
	sc, held, err := scanOf(ctx, cat, path, storage)
	if err != nil || !held {
		return err
	}
	if !sc.SidecarSHA256.IsZero() {
		store, err := r.storage(ctx, storage)
		if err != nil {
			return err
		}
		info, err := store.Stat(ctx, key)
		if fault.KindOf(err) == fault.NotFound {
			return nil
		}
		if err != nil {
			return storageError(err, "the scan's metadata cannot be read")
		}
		if !info.HasSHA256 || tenant.SHA256(info.SHA256) == sc.SidecarSHA256 {
			return nil
		}
	}
	store, release, err := r.admit(ctx, storage)
	if err != nil {
		return err
	}
	defer release()
	unlock := r.cfg.Lock(storage, path)
	defer unlock()
	sum, ok, err := fileSum(ctx, store, key)
	if err != nil || !ok {
		return err
	}
	if sc, held, err = scanOf(ctx, cat, path, storage); err != nil || !held || sc.SidecarSHA256 == sum {
		return err
	}
	if _, err := cat.RecordSidecarRewritten(ctx, sc.SeriesUID, storage, sum, r.cfg.Clock.Now()); err != nil {
		return err
	}
	return nil
}

// scanOf returns the catalog's scan at path, and whether it is held on
// storage with a manifest digest (a scan gone, moved or without one has no
// metadata digest to keep).
func scanOf(ctx context.Context, cat DigestCatalog, path label.Path, storage string) (tenant.CatalogScan, bool, error) {
	sc, err := cat.CatalogScan(ctx, path)
	if fault.KindOf(err) == fault.NotFound {
		return sc, false, nil
	}
	if err != nil {
		return sc, false, err
	}
	return sc, sc.Storage == storage && !sc.ManifestSHA256.IsZero(), nil
}

// fileSum returns the SHA-256 of the object at key: the storage's own when
// it keeps one, else from the object read whole; false when there is none.
func fileSum(ctx context.Context, store blob.Store, key string) (tenant.SHA256, bool, error) {
	info, err := store.Stat(ctx, key)
	if fault.KindOf(err) == fault.NotFound {
		return tenant.SHA256{}, false, nil
	}
	if err != nil {
		return tenant.SHA256{}, false, storageError(err, "the scan's metadata cannot be read")
	}
	if info.HasSHA256 {
		return tenant.SHA256(info.SHA256), true, nil
	}
	rc, err := store.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return tenant.SHA256{}, false, nil
	}
	if err != nil {
		return tenant.SHA256{}, false, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = rc.Close() }() // Only read from.
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return tenant.SHA256{}, false, storageError(err, "the scan's metadata cannot be read")
	}
	return tenant.SHA256(h.Sum(nil)), true, nil
}
