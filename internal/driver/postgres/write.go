package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// rollbackTimeout bounds how long a failed transaction waits for its
// ROLLBACK.
const rollbackTimeout = 5 * time.Second

// commitTimeout bounds how long a COMMIT, once sent, waits for the server.
const commitTimeout = 30 * time.Second

// write runs fn in one transaction and commits only when fn returns nil.
// Every changing operation and its audit entry go through here together,
// which is what makes them both or neither. The transaction is read
// committed; the statements in fn are written to be right under it (row
// locks and compare-and-set updates, never read, decide, write).
func (b *base) write(ctx context.Context, message string, fn func(tx pgx.Tx) error) error {
	if err := b.open(); err != nil {
		return err
	}
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return b.fail(ctx, err, message)
	}
	if err := fn(tx); err != nil {
		b.rollback(tx)
		return b.fail(ctx, err, message)
	}
	// A context that ended before the COMMIT is sent ends the call with
	// nothing committed. Once it is sent the COMMIT is not tied to the
	// context: canceling it while the server answers would return an error
	// for a transaction that committed, and a call that fails must have
	// committed nothing. So the call returns nil exactly when the commit
	// happened.
	if ctx.Err() != nil {
		b.rollback(tx)
		return b.fail(ctx, ctx.Err(), message)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if err := tx.Commit(cctx); err != nil {
		b.rollback(tx)
		return b.fail(ctx, err, message)
	}
	return nil
}

// rollback ends a transaction that failed. It does not use the request's
// context, which may be the reason for the failure and would then leave the
// connection with its transaction open; the pool destroys a connection whose
// rollback fails, so none is leaked.
func (b *base) rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx) // The error that made the rollback necessary is the one returned.
}
