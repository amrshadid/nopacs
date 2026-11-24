package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/fault"
)

// base is what every store on one SQLite file has, whatever it holds: its
// pools, whether it is closed, its name in error messages, and the queue of
// changes waiting to be written (write.go). Master and Tenant embed it, so
// the two classify a failure the same way, close the same way, and write
// changes the same way.
type base struct {
	p      *pools
	closed atomic.Bool
	name   dbName
	w      committer
}

// open fails once the store is closed, with a kind, rather than with
// database/sql's unclassified "database is closed".
func (b *base) open() error {
	if b.closed.Load() {
		return b.errClosed(nil)
	}
	return nil
}

// errClosed is the error of a call on a closed store, or in flight when it
// was closed.
func (b *base) errClosed(cause error) error {
	return fault.Wrap(fault.Unavailable, cause, "the "+string(b.name)+" is closed")
}

// fail gives err its kind. An error that already has one keeps it: a
// NotFound or Conflict decided inside a transaction is the answer even if
// the context then ended, since nothing was committed either way. Otherwise a
// context that ended makes it Unavailable, keeping the context's cause and
// the driver's error in the chain for the log and for errors.Is; a store
// closed under the call makes it Unavailable; and the driver's code decides
// the rest (classify).
func (b *base) fail(ctx context.Context, err error, message string) error {
	if err == nil {
		return nil
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		return err
	}
	if ctx.Err() != nil {
		return fault.Wrap(fault.Unavailable, errors.Join(context.Cause(ctx), err),
			"the request ended before the "+string(b.name)+" answered")
	}
	if b.closed.Load() {
		return b.errClosed(err)
	}
	return classify(err, b.name, message)
}

// notFound turns sql.ErrNoRows into NotFound with what, and gives any other
// error its kind with message.
func (b *base) notFound(ctx context.Context, err error, what, message string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fault.New(fault.NotFound, what)
	}
	return b.fail(ctx, err, message)
}

// notReady is the error of statements that would not prepare. They fail to
// prepare most often because the tables are not there yet; a context that
// ended, a closed store or a busy database keep their own message.
func (b *base) notReady(ctx context.Context, err error) error {
	notReady := "the " + string(b.name) + " is not ready; has it been migrated?"
	ferr := b.fail(ctx, err, notReady)
	if fault.KindOf(ferr) == fault.Internal {
		ferr = fault.Wrap(fault.Unavailable, err, notReady)
	}
	return ferr
}

// ping reaches both pools, since either failing fails requests.
func (b *base) ping(ctx context.Context) error {
	if err := b.open(); err != nil {
		return err
	}
	if err := b.p.read.PingContext(ctx); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the "+string(b.name)+" does not answer")
	}
	if err := b.p.write.PingContext(ctx); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the "+string(b.name)+" does not answer")
	}
	return nil
}

// shutdown closes the prepared statements (when close is not nil) and both
// pools, once; a second call does nothing.
func (b *base) shutdown(closeStatements func()) error {
	if b.closed.Swap(true) {
		return nil
	}
	if closeStatements != nil {
		closeStatements()
	}
	if err := b.p.close(); err != nil {
		return fault.Wrap(fault.Internal, err, "the "+string(b.name)+" did not close cleanly")
	}
	return nil
}
