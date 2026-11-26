package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A scan's manifest and metadata written again in place: by an instance
// delete's purge (the instance left out) and by a replace (the new
// revision in place of the old), and the digest of the metadata file each
// writes, which the catalog keeps (CatalogScan.SidecarSHA256) so the file
// is checked like the instances it describes.

// metaWritten is what a rewrite of a scan's manifest and metadata read and
// wrote: the SHA-256 of the manifest it read (before) and of the one it
// wrote (after), and of the metadata file it wrote (metadata, zero when
// the scan had none to write again).
type metaWritten struct {
	before, after, metadata tenant.SHA256
}

// rewriteMetaLocked writes a scan's manifest and metadata again on the
// storage named, inside the storage's write gate (gated), so a switch never
// finds half of them on the place it swaps away (rewriteMetaOn). The
// caller holds the scan's lock.
func (e *Engine) rewriteMetaLocked(ctx context.Context, storage string, p label.Path, drop string, add *sidecar.Instance,
	file *manifest.File) (before, after, metadata tenant.SHA256, err error) {
	var w metaWritten
	err = e.gated(ctx, storage, func(st blob.Store) error {
		var err error
		w, err = e.rewriteMetaOn(ctx, st, p, drop, add, file)
		return err
	})
	return w.before, w.after, w.metadata, err
}

// rewriteMeta writes a scan's manifest and metadata again on st, opened by
// a caller that holds the storage's write gate (hold), under the scan's
// lock (rewriteMetaOn).
func (e *Engine) rewriteMeta(ctx context.Context, st blob.Store, storage string, p label.Path, drop string, add *sidecar.Instance,
	file *manifest.File) (metaWritten, error) {
	mk, err := layout.ManifestKey(p)
	if err != nil {
		return metaWritten{}, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	unlock := e.lockScan(storage, mk)
	defer unlock()
	return e.rewriteMetaOn(ctx, st, p, drop, add, file)
}

// rewriteMetaOn writes a scan's manifest and metadata again at their keys
// on st: the instance drop left out (or replaced by add and file, a
// replace's new revision), and answers the SHA-256 of the manifest it read,
// of the one it wrote and of the metadata file it wrote. The metadata is
// written first: the manifest names what the scan holds. The caller holds
// the scan's lock and the storage's write gate.
func (e *Engine) rewriteMetaOn(ctx context.Context, st blob.Store, p label.Path, drop string, add *sidecar.Instance,
	file *manifest.File) (metaWritten, error) {
	var w metaWritten
	mk, _ := layout.ManifestKey(p)
	sk, _ := layout.MetadataKey(p)
	raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
	if err != nil {
		return w, err
	}
	w.before = sha256.Sum256(raw)
	if err := e.seams.at("meta-read"); err != nil {
		return w, err
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		return w, fault.Wrap(fault.Unavailable, err, "a scan's manifest cannot be read")
	}
	var files []manifest.File
	for _, f := range m.Files {
		if f.SOPInstanceUID != drop {
			files = append(files, f)
		}
	}
	if file != nil {
		files = append(files, *file)
	}
	old, err := st.Read(ctx, sk, 0, -1)
	switch {
	case fault.KindOf(err) == fault.NotFound:
	case err != nil:
		return w, err
	default:
		var buf bytes.Buffer
		var added []sidecar.Instance
		if add != nil {
			added = append(added, *add)
		}
		_, merr := sidecar.Merge(&buf, old, sidecar.List(added), func(uid string) bool { return uid == drop })
		_ = old.Close()
		if merr != nil {
			return w, fault.Wrap(fault.Unavailable, merr, "a scan's metadata cannot be written again")
		}
		sum := sha256.Sum256(buf.Bytes())
		if _, err := st.Put(ctx, sk, &buf); err != nil {
			return w, err
		}
		w.metadata = sum
	}
	b, err := manifest.Encode(manifest.Manifest{Files: files})
	if err != nil {
		return w, fault.Wrap(fault.Internal, err, "a scan's manifest cannot be made")
	}
	if _, err := st.Put(ctx, mk, bytes.NewReader(b)); err != nil {
		return w, err
	}
	w.after = sha256.Sum256(b)
	return w, nil
}

// sidecarRecorder is the part of the tenant's database that keeps the
// digest of a scan's metadata file rewritten without a publish
// (tenant.SidecarRewrites).
type sidecarRecorder interface {
	RecordSidecarRewritten(ctx context.Context, seriesUID, storage string, sidecar tenant.SHA256, now time.Time) (bool, error)
}

// scanReader is the part of the tenant's database that reads a scan by its
// labels (tenant.ScanReads).
type scanReader interface {
	CatalogScan(ctx context.Context, p label.Path) (tenant.CatalogScan, error)
}

// recordMeta keeps, once an instance delete's purge has ended, the digest
// of the metadata file it wrote on the scan's primary, so a read, a scrub
// and the replicas check that file again rather than leave it unchecked
// until the scan's next commit. It is called under the scan's lock, and
// keeps the digest only while the catalog holds the manifest the purge
// wrote with it: a digest kept beside another manifest would name a file
// that manifest was not written with. A failure is logged: the file stays
// unchecked, never checked against the wrong digest.
func (e *Engine) recordMeta(ctx context.Context, s tenant.CorrectionScan, manifestSum, metaSum tenant.SHA256) {
	rec, ok := e.cfg.Store.(sidecarRecorder)
	sr, reads := e.cfg.Store.(scanReader)
	if !ok || !reads || metaSum.IsZero() {
		return
	}
	scan, err := sr.CatalogScan(ctx, s.Path)
	if err != nil || scan.SeriesUID != s.SeriesUID || scan.ManifestSHA256 != manifestSum {
		return
	}
	if _, err := rec.RecordSidecarRewritten(ctx, s.SeriesUID, s.Storage, metaSum, e.now()); err != nil {
		e.cfg.Log.Warn("a purged scan's metadata digest could not be kept; the file is unchecked until the scan's next commit",
			"error", fault.Message(err))
	}
}
