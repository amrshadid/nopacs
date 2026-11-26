package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// CreateKey implements master.Store.
func (m *Master) CreateKey(ctx context.Context, k master.Key, audit master.AuditEntry, by master.Actor) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	if err := by.Validate(); err != nil {
		return err
	}
	return m.write(ctx, "the key cannot be created", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		_, err := tx.StmtContext(ctx, s.insertKey).ExecContext(ctx,
			k.ID, k.Hash, k.Holder, micros(k.Created), nullMicros(k.Expires), k.Rules.Networks.String(), k.Rules.ClientCert, boolInt(k.Masked))
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, err, "a key with this id exists")
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit)
	})
}

const keyColumns = `id, hash, holder, created, expires, last_used, revoked, networks, client_cert, last_address, successor, suspended, masked`

// scanKey reads one row of keyColumns.
func scanKey(row interface{ Scan(...any) error }) (master.Key, error) {
	var k master.Key
	var created int64
	var expires, lastUsed, revoked, suspended sql.NullInt64
	var networks string
	err := row.Scan(&k.ID, &k.Hash, &k.Holder, &created, &expires, &lastUsed, &revoked,
		&networks, &k.Rules.ClientCert, &k.LastAddress, &k.Successor, &suspended, &k.Masked)
	k.Created = fromMicros(created)
	k.Expires = setMicros(expires)
	k.LastUsed = fromNullMicros(lastUsed)
	k.Revoked = setMicros(revoked)
	k.Suspended = setMicros(suspended)
	if err == nil {
		k.Rules.Networks, err = sqlkit.ReadNetworks(networks)
	}
	return k, err
}

// Key implements master.Store: one read of the primary key's index.
func (m *Master) Key(ctx context.Context, id string) (master.Key, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, err
	}
	s, err := m.ready(ctx)
	if err != nil {
		return master.Key{}, err
	}
	k, err := scanKey(s.keyByID.QueryRowContext(ctx, id))
	return k, m.notFound(ctx, err, "no such key", "the key cannot be read")
}

// RotatedFrom implements master.Store: one read of the successor's index.
func (m *Master) RotatedFrom(ctx context.Context, id string) (master.Key, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, err
	}
	s, err := m.ready(ctx)
	if err != nil {
		return master.Key{}, err
	}
	k, err := scanKey(s.keyBySuccessor.QueryRowContext(ctx, id))
	return k, m.notFound(ctx, err, "no key was rotated to this one", "the key cannot be read")
}

// ListKeys implements master.Store.
func (m *Master) ListKeys(ctx context.Context, page master.Page) ([]master.Key, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	// Creation times are never before 1970, so (-1, "") is before every key.
	afterCreated, afterID := int64(-1), ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listKeys, "", 2)
		if err != nil {
			return nil, "", err
		}
		if afterCreated, err = sqlkit.DecodeCursorInt(f[0]); err != nil {
			return nil, "", err
		}
		afterID = f[1]
	}
	s, err := m.ready(ctx)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.listKeys.QueryContext(ctx, afterCreated, afterID, page.Limit+1)
	out, err := collect(rows, err, scanKey)
	if err != nil {
		return nil, "", m.fail(ctx, err, "the keys cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listKeys, "", strconv.FormatInt(micros(last.Created), 10), last.ID), nil
}

// MarkKeyUsed implements master.Store with one UPDATE whose WHERE holds the
// throttle, so a call within the minute changes no row and writes nothing
// to the file: no read first, and no commit to sync. The same condition
// keeps the stored time from moving backward: an older at matches no row.
func (m *Master) MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error) {
	if err := master.ValidateMark(id, at, address); err != nil {
		return false, err
	}
	s, err := m.ready(ctx)
	if err != nil {
		return false, err
	}
	res, err := s.markKeyUsed.ExecContext(ctx, micros(at), address, id, micros(at.Add(-time.Minute)))
	if err != nil {
		return false, m.fail(ctx, err, "the key's use cannot be recorded")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, m.fail(ctx, err, "the key's use cannot be recorded")
	}
	return n == 1, nil
}

