package correct

import (
	"context"
	"crypto/hmac"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// DeleteRequest is a delete a person asked for, after its preview.
type DeleteRequest struct {
	// Target is what to delete; Reason why; Confirm the preview's token.
	Target          tenant.CorrectionTarget
	Reason, Confirm string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// StoragePurge is what a storage is expected to do with an object a purge
// removes, as it says before any purge (blob.PurgeExpecter).
type StoragePurge struct {
	// Storage is its name.
	Storage string
	// Expected is the disposition expected; Why a short phrase why.
	Expected blob.Disposition
	Why      string
	// ReplicaOf names, for a replica, its primary; empty for a primary.
	ReplicaOf string
}

// Preview is what a delete would remove, the token it must carry, when its
// bytes would be purged if it were made now, and what each storage holding
// any of it is expected to do with them.
type Preview struct {
	tenant.DeletePreview
	// PurgeAfter is now plus the tenant's delete.grace.
	PurgeAfter time.Time
	// Purges are the storages' expectations, in order of storage.
	Purges []StoragePurge
}

// Preview answers what a delete of target would remove, as the catalog
// holds it now. A storage that cannot say what it will do is expected
// unknown.
func (e *Engine) Preview(ctx context.Context, target tenant.CorrectionTarget) (Preview, error) {
	now := e.now()
	p, err := e.cfg.Store.PreviewDelete(ctx, target, now)
	if err != nil {
		return Preview{}, err
	}
	g, err := e.grace(ctx)
	if err != nil {
		return Preview{}, err
	}
	if e.cfg.ConfirmKey != nil {
		mac, err := e.cfg.ConfirmKey.MAC([]byte(p.Confirm))
		if err != nil {
			return Preview{}, err
		}
		p.Confirm = tenant.KeyedConfirm(p.Confirm, mac)
	}
	out := Preview{DeletePreview: p, PurgeAfter: now.Add(g)}
	purges := make([]StoragePurge, 0, len(p.Storages)+len(p.Replicas))
	for _, name := range p.Storages {
		purges = append(purges, StoragePurge{Storage: name})
	}
	for _, r := range p.Replicas {
		purges = append(purges, StoragePurge{Storage: r.Replica, ReplicaOf: r.Primary})
	}
	// Ordered by storage name, primaries and replicas alike, as the
	// contract lists them.
	slices.SortFunc(purges, func(a, b StoragePurge) int { return strings.Compare(a.Storage, b.Storage) })
	for _, sp := range purges {
		sp.Expected, sp.Why = blob.Unknown, "the storage does not say"
		st, err := e.cfg.OpenStorage(ctx, sp.Storage)
		if err != nil {
			sp.Why = reasonStorage
		} else if x, ok := st.(blob.PurgeExpecter); ok {
			sp.Expected, sp.Why = x.PurgeExpected(ctx)
		}
		out.Purges = append(out.Purges, sp)
	}
	return out, nil
}

// Delete records, prepares and publishes a delete: its row, then its
// records on every storage its target touches, then the publishing
// transaction, then its records rewritten committed. A request whose token
// made a delete already answers that delete. A failure before the publish
// abandons the delete and is answered. From its prepare on it runs apart
// from ctx's end (detached), so a client that hangs up never leaves it
// prepared.
func (e *Engine) Delete(ctx context.Context, r DeleteRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	confirm, err := e.unkeyConfirm(ctx, r.Target, r.Confirm)
	if err != nil {
		return tenant.Correction{}, err
	}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: r.Target,
		Reason: r.Reason, Confirm: confirm, KeyID: r.KeyID, Now: now}, r.Audit, r.Guard)
	if err == nil && e.cfg.ConfirmKey != nil {
		e.remember(r.Confirm, confirm, r.Target)
	}
	if err != nil || !made {
		return c, err
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	if err := e.reached(c, "records"); err != nil {
		return c, err
	}
	var snapshots map[string]tenant.ScanSnapshot
	if c.Level != record.LevelInstance {
		if snapshots, err = e.snapshots(ctx, c.ID); err != nil {
			return e.abandon(ctx, c, reasonStorage, err)
		}
	}
	return e.publish(ctx, c, nil, snapshots)
}

// unkeyConfirm checks a token Preview made with the confirm key and
// returns the store's own token for it, made again at its issue time; the
// store then checks that token against what the delete takes now, in the
// delete's own transaction. Without a confirm key the token is the store's.
func (e *Engine) unkeyConfirm(ctx context.Context, target tenant.CorrectionTarget, token string) (string, error) {
	if e.cfg.ConfirmKey == nil {
		return token, nil
	}
	issued, mac, ok := tenant.SplitKeyedConfirm(token)
	if !ok {
		return "", fault.New(fault.Precondition, "the confirm token is not one this server's preview gave")
	}
	if c1, was, ok := e.used(token); ok {
		// A token that made a delete in this process, sent again: the
		// delete hid its target, so no preview is made again; the store
		// answers the delete the store's token made. Only with the target
		// it was made for: the store finds that delete by its token alone,
		// so a request naming another target would be answered with it.
		if was != target {
			return "", errTokenMismatch
		}
		return c1, nil
	}
	p, err := e.cfg.Store.PreviewDelete(ctx, target, issued)
	if err != nil {
		return "", err
	}
	want, err := e.cfg.ConfirmKey.MAC([]byte(p.Confirm))
	if err != nil {
		return "", err
	}
	if !hmac.Equal(mac, want) {
		return "", errTokenMismatch
	}
	return p.Confirm, nil
}

// errTokenMismatch answers a keyed token that is not one a preview of the
// request's target gave, as the contract answers a token that does not
// match what the delete would remove.
var errTokenMismatch = fault.New(fault.Precondition, "the confirm token is not one this server's preview gave, or the target changed since")

// maxUsedTokens bounds the tokens an engine remembers (remember): an hour
// of deletes at more than one a few seconds.
const maxUsedTokens = 1024

// remember keeps the store's token of a keyed token that made, or was
// answered, a delete, and the target it was for, so the same request sent
// again is answered without a preview, which its hidden target no longer
// gives. Only this process remembers, and at most maxUsedTokens: after a
// restart, or once forgotten, a repeat is answered not found, as a delete
// of a target the tenant no longer shows.
func (e *Engine) remember(keyed, c1 string, target tenant.CorrectionTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.usedTokens) >= maxUsedTokens {
		clear(e.usedTokens)
	}
	e.usedTokens[keyed] = usedToken{c1: c1, target: target}
}

