package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

const keyColumns = `id, hash, holder, created, expires, last_used, revoked, networks, client_cert, last_address, successor, suspended`

// scanKey reads one row of keyColumns.
func scanKey(row scanner) (master.Key, error) {
	var k master.Key
	var created time.Time
	var expires, lastUsed, revoked, suspended *time.Time
	var networks string
	err := row.Scan(&k.ID, &k.Hash, &k.Holder, &created, &expires, &lastUsed, &revoked,
		&networks, &k.Rules.ClientCert, &k.LastAddress, &k.Successor, &suspended)
	k.Created = created.UTC()
	k.Expires = setMicros(expires)
	k.LastUsed = fromNullMicros(lastUsed)
	k.Revoked = setMicros(revoked)
	k.Suspended = setMicros(suspended)
	if err == nil {
		k.Rules.Networks, err = sqlkit.ReadNetworks(networks)
	}
	return k, err
}

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
	return m.write(ctx, "the key cannot be created", func(tx pgx.Tx) error {
		if err := checkActor(ctx, tx, by, tenantLock{}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, hash, holder, created, expires, networks, client_cert) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			k.ID, k.Hash, k.Holder, micros(k.Created), nullMicros(k.Expires), k.Rules.Networks.String(), k.Rules.ClientCert)
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, m.red.scrub(err), "a key with this id exists")
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit)
	})
}

// Key implements master.Store: one read of the primary key's index.
func (m *Master) Key(ctx context.Context, id string) (master.Key, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, err
	}
	if err := m.open(); err != nil {
		return master.Key{}, err
	}
	k, err := scanKey(m.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = $1`, id))
	return k, m.notFound(ctx, err, "no such key", "the key cannot be read")
}

// RotatedFrom implements master.Store: one read of the successor's index,
// whose condition (a successor not empty) the statement repeats so the
// planner uses it.
func (m *Master) RotatedFrom(ctx context.Context, id string) (master.Key, error) {
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, err
	}
	if err := m.open(); err != nil {
		return master.Key{}, err
	}
	k, err := scanKey(m.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE successor = $1 AND successor <> ''`, id))
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
	if err := m.open(); err != nil {
		return nil, "", err
	}
	rows, err := m.pool.Query(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE (created, id) > ($1, $2) ORDER BY created, id LIMIT $3`,
		microsTime(afterCreated), afterID, page.Limit+1)
	out, err := collect(rows, err, scanKey)
	if err != nil {
		return nil, "", m.fail(ctx, err, "the keys cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listKeys, "", strconv.FormatInt(last.Created.UnixMicro(), 10), last.ID), nil
}

// MarkKeyUsed implements master.Store with one UPDATE whose WHERE holds the
// throttle, so a call within the minute matches no row, takes no row lock and
// writes nothing: no read first, and no commit to wait for. When two calls
// race on a row that does match, the second waits for the first's commit and
// then re-evaluates the WHERE against the row the first wrote (read
// committed), so it matches nothing: a busy key costs one write a minute. A
// use with no address (the local socket) keeps the address stored.
func (m *Master) MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error) {
	if err := master.ValidateMark(id, at, address); err != nil {
		return false, err
	}
	if err := m.open(); err != nil {
		return false, err
	}
	tag, err := m.pool.Exec(ctx, `UPDATE api_keys SET last_used = $1, last_address = COALESCE(NULLIF($2, ''), last_address) WHERE id = $3 AND (last_used IS NULL OR last_used < $4)`,
		micros(at), address, id, micros(at.Add(-time.Minute)))
	if err != nil {
		return false, m.fail(ctx, err, "the key's use cannot be recorded")
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeKey implements master.Store. The key's row is locked as its revoked
// time is read (the lock SetGrant takes too), so the audit entry says whether
// this call revoked the key or found it revoked, and a SetGrant racing this
// call is ordered with it: it either finishes before the revocation or finds
// the key revoked.
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
	err := m.write(ctx, "the key cannot be revoked", func(tx pgx.Tx) error {
		if err := lockKeys(ctx, tx, by, id); err != nil {
			return err
		}
		if err := checkActor(ctx, tx, by, tenantLock{}); err != nil {
			return err
		}
		var before *time.Time
		if err := tx.QueryRow(ctx, `SELECT revoked FROM api_keys WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&before); err != nil {
			return notFoundIn(err, "no such key")
		}
		if before != nil {
			revoked = before.UTC()
			return insertAudit(ctx, tx, m.red, audit.WithRevocation(true))
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked = $1 WHERE id = $2`, micros(at), id); err != nil {
			return err
		}
		revoked = micros(at)
		return insertAudit(ctx, tx, m.red, audit.WithRevocation(false))
	})
	if err != nil {
		return time.Time{}, err
	}
	return revoked, nil
}

// The row locks a change takes on a tenant's row, from the weakest. Each
// change takes the one it needs the first time its transaction touches the
// row, so no transaction holds a weaker lock on a row and then asks for a
// stronger one, which two transactions doing at once would deadlock on.
const (
	// lockKeyShare keeps the row and its key: what a grant on the tenant,
	// and the check of a role on it, need (RemoveTenant waits for it).
	lockKeyShare = "FOR KEY SHARE"
	// lockNoKeyUpdate is what a change of the row's state needs; it does not
	// wait for key share, so it does not wait for grants being set.
	lockNoKeyUpdate = "FOR NO KEY UPDATE"
	// lockUpdate is what removing the row needs.
	lockUpdate = "FOR UPDATE"
)

// lockKeys locks the acting key's row and the row of the key a change is
// about, FOR NO KEY UPDATE, in the order of their ids, before checkActor and
// the change take their own locks on them. Without it, a change by key A on
// key B (locking A for share, then B) and one by B on A (B, then A) could each
// hold what the other waits for; and two changes by A on A would each hold A
// for share and wait for the other to let go of it. Locked in one order, in
// one statement, the rows are taken one after the other by every such
// change. An actor that is not a key locks nothing here.
func lockKeys(ctx context.Context, tx pgx.Tx, by master.Actor, target string) error {
	if !by.IsKey() {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT id FROM api_keys WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, []string{by.KeyID(), target})
	return err
}

// SetKeyRules implements master.Store. The key's row is locked as its revoked
// time is read (the lock RevokeKey takes too), so no rule is set on a key
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
	return m.write(ctx, "the key's rules cannot be set", func(tx pgx.Tx) error {
		if err := lockKeys(ctx, tx, by, id); err != nil {
			return err
		}
		if err := checkActor(ctx, tx, by, tenantLock{}); err != nil {
			return err
		}
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT revoked FROM api_keys WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&revoked); err != nil {
			return notFoundIn(err, "no such key")
		}
		if revoked != nil {
			return fault.New(fault.Conflict, "the key is revoked")
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET networks = $1, client_cert = $2 WHERE id = $3`,
			rules.Networks.String(), rules.ClientCert, id); err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit)
	})
}

