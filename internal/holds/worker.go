package holds

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxOwedPasses bounds the pages of owed records one WriteOwed reads.
const maxOwedPasses = 100

// StaleRelease is how long a release may stay under way before the worker
// ends it as one whose writer stopped (tenant.Holds.AbortStaleHoldReleases):
// far longer than a release's writes take, so a writer still at work is not
// cut short, and short enough that the hold's active records are written
// again soon after its writer stopped.
const StaleRelease = 10 * time.Minute

// Run is the tenant's hold-record worker, until ctx ends: it adopts the
// holds found active on the tenant's storages with no row (AdoptFound),
// tried again each interval until it has read every storage, and writes
// the records owed (WriteOwed) every interval.
func (w *Writer) Run(ctx context.Context) {
	adopted := false
	for {
		if !adopted {
			a, err := w.AdoptFound(ctx)
			adopted = err == nil
			if err != nil && ctx.Err() == nil {
				w.cfg.Log.Warn("holds: the hold records of the tenant's storages could not all be read; read again shortly", "tenant",
					w.cfg.Tenant, "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
			}
			w.report(a)
		}
		if _, err := w.WriteOwed(ctx); err != nil && ctx.Err() == nil {
			w.cfg.Log.Warn("holds: hold records owed could not all be written; written again shortly", "tenant", w.cfg.Tenant,
				"kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.after():
		}
	}
}

// after returns the wait before the next pass.
func (w *Writer) after() <-chan time.Time { return time.After(w.cfg.Interval) }

// report logs what an adoption did: each hold made from its record, and
// each record refused, by its storage and key.
func (w *Writer) report(a Adoption) {
	for _, id := range a.Adopted {
		w.cfg.Log.Warn("holds: a hold record was found active with no row; the hold holds again", "tenant", w.cfg.Tenant, "hold", id)
	}
	for _, k := range a.Refused {
		w.cfg.Log.Error("holds: a hold record does not verify under the installation key, or is not in its form; it makes no hold",
			"tenant", w.cfg.Tenant, "record", k)
	}
}

// AdoptFound reads the hold records of every storage of the tenant and
// adopts each hold found active with no row (Adopt). A storage that cannot
// be read is the error, after the holds of those read are adopted.
func (w *Writer) AdoptFound(ctx context.Context) (Adoption, error) {
	var names []string
	cursor := ""
	for {
		page, next, err := w.cfg.Store.ListStorages(ctx, master.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return Adoption{}, err
		}
		for _, s := range page {
			names = append(names, s.Name)
		}
		if cursor = next; cursor == "" {
			break
		}
	}
	var found []Found
	var errs []error
	for _, n := range names {
		f, err := w.readStorage(ctx, n)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		found = append(found, f)
	}
	now := w.now()
	a, err := Adopt(ctx, w.cfg.Store, found, now, func(id, storage string) master.AuditEntry { return w.cfg.Audit(id, storage, now) })
	return a, errors.Join(append(errs, err)...)
}

// readStorage reads the hold records of the storage named.
func (w *Writer) readStorage(ctx context.Context, name string) (Found, error) {
	st, err := w.cfg.Open(ctx, name)
	if err != nil {
		return Found{}, err
	}
	defer func() { _ = st.Close() }()
	return ReadRecords(ctx, st, name, w.cfg.TenantID, w.cfg.Keys)
}

// WriteOwed ends the releases left under way past StaleRelease, owes again
// the records of active holds sealed under another version of the
// installation key than the one it seals with now, then writes each record
// a storage is owed, the hold as it stands, and marks it written at the
// hold's count; it answers how many it wrote. The records owed are read a
// page at a time after a cursor, and a storage that does not take one is
// left out of the rest of the pass and is the error, so a storage that
// does not answer holds back none of the records the others are owed.
func (w *Writer) WriteOwed(ctx context.Context) (int, error) {
	var errs []error
	ids, err := w.cfg.Store.AbortStaleHoldReleases(ctx, w.now().Add(-StaleRelease))
	if err != nil {
		errs = append(errs, err)
	}
	for _, id := range ids {
		w.cfg.Log.Warn("holds: a release left under way did not commit; the hold is active again and its records are written again", "tenant",
			w.cfg.Tenant, "hold", id)
	}
	if k, err := w.cfg.Store.OweHoldRecordsNotSealedUnder(ctx, w.cfg.Keys.SealVersion()); err != nil {
		errs = append(errs, err)
	} else if k > 0 {
		w.cfg.Log.Info("holds: hold records sealed under another version of the installation key are written again", "tenant",
			w.cfg.Tenant, "records", k)
	}
	n := 0
	var failed []string
	page := tenant.HoldOwedPage{Limit: tenant.MaxHoldList}
	for range maxOwedPasses {
		page.Skip = failed[:min(len(failed), tenant.MaxHoldSkip)]
		owed, err := w.cfg.Store.HoldRecordsOwed(ctx, page)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, o := range owed {
			page.AfterHold, page.AfterStorage = o.Hold.ID, o.Storage
			if slices.Contains(failed, o.Storage) {
				continue
			}
			wrote, refused, err := w.writeOwed(ctx, o)
			switch {
			case refused:
				failed = append(failed, o.Storage)
				errs = append(errs, err)
			case err != nil:
				errs = append(errs, err)
			case wrote:
				n++
			}
		}
		if len(owed) < page.Limit || ctx.Err() != nil {
			break
		}
	}
	return n, errors.Join(errs...)
}

// writeOwed writes one record owed, under the hold's lock, from the hold
// as it stands then: a release begun since the page was read writes its
// own records, and a storage written since is owed nothing. It answers
// whether it wrote, and whether the storage refused the record, with the
// error.
func (w *Writer) writeOwed(ctx context.Context, o tenant.HoldRecordOwed) (bool, bool, error) {
	unlock := recordLocks.lock(w.cfg.TenantID, o.Hold.ID)
	defer unlock()
	h, err := w.cfg.Store.Hold(ctx, o.Hold.ID)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return false, false, nil
	case err != nil:
		return false, false, err
	case h.Releasing || !slices.Contains(h.StoragesPending, o.Storage):
		return false, false, nil
	}
	if err := w.put(ctx, o.Storage, recordOf(h)); err != nil {
		return false, true, err
	}
	err = w.cfg.Store.MarkHoldRecord(ctx, h.ID, o.Storage, h.Seq, w.cfg.Keys.SealVersion())
	if err != nil && fault.KindOf(err) != fault.NotFound {
		return false, false, err
	}
	return true, false, nil
}
