package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
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
	// The records owed to replicas first: a purge waits for its own, and a
	// switch to a replica waits for every one.
	_, owedErr := e.WriteOwedRecords(ctx)
	now := e.now()
	if err := e.cfg.Store.CheckPurgeClock(ctx, now); err != nil {
		if owedErr != nil {
			return 0, errors.Join(err, owedErr)
		}
		return 0, err
	}
	due, err := e.cfg.Store.DueCorrections(ctx, now, maxDue)
	if err != nil {
		return 0, err
	}
	n := 0
	errs := []error{owedErr}
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
	if err := e.reached(c, "purge-started"); err != nil {
		return c, err
	}
	if err := e.waitCopies(ctx, c); err != nil {
		return c, err
	}
	// The copies are taken now: the primary of each scan and each replica
	// attached to it, every one of which holds the correction's record
	// before any object leaves any copy.
	copies, err := e.purgeCopies(ctx, c)
	if err != nil {
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
		return e.purgeInstance(ctx, c, scans[0], copies)
	}
	if err := e.purgeTargets(ctx, c, copies); err != nil {
		return c, err
	}
	if c.Kind == record.KindReplace || c.Kind == record.KindEdit {
		// The older versions of each scan's manifest and metadata name the
		// superseded revisions, and an edit's hold the values it changed.
		for _, s := range scans {
			for _, name := range copies.of(s.Storage) {
				if err := e.purgeOlderMeta(ctx, c, name, s.Path); err != nil {
					return c, e.waitStorage(ctx, c, err)
				}
			}
		}
	} else {
		for _, s := range scans {
			for _, name := range copies.of(s.Storage) {
				e.pruneDirs(ctx, name, s.Path)
			}
		}
	}
	return e.finish(ctx, c, tenant.SHA256{}, tenant.SHA256{})
}

// purgeInstance purges one instance's delete under its scan's lock: its
// object from every copy, then the scan's manifest and metadata written
// again without it on every copy and their older versions removed, then
// the end, which gives the scan the new manifest's checksum while the lock
// still keeps a reception of the scan from writing another. A replica's
// manifest and metadata name the instance and its header as the primary's
// did, so they are written again there too, from the replica's own (a
// replica that holds none of the scan yet has nothing to write); the end
// queues the scan for the replicas, which check them against the new
// manifest.
//
// The write gates of every copy are held before the scan's lock is taken,
// and released after the last write: a reception, the frames pass and a
// replace enter the gate first and then take the scan's lock, so a purge
// that took the lock first and then waited at a gate a switch paused would
// wait on a reception that waits on the purge, until the gate refused it.
func (e *Engine) purgeInstance(ctx context.Context, c tenant.Correction, s tenant.CorrectionScan, copies copySet) (tenant.Correction, error) {
	mk, err := layout.ManifestKey(s.Path)
	if err != nil {
		return c, fault.Wrap(fault.Internal, err, "a manifest's key cannot be made")
	}
	release, err := e.hold(ctx, copies.of(s.Storage))
	if err != nil {
		return c, e.waitStorage(ctx, c, err)
	}
	defer release()
	ctx = holding(ctx, copies.of(s.Storage))
	unlock := e.lockScan(s.Storage, mk)
	defer unlock()
	if err := e.purgeTargets(ctx, c, copies); err != nil {
		return c, err
	}
	before, after, metaSum, err := e.rewriteMetaLocked(ctx, s.Storage, s.Path, c.Target.Instance, nil, nil)
	if err != nil {
		return c, err
	}
	for _, name := range copies.replicas(s.Storage) {
		_, _, _, err := e.rewriteMetaLocked(ctx, name, s.Path, c.Target.Instance, nil, nil)
		if err != nil && fault.KindOf(err) != fault.NotFound {
			return c, e.waitStorage(ctx, c, err)
		}
	}
	if err := e.reached(c, "purge-meta"); err != nil {
		return c, err
	}
	for _, name := range copies.of(s.Storage) {
		if err := e.purgeOlderMeta(ctx, c, name, s.Path); err != nil {
			return c, e.waitStorage(ctx, c, err)
		}
	}
	if before == after {
		// Run again after a stop: the manifest was already written without
		// the instance, by this purge before the stop or by a reception
		// that made it from the catalog, and the catalog may still hold the
		// one from before the delete.
		before = tenant.SHA256{}
	}
	f, err := e.finish(ctx, c, after, before)
	if err != nil {
		return f, err
	}
	e.recordMeta(ctx, s, after, metaSum)
	return f, nil
}