// used returns the store's token and the target remember kept for a keyed
// token.
func (e *Engine) used(keyed string) (string, tenant.CorrectionTarget, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.usedTokens[keyed]
	return u.c1, u.target, ok
}

// usedToken is what a keyed token that made a delete stood for: the store's
// token, and the target, so the same token sent with another target is
// refused rather than answered with the first delete.
type usedToken struct {
	c1     string
	target tenant.CorrectionTarget
}

// snapshots reads each scan of a correction's manifest and metadata under
// the labels it has now: what its purge may remove, and only while their
// bytes are these.
func (e *Engine) snapshots(ctx context.Context, id string) (map[string]tenant.ScanSnapshot, error) {
	scans, err := e.cfg.Store.CorrectionScans(ctx, id)
	if err != nil {
		return nil, err
	}
	out := map[string]tenant.ScanSnapshot{}
	for _, s := range scans {
		st, err := e.cfg.OpenStorage(ctx, s.Storage)
		if err != nil {
			return nil, err
		}
		snap, err := tenant.ReadScanSnapshot(ctx, st, s.Path)
		if err != nil {
			return nil, err
		}
		out[s.SeriesUID] = snap
	}
	return out, nil
}

// publish runs the publishing transaction of a prepared correction and
// rewrites its records.
func (e *Engine) publish(ctx context.Context, c tenant.Correction, manifests map[string]tenant.SHA256,
	snapshots map[string]tenant.ScanSnapshot) (tenant.Correction, error) {
	return e.publishWith(ctx, c, manifests, snapshots, nil)
}

