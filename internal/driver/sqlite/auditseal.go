package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The audit seal (master.AuditSeals) of the master and of every tenant, on
// the tables of migrations/master/0004_audit_seal.sql and
// migrations/tenant/0017_audit_seal.sql, which have the same shape. The
// methods are on base, which Master and Tenant both embed, so the one
// implementation serves both and the two cannot drift. Each statement is
// prepared when it runs rather than with the store's statements: a seal runs
// once an interval, and a store opened on an older schema still prepares
// the statements it runs on every request.

const checkpointColumns = `to_seq, from_seq, installation, log, head, previous, digest, at, key_version, signature, pruned`

// errSealedSince is the Conflict of a seal that no longer follows the log.
var errSealedSince = fault.New(fault.Conflict, "the audit log was sealed since its entries were read, or an entry to seal is sealed already or gone; nothing was sealed")

// scanCheckpoint reads one row of checkpointColumns.
func scanCheckpoint(row interface{ Scan(...any) error }) (master.AuditCheckpoint, error) {
	var c master.AuditCheckpoint
	var head, previous, digest []byte
	var at, version, pruned int64
	if err := row.Scan(&c.To, &c.From, &c.Installation, &c.Log, &head, &previous, &digest, &at, &version, &c.Signature, &pruned); err != nil {
		return c, err
	}
	if len(head) != master.AuditDigestSize || len(previous) != master.AuditDigestSize || len(digest) != master.AuditDigestSize ||
		version < 1 || version > master.MaxAuditKeyVersion {
		return c, fault.New(fault.Internal, "an audit checkpoint's row does not hold its sizes")
	}
	copy(c.Head[:], head)
	copy(c.Previous[:], previous)
	copy(c.Digest[:], digest)
	c.Time = fromMicros(at)
	c.KeyVersion = uint32(version)
	c.Pruned = pruned == 1
	return c, nil
}

// CountUnsealedAudit implements master.AuditSealReads.
func (b *base) CountUnsealedAudit(ctx context.Context) (int64, error) {
	if err := b.open(); err != nil {
		return 0, err
	}
	var n int64
	err := b.p.read.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE seq IS NULL`).Scan(&n)
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
	rows, err := b.p.read.QueryContext(ctx, `SELECT seq, `+auditColumns+` FROM audit WHERE seq >= ? ORDER BY seq LIMIT ?`, fromSeq, limit)
	out, err := collect(rows, err, func(row interface{ Scan(...any) error }) (master.SealedAuditEntry, error) {
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
	row interface{ Scan(...any) error }
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
	rows, err := b.p.read.QueryContext(ctx, `SELECT `+checkpointColumns+` FROM audit_checkpoints WHERE to_seq >= ? ORDER BY to_seq LIMIT ?`, fromSeq, limit)
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
	query, args := `SELECT `+checkpointColumns+` FROM audit_checkpoints ORDER BY to_seq DESC LIMIT ?`, []any{limit}
	if beforeSeq > 0 {
		query, args = `SELECT `+checkpointColumns+` FROM audit_checkpoints WHERE to_seq < ? ORDER BY to_seq DESC LIMIT ?`, []any{beforeSeq, limit}
	}
	rows, err := b.p.read.QueryContext(ctx, query, args...)
	out, err := collect(rows, err, scanCheckpoint)
	return out, b.fail(ctx, err, "the audit checkpoints cannot be read")
}

// AuditAnchors implements master.AuditSealReads.
func (b *base) AuditAnchors(ctx context.Context) ([]master.AuditAnchor, error) {
	if err := b.open(); err != nil {
		return nil, err
	}
	rows, err := b.p.read.QueryContext(ctx, `SELECT log, to_seq, digest FROM audit_anchors ORDER BY log`)
	out, err := collect(rows, err, scanAnchor)
	return out, b.fail(ctx, err, "the audit anchors cannot be read")
}

// scanAnchor reads one row of log, to_seq, digest.
func scanAnchor(row interface{ Scan(...any) error }) (master.AuditAnchor, error) {
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
	query, args := `SELECT `+auditColumns+` FROM audit WHERE seq IS NULL ORDER BY at, id LIMIT ?`, []any{limit}
	if !after.Time.IsZero() || after.ID != "" {
		query = `SELECT ` + auditColumns + ` FROM audit WHERE seq IS NULL AND (at, id) > (?, ?) ORDER BY at, id LIMIT ?`
		args = []any{micros(after.Time), after.ID, limit}
	}
	rows, err := b.p.read.QueryContext(ctx, query, args...)
	out, err := collect(rows, err, scanAudit)
	return out, b.fail(ctx, err, "the unsealed audit entries cannot be read")
}

// SealAudit implements master.AuditSealWrites.
func (b *base) SealAudit(ctx context.Context, s master.AuditSeal) error {
	if err := s.Validate(); err != nil {
		return err
	}
	c := s.Checkpoint
	return b.writeTx(ctx, "the audit entries cannot be sealed", func(tx *sql.Tx) error {
		var lastTo int64
		var lastDigest []byte
		err := tx.QueryRowContext(ctx, `SELECT to_seq, digest FROM audit_checkpoints ORDER BY to_seq DESC LIMIT 1`).Scan(&lastTo, &lastDigest)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			lastDigest = make([]byte, master.AuditDigestSize)
		case err != nil:
			return err
		}
		if c.From != lastTo+1 || string(c.Previous[:]) != string(lastDigest) {
			return errSealedSince
		}
		stmt, err := tx.PrepareContext(ctx, `UPDATE audit SET seq = ? WHERE id = ? AND seq IS NULL`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i, id := range s.IDs {
			res, err := stmt.ExecContext(ctx, c.From+int64(i), id)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return errSealedSince
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_checkpoints (`+checkpointColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			c.To, c.From, c.Installation, c.Log, c.Head[:], c.Previous[:], c.Digest[:], micros(c.Time), int64(c.KeyVersion), c.Signature)
		if isUniqueViolation(err) {
			return errSealedSince
		}
		return err
	})
}

