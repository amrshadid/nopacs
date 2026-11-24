package correct

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
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
			if c.State != record.StatePrepared || c.Kind == record.KindRelabel {
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
