package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// How a tenant's use is counted against its quota (tenant.Quotas). Three
// running totals, each changed in the transaction of the change it counts,
// so a read never sums the catalog or walks a storage:
//
//   - what the catalog holds: catalog_totals' instances and the sum of
//     storage_totals, which every commit and correction already moves;
//   - what corrections own and have not purged: purge_totals, moved here
//     wherever a correction's instance objects (correction_keys of ord 1)
//     are recorded, settled or let go;
//   - what receptions received and have not decided yet: the receptions
//     still holding files in the incoming area, a few rows by their index.

// QuotaUse implements tenant.Quotas.
func (k Sending) QuotaUse(ctx context.Context) (tenant.QuotaUse, error) {
	var u tenant.QuotaUse
	err := k.Host.Read(ctx, "the tenant's use cannot be read", func(tx SendingTx) error {
		u = tenant.QuotaUse{}
		// One statement, so it reads one snapshot also at read committed:
		// a commit that archives a reception's files between two reads
		// would otherwise be missed by both, or counted by both.
		//
		// A reception's files not yet decided are those received, less
		// those archived, found again or quarantined. The archived bytes
		// are those of the files as archived, which a de-identification may
		// have made larger or smaller than they came, so a reception's
		// difference is held at zero rather than allowed to go below it.
		// The condition is the one the receptions_holding index is made
		// with, so only the receptions holding files are read.
		_, err := tx.QueryRow(ctx, `SELECT
			(SELECT instances FROM catalog_totals),
			(SELECT coalesce(sum(bytes), 0) FROM storage_totals),
			(SELECT instances FROM purge_totals),
			(SELECT bytes FROM purge_totals),
			(SELECT coalesce(sum(CASE WHEN files_received > files_archived + files_duplicate + files_quarantined
				THEN files_received - files_archived - files_duplicate - files_quarantined ELSE 0 END), 0)
				FROM receptions WHERE ended = ? OR (state = 'failed' AND retry_reception = '' AND discard_reason = '')),
			(SELECT coalesce(sum(CASE WHEN bytes_received > bytes_archived + bytes_duplicate + bytes_quarantined
				THEN bytes_received - bytes_archived - bytes_duplicate - bytes_quarantined ELSE 0 END), 0)
				FROM receptions WHERE ended = ? OR (state = 'failed' AND retry_reception = '' AND discard_reason = ''))`,
			[]any{k.D.Bool(false), k.D.Bool(false)}, &u.HeldInstances, &u.HeldBytes, &u.PurgeInstances, &u.PurgeBytes,
			&u.ReceivingInstances, &u.ReceivingBytes)
		return err
	})
	return u, err
}

// ownedUnpurged returns the instance objects the correction id owns that
// no purge has settled: its keys of ord 1 with no outcome, read by the
// correction_keys_pending index.
func ownedUnpurged(ctx context.Context, tx SendingTx, id string) (n, bytes int64, err error) {
	_, err = tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(size), 0) FROM correction_keys
		WHERE correction_id = ? AND ord = 1 AND outcome = ''`, []any{id}, &n, &bytes)
	return n, bytes, err
}

// movePurgeTotals adds n instance objects of bytes bytes, either of them
// negative, to what corrections own and have not purged. A total is held
// at zero rather than taken below it: a count that went wrong must never
// refuse the purge whose transaction moves it, since a purge removes what
// a person was promised would go; a rebuild counts the totals again.
func movePurgeTotals(ctx context.Context, tx SendingTx, n, bytes int64) error {
	if n == 0 && bytes == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE purge_totals SET
		instances = CASE WHEN instances + ? < 0 THEN 0 ELSE instances + ? END,
		bytes = CASE WHEN bytes + ? < 0 THEN 0 ELSE bytes + ? END`, n, n, bytes, bytes)
	return err
}

// countOwned runs write, which records or lets go keys of the correction
// id, and moves purge_totals by what it changed of the correction's
// unpurged instance objects. It reads them before and after, by the index
// of a correction's keys with no outcome, so a statement over many rows (an
// INSERT ... SELECT that skips keys already held, an UPDATE or a DELETE of
// every key) is counted exactly, at the cost of reading what it touched.
func countOwned(ctx context.Context, tx SendingTx, id string, write func() error) error {
	n0, b0, err := ownedUnpurged(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	n1, b1, err := ownedUnpurged(ctx, tx, id)
	if err != nil {
		return err
	}
	return movePurgeTotals(ctx, tx, n1-n0, b1-b0)
}

// RecountPurgeTotals makes purge_totals again from the keys every
// correction owns and has not purged, in tx. A rebuild runs it in its
// activation, so a total that ever went wrong is made right by one.
func RecountPurgeTotals(ctx context.Context, tx SendingTx) error {
	_, err := tx.Exec(ctx, `UPDATE purge_totals SET
		instances = (SELECT count(*) FROM correction_keys WHERE ord = 1 AND outcome = ''),
		bytes = (SELECT coalesce(sum(size), 0) FROM correction_keys WHERE ord = 1 AND outcome = '')`)
	return err
}
