package correct

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pendingPage is how many corrections Recover reads at once.
const pendingPage = 100

// Recover takes up, at start, what a stop left of every correction: a
// delete or a replace left prepared is abandoned (its request was never
// answered, so it did not happen), what a replace wrote written back; a
// relabel left prepared goes on with its copy and its publish; an
// abandoned relabel whose copies were not swept (a stop between its
// abandon and the sweep, or a copy that wrote after the stop's sweep) has
// them removed; an edit's undo left part way is finished, and an undone
// edit's new revisions not yet removed are; a purge left purging goes on;
// and records that lag their row are rewritten. It goes on past one correction's failure and answers
// them all.
func (e *Engine) Recover(ctx context.Context) error {
	var errs []error
	after := ""
	for {
		page, err := e.cfg.Store.PendingCorrections(ctx, after, pendingPage)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, c := range page {
			after = c.ID
			if err := e.recoverOne(ctx, c); err != nil {
				errs = append(errs, err)
			}
		}
		if len(page) < pendingPage {
			return errors.Join(errs...)
		}
	}
}

// recoverOne takes up one correction.
func (e *Engine) recoverOne(ctx context.Context, c tenant.Correction) error {
	switch {
	case c.State == record.StatePrepared && c.Kind == record.KindRelabel:
		_, err := e.ContinueRelabel(ctx, c.ID)
		return err
	case c.State == record.StatePrepared:
		if err := e.undoWrites(ctx, c); err != nil {
			return err
		}
		a, err := e.cfg.Store.AbandonCorrection(ctx, c.ID, reasonStopped, e.now())
		if err != nil {
			return err
		}
		return e.writeRecords(ctx, a)
	case c.State == record.StatePurging:
		_, err := e.purge(ctx, c)
		return err
	case c.Kind == record.KindRelabel && c.State == record.StateAbandoned && !c.Swept:
		if err := e.sweep(ctx, c); err != nil {
			return err
		}
	case c.Kind == record.KindEdit && c.State == record.StateCommitted && c.StateReason == tenant.StateReasonUndoing:
		// The undo a person asked for, stopped part way: it is finished, or,
		// when it may no longer be published (a hold placed since), written
		// back and abandoned, which is not a failure of the start.
		_, err := e.continueUndo(ctx, c)
		if fault.KindOf(err) == fault.Precondition {
			e.cfg.Log.Warn("an edit's undo stopped part way may not be published now; it was written back and abandoned", "correction", c.ID,
				"reason", fault.Message(err))
			return nil
		}
		return err
	case c.Kind == record.KindEdit && c.State == record.StateUndone && !c.Swept:
		if err := e.sweepEdit(ctx, c); err != nil {
			return err
		}
	}
	if c.RecordsState == c.State {
		return nil
	}
	return e.writeRecords(ctx, c)
}
