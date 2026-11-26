package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A hold's release (tenant.Holds, "A release reserves its count first"):
// begun, it moves the hold to the release's count before any record says
// released; committed at that count; or ended, the hold active at the
// count after, so a released record written for a release that did not
// commit never outranks the hold's active records.

// ReleaseHold implements tenant.Holds.
func (k Sending) ReleaseHold(ctx context.Context, r tenant.HoldRelease, audit master.AuditEntry, guard tenant.Guard) (tenant.Hold, error) {
	if err := r.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	var out tenant.Hold
	err := k.Host.Write(ctx, "the hold cannot be released", func(tx SendingTx) error {
		if err := k.lockRetention(ctx, tx); err != nil {
			return err
		}
		h, found, err := holdIn(ctx, tx, r.ID, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no hold with this id")
		}
		if h.State == tenant.HoldActive {
			q := ""
			switch {
			case h.Releasing && r.Seq != h.Seq:
				return tenant.Precondition("the hold's release under way is not at this count")
			case h.Releasing:
				// The records were written at the count the release
				// reserved, and stand as they were marked.
				q = `UPDATE holds SET state = 'released', release_reason = ?, released_by = ?, released_at = ?, releasing_at = NULL
					WHERE id = ?`
			case r.Seq != 0:
				return tenant.Precondition("no release of the hold is under way")
			default:
				q = `UPDATE holds SET state = 'released', release_reason = ?, released_by = ?, released_at = ?, seq = seq + 1 WHERE id = ?`
			}
			if _, err := tx.Exec(ctx, q, r.Reason, r.KeyID, k.D.Time(r.Now), r.ID); err != nil {
				return err
			}
		}
		if out, _, err = holdIn(ctx, tx, r.ID, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, withObject(audit, r.ID), guard)
	})
	return out, err
}

// BeginHoldRelease implements tenant.Holds.
func (k Sending) BeginHoldRelease(ctx context.Context, r tenant.HoldRelease) (tenant.Hold, error) {
	if err := r.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	var out tenant.Hold
	err := k.Host.Write(ctx, "the hold's release cannot begin", func(tx SendingTx) error {
		h, found, err := holdIn(ctx, tx, r.ID, k.D.Lock)
		switch {
		case err != nil:
			return err
		case !found:
			return notFound("the tenant has no hold with this id")
		case h.State == tenant.HoldReleased:
			out = h
			return nil
		case h.Releasing:
			return tenant.Precondition("a release of the hold is under way")
		case h.Seq >= tenant.MaxHoldSeq:
			return fault.New(fault.OutOfRange, "the hold's count is at its bound")
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET seq = seq + 1, releasing_at = ? WHERE id = ?`, k.D.Time(r.Now), r.ID); err != nil {
			return err
		}
		out, _, err = holdIn(ctx, tx, r.ID, "")
		return err
	})
	return out, err
}

// AbortHoldRelease implements tenant.Holds.
func (k Sending) AbortHoldRelease(ctx context.Context, id string, seq int64) (tenant.Hold, error) {
	if err := tenant.ValidateHoldID(id); err != nil {
		return tenant.Hold{}, err
	}
	if seq < 1 || seq > tenant.MaxHoldSeq {
		return tenant.Hold{}, fault.New(fault.Invalid, "a hold's count is 1 or more")
	}
	var out tenant.Hold
	err := k.Host.Write(ctx, "the hold's release cannot be ended", func(tx SendingTx) error {
		h, found, err := holdIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no hold with this id")
		}
		if h.Releasing && h.Seq >= seq {
			if _, err := tx.Exec(ctx, `UPDATE holds SET seq = seq + 1, releasing_at = NULL WHERE id = ?`, id); err != nil {
				return err
			}
		}
		out, _, err = holdIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// AbortStaleHoldReleases implements tenant.Holds.
func (k Sending) AbortStaleHoldReleases(ctx context.Context, before time.Time) ([]string, error) {
	if err := master.ValidateTime("time", before); err != nil {
		return nil, err
	}
	var out []string
	err := k.Host.Write(ctx, "the holds' releases left under way cannot be ended", func(tx SendingTx) error {
		out = nil
		if err := tx.Query(ctx, `SELECT id FROM holds WHERE releasing_at IS NOT NULL AND releasing_at < ? ORDER BY id`+k.D.Lock,
			[]any{k.D.Time(before)}, func(r Row) error {
				var id string
				err := r.Scan(&id)
				out = append(out, id)
				return err
			}); err != nil || len(out) == 0 {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE holds SET seq = seq + 1, releasing_at = NULL WHERE releasing_at IS NOT NULL AND releasing_at < ?`,
			k.D.Time(before))
		return err
	})
	return out, err
}