// SetAuditAnchor implements master.AuditSealWrites.
func (b *base) SetAuditAnchor(ctx context.Context, a master.AuditAnchor) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return b.writeTx(ctx, "the audit anchor cannot be kept", func(tx *sql.Tx) error {
		held, err := scanAnchor(tx.QueryRowContext(ctx, `SELECT log, to_seq, digest FROM audit_anchors WHERE log = ?`, a.Log))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case held == a:
			return nil
		case a.To <= held.To:
			return fault.New(fault.Conflict, "the anchor would go back from the checkpoint this log keeps of the other: the other log went back")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_anchors (log, to_seq, digest) VALUES (?, ?, ?)
			ON CONFLICT (log) DO UPDATE SET to_seq = excluded.to_seq, digest = excluded.digest`, a.Log, a.To, a.Digest[:])
		return err
	})
}

// PruneAudit implements master.AuditSealWrites.
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
	err := b.writeTx(ctx, "the audit log cannot be pruned", func(tx *sql.Tx) error {
		out = master.AuditPruned{}
		rows, err := tx.QueryContext(ctx, `SELECT from_seq, to_seq FROM audit_checkpoints
			WHERE pruned = 0 AND at < ? ORDER BY to_seq LIMIT ?`, micros(before), limit+1)
		ranges, err := collect(rows, err, func(row interface{ Scan(...any) error }) ([2]int64, error) {
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
			res, err := tx.ExecContext(ctx, `DELETE FROM audit WHERE seq BETWEEN ? AND ?`, r[0], r[1])
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			out.Entries += n
			if _, err := tx.ExecContext(ctx, `UPDATE audit_checkpoints SET pruned = 1 WHERE to_seq = ?`, r[1]); err != nil {
				return err
			}
		}
		out.Checkpoints = len(ranges)
		args, err := auditArgs(audit.WithPruned(out.Checkpoints, out.Entries, ranges[0][0], ranges[len(ranges)-1][1]))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit (`+auditColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		return duplicateAudit(err)
	})
	if err != nil {
		return master.AuditPruned{}, err
	}
	return out, nil
}
