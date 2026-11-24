package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

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

// maxDue is how many due corrections, and retained versions, one PurgeDue
// takes.
const maxDue = 100

// targetPage is how many of a correction's objects a purge reads at once.
const targetPage = 500

// PurgeNow starts the purge of a committed correction now, as an admin
// asks (confirming an unresolved delete), and runs it. An admin's purge
// trusts the clock it runs at (tenant.Corrections.StartPurge).
func (e *Engine) PurgeNow(ctx context.Context, id string, audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	c, err := e.cfg.Store.StartPurge(ctx, id, e.now(), &audit, guard)
	if err != nil {
		return c, err
	}
	if c.State == record.StatePurged {
		return c, nil
	}
	return e.purge(ctx, c)
}

// StartPurge moves a committed correction to purging now, as an admin asks
// (confirming an unresolved delete), with the admin's audit entry, by the
// engine's clock, and answers it without running the purge, which the
// caller leaves to the background (ContinuePurge). A correction purging or
// purged is answered as it stands.
func (e *Engine) StartPurge(ctx context.Context, id string, audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	return e.cfg.Store.StartPurge(ctx, id, e.now(), &audit, guard)
}

// ContinuePurge runs the purge of a correction already purging, as when an
// admin's start of it (tenant.Corrections.StartPurge, with its audit entry)
// was answered first and its purge left to the background. A correction
// that is not purging is answered as it stands.
func (e *Engine) ContinuePurge(ctx context.Context, id string) (tenant.Correction, error) {
	c, err := e.cfg.Store.Correction(ctx, id)
	if err != nil || c.State != record.StatePurging {
		return c, err
	}
	return e.purge(ctx, c)
}

