package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"

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

// An edit's manifests and metadata written again, and what is written
// back: by a stop or a failure before its publish, and by its undo.

// rewriteMetaMany writes a scan's manifest and metadata again on st, opened
// by a caller that holds the storage's write gate, under the scan's lock:
// each instance of lines given its line and its manifest entry of files in
// place of its own, and, when change is not nil, the scan header's values
// changed by it (a sidecar of version 3; an older one has none). The
// metadata is written first: the manifest names what the scan holds. When
// keep is not nil it is given what was written before the lock is let go
// (a rollback keeps the digests as the catalog's with it).
func (e *Engine) rewriteMetaMany(ctx context.Context, st blob.Store, storage string, p label.Path, lines map[string]sidecar.Instance,
	files map[string]manifest.File, change func(*sidecar.Values), keep func(metaWritten) error) (metaWritten, error) {
	mk, err := layout.ManifestKey(p)
	if err != nil {
		return metaWritten{}, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	unlock := e.lockScan(storage, mk)
	defer unlock()
	return e.rewriteMetaManyOn(ctx, st, p, lines, files, change, keep)
}

// rewriteMetaManyOn is rewriteMetaMany for a caller that holds the scan's
// lock.
func (e *Engine) rewriteMetaManyOn(ctx context.Context, st blob.Store, p label.Path, lines map[string]sidecar.Instance,
	files map[string]manifest.File, change func(*sidecar.Values), keep func(metaWritten) error) (metaWritten, error) {
	var w metaWritten
	mk, err := layout.ManifestKey(p)
	if err != nil {
		return w, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	sk, _ := layout.MetadataKey(p)
	raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
	if err != nil {
		return w, err
	}
	w.before = sha256.Sum256(raw)
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		return w, fault.Wrap(fault.Unavailable, err, "a scan's manifest cannot be read")
	}
	out := make([]manifest.File, 0, len(m.Files))
	for _, f := range m.Files {
		if _, ok := files[f.SOPInstanceUID]; !ok {
			out = append(out, f)
		}
	}
	uids := make([]string, 0, len(files))
	for uid := range files {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	for _, uid := range uids {
		out = append(out, files[uid])
	}
	scan, err := scanHeaderOf(ctx, st, sk)
	if err != nil {
		return w, err
	}
	old, err := st.Read(ctx, sk, 0, -1)
	switch {
	case fault.KindOf(err) == fault.NotFound:
	case err != nil:
		return w, err
	default:
		added := make([]sidecar.Instance, 0, len(uids))
		for _, uid := range uids {
			if in, ok := lines[uid]; ok {
				added = append(added, in)
			}
		}
		var buf bytes.Buffer
		var merr error
		drop := func(uid string) bool { _, ok := lines[uid]; return ok }
		if scan != nil && change != nil {
			h := *scan
			change(&h.Values)
			_, merr = sidecar.MergeScan(&buf, old, h, sidecar.List(added), drop)
		} else {
			_, merr = sidecar.Merge(&buf, old, sidecar.List(added), drop)
		}
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
	b, err := manifest.Encode(manifest.Manifest{Files: out})
	if err != nil {
		return w, fault.Wrap(fault.Internal, err, "a scan's manifest cannot be made")
	}
	if _, err := st.Put(ctx, mk, bytes.NewReader(b)); err != nil {
		return w, err
	}
	w.after = sha256.Sum256(b)
	if keep != nil {
		return w, keep(w)
	}
	return w, nil
}

// scanHeaderOf reads the scan header of a sidecar of version 3; nil for an
// older one, or none.
func scanHeaderOf(ctx context.Context, st blob.Store, key string) (*sidecar.ScanHeader, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	if _, _, err := rd.Next(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fault.Wrap(fault.Unavailable, err, "the scan's metadata cannot be read")
	}
	return rd.Scan(), nil
}

// oldScan is one scan of an edit's instances: the lines and manifest
// entries that name the old revisions again and the scan's values as they
// were, and those that name the new revisions and its values now, which an
// undo that may not be published writes back.
type oldScan struct {
	storage string
	path    label.Path
	series  string
	lines   map[string]sidecar.Instance
	files   map[string]manifest.File
	before  tenant.ScanValues
	// newLines, newFiles and after are the published side.
	newLines map[string]sidecar.Instance
	newFiles map[string]manifest.File
	after    tenant.ScanValues
}

// oldScans groups an edit's instances by scan, each with what names its
// old and its new revision; an instance whose line was cleared is left out
// of lines (or newLines).
func oldScans(edits []tenant.EditedInstance) ([]*oldScan, error) {
	var out []*oldScan
	for _, in := range edits {
		if n := len(out); n == 0 || out[n-1].series != in.SeriesUID {
			out = append(out, &oldScan{storage: in.Storage, path: in.Path, series: in.SeriesUID, before: in.Before, after: in.After,
				lines: map[string]sidecar.Instance{}, files: map[string]manifest.File{},
				newLines: map[string]sidecar.Instance{}, newFiles: map[string]manifest.File{}})
		}
		s := out[len(out)-1]
		for _, kept := range []struct {
			raw  []byte
			into map[string]sidecar.Instance
		}{{in.RestoreLine, s.lines}, {in.NewLine, s.newLines}} {
			if len(kept.raw) == 0 {
				continue
			}
			var line sidecar.Instance
			if err := json.Unmarshal(kept.raw, &line); err != nil {
				return nil, fault.Wrap(fault.Internal, err, "an edit's kept line cannot be read")
			}
			kept.into[in.UID] = line
		}
		s.files[in.UID] = manifest.File{Key: in.OldKey, Length: in.OldSize, Size: in.OldSize, SHA256: sha256Text(in.OldSHA256),
			SOPInstanceUID: in.UID}
		if !in.NewSHA256.IsZero() {
			if key, err := layout.RevisionKey(in.Path, in.UID, in.NewRevision); err == nil {
				s.newFiles[in.UID] = manifest.File{Key: key, Length: in.NewSize, Size: in.NewSize, SHA256: sha256Text(in.NewSHA256),
					SOPInstanceUID: in.UID}
			}
		}
	}
	return out, nil
}

// lockScans takes the lock of every scan, in one order (storage, then
// manifest key) so two callers never wait on each other, and returns the
// release of them all.
func (e *Engine) lockScans(scans []*oldScan) (func(), error) {
	type at struct{ storage, key string }
	keys := make([]at, 0, len(scans))
	for _, s := range scans {
		mk, err := layout.ManifestKey(s.path)
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
		}
		keys = append(keys, at{s.storage, mk})
	}
	slices.SortFunc(keys, func(a, b at) int { return strings.Compare(a.storage+"\x00"+a.key, b.storage+"\x00"+b.key) })
	keys = slices.Compact(keys)
	unlocks := make([]func(), 0, len(keys))
	for _, k := range keys {
		unlocks = append(unlocks, e.lockScan(k.storage, k.key))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		})
	}, nil
}

