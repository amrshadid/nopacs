package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A hold follows its target (tenant.Holds, "Hold records"): its records
// are owed where the target spreads after the hold is placed, and a
// relabel moves its labels.

// oweHoldRecords adds, for each hold of holds, a record owed (written_seq
// 0, below every hold's count) on each storage its target now touches
// (holdStoragesIn) that has none: a hold record must be on every storage
// and replica the target's data is on, so that a rebuild or restore from
// any of them finds the hold. A target the catalog no longer shows keeps
// the records it has.
func oweHoldRecords(ctx context.Context, tx SendingTx, holds heldSet) error {
	for _, h := range holds {
		storages, err := holdStoragesIn(ctx, tx, h.target)
		if fault.KindOf(err) == fault.NotFound {
			continue
		}
		if err != nil {
			return err
		}
		for _, s := range storages {
			if _, err := tx.Exec(ctx, `INSERT INTO hold_records (hold_id, storage, written_seq) VALUES (?, ?, 0)
				ON CONFLICT (hold_id, storage) DO NOTHING`, h.id, s.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdsAfterAttach owes, in an attach's transaction after the replica is
// recorded as one, the records of every active hold whose target has data
// on its primary to the new replica (oweHoldRecords): the records were put
// only on the storages the target touched when the hold was placed.
func holdsAfterAttach(ctx context.Context, tx SendingTx) error {
	holds, err := activeHolds(ctx, tx)
	if err != nil || len(holds) == 0 {
		return err
	}
	return oweHoldRecords(ctx, tx, holds)
}

// CommitHolds owes, in a commit's transaction that made a new scan, the
// records of the active holds that cover the scan to its storage and that
// storage's replicas when a hold has none there (oweHoldRecords): a held
// subject's or session's new scan may land on a primary its hold was never
// put on. It reads the holds after the commit holds retention's row shared
// (CommitRetention, orderRetention), so a hold placed beside it, which
// takes that row for update, either sees the scan or is seen by it.
func (k Sending) CommitHolds(ctx context.Context, tx SendingTx, c tenant.ScanCommit, newScan bool) error {
	if !newScan {
		return nil
	}
	holds, err := activeHolds(ctx, tx)
	if err != nil || len(holds) == 0 {
		return err
	}
	p := label.Path{Subject: c.SubjectLabel, Session: c.SessionLabel, Scan: c.ScanLabel}
	var covering heldSet
	for _, h := range holds {
		if h.target.Covers(p) {
			covering = append(covering, h)
		}
	}
	return oweHoldRecords(ctx, tx, covering)
}

// relabelHolds rewrites, in a relabel's publishing transaction, the labels
// of the active holds whose target it moves, and counts each change, so
// their records are owed again: a hold follows what it named, not a label.
func relabelHolds(ctx context.Context, tx SendingTx, c tenant.Correction) error {
	t, to, folded := c.Target, c.ToLabel, label.Fold(c.ToLabel)
	var q string
	var args []any
	switch c.Level {
	case corr.LevelSubject:
		q, args = `UPDATE holds SET subject_label = ?, subject_folded = ?, seq = seq + 1
			WHERE state = 'active' AND level <> 'tenant' AND subject_folded = ?`, []any{to, folded, label.Fold(t.Subject)}
	case corr.LevelSession:
		q, args = `UPDATE holds SET session_label = ?, session_folded = ?, seq = seq + 1
			WHERE state = 'active' AND subject_folded = ? AND session_folded = ?`, []any{to, folded, label.Fold(t.Subject), label.Fold(t.Session)}
	default:
		q, args = `UPDATE holds SET scan_label = ?, scan_folded = ?, seq = seq + 1
			WHERE state = 'active' AND subject_folded = ? AND session_folded = ? AND scan_folded = ?`,
			[]any{to, folded, label.Fold(t.Subject), label.Fold(t.Session), label.Fold(t.Scan)}
	}
	_, err := tx.Exec(ctx, q, args...)
	return err
}
