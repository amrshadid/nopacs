package correct

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

// ReplaceRequest is a replace a person asked for, from a quarantined file
// the caller has already de-identified as its receiver's profile version
// says (as a release would).
type ReplaceRequest struct {
	// Quarantine is the quarantined file's id.
	Quarantine string
	// Storage and Path are where the archived instance is, and UID its SOP
	// Instance UID, as the catalog holds it now.
	Storage string
	Path    label.Path
	UID     string
	// Open opens the new bytes; it is called twice (to hash them, then to
	// put them) and must give the same bytes each time.
	Open func() (io.ReadCloser, error)
	// Line is the new revision's line of the scan's metadata, as the
	// pipeline makes it for these bytes; the engine sets its key,
	// revision, size and SHA-256, and its held time to the old line's (when
	// the instance was first held, which a replace does not change).
	Line sidecar.Instance
	// Reason is why; KeyID the key that asked.
	Reason, KeyID string
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// Replace puts the quarantined file in place of an archived instance at
// the instance's next revision key, never at the key it has: its row, its
// records, the new object read back, the scan's manifest and metadata
// naming it, then the publish, which also clears the instance's damage
// mark. A failure before the publish writes the manifest and metadata
// back, removes the new object, and abandons it. The same file asked again
// answers the replace it made. From its prepare on it runs apart from
// ctx's end (detached), so a client that hangs up never leaves it
// prepared with its new object half written.
func (e *Engine) Replace(ctx context.Context, r ReplaceRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	size, sum, err := hashReader(r.Open)
	if err != nil {
		return tenant.Correction{}, err
	}
	st, err := e.cfg.OpenStorage(ctx, r.Storage)
	if err != nil {
		return tenant.Correction{}, err
	}
	restore, err := lineOf(ctx, st, r.Path, r.UID)
	if err != nil {
		return tenant.Correction{}, err
	}
	var was sidecar.Instance
	if err := json.Unmarshal(restore, &was); err != nil {
		return tenant.Correction{}, fault.Wrap(fault.Internal, err, "an instance's line cannot be read back")
	}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindReplace, Quarantine: r.Quarantine,
		NewSHA256: sum, NewSize: size, RestoreLine: restore, Reason: r.Reason, KeyID: r.KeyID, Now: now}, r.Audit, r.Guard)
	if err != nil || !made {
		return c, err
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if c.Target.Path() != r.Path || c.Target.Instance != r.UID {
		return e.abandon(ctx, c, reasonChanged, fault.New(fault.Precondition, "the quarantined file names another instance than the request"))
	}
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	if err := e.reached(c, "records"); err != nil {
		return c, err
	}
	key, err := layout.RevisionKey(r.Path, r.UID, c.NewRevision)
	if err != nil {
		return e.abandon(ctx, c, reasonUnreadable, fault.Wrap(fault.Internal, err, "a revision key cannot be made"))
	}
	// The new revision, the manifest and metadata naming it and the publish
	// all land on one place: the storage's gate is held from before it is
	// opened again until the publish is answered, so a switch waits for
	// them, and never leaves the new revision on a place the catalog then
	// names no longer.
	release, err := e.hold(ctx, []string{r.Storage})
	if err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	defer release()
	abandon := func(why string, err error) (tenant.Correction, error) {
		release()
		return e.abandon(ctx, c, why, err)
	}
	if st, err = e.cfg.OpenStorage(ctx, r.Storage); err != nil {
		return abandon(reasonStorage, err)
	}
	rc, err := r.Open()
	if err != nil {
		return abandon(reasonUnreadable, err)
	}
	w, err := st.Put(ctx, key, rc)
	_ = rc.Close()
	if err == nil && (w.Size != size || tenant.SHA256(w.SHA256) != sum) {
		err = errCopy
	}
	if err == nil {
		n, back, herr := hashOf(ctx, st, key)
		if err = herr; err == nil && (n != size || tenant.SHA256(back) != sum) {
			err = errCopy
		}
	}
	if err != nil {
		return abandon(reasonUnreadable, err)
	}
	if err := e.reached(c, "object"); err != nil {
		return c, err
	}
	line := r.Line
	line.SOPInstanceUID, line.Key, line.Size, line.SHA256 = r.UID, key, size, sha256Text(sum)
	line.Revision = c.NewRevision
	// held_at is when the instance was first held, which a replace keeps on
	// its row (a job bounded by a time before the replace still holds the
	// instance); the line says so too, since once the purge removes the old
	// revision's line it is the only place a rebuild reads it from.
	if !was.HeldAt.IsZero() {
		line.HeldAt = was.HeldAt
	}
	// The checksum the file was received with, which the catalog takes as
	// the instance's, when it is not the archived one (a de-identified
	// file), as a rebuild reads it back.
	line.ReceivedSHA256 = ""
	if c.ReceivedSHA256 != sum {
		line.ReceivedSHA256 = sha256Text(c.ReceivedSHA256)
	}
	file := manifest.File{Key: key, Length: size, Size: size, SHA256: sha256Text(sum), SOPInstanceUID: r.UID}
	meta, err := e.rewriteMeta(ctx, st, r.Storage, r.Path, r.UID, &line, &file)
	if err != nil {
		return abandon(reasonStorage, err)
	}
	if err := e.reached(c, "meta"); err != nil {
		return c, err
	}
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil || len(scans) != 1 {
		return abandon(reasonStopped, errors.Join(fault.New(fault.Internal, "a replace touches one scan"), err))
	}
	// The metadata file's digest is kept with the new manifest, so the
	// scan's metadata stays checked after the replace.
	series := scans[0].SeriesUID
	cc := tenant.CorrectionCommit{ID: c.ID, Manifests: map[string]tenant.SHA256{series: meta.after}}
	if !meta.metadata.IsZero() {
		cc.Sidecars = map[string]tenant.SHA256{series: meta.metadata}
	}
	return e.published(ctx, c, func() (tenant.Correction, string, error) { return e.commit(ctx, cc) }, release)
}