// finish checks every key the purge removed absent on every copy
// (checkGone), clears what else names a delete's data, ends the purge and
// rewrites its records.
func (e *Engine) finish(ctx context.Context, c tenant.Correction, manifestSum, before tenant.SHA256) (tenant.Correction, error) {
	if err := e.checkGone(ctx, c); err != nil {
		return c, err
	}
	if err := e.reached(c, "purge-objects"); err != nil {
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
	if err := e.reached(c, "purged"); err != nil {
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
//
// Each object is removed from every copy (the primary, then each replica
// of it): a copy that does not answer, or a hold seen before the next
// copy, stops the purge there, and what the copies before it did is
// recorded with the object left to settle (OutcomePending), so the purge
// run again asks only the copies left and never claims an object purged
// while one copy may still hold it.
func (e *Engine) purgeTargets(ctx context.Context, c tenant.Correction, copies copySet) error {
	for {
		targets, err := e.cfg.Store.PurgeTargets(ctx, c.ID, targetPage)
		if err != nil {
			if errors.Is(err, tenant.ErrHeld) {
				e.note(ctx, c, tenant.StateReasonHeld)
			}
			return err
		}
		if len(targets) == 0 {
			return nil
		}
		settled := make([]tenant.PurgeSettled, 0, len(targets))
		var stop error
		for _, t := range targets {
			s, err := e.purgeTarget(ctx, c, t, copies)
			if err != nil {
				if s.Outcome == tenant.OutcomePending && len(s.Removals) > 0 {
					settled = append(settled, s)
				}
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
			return e.waitStorage(ctx, c, stop)
		}
	}
}

// errStopHere is in the chain of a seam's error that stops a purge before
// it settles what it purged, as a crash would.
var errStopHere = errors.New("the purge stopped before it settled its page")

// purgeTarget purges one object from each of its copies and answers what
// became of it. Whether it is kept is decided on the primary, which the
// catalog reads: a replica holds the primary's keys, and follows. A copy
// whose removal is recorded already (t.Done) is not asked again. A hold is
// asked before each copy; a hold seen, or a copy that does not answer,
// after another copy removed the object answers OutcomePending with what
// was removed, and the error.
func (e *Engine) purgeTarget(ctx context.Context, c tenant.Correction, t tenant.PurgeTarget, copies copySet) (tenant.PurgeSettled, error) {
	out := tenant.PurgeSettled{Target: t}
	if e.seams.held(ctx, c) {
		e.note(ctx, c, tenant.StateReasonHeld)
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
	if t.Ord != tenant.TargetInstance && !slices.Contains(t.Done, t.Storage) {
		st, err := e.cfg.OpenStorage(ctx, t.Storage)
		if err != nil {
			return out, err
		}
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
	now := e.now()
	for i, name := range copies.of(t.Storage) {
		if slices.Contains(t.Done, name) {
			continue
		}
		if i > 0 && e.seams.held(ctx, c) {
			e.note(ctx, c, tenant.StateReasonHeld)
			return pending(out), fault.New(fault.Precondition, "the purge is held")
		}
		var removals []blob.Removal
		err := e.gated(ctx, name, func(st blob.Store) error {
			var err error
			removals, err = e.purgeKey(ctx, st, key)
			return err
		})
		if err != nil {
			return pending(out), err
		}
		for _, r := range removals {
			out.Removals = append(out.Removals, tenant.PurgedObject{ID: c.ID, Storage: name, Key: key, Version: r.Version, Size: t.Size,
				SHA256: t.SHA256, Disposition: tenant.Disposition(r.Disposition), RetainUntil: r.RetainUntil, Now: now})
		}
	}
	out.Outcome = tenant.OutcomePurged
	if len(out.Removals) == 0 {
		// Every copy's removal was recorded before a stop: the object is
		// settled now, its removals already counted.
		return out, nil
	}
	if err := e.reached(c, "purge-object"); err != nil {
		return out, errors.Join(errStopHere, err)
	}
	return out, nil
}

// pending answers a target some of whose copies removed it, to settle
// later: OutcomePending, with the removals they answered.
func pending(s tenant.PurgeSettled) tenant.PurgeSettled {
	s.Outcome = tenant.OutcomePending
	return s
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
	for _, k := range []func(label.Path) (string, error){layout.MetadataKey, layout.ManifestKey} {
		key, err := k(p)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a scan's key cannot be made")
		}
		var removals []blob.Removal
		err = e.gated(ctx, storage, func(st blob.Store) error {
			vp, ok := st.(blob.VersionPurger)
			if !ok {
				return nil
			}
			var err error
			removals, err = vp.PurgeOlder(ctx, key)
			return err
		})
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
		var r blob.Removal
		purged := false
		err := e.gated(ctx, o.Storage, func(st blob.Store) error {
			vp, ok := st.(blob.VersionPurger)
			if !ok {
				return nil
			}
			var err error
			r, err = vp.PurgeVersion(ctx, o.Key, o.Version)
			purged = err == nil
			return err
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !purged {
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
	// A replace's and an edit's scans stay in the catalog whole: only the
	// revisions their publish fixed are theirs.
	if c.Kind == record.KindReplace || c.Kind == record.KindEdit || c.Level == record.LevelInstance || c.Counts.ObjectsOwned > 0 {
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

// pruneDirs removes, on the storage named, inside its write gate (gated),
// the directories of a scan's path a purge left empty (pruneOn). A storage
// that cannot be opened or admitted keeps them: a directory is never
// listed as an object, and the next purge or sweep of the path tries again.
func (e *Engine) pruneDirs(ctx context.Context, storage string, p label.Path) {
	_ = e.gated(ctx, storage, func(st blob.Store) error {
		e.pruneOn(ctx, st, p)
		return nil
	})
}

// pruneOn removes, on a storage whose directories outlive their objects,
// the directories of a scan's path a purge or a sweep left empty, and those
// above them left empty: a directory named for a withdrawn subject would
// otherwise still name it. Only empty ones go; a failure is logged, since
// the objects are gone and a directory is never listed as one. The caller
// holds the storage's write gate.
func (e *Engine) pruneOn(ctx context.Context, st blob.Store, p label.Path) {
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