// publishWith is publish with, for a relabel, the manifests it wrote on
// replicas in step (tenant.CorrectionCommit.ReplicaManifests).
func (e *Engine) publishWith(ctx context.Context, c tenant.Correction, manifests map[string]tenant.SHA256,
	snapshots map[string]tenant.ScanSnapshot, replicas map[string]map[string]tenant.SHA256) (tenant.Correction, error) {
	g, err := e.grace(ctx)
	if err != nil {
		return e.abandon(ctx, c, reasonStopped, err)
	}
	pc, err := e.cfg.Store.CommitCorrection(ctx, tenant.CorrectionCommit{ID: c.ID, Now: e.now(), Grace: g, Manifests: manifests,
		Snapshots: snapshots, ReplicaManifests: replicas})
	if err != nil {
		why := reasonPublish
		if errors.Is(err, tenant.ErrTargetChanged) {
			why = reasonChanged
		}
		return e.abandon(ctx, c, why, err)
	}
	if err := e.reached(c, "committed"); err != nil {
		return pc, err
	}
	e.settle(ctx, pc)
	return pc, nil
}

// abandon ends a prepared correction whose request failed before its
// publish: what it wrote is undone, its row abandoned with why, its records
// rewritten so; cause is answered. A correction published meanwhile (the
// commit's answer lost) is left as it is. A relabel an admin stopped
// meanwhile (its publish refused as not prepared) has its copies removed
// here, since its copy may have written after the stop's own removal. It
// runs apart from ctx's end (detached): it is called once a step failed,
// often because ctx ended, and an abandon cut short leaves the correction
// prepared.
func (e *Engine) abandon(ctx context.Context, c tenant.Correction, why string, cause error) (tenant.Correction, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	cur, err := e.cfg.Store.Correction(ctx, c.ID)
	if err == nil && cur.Kind == record.KindRelabel && cur.State == record.StateAbandoned && !cur.Swept {
		if err := e.sweep(ctx, cur); err != nil {
			e.cfg.Log.Error("a stopped relabel's copies could not be removed; they are removed at the next start", "correction", c.ID,
				"error", fault.Message(err))
		}
		return cur, cause
	}
	if err != nil || cur.State != record.StatePrepared {
		return cur, cause
	}
	if err := e.undoWrites(ctx, cur); err != nil {
		e.cfg.Log.Error("what a correction wrote could not be undone; it is undone at the next start", "correction", c.ID,
			"error", err.Error())
		return cur, cause
	}
	a, err := e.cfg.Store.AbandonCorrection(ctx, c.ID, why, e.now())
	if err != nil {
		return cur, cause
	}
	if a.Kind == record.KindRelabel {
		if err := e.cfg.Store.MarkSwept(ctx, a.ID); err != nil {
			return a, cause
		}
	}
	e.settle(ctx, a)
	return a, cause
}

// sweep removes an abandoned relabel's copies and records them swept.
func (e *Engine) sweep(ctx context.Context, c tenant.Correction) error {
	if err := e.removeCopies(ctx, c); err != nil {
		return err
	}
	return e.cfg.Store.MarkSwept(ctx, c.ID)
}

// undoWrites removes or writes back what a prepared correction wrote on
// its storages, before it is abandoned: a relabel's copies, a replace's
// new revision and its manifest and metadata. A delete writes nothing but
// its records.
func (e *Engine) undoWrites(ctx context.Context, c tenant.Correction) error {
	switch c.Kind {
	case record.KindRelabel:
		return e.removeCopies(ctx, c)
	case record.KindReplace:
		return e.rollbackReplace(ctx, c)
	}
	return nil
}

// Undo undoes a committed delete in its grace period (its rows shown
// again, its records rewritten undone), or stops a prepared relabel (its
// copies removed, abandoned).
func (e *Engine) Undo(ctx context.Context, id, reason string, audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	c, err := e.cfg.Store.UndoCorrection(ctx, id, reason, e.now(), audit, guard)
	if err != nil {
		return c, err
	}
	if err := e.reached(c, "undone"); err != nil {
		return c, err
	}
	// A relabel whose copy runs here sees the stop before its next write
	// and removes its copies itself, after its last write; one whose copy
	// does not run here has them removed now.
	if c.Kind == record.KindRelabel && c.State == record.StateAbandoned && !c.Swept && !e.isCopying(c.ID) {
		if err := e.sweep(ctx, c); err != nil {
			return c, err
		}
	}
	if c.RecordsState != c.State {
		e.settle(ctx, c)
	}
	return c, nil
}