// RevokeKey implements master.Store. The revoked time is read and, when it is
// unset, written in one write transaction, so the audit entry says whether
// this call revoked the key or found it revoked.
func (m *Master) RevokeKey(ctx context.Context, id string, at time.Time, audit master.AuditEntry, by master.Actor) (time.Time, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return time.Time{}, err
	}
	if err := master.ValidateTime("revocation time", at); err != nil {
		return time.Time{}, err
	}
	if err := audit.ValidateWith(master.DetailAlreadyRevoked); err != nil {
		return time.Time{}, err
	}
	if err := by.Validate(); err != nil {
		return time.Time{}, err
	}
	var revoked time.Time
	err := m.write(ctx, "the key cannot be revoked", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		var before sql.NullInt64
		err := tx.StmtContext(ctx, s.keyRevoked).QueryRowContext(ctx, id).Scan(&before)
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New(fault.NotFound, "no such key")
		}
		if err != nil {
			return err
		}
		if before.Valid {
			revoked = fromMicros(before.Int64)
			return insertAudit(ctx, tx, s, audit.WithRevocation(true))
		}
		if _, err := tx.StmtContext(ctx, s.revokeKey).ExecContext(ctx, micros(at), id); err != nil {
			return err
		}
		revoked = fromMicros(micros(at))
		return insertAudit(ctx, tx, s, audit.WithRevocation(false))
	})
	if err != nil {
		return time.Time{}, err
	}
	return revoked, nil
}

// SetKeyRules implements master.Store. The key's revoked time is read and
// its rules written in one write transaction, so no rule is set on a key
// after its revocation.
func (m *Master) SetKeyRules(ctx context.Context, id string, rules master.KeyRules, audit master.AuditEntry, by master.Actor) error {
	if err := master.ValidateID("key id", id); err != nil {
		return err
	}
	if err := rules.Validate(); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	if err := by.Validate(); err != nil {
		return err
	}
	return m.write(ctx, "the key's rules cannot be set", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		var revoked sql.NullInt64
		err := tx.StmtContext(ctx, s.keyRevoked).QueryRowContext(ctx, id).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New(fault.NotFound, "no such key")
		}
		if err != nil {
			return err
		}
		if revoked.Valid {
			return fault.New(fault.Conflict, "the key is revoked")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET networks = ?, client_cert = ? WHERE id = ?`,
			rules.Networks.String(), rules.ClientCert, id); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit)
	})
}

// RotateKey implements master.Store. The rotated key is read, its successor
// added with its holder and rules and a copy of its grants, and its end and
// successor written, in one write transaction, which holds the database's
// write lock from its start: no revocation, grant change or second rotation
// falls between the read and the writes.
func (m *Master) RotateKey(ctx context.Context, id string, successor master.Key, oldEnds time.Time, audit master.AuditEntry, by master.Actor) (master.Key, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, err
	}
	if err := master.ValidateRotation(successor, oldEnds, audit); err != nil {
		return master.Key{}, err
	}
	if err := by.Validate(); err != nil {
		return master.Key{}, err
	}
	var made master.Key
	err := m.write(ctx, "the key cannot be rotated", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		old, err := scanKey(tx.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New(fault.NotFound, "no such key")
		}
		if err != nil {
			return err
		}
		if err := master.RotationConflict(old, successor.Created); err != nil {
			return err
		}
		next := master.Successor(old, successor)
		_, err = tx.StmtContext(ctx, s.insertKey).ExecContext(ctx,
			next.ID, next.Hash, next.Holder, micros(next.Created), nullMicros(next.Expires), next.Rules.Networks.String(), next.Rules.ClientCert, boolInt(next.Masked))
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, err, "a key with this id exists")
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO grants (key_id, scope, tenant_id, role)
			SELECT ?, scope, tenant_id, role FROM grants WHERE key_id = ?`, next.ID, id); err != nil {
			return err
		}
		ends := fromMicros(micros(master.RotatedExpiry(old.Expires, oldEnds)))
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET expires = ?, successor = ? WHERE id = ?`, micros(ends), next.ID, id); err != nil {
			return err
		}
		next.Created = fromMicros(micros(next.Created))
		if !next.Expires.IsZero() {
			next.Expires = fromMicros(micros(next.Expires))
		}
		made = next
		return insertAudit(ctx, tx, s, audit.WithDetails(master.DetailSuccessor, next.ID, master.DetailOldExpires, ends.Format(time.RFC3339Nano)))
	})
	if err != nil {
		return master.Key{}, err
	}
	made.Hash = nil
	return made, nil
}
