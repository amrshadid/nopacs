package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The audit seal (master.AuditSeals) of the master and of every tenant, on
// the tables of migrations/master/0004_audit_seal.sql and
// migrations/tenant/0017_audit_seal.sql, which have the same shape. The
// methods are on base, which Master and Tenant both embed, so the one
// implementation serves both and the two cannot drift.

const checkpointColumns = `to_seq, from_seq, installation, log, head, previous, digest, at, key_version, signature, pruned`

// errSealedSince is the Conflict of a seal that no longer follows the log.
var errSealedSince = fault.New(fault.Conflict, "the audit log was sealed since its entries were read, or an entry to seal is sealed already or gone; nothing was sealed")

// The named constraints of audit_checkpoints a racing seal breaks.
var checkpointKeys = []string{"audit_checkpoints_pkey", "audit_checkpoints_from_key", "audit_checkpoints_digest_key"}

// scanCheckpoint reads one row of checkpointColumns.
func scanCheckpoint(row scanner) (master.AuditCheckpoint, error) {
	var c master.AuditCheckpoint
	var head, previous, digest []byte
	var at time.Time
	var version int32
	if err := row.Scan(&c.To, &c.From, &c.Installation, &c.Log, &head, &previous, &digest, &at, &version, &c.Signature, &c.Pruned); err != nil {
		return c, err
	}
	if len(head) != master.AuditDigestSize || len(previous) != master.AuditDigestSize || len(digest) != master.AuditDigestSize || version < 1 {
		return c, fault.New(fault.Internal, "an audit checkpoint's row does not hold its sizes")
	}
	copy(c.Head[:], head)
	copy(c.Previous[:], previous)
	copy(c.Digest[:], digest)
	c.Time = at.UTC()
	c.KeyVersion = uint32(version)
	return c, nil
}

