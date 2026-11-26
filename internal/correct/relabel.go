package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RelabelRequest is a relabel a person asked for.
type RelabelRequest struct {
	// Target is the subject, session or scan; ToLabel its new label.
	Target  tenant.CorrectionTarget
	ToLabel string
	// Reason is why; KeyID the key that asked.
	Reason, KeyID string
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// copyCheck is how many objects a relabel copies between records of its
// progress. Its row is read before every write, so a relabel an admin
// stopped writes nothing after the stop is seen.
const copyCheck = 64

// Relabel records a relabel and puts its records, prepared, and answers
// it; the copy and the publish are ContinueRelabel's, which the caller
// runs in the background (and Recover after a stop). The same relabel
// asked again answers it.
func (e *Engine) Relabel(ctx context.Context, r RelabelRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel, Target: r.Target,
		ToLabel: r.ToLabel, Reason: r.Reason, KeyID: r.KeyID, Now: now}, r.Audit, r.Guard)
	if err != nil || !made {
		return c, err
	}
	// Its records are written apart from ctx's end (detached), so a client
	// that hangs up does not leave it prepared with no copy running.
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	return c, e.reached(c, "records")
}

// ContinueRelabel copies every object of a prepared relabel to its key
// under the new labels (each read back and compared), writes the new
// manifests and metadata, and publishes it. Run again after a stop it
// copies again from the start; each object goes to the same key. An object
// that cannot be read whole, or a target that changed meanwhile, abandons
// the relabel and removes its copies. A relabel an admin stopped while it
// copied writes nothing more and has its copies removed, whichever write it
// was at. A relabel no longer prepared is answered as it stands.
func (e *Engine) ContinueRelabel(ctx context.Context, id string) (tenant.Correction, error) {
	defer e.startCopy(id)()
	c, err := e.cfg.Store.Correction(ctx, id)
	if err != nil || c.Kind != record.KindRelabel || c.State != record.StatePrepared {
		return c, err
	}
	if err := e.fence(ctx); err != nil {
		return c, err
	}
	if c.RecordsState != record.StatePrepared {
		if err := e.writeRecords(ctx, c); err != nil {
			return c, err
		}
	}
	scans, err := e.cfg.Store.CorrectionScans(ctx, id)
	if err != nil {
		return c, err
	}
	copies, err := e.cfg.Store.CorrectionCopies(ctx, id)
	if err != nil {
		return c, err
	}
	// Every copy, on the primaries and the replicas in step, and the publish
	// that names them land on one place per storage: each storage's gate is
	// held from before it is opened until the publish is answered, so a
	// switch waits for the relabel (or is refused at its drain's end), and
	// never swaps a place between two of its writes, which would leave some
	// copies on a place the catalog then names no longer.
	written := make([]string, 0, len(scans)+len(copies))
	for _, s := range scans {
		written = append(written, s.Storage)
	}
	for _, cp := range copies {
		written = append(written, cp.Replica)
	}
	release, err := e.hold(ctx, written)
	if err != nil {
		return c, err
	}
	defer release()
	manifests := map[string]tenant.SHA256{}
	sidecars := map[string]tenant.SHA256{}
	snapshots := map[string]tenant.ScanSnapshot{}
	replicas := map[string]map[string]tenant.SHA256{}
	var copied int64
	for _, s := range scans {
		st, err := e.cfg.OpenStorage(ctx, s.Storage)
		if err != nil {
			return c, err
		}
		sum, snap, meta, n, err := e.copyScan(ctx, c, st, s, &copied, tenant.SHA256{})
		if errors.Is(err, errStopped) {
			release()
			return e.stopped(ctx, id)
		}
		if errors.Is(err, errCopy) {
			release()
			return e.abandon(ctx, c, reasonUnreadable, err)
		}
		if err != nil {
			return c, err
		}
		manifests[s.SeriesUID], snapshots[s.SeriesUID] = sum, snap
		if !meta.IsZero() {
			// The new metadata file's digest is kept with the new manifest,
			// so the scan's metadata stays checked under its new labels.
			sidecars[s.SeriesUID] = meta
		}
		copied += n
		// Each replica in step for the scan gets the new keys before the
		// publish, from its own objects, so a read may turn to it the moment
		// the labels switch; a replica not in step, or that does not answer,
		// is not waited for: the publish queues it, and the replicator
		// copies the scan under its new labels.
		for _, cp := range copies {
			if cp.Primary != s.Storage || !cp.HoldsScan(s.SeriesUID) {
				continue
			}
			rsum, err := e.copyToReplica(ctx, c, cp.Replica, s, snap.Manifest)
			if errors.Is(err, errStopped) {
				release()
				return e.stopped(ctx, id)
			}
			if err != nil {
				e.cfg.Log.Warn("a relabel did not copy to a replica in step; the replica copies the scan after the publish", "correction", id,
					"replica", cp.Replica, "error", err.Error())
				continue
			}
			if rsum == sum {
				if replicas[cp.Replica] == nil {
					replicas[cp.Replica] = map[string]tenant.SHA256{}
				}
				replicas[cp.Replica][s.SeriesUID] = rsum
			}
		}
	}
	if err := e.cfg.Store.RecordCopied(ctx, id, copied); err != nil {
		return c, err
	}
	if err := e.reached(c, "copied"); err != nil {
		return c, err
	}
	cc := tenant.CorrectionCommit{ID: c.ID, Manifests: manifests, Sidecars: sidecars, Snapshots: snapshots, ReplicaManifests: replicas}
	return e.published(ctx, c, func() (tenant.Correction, string, error) { return e.commit(ctx, cc) }, release)
}

