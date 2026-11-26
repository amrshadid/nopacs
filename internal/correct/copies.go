package correct

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a correction does on the replicas of its scans' storages
// (tenant.Corrections, "Every copy"): its record put on each replica, or
// owed to one that does not take it and written later; the copies its
// purge removes each object from; and why a purge waits.

// copySet is the copies a purge reaches: by primary, its replicas.
type copySet map[string][]string

// of returns the copies of the primary named, the primary first, then its
// replicas by name.
func (s copySet) of(primary string) []string {
	return append([]string{primary}, s[primary]...)
}

// replicas returns the replicas of the primary named, by name.
func (s copySet) replicas(primary string) []string { return s[primary] }

// writeReplicaRecords puts c's record on each replica of the storages of
// its scans (groups, by storage), as on the primary, and records it
// written; a replica that does not take it is owed it (OweRecord), and is
// written again later (WriteOwedRecords). A replica down never stops a
// correction: only its purge, which waits for the record (purgeCopies).
func (e *Engine) writeReplicaRecords(ctx context.Context, c tenant.Correction, groups map[string][]tenant.CorrectionScan) error {
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, cp := range copies {
		if err := e.putRecord(ctx, c, groups[cp.Primary], cp.Replica); err != nil {
			e.cfg.Log.Warn("a replica did not take a correction's record; it is owed and written again later", "correction", c.ID,
				"replica", cp.Replica, "error", fault.Message(err))
			if err := e.cfg.Store.OweRecord(ctx, c.ID, cp.Replica, e.now()); err != nil {
				return err
			}
			continue
		}
		if err := e.cfg.Store.MarkRecordWritten(ctx, c.ID, cp.Replica, c.State); err != nil {
			return err
		}
	}
	return nil
}

// WriteOwedRecords writes each correction record owed to a replica, as the
// correction's row says now, and records each written; it answers how many
// it wrote. A replica that still does not answer stays owed, and its error
// is answered with the others' once every record was tried. PurgeDue runs
// it first, so a record owed is retried as often as a purge.
func (e *Engine) WriteOwedRecords(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	after := tenant.RecordOwed{}
	for {
		owed, err := e.cfg.Store.RecordsOwed(ctx, after, tenant.MaxRecordsOwed)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, o := range owed {
			wrote, err := e.writeOwed(ctx, o)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if wrote {
				n++
			}
		}
		if len(owed) < tenant.MaxRecordsOwed {
			return n, errors.Join(errs...)
		}
		after = owed[len(owed)-1]
	}
}

// writeOwed writes one record owed to a replica and records it written,
// answering whether it wrote it.
func (e *Engine) writeOwed(ctx context.Context, o tenant.RecordOwed) (bool, error) {
	c, err := e.cfg.Store.Correction(ctx, o.Correction)
	if err != nil {
		return false, err
	}
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return false, err
	}
	for _, cp := range copies {
		if cp.Replica != o.Replica {
			continue
		}
		scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
		if err != nil {
			return false, err
		}
		_, groups := byStorage(scans)
		if err := e.putRecord(ctx, c, groups[cp.Primary], cp.Replica); err != nil {
			return false, err
		}
		return true, e.cfg.Store.MarkRecordWritten(ctx, c.ID, cp.Replica, c.State)
	}
	// No longer a replica of the correction's storages: nothing is written
	// to a storage that is not a copy, so what it is owed is dropped, or
	// it would stay owed for ever (the store keeps it when the storage
	// became a copy again since the copies were read).
	_, err = e.cfg.Store.DropRecordOwed(ctx, c.ID, o.Replica)
	return false, err
}

