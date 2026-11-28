package correct

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// stepsBound bounds a correction's steps from its prepare on, which run
// apart from the request that asked (detached): a record on each storage
// the target touches, each scan's manifest and metadata read or written
// once, a replace's new object written and read back. Five minutes is far
// past what they take on a slow storage, and bounds how long a storage
// that hangs keeps the tenant's requests behind the next Recover, and the
// server's stop, waiting.
const stepsBound = 5 * time.Minute

// detached returns a context that keeps ctx's values but not its end, ended
// after stepsBound instead. A correction prepared is taken to its publish or
// its abandon whatever becomes of its request: a client that hangs up
// between the two would otherwise leave it prepared, holding its target,
// with nothing to take it up but the next start.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stepsBound)
}

// reasonLeft is the state reason of a correction a request left prepared.
const reasonLeft = "its request ended before it was published"

// abandonLeft abandons every delete and replace the store holds prepared,
// as a start's Recover does, what a replace wrote written back. Its caller
// makes sure no request runs meanwhile, so each was left by a request that
// ended without its publish or its abandon. A relabel left prepared is
// Recover's, whose copy may take long. It goes on past one correction's
// failure and answers how many it abandoned, and the failures.
func (e *Engine) abandonLeft(ctx context.Context) (int, error) {
	var errs []error
	n, after := 0, ""
	for {
		page, err := e.cfg.Store.PendingCorrections(ctx, after, pendingPage)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, c := range page {
			after = c.ID
			if c.State != record.StatePrepared || c.Kind.CopiesObjects() {
				continue
			}
			if err := e.undoWrites(ctx, c); err != nil {
				errs = append(errs, err)
				continue
			}
			a, err := e.cfg.Store.AbandonCorrection(ctx, c.ID, reasonLeft, e.now())
			if err != nil {
				errs = append(errs, err)
				continue
			}
			n++
			if err := e.writeRecords(ctx, a); err != nil {
				errs = append(errs, err)
			}
		}
		if len(page) < pendingPage {
			return n, errors.Join(errs...)
		}
	}
}

// errLeftForRebuild is a correction a rebuild's settle could not bring to
// a state the build reads right.
var errLeftForRebuild = fault.New(fault.Unavailable, "a correction a stop or a failure left could not be settled before the rebuild; it is tried again")

// settleForRebuild brings every correction to a state a rebuild's build
// reads right from the storages, before it reads them: a delete or a
// replace left prepared (by a stop, a request whose abandon failed, or one
// left before the first Recover) is abandoned with what it wrote undone, as
// Recover does, since its request was never answered; and every record
// that lags its row (a rewrite a stop or a failure missed, such as an undo
// whose record still says committed) is rewritten. A relabel left prepared
// and a purge left purging stay as they are, held until the rebuild ends:
// their records say what they are, and the build reads them so. Its caller
// holds the tenant's gate and work lock, so nothing else writes meanwhile.
//
// It fails, so the drain fails and the attempt is tried again, while a
// delete or a replace is still prepared after it: a replace's writes not
// undone would be read as they are, whatever a record says. A record that
// cannot be rewritten (its storage cannot be opened or written) is logged
// and left: the server's rebuilds read each record as its row says
// (rebuild.ByRow), so a lagging record cannot change what the build
// catalogs, and a storage that does not answer must not hold every replace
// of the tenant; the next start rewrites it (PendingCorrections).
func (e *Engine) settleForRebuild(ctx context.Context) error {
	var errs []error
	rewrite := func(c tenant.Correction) {
		if err := e.writeRecords(ctx, c); err != nil {
			e.cfg.Log.Warn("a correction record that lags its row could not be rewritten before a rebuild; the build reads its row",
				"correction", c.ID, "state", string(c.State), "error", fault.Message(err))
		}
	}
	if err := e.eachPending(ctx, func(c tenant.Correction) error {
		if c.State == record.StatePrepared && !c.Kind.CopiesObjects() {
			if err := e.undoWrites(ctx, c); err != nil {
				return err
			}
			a, err := e.cfg.Store.AbandonCorrection(ctx, c.ID, reasonStopped, e.now())
			if err != nil {
				return err
			}
			rewrite(a)
			return nil
		}
		if c.RecordsState != c.State {
			rewrite(c)
		}
		return nil
	}, &errs); err != nil {
		return err
	}
	if len(errs) > 0 {
		return errors.Join(append([]error{errLeftForRebuild}, errs...)...)
	}
	var left int
	if err := e.eachPending(ctx, func(c tenant.Correction) error {
		if c.State == record.StatePrepared && !c.Kind.CopiesObjects() {
			left++
		}
		return nil
	}, &errs); err != nil {
		return err
	}
	if left > 0 {
		return errLeftForRebuild
	}
	return nil
}

// eachPending calls fn with every correction the store answers pending
// (PendingCorrections: not ended, or whose records lag their row), going on
// past fn's failures, which it adds to errs; a failure to read is answered.
func (e *Engine) eachPending(ctx context.Context, fn func(tenant.Correction) error, errs *[]error) error {
	after := ""
	for {
		page, err := e.cfg.Store.PendingCorrections(ctx, after, pendingPage)
		if err != nil {
			return err
		}
		for _, c := range page {
			after = c.ID
			if err := fn(c); err != nil {
				*errs = append(*errs, err)
			}
		}
		if len(page) < pendingPage {
			return nil
		}
	}
}
