package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// A move's preview (tenant.Moves): package sqlkit's one implementation, run
// here in Postgres's transactions, as the other corrections' are.

// PreviewMove implements tenant.Moves.
func (t *Tenant) PreviewMove(ctx context.Context, target tenant.CorrectionTarget, to string, now time.Time) (tenant.MovePreview, error) {
	return t.sending().PreviewMove(ctx, target, to, now)
}

// IdentityRewritten implements tenant.Moves.
func (t *Tenant) IdentityRewritten(ctx context.Context, id string) error {
	return t.sending().IdentityRewritten(ctx, id)
}

// OwedIdentityRewrites implements tenant.Moves.
func (t *Tenant) OwedIdentityRewrites(ctx context.Context, after string, limit int) ([]tenant.Correction, error) {
	return t.sending().OwedIdentityRewrites(ctx, after, limit)
}

// IdentityRefused implements tenant.Moves.
func (t *Tenant) IdentityRefused(ctx context.Context, id, why string) error {
	return t.sending().IdentityRefused(ctx, id, why)
}