// RotateKey implements master.Store. The rotated key's row is locked as it
// is read (the lock RevokeKey and SetGrant take), so a revocation, a grant
// change or a second rotation is ordered before the rotation or after it.
// The tenants of its grants are locked for key share before the grants are
// copied, so a tenant being removed is gone, with its grants, before the
// copy reads them, and the copy never meets a tenant removed under it.
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
	err := m.write(ctx, "the key cannot be rotated", func(tx pgx.Tx) error {
		if err := lockKeys(ctx, tx, by, id); err != nil {
			return err
		}
		if err := checkActor(ctx, tx, by, tenantLock{}); err != nil {
			return err
		}
		old, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = $1 FOR NO KEY UPDATE`, id))
		if err != nil {
			return notFoundIn(err, "no such key")
		}
		if err := master.RotationConflict(old, successor.Created); err != nil {
			return err
		}
		next := successor
		next.Holder, next.Rules = old.Holder, old.Rules
		_, err = tx.Exec(ctx, `INSERT INTO api_keys (id, hash, holder, created, expires, networks, client_cert) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			next.ID, next.Hash, next.Holder, micros(next.Created), nullMicros(next.Expires), next.Rules.Networks.String(), next.Rules.ClientCert)
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, m.red.scrub(err), "a key with this id exists")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT t.id FROM tenants t JOIN grants g ON g.tenant_id = t.id
			WHERE g.key_id = $1 ORDER BY t.id FOR KEY SHARE OF t`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grants (key_id, scope, tenant_id, role)
			SELECT $1, scope, tenant_id, role FROM grants WHERE key_id = $2`, next.ID, id); err != nil {
			return err
		}
		ends := micros(master.RotatedExpiry(old.Expires, oldEnds))
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET expires = $1, successor = $2 WHERE id = $3`, ends, next.ID, id); err != nil {
			return err
		}
		next.Created = micros(next.Created)
		if !next.Expires.IsZero() {
			next.Expires = micros(next.Expires)
		}
		made = next
		return insertAudit(ctx, tx, m.red, audit.WithDetails(master.DetailSuccessor, next.ID, master.DetailOldExpires, ends.Format(time.RFC3339Nano)))
	})
	if err != nil {
		return master.Key{}, err
	}
	made.Hash = nil
	return made, nil
}