// purgeCopies answers the copies a purge of c reaches, once each replica
// among them holds c's record: a record owed is written again first, and a
// replica that still does not take it keeps the purge from every copy,
// with the state reason that a storage does not answer. A rebuild or a
// switch reading that replica alone would otherwise find objects gone
// with no correction to say why.
func (e *Engine) purgeCopies(ctx context.Context, c tenant.Correction) (copySet, error) {
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := copySet{}
	for _, cp := range copies {
		out[cp.Primary] = append(out[cp.Primary], cp.Replica)
	}
	cur, err := e.cfg.Store.Correction(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	for _, replica := range cur.RecordsOwed {
		if _, err := e.writeOwed(ctx, tenant.RecordOwed{Correction: c.ID, Replica: replica}); err != nil {
			e.note(ctx, c, tenant.StateReasonStorage)
			return nil, fault.Wrap(fault.Unavailable, err, "the replica "+replica+" is owed the correction's record; the purge waits for it")
		}
	}
	return out, nil
}

// checkGone is a purge's last pass before its correction is purged: each
// key it settled purged is asked of every copy attached now (the primary
// and its replicas, read again, not the copies the purge began with), and
// one found there again is removed and recorded. A copy put while the
// purge ran (a replicator's copy claimed before it began, landing after
// the replica's removal) would otherwise keep what the correction says is
// gone. A copy that does not answer, or a hold, stops the pass, and the
// purge is run again.
func (e *Engine) checkGone(ctx context.Context, c tenant.Correction) error {
	copies, err := e.cfg.Store.CorrectionCopies(ctx, c.ID)
	if err != nil {
		return err
	}
	set := copySet{}
	for _, cp := range copies {
		set[cp.Primary] = append(set[cp.Primary], cp.Replica)
	}
	after := tenant.PurgeTarget{}
	for {
		targets, err := e.cfg.Store.PurgedTargets(ctx, c.ID, after, targetPage)
		if err != nil {
			return err
		}
		for _, t := range targets {
			key, err := t.Key()
			if err != nil {
				return fault.Wrap(fault.Internal, err, "a correction's object names no key")
			}
			for _, name := range set.of(t.Storage) {
				st, err := e.cfg.OpenStorage(ctx, name)
				if err != nil {
					return e.waitStorage(ctx, c, err)
				}
				_, err = st.Stat(ctx, key)
				if fault.KindOf(err) == fault.NotFound {
					continue
				}
				if err != nil {
					return e.waitStorage(ctx, c, err)
				}
				if e.seams.held(ctx, c) {
					e.note(ctx, c, tenant.StateReasonHeld)
					return fault.New(fault.Precondition, "the purge is held")
				}
				// The removal enters the storage's gate and reopens the
				// storage inside it (gated): the store the presence was
				// read from may name the place a switch swapped away meanwhile.
				var removals []blob.Removal
				err = e.gated(ctx, name, func(st blob.Store) error {
					var err error
					removals, err = e.purgeKey(ctx, st, key)
					return err
				})
				if err != nil {
					return e.waitStorage(ctx, c, err)
				}
				if err := e.record(ctx, c.ID, name, key, t.Size, t.SHA256, removals); err != nil {
					return err
				}
				e.cfg.Log.Warn("a purged object was found again on a copy and removed", "correction", c.ID, "storage", name)
			}
		}
		if len(targets) < targetPage {
			return nil
		}
		after = targets[len(targets)-1]
	}
}

// waitStorage records, for a purge stopped by a storage that does not
// answer, that it waits for one, and answers err as it is.
func (e *Engine) waitStorage(ctx context.Context, c tenant.Correction, err error) error {
	if fault.KindOf(err) == fault.Unavailable {
		e.note(ctx, c, tenant.StateReasonStorage)
	}
	return err
}

// note sets the state reason a purge waits with; a failure is logged, since
// the reason only tells a person, and the purge is run again all the same.
func (e *Engine) note(ctx context.Context, c tenant.Correction, why string) {
	if err := e.cfg.Store.NotePurge(ctx, c.ID, why); err != nil {
		e.cfg.Log.Warn("a purge's state reason could not be recorded", "correction", c.ID, "error", fault.Message(err))
	}
}