// copyToReplica copies one scan of a relabel on a replica in step for it:
// from the replica's own objects under the old labels to their keys under
// the new ones, each read back and compared, and the new metadata and
// manifest. It answers the new manifest's SHA-256. The replica's manifest
// must be the primary's (want), read before the copy; one that is not is
// not in step, and nothing is written there.
func (e *Engine) copyToReplica(ctx context.Context, c tenant.Correction, replica string, s tenant.CorrectionScan,
	want tenant.SHA256) (tenant.SHA256, error) {
	st, err := e.cfg.OpenStorage(ctx, replica)
	if err != nil {
		return tenant.SHA256{}, err
	}
	sum, _, _, _, err := e.copyScan(ctx, c, st, s, nil, want)
	return sum, err
}

// stopped removes the copies of a relabel its copy found stopped, after its
// last write, and answers the relabel as it is.
func (e *Engine) stopped(ctx context.Context, id string) (tenant.Correction, error) {
	c, err := e.cfg.Store.Correction(ctx, id)
	if err != nil {
		return c, err
	}
	if c.State == record.StateAbandoned && !c.Swept {
		if err := e.sweep(ctx, c); err != nil {
			return c, err
		}
		if c, err = e.cfg.Store.Correction(ctx, id); err != nil {
			return c, err
		}
		if c.RecordsState != c.State {
			e.settle(ctx, c)
		}
	}
	return c, nil
}

// errStopped is a relabel an admin stopped while it copied.
var errStopped = errors.New("the relabel was stopped")

// errNotInStep is a replica whose manifest of a scan is not the primary's:
// a relabel does not copy to it before the publish.
var errNotInStep = errors.New("the replica's manifest is not the primary's")

// newKey returns the key of an object of the relabel's scan s under its
// new labels: the same name, revision and all, in the new scan's tree.
func newKey(key string, to label.Path) (string, error) {
	k, err := layout.Parse(key)
	if err != nil {
		return "", fault.Wrap(fault.Unavailable, err, "a manifest names a key that is not one of the layout")
	}
	k.Path = to
	return k.String()
}