// CountUnsealedAudit implements master.AuditSealReads.
func (b *base) CountUnsealedAudit(ctx context.Context) (int64, error) {
	if err := b.open(); err != nil {
		return 0, err
	}
	var n int64
	err := b.pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE seq IS NULL`).Scan(&n)
	return n, b.fail(ctx, err, "the unsealed audit entries cannot be counted")
}

// SealedAudit implements master.AuditSealReads.
func (b *base) SealedAudit(ctx context.Context, fromSeq int64, limit int) ([]master.SealedAuditEntry, error) {
	if err := master.ValidateAuditSealLimit(limit, master.MaxAuditSealBatch); err != nil {
		return nil, err
	}
	if fromSeq < 1 {
		return nil, fault.New(fault.Invalid, "a seq is from 1")
	}
	if err := b.open(); err != nil {
		return nil, err
	}
	rows, err := b.pool.Query(ctx, `SELECT seq, `+auditColumns+` FROM audit WHERE seq >= $1 ORDER BY seq LIMIT $2`, fromSeq, limit)
	out, err := collect(rows, err, func(row scanner) (master.SealedAuditEntry, error) {
		var s master.SealedAuditEntry
		e, err := scanAudit(seqScanner{row: row, seq: &s.Seq})
		s.Entry = e
		return s, err
	})
	return out, b.fail(ctx, err, "the sealed audit entries cannot be read")
}

// seqScanner reads a row of seq then auditColumns: the seq into seq, the
// rest with scanAudit's own Scan.
type seqScanner struct {
	row scanner
	seq *int64
}

// Scan reads the seq first, then dest.
func (s seqScanner) Scan(dest ...any) error {
	return s.row.Scan(append([]any{s.seq}, dest...)...)
}

// AuditCheckpoints implements master.AuditSealReads.
func (b *base) AuditCheckpoints(ctx context.Context, fromSeq int64, limit int) ([]master.AuditCheckpoint, error) {
	if err := master.ValidateAuditSealLimit(limit, master.MaxPageLimit); err != nil {
		return nil, err
	}
	if fromSeq < 1 {
		return nil, fault.New(fault.Invalid, "a seq is from 1")
	}
	if err := b.open(); err != nil {
		return nil, err
	}
	rows, err := b.pool.Query(ctx, `SELECT `+checkpointColumns+` FROM audit_checkpoints WHERE to_seq >= $1 ORDER BY to_seq LIMIT $2`, fromSeq, limit)
	out, err := collect(rows, err, scanCheckpoint)
	return out, b.fail(ctx, err, "the audit checkpoints cannot be read")
}

// RecentAuditCheckpoints implements master.AuditSealReads.
func (b *base) RecentAuditCheckpoints(ctx context.Context, beforeSeq int64, limit int) ([]master.AuditCheckpoint, error) {
	if err := master.ValidateAuditSealLimit(limit, master.MaxPageLimit); err != nil {
		return nil, err
	}
	if beforeSeq < 0 {
		return nil, fault.New(fault.Invalid, "a seq is not negative")
	}
	if err := b.open(); err != nil {
		return nil, err
	}
	query, args := `SELECT `+checkpointColumns+` FROM audit_checkpoints ORDER BY to_seq DESC LIMIT $1`, []any{limit}
	if beforeSeq > 0 {
		query, args = `SELECT `+checkpointColumns+` FROM audit_checkpoints WHERE to_seq < $1 ORDER BY to_seq DESC LIMIT $2`, []any{beforeSeq, limit}
	}
	rows, err := b.pool.Query(ctx, query, args...)
	out, err := collect(rows, err, scanCheckpoint)
	return out, b.fail(ctx, err, "the audit checkpoints cannot be read")
}

// AuditAnchors implements master.AuditSealReads.
func (b *base) AuditAnchors(ctx context.Context) ([]master.AuditAnchor, error) {
	if err := b.open(); err != nil {
		return nil, err
	}
	rows, err := b.pool.Query(ctx, `SELECT log, to_seq, digest FROM audit_anchors ORDER BY log`)
	out, err := collect(rows, err, scanAnchor)
	return out, b.fail(ctx, err, "the audit anchors cannot be read")
}

// scanAnchor reads one row of log, to_seq, digest.
func scanAnchor(row scanner) (master.AuditAnchor, error) {
	var a master.AuditAnchor
	var digest []byte
	if err := row.Scan(&a.Log, &a.To, &digest); err != nil {
		return a, err
	}
	if len(digest) != master.AuditDigestSize {
		return a, fault.New(fault.Internal, "an audit anchor's row does not hold a digest")
	}
	copy(a.Digest[:], digest)
	return a, nil
}

// UnsealedAudit implements master.AuditSealWrites. It reads the partial
// index audit_unsealed, whose rows are exactly the unsealed entries.
func (b *base) UnsealedAudit(ctx context.Context, after master.AuditPosition, limit int) ([]master.AuditEntry, error) {
	if err := master.ValidateAuditSealLimit(limit, master.MaxAuditSealBatch); err != nil {
		return nil, err
	}
	if err := b.open(); err != nil {
		return nil, err
	}
	query, args := `SELECT `+auditColumns+` FROM audit WHERE seq IS NULL ORDER BY at, id LIMIT $1`, []any{limit}
	if !after.Time.IsZero() || after.ID != "" {
		query = `SELECT ` + auditColumns + ` FROM audit WHERE seq IS NULL AND (at, id) > ($1, $2) ORDER BY at, id LIMIT $3`
		args = []any{micros(after.Time), after.ID, limit}
	}
	rows, err := b.pool.Query(ctx, query, args...)
	out, err := collect(rows, err, scanAudit)
	return out, b.fail(ctx, err, "the unsealed audit entries cannot be read")
}

// SealAudit implements master.AuditSealWrites. The entries get their seq in
// one UPDATE over two arrays, so a seal of 10,000 entries is one round trip.
// Under read committed, a seal racing another for the same entries waits on
// their row locks and then finds them sealed (seq IS NULL no longer holds),
// and one racing for the same place in the log breaks a unique constraint
// of audit_checkpoints: either way it is Conflict and keeps nothing.
func (b *base) SealAudit(ctx context.Context, s master.AuditSeal) error {
	if err := s.Validate(); err != nil {
		return err
	}
	c := s.Checkpoint
	return b.write(ctx, "the audit entries cannot be sealed", func(tx pgx.Tx) error {
		var lastTo int64
		var lastDigest []byte
		err := tx.QueryRow(ctx, `SELECT to_seq, digest FROM audit_checkpoints ORDER BY to_seq DESC LIMIT 1`).Scan(&lastTo, &lastDigest)
		switch {
		case isNoRow(err):
			lastDigest = make([]byte, master.AuditDigestSize)
		case err != nil:
			return err
		}
		if c.From != lastTo+1 || string(c.Previous[:]) != string(lastDigest) {
			return errSealedSince
		}
		seqs := make([]int64, len(s.IDs))
		for i := range seqs {
			seqs[i] = c.From + int64(i)
		}
		tag, err := tx.Exec(ctx, `UPDATE audit SET seq = v.seq FROM unnest($1::text[], $2::bigint[]) AS v(id, seq)
			WHERE audit.id = v.id AND audit.seq IS NULL`, s.IDs, seqs)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(s.IDs)) {
			return errSealedSince
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_checkpoints (`+checkpointColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, false)`,
			c.To, c.From, c.Installation, c.Log, c.Head[:], c.Previous[:], c.Digest[:], micros(c.Time), int32(c.KeyVersion), c.Signature) //nolint:gosec // G115: Validate holds the key version to 2^31-1.
		for _, name := range checkpointKeys {
			if constraintViolation(err, name) {
				return errSealedSince
			}
		}
		if constraintViolation(err, "audit_by_seq") {
			return errSealedSince
		}
		return err
	})
}

// SetAuditAnchor implements master.AuditSealWrites. The held row is read
// with FOR UPDATE, so two writers of one anchor are ordered and each
// compares with what the other left.
func (b *base) SetAuditAnchor(ctx context.Context, a master.AuditAnchor) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return b.write(ctx, "the audit anchor cannot be kept", func(tx pgx.Tx) error {
		held, err := scanAnchor(tx.QueryRow(ctx, `SELECT log, to_seq, digest FROM audit_anchors WHERE log = $1 FOR UPDATE`, a.Log))
		switch {
		case isNoRow(err):
		case err != nil:
			return err
		case held == a:
			return nil
		case a.To <= held.To:
			return fault.New(fault.Conflict, "the anchor would go back from the checkpoint this log keeps of the other: the other log went back")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO audit_anchors (log, to_seq, digest) VALUES ($1, $2, $3)
			ON CONFLICT (log) DO UPDATE SET to_seq = EXCLUDED.to_seq, digest = EXCLUDED.digest
			WHERE audit_anchors.to_seq < EXCLUDED.to_seq`, a.Log, a.To, a.Digest[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			// A first anchor of this log written by another caller since the
			// read above, at a To not below this one's.
			return fault.New(fault.Conflict, "the anchor would go back from the checkpoint this log keeps of the other: the other log went back")
		}
		return nil
	})
}