// writeNewOn writes a scan's manifest and metadata naming an edit's new
// revisions again, with the scan's values now, for a caller that holds the
// scan's lock: what an undo that may not be published leaves.
func (e *Engine) writeNewOn(ctx context.Context, st blob.Store, s *oldScan) (metaWritten, error) {
	after := sidecar.ValuesOf(s.after)
	return e.rewriteMetaManyOn(ctx, st, s.path, s.newLines, s.newFiles, func(v *sidecar.Values) {
		v.Modality, v.SeriesDescription, v.SeriesNumber = after.Modality, after.SeriesDescription, after.SeriesNumber
	}, nil)
}

// writeOld writes a scan's manifest and metadata naming an edit's old
// revisions again, with the values the scan had; keep as rewriteMetaMany's.
func (e *Engine) writeOld(ctx context.Context, st blob.Store, s *oldScan, keep func(metaWritten) error) (metaWritten, error) {
	mk, err := layout.ManifestKey(s.path)
	if err != nil {
		return metaWritten{}, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	unlock := e.lockScan(s.storage, mk)
	defer unlock()
	return e.writeOldOn(ctx, st, s, keep)
}

// writeOldOn is writeOld for a caller that holds the scan's lock.
func (e *Engine) writeOldOn(ctx context.Context, st blob.Store, s *oldScan, keep func(metaWritten) error) (metaWritten, error) {
	before := sidecar.ValuesOf(s.before)
	return e.rewriteMetaManyOn(ctx, st, s.path, s.lines, s.files, func(v *sidecar.Values) {
		v.Modality, v.SeriesDescription, v.SeriesNumber = before.Modality, before.SeriesDescription, before.SeriesNumber
	}, keep)
}

// keepRolledBack returns the keep of a prepared correction's rollback of
// the scan of series: the digests of what it wrote kept as the catalog's,
// under the scan's lock. A reception that committed the scan between the
// correction's write and its rollback (and so made the publish refuse it,
// the scan's instances changed) left the catalog naming the manifest the
// rollback wrote over.
func (e *Engine) keepRolledBack(ctx context.Context, id, series string) func(metaWritten) error {
	return func(w metaWritten) error {
		return e.cfg.Store.KeepRolledBack(ctx, id, series, w.after, w.metadata)
	}
}

// rollbackEdit writes back what a prepared edit changed on its storages:
// each scan's manifest and metadata name the old revisions again, kept as
// the catalog's (keepRolledBack), and every new revision's object is
// removed (one never written is not there).
func (e *Engine) rollbackEdit(ctx context.Context, c tenant.Correction) error {
	edits, err := e.editsOf(ctx, c)
	if err != nil {
		return err
	}
	scans, err := oldScans(edits)
	if err != nil {
		return err
	}
	for _, s := range scans {
		err := e.gated(ctx, s.storage, func(st blob.Store) error {
			if _, err := e.writeOld(ctx, st, s, e.keepRolledBack(ctx, c.ID, s.series)); err != nil && fault.KindOf(err) != fault.NotFound {
				return err
			}
			return removeNew(ctx, st, edits, s.series)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// removeNew removes from st the new revision of each instance of an edit
// in the scan of series; one not there is not an error.
func removeNew(ctx context.Context, st blob.Store, edits []tenant.EditedInstance, series string) error {
	for _, in := range edits {
		if in.SeriesUID != series {
			continue
		}
		key, err := layout.RevisionKey(in.Path, in.UID, in.NewRevision)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
		}
		if err := st.Delete(ctx, key); err != nil && fault.KindOf(err) != fault.NotFound {
			return err
		}
	}
	return nil
}

// removeUndone removes from st the new revision an undone edit wrote of
// one instance, only when the object at its key holds the bytes the edit's
// publish recorded: a key holds the file one correction wrote, and an
// object that is not that file (none recorded, as for an edit known only
// from records written before its publish, or other bytes) is left for the
// scrub to name, never removed on a guess. One not there is not an error.
func removeUndone(ctx context.Context, st blob.Store, in tenant.EditedInstance) error {
	if in.NewSHA256.IsZero() {
		return nil
	}
	key, err := layout.RevisionKey(in.Path, in.UID, in.NewRevision)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
	}
	_, sum, err := hashOf(ctx, st, key)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil
	case err != nil:
		return err
	case tenant.SHA256(sum) != in.NewSHA256:
		return nil
	}
	if err := st.Delete(ctx, key); err != nil && fault.KindOf(err) != fault.NotFound {
		return err
	}
	return nil
}

// editsOf returns an edit's instances: those c carries, else the store's.
func (e *Engine) editsOf(ctx context.Context, c tenant.Correction) ([]tenant.EditedInstance, error) {
	if len(c.Edits) > 0 {
		return c.Edits, nil
	}
	ed, err := e.edits()
	if err != nil {
		return nil, err
	}
	return ed.CorrectionEdits(ctx, c.ID)
}

// UndoEdit undoes a committed edit before its purge began: it is marked
// being undone, each scan's manifest and metadata are written to name the
// old revisions again, then the undo is published (the old revisions in
// use, the scans' values as they were, the new revisions the edit's own),
// its records rewritten undone, and the new revisions removed from every
// copy (sweepEdit). A stop part way is taken up by Recover: an edit marked
// being undone is undone, an undone one not swept is swept.
func (e *Engine) UndoEdit(ctx context.Context, id, reason string, audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	ed, err := e.edits()
	if err != nil {
		return tenant.Correction{}, err
	}
	cur, err := e.cfg.Store.Correction(ctx, id)
	if err != nil {
		return cur, err
	}
	if cur.State == record.StateCommitted {
		for _, in := range cur.Edits {
			// An edit known only from its records (a restore) holds no line
			// of its old revisions to write back.
			if len(in.RestoreLine) == 0 {
				return cur, fault.New(fault.Precondition, "the edit was taken from its records and keeps no line of its old revisions; it is not undone")
			}
		}
	}
	c, err := ed.BeginEditUndo(ctx, id, reason, e.now(), audit, guard)
	if err != nil {
		return c, err
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if c.State == record.StateUndone {
		if !c.Swept {
			return c, e.sweepEdit(ctx, c)
		}
		return c, nil
	}
	return e.continueUndo(ctx, c)
}

// continueUndo writes the manifests and metadata of an edit marked being
// undone, publishes the undo and sweeps its new revisions.
//
// Every scan of the edit is locked from the first write of its old
// manifest until the undo is published or written back: a reception of
// the scan builds its manifest from the catalog, which names the new
// revisions until the publish, and the publish keeps the digest of the
// manifest written here. An undo that may not be published (a hold placed,
// a rebuild, a switch or a repair since it was asked; checked before the
// first write and again by the publish) is written back: each scan's
// manifest and metadata name the new revisions again, their digests kept
// as the catalog's, and the undo is abandoned, the edit committed as it
// was. Abandoning it, rather than refusing the hold, keeps a hold what it
// is everywhere: it wins over a correction of what it covers, and the
// person asks for the undo again once it is released.
func (e *Engine) continueUndo(ctx context.Context, c tenant.Correction) (tenant.Correction, error) {
	ed, err := e.edits()
	if err != nil {
		return c, err
	}
	edits, err := e.editsOf(ctx, c)
	if err != nil {
		return c, err
	}
	scans, err := oldScans(edits)
	if err != nil {
		return c, err
	}
	release, err := e.hold(ctx, c.Storages)
	if err != nil {
		return c, err
	}
	defer release()
	unlock, err := e.lockScans(scans)
	if err != nil {
		return c, err
	}
	defer unlock()
	stores := make([]blob.Store, len(scans))
	for i, s := range scans {
		if stores[i], err = e.cfg.OpenStorage(ctx, s.storage); err != nil {
			return c, err
		}
	}
	if err := ed.CheckEditUndo(ctx, c.ID, e.now()); err != nil {
		if errors.Is(err, tenant.ErrNotBeingUndone) {
			// Another run of its undo ended it (published, or abandoned)
			// while this one waited for the scans: it writes nothing.
			return e.cfg.Store.Correction(ctx, c.ID)
		}
		if fault.KindOf(err) != fault.Precondition {
			return c, err
		}
		// A run stopped before may have written the old revisions' files.
		return c, errors.Join(err, e.abandonUndo(ctx, ed, c, scans, stores, err))
	}
	u := tenant.EditUndo{ID: c.ID, Manifests: map[string]tenant.SHA256{}, Sidecars: map[string]tenant.SHA256{}}
	for i, s := range scans {
		meta, err := e.writeOldOn(ctx, stores[i], s, nil)
		if err != nil {
			return c, err
		}
		u.Manifests[s.series] = meta.after
		if !meta.metadata.IsZero() {
			u.Sidecars[s.series] = meta.metadata
		}
	}
	if err := e.reached(c, "undo-meta"); err != nil {
		return c, err
	}
	u.Now = e.now()
	done, err := ed.UndoEdit(ctx, u)
	if errors.Is(err, tenant.ErrNotBeingUndone) {
		// Ended since the check by what holds none of these scans' locks
		// (an admin settling it by hand): nothing is written back over what
		// that left; the edit is answered as it stands.
		return e.cfg.Store.Correction(ctx, c.ID)
	}
	if fault.KindOf(err) == fault.Precondition {
		return c, errors.Join(err, e.abandonUndo(ctx, ed, c, scans, stores, err))
	}
	unlock()
	release()
	if err != nil {
		return c, err
	}
	if err := e.reached(done, "undone"); err != nil {
		return done, err
	}
	e.settle(ctx, done)
	if err := e.sweepEdit(ctx, done); err != nil {
		return done, err
	}
	return e.cfg.Store.Correction(ctx, done.ID)
}

// abandonUndo writes each scan of an edit back to name its new revisions,
// for a caller that holds every scan's lock, and abandons the undo with the
// digests of what it wrote (tenant.Edits.AbandonEditUndo), the server's
// audit entry saying why: cause's message, which names conditions (a hold,
// a rebuild), never a value.
func (e *Engine) abandonUndo(ctx context.Context, ed tenant.Edits, c tenant.Correction, scans []*oldScan, stores []blob.Store,
	cause error) error {
	now := e.now()
	audit := e.serverAudit("UndoCorrection", c.ID, now)
	audit.Outcome = master.OutcomeFailed
	audit.Reason = "undo taken back: " + fault.Message(cause)
	if len(audit.Reason) > master.MaxAuditReasonLength {
		audit.Reason = strings.ToValidUTF8(audit.Reason[:master.MaxAuditReasonLength], "")
	}
	u := tenant.EditUndo{ID: c.ID, Now: now, Manifests: map[string]tenant.SHA256{}, Sidecars: map[string]tenant.SHA256{}, Audit: audit}
	for i, s := range scans {
		meta, err := e.writeNewOn(ctx, stores[i], s)
		if err != nil {
			return err
		}
		u.Manifests[s.series] = meta.after
		if !meta.metadata.IsZero() {
			u.Sidecars[s.series] = meta.metadata
		}
	}
	_, err := ed.AbandonEditUndo(ctx, u)
	return err
}

// sweepEdit removes an undone edit's new revisions from the primary of
// each of its scans and from each replica of it, then records it swept. It
// waits, leaving it to a later run, while a copy to a replica that the
// undo ended can still put a new revision there (CopiesInFlightUntil).
func (e *Engine) sweepEdit(ctx context.Context, c tenant.Correction) error {
	if c.Kind != record.KindEdit || c.State != record.StateUndone || c.Swept {
		return nil
	}
	if e.now().Before(c.CopiesInFlightUntil) {
		return nil
	}
	edits, err := e.editsOf(ctx, c)
	if err != nil {
		return err
	}
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, s := range c.Storages {
		places := []string{s}
		for _, cp := range copies {
			if cp.Primary == s {
				places = append(places, cp.Replica)
			}
		}
		for _, place := range places {
			err := e.gated(ctx, place, func(st blob.Store) error {
				for _, in := range edits {
					if in.Storage == s {
						if err := removeUndone(ctx, st, in); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return e.cfg.Store.MarkSwept(ctx, c.ID)
}