// copyScan copies one scan's objects on st and writes its manifest and
// metadata under the new labels; it answers the new manifest's SHA-256,
// what it read of the old manifest and metadata (what the purge may
// remove), the new metadata file's SHA-256 (zero when the scan has none),
// and how many objects it copied. The relabel's row is read before every
// write, and its progress recorded from copied (nil for a replica's copy,
// which counts nothing). want, when not zero, is the manifest the scan's
// must be (a replica's, the primary's): another is errNotInStep, before
// anything is written. The caller holds st's write gate.
func (e *Engine) copyScan(ctx context.Context, c tenant.Correction, st blob.Store, s tenant.CorrectionScan,
	copied *int64, want tenant.SHA256) (sum tenant.SHA256, snap tenant.ScanSnapshot, meta tenant.SHA256, n int64, err error) {
	// The results are named so each failure below answers what was done so
	// far (snap, n) with no new manifest and no metadata digest.
	fail := func(err error) (tenant.SHA256, tenant.ScanSnapshot, tenant.SHA256, int64, error) {
		return tenant.SHA256{}, snap, tenant.SHA256{}, n, err
	}
	progress := func() int64 {
		if copied == nil {
			return 0
		}
		return *copied + n
	}
	mk, _ := layout.ManifestKey(s.Path)
	raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
	if err != nil {
		return fail(errors.Join(errCopy, err))
	}
	snap.Manifest, snap.ManifestSize = sha256.Sum256(raw), int64(len(raw))
	if !want.IsZero() && snap.Manifest != want {
		return fail(errNotInStep)
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		return fail(errors.Join(errCopy, err))
	}
	files := make([]manifest.File, len(m.Files))
	for i, f := range m.Files {
		if err := e.checkStill(ctx, c.ID, progress()); err != nil {
			return fail(err)
		}
		to, err := newKey(f.Key, s.NewPath)
		if err != nil {
			return fail(errors.Join(errCopy, err))
		}
		if err := copyObject(ctx, st, f.Key, to, f.Size, shaOf(f.SHA256)); err != nil {
			return fail(err)
		}
		f.Key = to
		files[i] = f
		n++
		if err := e.reached(c, "relabel-object"); err != nil {
			return fail(err)
		}
	}
	if err := e.checkStill(ctx, c.ID, progress()); err != nil {
		return fail(err)
	}
	if snap.Metadata, snap.MetadataSize, meta, err = copyMetadata(ctx, st, s.Path, s.NewPath); err != nil {
		return fail(err)
	}
	if err := e.reached(c, "relabel-metadata"); err != nil {
		return fail(err)
	}
	b, err := manifest.Encode(manifest.Manifest{Files: files})
	if err != nil {
		return fail(fault.Wrap(fault.Internal, err, "a scan's manifest cannot be made"))
	}
	if err := e.checkStill(ctx, c.ID, progress()); err != nil {
		return fail(err)
	}
	nk, _ := layout.ManifestKey(s.NewPath)
	if _, err := st.Put(ctx, nk, bytes.NewReader(b)); err != nil {
		return fail(err)
	}
	if err := e.reached(c, "relabel-manifest"); err != nil {
		return fail(err)
	}
	return sha256.Sum256(b), snap, meta, n, nil
}

// checkStill answers errStopped when the relabel is no longer prepared,
// and records its progress every copyCheck objects.
func (e *Engine) checkStill(ctx context.Context, id string, copied int64) error {
	if copied > 0 && copied%copyCheck == 0 {
		if err := e.cfg.Store.RecordCopied(ctx, id, copied); err != nil {
			return err
		}
	}
	c, err := e.cfg.Store.Correction(ctx, id)
	if err != nil {
		return err
	}
	if c.State != record.StatePrepared {
		return errStopped
	}
	return nil
}

// copyObject copies from to to, then reads the copy back: it must have the
// size and SHA-256 the manifest names, or the copy is errCopy.
func copyObject(ctx context.Context, st blob.Store, from, to string, size int64, sum tenant.SHA256) error {
	rc, err := st.Read(ctx, from, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return errors.Join(errCopy, err)
	}
	if err != nil {
		return err
	}
	w, err := st.Put(ctx, to, rc)
	_ = rc.Close()
	if err != nil {
		return err
	}
	if w.Size != size || tenant.SHA256(w.SHA256) != sum {
		return errCopy
	}
	n, back, err := hashOf(ctx, st, to)
	if err != nil {
		return err
	}
	if n != size || tenant.SHA256(back) != sum {
		return errCopy
	}
	return nil
}