// PruneAudit implements master.AuditSealWrites. The checkpoints are locked
// with FOR UPDATE, so two prunings never count the same checkpoint.
func (b *base) PruneAudit(ctx context.Context, before time.Time, limit int, audit master.AuditEntry) (master.AuditPruned, error) {
	if err := master.ValidateAuditSealLimit(limit, master.MaxAuditPruneBatch); err != nil {
		return master.AuditPruned{}, err
	}
	if err := master.ValidateTime("pruning time", before); err != nil {
		return master.AuditPruned{}, err
	}
	if err := audit.Validate(); err != nil {
		return master.AuditPruned{}, err
	}
	if err := audit.ValidatePruned(); err != nil {
		return master.AuditPruned{}, err
	}
	var out master.AuditPruned
	err := b.write(ctx, "the audit log cannot be pruned", func(tx pgx.Tx) error {
		out = master.AuditPruned{}
		rows, err := tx.Query(ctx, `SELECT from_seq, to_seq FROM audit_checkpoints
			WHERE NOT pruned AND at < $1 ORDER BY to_seq LIMIT $2 FOR UPDATE`, micros(before), limit+1)
		ranges, err := collect(rows, err, func(row scanner) ([2]int64, error) {
			var r [2]int64
			err := row.Scan(&r[0], &r[1])
			return r, err
		})
		if err != nil {
			return err
		}
		if len(ranges) > limit {
			ranges, out.More = ranges[:limit], true
		}
		if len(ranges) == 0 {
			return nil
		}
		for _, r := range ranges {
			tag, err := tx.Exec(ctx, `DELETE FROM audit WHERE seq BETWEEN $1 AND $2`, r[0], r[1])
			if err != nil {
				return err
			}
			out.Entries += tag.RowsAffected()
			if _, err := tx.Exec(ctx, `UPDATE audit_checkpoints SET pruned = true WHERE to_seq = $1`, r[1]); err != nil {
				return err
			}
		}
		out.Checkpoints = len(ranges)
		return insertAudit(ctx, tx, b.red, audit.WithPruned(out.Checkpoints, out.Entries, ranges[0][0], ranges[len(ranges)-1][1]))
	})
	if err != nil {
		return master.AuditPruned{}, err
	}
	return out, nil
}
