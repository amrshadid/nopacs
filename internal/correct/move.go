package correct

import (
	"context"
	"crypto/hmac"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MoveRequest is a move a person asked for, after its preview: a session
// filed under another subject, or a whole subject merged into another.
type MoveRequest struct {
	// Target is the session to move, or the subject to merge; To the label
	// of the subject it goes under.
	Target tenant.CorrectionTarget
	To     string
	// Reason is why; Confirm the preview's token; KeyID the key that asked.
	Reason, Confirm, KeyID string
	// RewriteIdentity asks that each moved instance be given the
	// destination's archived subject identity once the move is published
	// (RewriteIdentity), recorded with the move so a stop does not lose it.
	RewriteIdentity bool
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// MovePreview is what a move would do, the token it must carry, and when
// its old tree would be purged if it were made now.
type MovePreview struct {
	tenant.MovePreview
	// PurgeAfter is now plus the tenant's delete.grace.
	PurgeAfter time.Time
}

// moves returns the tenant's database as it previews moves.
func (e *Engine) moves() (tenant.Moves, error) {
	m, ok := e.cfg.Store.(tenant.Moves)
	if !ok {
		return nil, fault.New(fault.Unavailable, "the tenant's database keeps no moves")
	}
	return m, nil
}

// PreviewMove answers what a move of target under the subject labeled to
// would do, as the catalog holds it now, with its token: keyed with the
// confirm key, as a delete's is, so only this server makes one a move
// takes.
func (e *Engine) PreviewMove(ctx context.Context, target tenant.CorrectionTarget, to string) (MovePreview, error) {
	m, err := e.moves()
	if err != nil {
		return MovePreview{}, err
	}
	now := e.now()
	p, err := m.PreviewMove(ctx, target, to, now)
	if err != nil {
		return MovePreview{}, err
	}
	g, err := e.grace(ctx)
	if err != nil {
		return MovePreview{}, err
	}
	if e.cfg.ConfirmKey != nil {
		mac, err := e.cfg.ConfirmKey.MAC([]byte(p.Confirm))
		if err != nil {
			return MovePreview{}, err
		}
		p.Confirm = tenant.KeyedConfirm(p.Confirm, mac)
	}
	return MovePreview{MovePreview: p, PurgeAfter: now.Add(g)}, nil
}

// Move records a move and puts its records, prepared, and answers it; the
// copy of every object to its key under the destination and the publish
// are ContinueRelabel's, which the caller runs in the background (and
// Recover after a stop), then RewriteIdentity when it was asked. A request
// whose token made a move already answers that move.
func (e *Engine) Move(ctx context.Context, r MoveRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	confirm, err := e.unkeyMoveConfirm(ctx, r.Target, r.To, r.Confirm)
	if err != nil {
		return tenant.Correction{}, err
	}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindMove, Target: r.Target,
		ToLabel: r.To, Reason: r.Reason, Confirm: confirm, KeyID: r.KeyID, RewriteIdentity: r.RewriteIdentity, Now: now}, r.Audit, r.Guard)
	if err == nil && e.cfg.ConfirmKey != nil {
		e.remember(r.Confirm, confirm, r.Target)
	}
	if err != nil {
		return c, err
	}
	if !made {
		// The token's move, found by its token alone: answered only to the
		// same request.
		if c.Kind != record.KindMove || c.Target != r.Target || c.ToLabel != r.To {
			return tenant.Correction{}, errTokenMismatch
		}
		return c, nil
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	return c, e.reached(c, "records")
}

// unkeyMoveConfirm checks a token PreviewMove made with the confirm key and
// returns the store's own token for it, made again at its issue time; the
// store then checks that token against what the move does now, in the
// move's own transaction. Without a confirm key the token is the store's.
func (e *Engine) unkeyMoveConfirm(ctx context.Context, target tenant.CorrectionTarget, to, token string) (string, error) {
	if e.cfg.ConfirmKey == nil {
		return token, nil
	}
	issued, mac, ok := tenant.SplitKeyedConfirm(token)
	if !ok {
		return "", fault.New(fault.Precondition, "the confirm token is not one this server's preview gave")
	}
	if c1, was, ok := e.used(token); ok {
		// A token that made a move in this process, sent again: a merge's
		// subject no longer answers a preview, so the store answers the move
		// its token made (Move checks it is this request's).
		if was != target {
			return "", errTokenMismatch
		}
		return c1, nil
	}
	m, err := e.moves()
	if err != nil {
		return "", err
	}
	p, err := m.PreviewMove(ctx, target, to, issued)
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