// copyMetadata writes the scan's metadata under the new labels, each
// line's key the object's under them, and answers the SHA-256 and size of
// the metadata it read and the SHA-256 of the one it wrote. A scan with no
// metadata has none to copy, and answers no digest of a file written.
func copyMetadata(ctx context.Context, st blob.Store, from, to label.Path) (tenant.SHA256, int64, tenant.SHA256, error) {
	var sum, none tenant.SHA256
	sk, _ := layout.MetadataKey(from)
	rc, err := st.Read(ctx, sk, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return sum, 0, none, nil
	}
	if err != nil {
		return sum, 0, none, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(rc, h)}
	rd := sidecar.NewReader(counted)
	var buf bytes.Buffer
	var w *sidecar.Writer
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, 0, none, errors.Join(errCopy, err)
		}
		if w == nil {
			if hd := rd.Scan(); hd != nil {
				w = sidecar.NewWriterV3(&buf, *hd)
			} else {
				w = sidecar.NewWriter(&buf)
			}
		}
		if in.Key, err = newKey(in.Key, to); err != nil {
			return sum, 0, none, errors.Join(errCopy, err)
		}
		if err := w.Add(in); err != nil {
			return sum, 0, none, errors.Join(errCopy, err)
		}
	}
	// What the reader left unread (none for a whole sidecar) is hashed
	// too, so the checksum is the object's.
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return sum, 0, none, fault.Wrap(fault.Unavailable, err, "a scan's metadata cannot be read")
	}
	h.Sum(sum[:0])
	if w == nil {
		return sum, counted.n, none, nil
	}
	if err := w.Close(); err != nil {
		return sum, 0, none, errors.Join(errCopy, err)
	}
	written := tenant.SHA256(sha256.Sum256(buf.Bytes()))
	nk, _ := layout.MetadataKey(to)
	if _, err := st.Put(ctx, nk, &buf); err != nil {
		return sum, 0, none, err
	}
	return sum, counted.n, written, nil
}

// countingReader counts what passes through it.
type countingReader struct {
	r io.Reader
	n int64
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// removeCopies removes what a relabel that will not be published copied:
// each object's key under the new labels, as the old manifest names the
// objects, and the new manifests and metadata, then the directories that
// leaves empty. Each key is the relabel's own while no live scan and no
// other correction not ended holds its new path (SweepableScans); a scan
// whose new path is held since (the relabel was stopped long ago, and the
// label taken) is left, its copies the holder's to judge.
func (e *Engine) removeCopies(ctx context.Context, c tenant.Correction) error {
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil {
		return err
	}
	free, err := e.cfg.Store.SweepableScans(ctx, c.ID)
	if err != nil {
		return err
	}
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, s := range scans {
		if !free[s.SeriesUID] {
			e.cfg.Log.Warn("a stopped relabel's copies are left: their new labels are held since", "correction", c.ID)
			continue
		}
		// Each copy's removals run inside its storage's write gate (gated),
		// as the copies were written: through a store opened before a switch
		// swapped its place they would miss the copies, now on the new place.
		var files []manifest.File
		if err := e.gated(ctx, s.Storage, func(st blob.Store) error {
			mk, _ := layout.ManifestKey(s.Path)
			raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
			if err == nil {
				if m, err := manifest.Decode(bytes.NewReader(raw)); err == nil {
					files = m.Files
				}
			} else if fault.KindOf(err) != fault.NotFound {
				return err
			}
			return e.removeCopiesOn(ctx, st, s, files)
		}); err != nil {
			return err
		}
		// A replica the copy reached holds the same keys under the new
		// labels. One that does not answer keeps them: the same bytes under
		// labels nothing reads, which a later copy of a scan there writes
		// over, and no reason to keep the relabel prepared.
		for _, cp := range copies {
			if cp.Primary != s.Storage {
				continue
			}
			err := e.gated(ctx, cp.Replica, func(rst blob.Store) error { return e.removeCopiesOn(ctx, rst, s, files) })
			if err != nil {
				e.cfg.Log.Warn("a stopped relabel's copies on a replica could not be removed", "correction", c.ID, "replica", cp.Replica,
					"error", fault.Message(err))
			}
		}
	}
	return nil
}

// removeCopiesOn removes, from one copy, a relabel's copies of scan s:
// each object the old manifest names (files) at its key under the new
// labels, then the new metadata and manifest, then the directories that
// leaves empty. The caller holds st's write gate.
func (e *Engine) removeCopiesOn(ctx context.Context, st blob.Store, s tenant.CorrectionScan, files []manifest.File) error {
	for _, f := range files {
		to, err := newKey(f.Key, s.NewPath)
		if err != nil {
			continue
		}
		if err := st.Delete(ctx, to); err != nil {
			return err
		}
	}
	for _, k := range []func(label.Path) (string, error){layout.MetadataKey, layout.ManifestKey} {
		key, _ := k(s.NewPath)
		if err := st.Delete(ctx, key); err != nil {
			return err
		}
	}
	e.pruneOn(ctx, st, s.NewPath)
	return nil
}
