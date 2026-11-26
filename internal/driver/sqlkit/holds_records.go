package sqlkit

import (
	"context"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Where each hold's records stand (tenant.Holds, "Hold records"): those
// owed, each marked once written with the count and the key version it
// was sealed under, and those owed again when that version is not the
// installation's.

// HoldRecordsOwed implements tenant.Holds. The page goes by hold id then
// storage, after the cursor, so a storage left out of a pass, or one whose
// records are never taken, cannot hold every other storage behind it; a
// storage the tenant no longer has is owed nothing, since nothing can be
// written there.
func (k Sending) HoldRecordsOwed(ctx context.Context, page tenant.HoldOwedPage) ([]tenant.HoldRecordOwed, error) {
	if err := page.Validate(); err != nil {
		return nil, err
	}
	where := []string{"r.written_seq < h.seq", "h.releasing_at IS NULL", "EXISTS (SELECT 1 FROM storages AS s WHERE s.name = r.storage)",
		"(r.hold_id > ? OR (r.hold_id = ? AND r.storage > ?))"}
	args := []any{page.AfterHold, page.AfterHold, page.AfterStorage}
	if len(page.Skip) > 0 {
		where = append(where, "r.storage NOT IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(page.Skip)), ", ")+")")
		for _, s := range page.Skip {
			args = append(args, s)
		}
	}
	args = append(args, page.Limit)
	var out []tenant.HoldRecordOwed
	err := k.Host.Read(ctx, "the hold records owed cannot be read", func(tx SendingTx) error {
		out = nil
		type owed struct{ id, storage string }
		var rows []owed
		// #nosec G202 -- every fragment is a constant; values are parameters.
		if err := tx.Query(ctx, `SELECT r.hold_id, r.storage FROM hold_records AS r JOIN holds AS h ON h.id = r.hold_id
			WHERE `+strings.Join(where, " AND ")+` ORDER BY r.hold_id, r.storage LIMIT ?`, args, func(r Row) error {
			var o owed
			err := r.Scan(&o.id, &o.storage)
			rows = append(rows, o)
			return err
		}); err != nil {
			return err
		}
		for _, o := range rows {
			h, _, err := holdIn(ctx, tx, o.id, "")
			if err != nil {
				return err
			}
			out = append(out, tenant.HoldRecordOwed{Hold: h, Storage: o.storage})
		}
		return nil
	})
	return out, err
}

// MarkHoldRecord implements tenant.Holds. A record written again at the
// same count under another key version is marked with that version.
func (k Sending) MarkHoldRecord(ctx context.Context, id, storage string, seq int64, sealVersion uint32) error {
	if err := tenant.ValidateHoldID(id); err != nil {
		return err
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return err
	}
	if seq < 1 {
		return fault.New(fault.Invalid, "a hold's count is 1 or more")
	}
	if sealVersion < 1 {
		return fault.New(fault.Invalid, "a hold record's key version is 1 or more")
	}
	return k.Host.Write(ctx, "the hold record cannot be marked", func(tx SendingTx) error {
		h, found, err := holdIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if !found || !slices.Contains(h.Storages, storage) {
			return notFound("the hold is not on this storage")
		}
		if seq > h.Seq {
			return tenant.Precondition("the hold has not reached this count")
		}
		_, err = tx.Exec(ctx, `UPDATE hold_records SET written_seq = ?, seal_version = ? WHERE hold_id = ? AND storage = ?
			AND (written_seq < ? OR (written_seq = ? AND seal_version <> ?))`, seq, int64(sealVersion), id, storage, seq, seq,
			int64(sealVersion))
		return err
	})
}

// OweHoldRecordsNotSealedUnder implements tenant.Holds.
func (k Sending) OweHoldRecordsNotSealedUnder(ctx context.Context, version uint32) (int, error) {
	if version < 1 {
		return 0, fault.New(fault.Invalid, "a key version is 1 or more")
	}
	var n int64
	err := k.Host.Write(ctx, "the hold records sealed under another key cannot be owed", func(tx SendingTx) error {
		var err error
		n, err = tx.Exec(ctx, `UPDATE hold_records SET written_seq = 0 WHERE written_seq > 0 AND seal_version <> ?
			AND hold_id IN (SELECT id FROM holds WHERE state = 'active')`, int64(version))
		return err
	})
	return int(n), err
}

// DropHoldRecordsInTx removes, in the transaction that removes a storage
// or detaches a replica, what the tenant's holds keep of their records on
// it: nothing more is written there, and a row left would be owed for
// ever.
func DropHoldRecordsInTx(ctx context.Context, tx SendingTx, storage string) error {
	_, err := tx.Exec(ctx, `DELETE FROM hold_records WHERE storage = ?`, storage)
	return err
}