// PurgeDue runs, when the server's clock allows it (CheckPurgeClock), the
// purge of each committed correction whose grace period has ended and of
// each one a stop or a failure left purging, then tries again each version
// a storage retained whose retention has ended; it returns how many
// corrections it purged.
func (e *Engine) PurgeDue(ctx context.Context) (int, error) {
	now := e.now()
	if err := e.cfg.Store.CheckPurgeClock(ctx, now); err != nil {
		return 0, err
	}
	due, err := e.cfg.Store.DueCorrections(ctx, now, maxDue)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, d := range due {
		c := d
		if c.State == record.StateCommitted {
			c, err = e.cfg.Store.StartPurge(ctx, d.ID, e.now(), nil, nil)
		}
		if err == nil {
			_, err = e.purge(ctx, c)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if err := e.repurge(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

// purge removes the objects a purging correction owns (PurgeTargets),
// recording each, clears what else names a delete's data, then ends it
// purged. It never decides what to remove from what a storage holds now:
// an object a live row resolves to stays, and so does a manifest or a
// metadata whose bytes changed since the publish. Run again after a stop
// it goes on from the objects not yet settled.
func (e *Engine) purge(ctx context.Context, c tenant.Correction) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return c, err
	}
	if c.RecordsState != record.StatePurging {
		if err := e.writeRecords(ctx, c); err != nil {
			return c, err
		}
	}
	if err := e.seams.at("purge-started"); err != nil {
		return c, err
	}
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil {
		return c, err
	}
	if err := e.seedTargets(ctx, c, scans); err != nil {
		return c, err
	}
	if c.Kind == record.KindDelete && c.Level == record.LevelInstance {
		return e.purgeInstance(ctx, c, scans[0])
	}
	if err := e.purgeTargets(ctx, c); err != nil {
		return c, err
	}
	if c.Kind == record.KindReplace {
		if err := e.purgeOlderMeta(ctx, c, scans[0].Storage, scans[0].Path); err != nil {
			return c, err
		}
	} else {
		for _, s := range scans {
			if st, err := e.cfg.OpenStorage(ctx, s.Storage); err == nil {
				e.pruneDirs(ctx, st, s.Path)
			}
		}
	}
	return e.finish(ctx, c, tenant.SHA256{}, tenant.SHA256{})
}

// purgeInstance purges one instance's delete under its scan's lock: its
// object, then the scan's manifest and metadata written again without it
// and their older versions removed, then the end, which gives the scan the
// new manifest's checksum while the lock still keeps a reception of the
// scan from writing another.
func (e *Engine) purgeInstance(ctx context.Context, c tenant.Correction, s tenant.CorrectionScan) (tenant.Correction, error) {
	mk, err := layout.ManifestKey(s.Path)
	if err != nil {
		return c, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	unlock := e.lockScan(s.Storage, mk)
	defer unlock()
	if err := e.purgeTargets(ctx, c); err != nil {
		return c, err
	}
	before, after, err := e.rewriteMetaLocked(ctx, s.Storage, s.Path, c.Target.Instance, nil, nil)
	if err != nil {
		return c, err
	}
	if err := e.seams.at("purge-meta"); err != nil {
		return c, err
	}
	if err := e.purgeOlderMeta(ctx, c, s.Storage, s.Path); err != nil {
		return c, err
	}
	if before == after {
		// Run again after a stop: the manifest was already written without
		// the instance, by this purge before the stop or by a reception
		// that made it from the catalog, and the catalog may still hold the
		// one from before the delete.
		before = tenant.SHA256{}
	}
	return e.finish(ctx, c, after, before)
}

// finish clears what else names a delete's data, ends the purge and
// rewrites its records.
func (e *Engine) finish(ctx context.Context, c tenant.Correction, manifestSum, before tenant.SHA256) (tenant.Correction, error) {
	if err := e.seams.at("purge-objects"); err != nil {
		return c, err
	}
	if c.Kind == record.KindDelete {
		paths, err := e.cfg.Store.ClearWithdrawn(ctx, c.ID)
		if err != nil {
			return c, err
		}
		// The rows that kept them are gone: the bytes are removed now, or
		// by the reception pipeline's start, which removes what no row
		// keeps.
		if len(paths) > 0 && e.cfg.RemoveIncoming != nil {
			if err := e.cfg.RemoveIncoming(ctx, paths); err != nil {
				e.cfg.Log.Warn("a purged delete's held files are removed at the next start", "correction", c.ID,
					"error", fault.Message(err))
			}
		}
	}
	now := e.now()
	f, err := e.cfg.Store.FinishPurge(ctx, tenant.PurgeFinish{ID: c.ID, Now: now, Manifest: manifestSum, ManifestBefore: before,
		Audit: e.serverAudit("PurgeCorrection", c.ID, now)})
	if err != nil {
		return c, err
	}
	if err := e.seams.at("purged"); err != nil {
		return f, err
	}
	e.settle(ctx, f)
	return f, nil
}

// purgeTargets removes each object the correction owns and has not
// settled, in order, and settles it: kept when a live row resolves to it,
// or (a manifest or metadata) when its bytes are no longer the snapshot's;
// else purged through its storage, each version recorded with what the
// storage did. A page of objects is settled in one transaction, so the
// database's cost does not grow with a transaction per object; a stop
// before it leaves its objects unsettled, and the purge run again asks the
// storage of each again, which answers what it knows of a key already
// purged (purgeKey).
func (e *Engine) purgeTargets(ctx context.Context, c tenant.Correction) error {
	for {
		targets, err := e.cfg.Store.PurgeTargets(ctx, c.ID, targetPage)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			return nil
		}
		settled := make([]tenant.PurgeSettled, 0, len(targets))
		var stop error
		for _, t := range targets {
			s, err := e.purgeTarget(ctx, c, t)
			if err != nil {
				stop = err
				break
			}
			settled = append(settled, s)
		}
		if len(settled) > 0 && !errors.Is(stop, errStopHere) {
			if err := e.cfg.Store.SettlePurgeTargets(ctx, c.ID, settled); err != nil {
				return err
			}
		}
		if stop != nil {
			return stop
		}
	}
}

// errStopHere is in the chain of a seam's error that stops a purge before
// it settles what it purged, as a crash would.
var errStopHere = errors.New("the purge stopped before it settled its page")

// purgeTarget purges one object and answers what became of it.
func (e *Engine) purgeTarget(ctx context.Context, c tenant.Correction, t tenant.PurgeTarget) (tenant.PurgeSettled, error) {
	out := tenant.PurgeSettled{Target: t}
	if e.seams.held(ctx, c) {
		return out, fault.New(fault.Precondition, "the purge is held")
	}
	if t.Shared {
		out.Outcome = tenant.OutcomeKeptShared
		return out, nil
	}
	key, err := t.Key()
	if err != nil {
		return out, fault.Wrap(fault.Internal, err, "a correction's object names no key")
	}
	st, err := e.cfg.OpenStorage(ctx, t.Storage)
	if err != nil {
		return out, err
	}
	if t.Ord != tenant.TargetInstance {
		_, sum, err := hashOf(ctx, st, key)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			// Gone already (a purge before a stop removed it): the
			// storage answers what it knows of the key.
		case err != nil:
			return out, err
		case tenant.SHA256(sum) != t.SHA256:
			out.Outcome = tenant.OutcomeKeptChanged
			return out, nil
		}
	}
	removals, err := e.purgeKey(ctx, st, key)
	if err != nil {
		return out, err
	}
	out.Outcome = tenant.OutcomePurged
	now := e.now()
	for _, r := range removals {
		out.Removals = append(out.Removals, tenant.PurgedObject{ID: c.ID, Storage: t.Storage, Key: key, Version: r.Version, Size: t.Size,
			SHA256: t.SHA256, Disposition: tenant.Disposition(r.Disposition), RetainUntil: r.RetainUntil, Now: now})
	}
	if err := e.seams.at("purge-object"); err != nil {
		return out, errors.Join(errStopHere, err)
	}
	return out, nil
}

// purgeKey removes one key through its storage's Purge and answers what
// the storage did with each version. A storage that answers nothing knows
// nothing is there (blob.Store.Purge): the bytes are not on it, which is
// answered removed.
func (e *Engine) purgeKey(ctx context.Context, st blob.Store, key string) ([]blob.Removal, error) {
	removals, err := st.Purge(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(removals) == 0 {
		removals = []blob.Removal{{Disposition: blob.Removed}}
	}
	return removals, nil
}

// record records what a storage did with each version of a key.
func (e *Engine) record(ctx context.Context, id, storage, key string, size int64, sum tenant.SHA256, removals []blob.Removal) error {
	for _, r := range removals {
		if err := e.cfg.Store.RecordPurgedObject(ctx, tenant.PurgedObject{ID: id, Storage: storage, Key: key, Version: r.Version,
			Size: size, SHA256: sum, Disposition: tenant.Disposition(r.Disposition), RetainUntil: r.RetainUntil, Now: e.now()}); err != nil {
			return err
		}
	}
	return nil
}

// purgeOlderMeta removes, on a storage that keeps versions, the older
// versions of a scan's manifest and metadata a correction wrote again in
// place: they still list what it removed (an instance's UID and its
// header).
func (e *Engine) purgeOlderMeta(ctx context.Context, c tenant.Correction, storage string, p label.Path) error {
	st, err := e.cfg.OpenStorage(ctx, storage)
	if err != nil {
		return err
	}
	vp, ok := st.(blob.VersionPurger)
	if !ok {
		return nil
	}
	for _, k := range []func(label.Path) (string, error){layout.MetadataKey, layout.ManifestKey} {
		key, err := k(p)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a scan's key cannot be made")
		}
		removals, err := vp.PurgeOlder(ctx, key)
		if err != nil {
			return err
		}
		if err := e.record(ctx, c.ID, storage, key, 0, tenant.SHA256{}, removals); err != nil {
			return err
		}
	}
	return nil
}

// repurge tries again each version a storage retained for a purged
// correction whose retention has ended, by its version id: the correction
// stays purged, and its record says what the storage did now.
func (e *Engine) repurge(ctx context.Context, now time.Time) error {
	due, err := e.cfg.Store.RetainedDue(ctx, now, maxDue)
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	var errs []error
	for _, o := range due {
		st, err := e.cfg.OpenStorage(ctx, o.Storage)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		vp, ok := st.(blob.VersionPurger)
		if !ok {
			continue
		}
		r, err := vp.PurgeVersion(ctx, o.Key, o.Version)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := e.record(ctx, o.ID, o.Storage, o.Key, o.Size, o.SHA256, []blob.Removal{r}); err != nil {
			errs = append(errs, err)
			continue
		}
		changed[o.ID] = true
	}
	for id := range changed {
		if c, err := e.cfg.Store.Correction(ctx, id); err == nil {
			e.settle(ctx, c)
		}
	}
	return errors.Join(errs...)
}

// seedTargets records, for a correction whose publish fixed no objects (one
// known only from its record, after a restore), the objects each of its
// scans' manifests under the old labels names, with that manifest and the
// metadata, read now; the purge's own checks (a live row, the bytes read)
// then keep what is not its own. A scan a live scan sits at is the
// catalog's, and nothing of it is taken.
func (e *Engine) seedTargets(ctx context.Context, c tenant.Correction, scans []tenant.CorrectionScan) error {
	if c.Kind == record.KindReplace || c.Level == record.LevelInstance || c.Counts.ObjectsOwned > 0 {
		return nil
	}
	for _, s := range scans {
		st, err := e.cfg.OpenStorage(ctx, s.Storage)
		if err != nil {
			return err
		}
		mk, _ := layout.ManifestKey(s.Path)
		raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
		if fault.KindOf(err) == fault.NotFound {
			continue
		}
		if err != nil {
			return err
		}
		m, err := manifest.Decode(bytes.NewReader(raw))
		if err != nil {
			return fault.Wrap(fault.Unavailable, err, "a scan's manifest cannot be read")
		}
		targets := []tenant.PurgeTarget{{Ord: tenant.TargetManifest, Storage: s.Storage, Path: s.Path, Size: int64(len(raw)),
			SHA256: sha256.Sum256(raw)}}
		sk, _ := layout.MetadataKey(s.Path)
		if n, sum, err := hashOf(ctx, st, sk); err == nil {
			targets = append(targets, tenant.PurgeTarget{Ord: tenant.TargetMetadata, Storage: s.Storage, Path: s.Path, Size: n, SHA256: sum})
		} else if fault.KindOf(err) != fault.NotFound {
			return err
		}
		for _, f := range m.Files {
			k, err := layout.Parse(f.Key)
			if err != nil || k.Kind != layout.Instance || k.Path != s.Path {
				continue
			}
			t := tenant.PurgeTarget{Ord: tenant.TargetInstance, Storage: s.Storage, Path: s.Path, UID: k.SOPInstanceUID, Size: f.Size,
				SHA256: shaOf(f.SHA256)}
			if k.Rev() > 1 {
				t.StorageKey = f.Key
			}
			targets = append(targets, t)
		}
		for start := 0; start < len(targets); start += tenant.MaxPurgeTargets {
			if err := e.cfg.Store.AddPurgeTargets(ctx, c.ID, targets[start:min(start+tenant.MaxPurgeTargets, len(targets))]); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneDirs removes, on a storage whose directories outlive their objects,
// the directories of a scan's path a purge or a sweep left empty, and those
// above them left empty: a directory named for a withdrawn subject would
// otherwise still name it. Only empty ones go; a failure is logged, since
// the objects are gone and a directory is never listed as one.
func (e *Engine) pruneDirs(ctx context.Context, st blob.Store, p label.Path) {
	pr, ok := st.(blob.DirPruner)
	if !ok {
		return
	}
	prefix, err := layout.InstancePrefix(p)
	if err != nil {
		return
	}
	dataDir := strings.TrimSuffix(prefix, "/")
	metaDir := layout.MetaRoot + strings.TrimPrefix(dataDir, layout.DataRoot)
	for _, d := range []string{dataDir, metaDir} {
		if err := pr.RemoveEmptyDirs(ctx, d); err != nil {
			e.cfg.Log.Warn("an emptied directory could not be removed", "error", fault.Message(err))
		}
	}
}

// rewriteMeta writes a scan's manifest and metadata again at their keys
// under the scan's lock (rewriteMetaLocked), and answers the new
// manifest's SHA-256.
func (e *Engine) rewriteMeta(ctx context.Context, storage string, p label.Path, drop string, add *sidecar.Instance,
	file *manifest.File) (tenant.SHA256, error) {
	mk, err := layout.ManifestKey(p)
	if err != nil {
		return tenant.SHA256{}, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	unlock := e.lockScan(storage, mk)
	defer unlock()
	_, after, err := e.rewriteMetaLocked(ctx, storage, p, drop, add, file)
	return after, err
}

// rewriteMetaLocked writes a scan's manifest and metadata again at their
// keys: the instance drop left out (or replaced by add and file, a
// replace's new revision), and answers the SHA-256 of the manifest it read
// and of the one it wrote. The metadata is written first: the manifest
// names what the scan holds. The caller holds the scan's lock.
func (e *Engine) rewriteMetaLocked(ctx context.Context, storage string, p label.Path, drop string, add *sidecar.Instance,
	file *manifest.File) (before, after tenant.SHA256, err error) {
	st, err := e.cfg.OpenStorage(ctx, storage)
	if err != nil {
		return before, after, err
	}
	mk, _ := layout.ManifestKey(p)
	sk, _ := layout.MetadataKey(p)
	raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
	if err != nil {
		return before, after, err
	}
	before = sha256.Sum256(raw)
	if err := e.seams.at("meta-read"); err != nil {
		return before, after, err
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		return before, after, fault.Wrap(fault.Unavailable, err, "a scan's manifest cannot be read")
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
		return before, after, err
	default:
		var buf bytes.Buffer
		var added []sidecar.Instance
		if add != nil {
			added = append(added, *add)
		}
		_, merr := sidecar.Merge(&buf, old, sidecar.List(added), func(uid string) bool { return uid == drop })
		_ = old.Close()
		if merr != nil {
			return before, after, fault.Wrap(fault.Unavailable, merr, "a scan's metadata cannot be written again")
		}
		if _, err := st.Put(ctx, sk, &buf); err != nil {
			return before, after, err
		}
	}
	b, err := manifest.Encode(manifest.Manifest{Files: files})
	if err != nil {
		return before, after, fault.Wrap(fault.Internal, err, "a scan's manifest cannot be made")
	}
	if _, err := st.Put(ctx, mk, bytes.NewReader(b)); err != nil {
		return before, after, err
	}
	return before, sha256.Sum256(b), nil
}