// hashReader reads what open gives and returns its size and SHA-256.
func hashReader(open func() (io.ReadCloser, error)) (int64, tenant.SHA256, error) {
	if open == nil {
		return 0, tenant.SHA256{}, fault.New(fault.Invalid, "a replace names its new bytes")
	}
	rc, err := open()
	if err != nil {
		return 0, tenant.SHA256{}, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	var sum tenant.SHA256
	h.Sum(sum[:0])
	if err != nil {
		return 0, sum, fault.Wrap(fault.Unavailable, err, "the new bytes cannot be read")
	}
	return n, sum, nil
}

// lineOf returns the instance's line of the scan's metadata, as JSON: what
// a replace stopped before its publish writes back.
func lineOf(ctx context.Context, st interface {
	Read(context.Context, string, int64, int64) (io.ReadCloser, error)
}, p label.Path, uid string) ([]byte, error) {
	sk, _ := layout.MetadataKey(p)
	rc, err := st.Read(ctx, sk, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil, fault.New(fault.Precondition, "the scan's metadata holds no line of the instance")
		}
		if err != nil {
			return nil, fault.Wrap(fault.Unavailable, err, "the scan's metadata cannot be read")
		}
		if in.SOPInstanceUID == uid {
			b, err := json.Marshal(in)
			if err != nil {
				return nil, fault.Wrap(fault.Internal, err, "an instance's line cannot be kept")
			}
			return b, nil
		}
	}
}

// rollbackReplace writes back what a prepared replace changed on its
// storage: the scan's manifest and metadata name the old revision again
// (from the catalog's checksum and the line kept with the row), and the new
// revision's object is removed.
func (e *Engine) rollbackReplace(ctx context.Context, c tenant.Correction) error {
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil || len(scans) != 1 {
		return errors.Join(fault.New(fault.Internal, "a replace touches one scan"), err)
	}
	s := scans[0]
	key, err := layout.RevisionKey(s.Path, c.Target.Instance, c.NewRevision)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
	}
	// Inside the storage's gate, like the writes it undoes: what it writes
	// back must land where the replace wrote.
	return e.gated(ctx, s.Storage, func(st blob.Store) error {
		if len(c.RestoreLine) > 0 {
			var old sidecar.Instance
			if err := json.Unmarshal(c.RestoreLine, &old); err != nil {
				return fault.Wrap(fault.Internal, err, "a replace's kept line cannot be read")
			}
			file := manifest.File{Key: c.InstanceKey, Length: c.InstanceSize, Size: c.InstanceSize, SHA256: sha256Text(c.OldSHA256),
				SOPInstanceUID: c.Target.Instance}
			mk, err := layout.ManifestKey(s.Path)
			if err != nil {
				return fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
			}
			// Written and kept as the catalog's under the scan's lock
			// (keepRolledBack): a reception may have committed the scan
			// since the replace wrote it.
			unlock := e.lockScan(s.Storage, mk)
			defer unlock()
			w, err := e.rewriteMetaOn(ctx, st, s.Path, c.Target.Instance, &old, &file)
			if err != nil {
				return err
			}
			if err := e.keepRolledBack(ctx, c.ID, s.SeriesUID)(w); err != nil {
				return err
			}
		}
		return st.Delete(ctx, key)
	})
}
