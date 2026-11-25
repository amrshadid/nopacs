package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// --- Storages ---

// storageColumns are a storage's columns as a read returns them, package
// sqlkit's: the secret only as whether one is set, so no read can carry it
// out.
const storageColumns = sqlkit.StorageColumns

// scanStorage reads one row of storageColumns.
func scanStorage(row interface{ Scan(...any) error }) (tenant.Storage, error) {
	return sqlkit.ScanStorage(row)
}

// storageInTx reads a storage inside a change's transaction, or NotFound.
func storageInTx(ctx context.Context, tx *sql.Tx, name string) (tenant.Storage, error) {
	s, err := scanStorage(tx.QueryRowContext(ctx, `SELECT `+storageColumns+` FROM storages WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return s, fault.New(fault.NotFound, "the tenant has no storage named "+name)
	}
	return s, err
}

// checkStorageOverlap refuses s when another storage of the tenant would
// share its files (tenant.StoragesOverlap). It reads only the storages of
// s's root or of its endpoint and bucket, through their partial indexes.
func checkStorageOverlap(ctx context.Context, tx *sql.Tx, s tenant.Storage) error {
	var rows *sql.Rows
	var err error
	if s.Type == tenant.StorageDirectory {
		rows, err = tx.QueryContext(ctx, `SELECT `+storageColumns+` FROM storages
			WHERE type = 'directory' AND dir_root = ? AND name <> ?`, s.Directory.Root, s.Name)
	} else {
		rows, err = tx.QueryContext(ctx, `SELECT `+storageColumns+` FROM storages
			WHERE type = 's3' AND s3_endpoint = ? AND s3_bucket = ? AND name <> ?`, s.S3.Endpoint, s.S3.Bucket, s.Name)
	}
	others, err := collect(rows, err, scanStorage)
	if err != nil {
		return err
	}
	for _, o := range others {
		if tenant.StoragesOverlap(s, o) {
			return tenant.Precondition("the storage would overlap the tenant's storage " + o.Name)
		}
	}
	return nil
}

// storageArgs are a storage's values for an INSERT of every column, in the
// table's order. A secret not given is kept by the caller of an UPDATE, so
// it is passed here as it is: NULL for none.
func storageArgs(s tenant.Storage, created, updated time.Time) []any {
	var secret any
	if len(s.S3.SealedSecret) > 0 {
		secret = s.S3.SealedSecret
	}
	return []any{s.Name, string(s.Type), s.Directory.Root, s.Directory.Path, s.S3.Endpoint, s.S3.Region, s.S3.Bucket,
		s.S3.Prefix, boolInt(s.S3.PathStyle), s.S3.AccessKeyID, secret, boolInt(s.S3.KeepVersions), s.CapacityBytes, micros(created), micros(updated),
		s.ReplicaOf, string(s.Tier), s.S3.RecallDays, string(s.S3.RecallSpeed), boolInt(s.EncryptedAtRestDeclared)}
}

// AddStorage implements tenant.Pipeline.
func (t *Tenant) AddStorage(ctx context.Context, s tenant.Storage, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	s, err := s.Normalize()
	if err != nil {
		return tenant.Storage{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Storage{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, err
	}
	var out tenant.Storage
	err = t.write(ctx, "the storage cannot be added", func(tx *sql.Tx, st *tenantStatements) error {
		taken, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = ?`, s.Name)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "the tenant has a storage named "+s.Name)
		}
		if err := checkStorageOverlap(ctx, tx, s); err != nil {
			return err
		}
		if err := t.sending().CheckNewStorage(ctx, sendingTx{tx: tx}, s); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO storages (name, type, dir_root, dir_path, s3_endpoint, s3_region, s3_bucket,
			s3_prefix, s3_path_style, s3_access_key_id, s3_secret, s3_keep_versions, capacity_bytes, created, updated,
			replica_of, tier, s3_recall_days, s3_recall_tier, dir_encrypted)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, storageArgs(s, now, now)...)
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, err, "the tenant has a storage named "+s.Name)
		}
		if err != nil {
			return err
		}
		if err := t.sending().AfterNewStorage(ctx, sendingTx{tx: tx}, s, now); err != nil {
			return err
		}
		if out, err = storageInTx(ctx, tx, s.Name); err != nil {
			return err
		}
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, err
	}
	return out, nil
}

// Storage implements tenant.Pipeline.
func (t *Tenant) Storage(ctx context.Context, name string) (tenant.Storage, error) {
	if err := tenant.ValidateName("storage", name); err != nil {
		return tenant.Storage{}, err
	}
	return readOne(ctx, t, "the tenant has no storage named "+name, "the storage cannot be read",
		`SELECT `+storageColumns+` FROM storages WHERE name = ?`, []any{name}, scanStorage)
}

// StorageSecret implements tenant.StorageSecrets. The sealed bytes are
// read as stored and handed back as they are; nothing here opens or logs
// them.
func (t *Tenant) StorageSecret(ctx context.Context, name string) ([]byte, error) {
	if err := tenant.ValidateName("storage", name); err != nil {
		return nil, err
	}
	return readOne(ctx, t, "the tenant has no S3 storage named "+name, "the storage's secret cannot be read",
		`SELECT s3_secret FROM storages WHERE name = ? AND type = 's3' AND s3_secret IS NOT NULL`, []any{name},
		func(row interface{ Scan(...any) error }) ([]byte, error) {
			var b []byte
			err := row.Scan(&b)
			return b, err
		})
}

// ListStorages implements tenant.Pipeline.
func (t *Tenant) ListStorages(ctx context.Context, page master.Page) ([]tenant.Storage, string, error) {
	after, err := t.pageAfterName(page, listStorages)
	if err != nil {
		return nil, "", err
	}
	out, err := readRows(ctx, t, "the storages cannot be listed",
		listStoragesSQL, []any{after, page.Limit + 1}, scanStorage)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listStorages, t.binding(), last.Name), nil
}

// SetStorage implements tenant.Pipeline.
func (t *Tenant) SetStorage(ctx context.Context, s tenant.Storage, fields []tenant.StorageField, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	if err := tenant.ValidateName("storage", s.Name); err != nil {
		return tenant.Storage{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Storage{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, err
	}
	var out tenant.Storage
	err := t.write(ctx, "the storage cannot be changed", func(tx *sql.Tx, st *tenantStatements) error {
		stored, err := storageInTx(ctx, tx, s.Name)
		if err != nil {
			return err
		}
		next, location, err := tenant.ApplyStorage(stored, s, fields)
		if err != nil {
			return err
		}
		moved := location && (next.Directory != stored.Directory || next.S3.Endpoint != stored.S3.Endpoint ||
			next.S3.Bucket != stored.S3.Bucket || next.S3.Prefix != stored.S3.Prefix)
		if err := t.sending().CheckStorageChange(ctx, sendingTx{tx: tx}, stored, next, moved); err != nil {
			return err
		}
		if moved {
			if why, err := storageInUse(ctx, tx, s.Name); err != nil || why != "" {
				if err != nil {
					return err
				}
				return tenant.Precondition("where the storage's files are cannot change while " + why)
			}
		}
		if err := checkStorageOverlap(ctx, tx, next); err != nil {
			return err
		}
		// The secret is written only when the change names it
		// (coalesce keeps the stored one otherwise).
		args := storageArgs(next, stored.Created, now)
		_, err = tx.ExecContext(ctx, `UPDATE storages SET dir_root = ?, dir_path = ?, s3_endpoint = ?, s3_region = ?,
			s3_bucket = ?, s3_prefix = ?, s3_path_style = ?, s3_access_key_id = ?, s3_secret = coalesce(?, s3_secret),
			s3_keep_versions = ?, capacity_bytes = ?, updated = ?, tier = ?, s3_recall_days = ?, s3_recall_tier = ?,
			dir_encrypted = ? WHERE name = ?`,
			args[2], args[3], args[4], args[5], args[6], args[7], args[8], args[9], args[10], args[11], args[12], args[14],
			args[16], args[17], args[18], args[19], s.Name)
		if err != nil {
			return err
		}
		if out, err = storageInTx(ctx, tx, s.Name); err != nil {
			return err
		}
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, err
	}
	return out, nil
}

// storageInUse says why a storage's files may not move or its
// configuration go, or "" when nothing holds it: it holds an instance, a
// session names it, or a reception that has not ended names it (its files
// may be on their way to it). A reception that has not ended and is not yet identified names no
// storage; the receiver it arrived at does, and a storage a receiver names
// is refused by RemoveStorage before this.
func storageInUse(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	// The storage's row first, so a commit or an identify that names it is
	// ordered with this change (lockStorage), then what holds it.
	if err := lockStorage(ctx, tx, name); err != nil {
		return "", err
	}
	holds, err := exists(ctx, tx, storageHoldsSQL, name)
	if err != nil || holds {
		return "it holds archived files", err
	}
	named, err := exists(ctx, tx, sessionsOnStorageSQL, name)
	if err != nil || named {
		return "a session's files are on it, where every arrival of the session is archived", err
	}
	used, err := exists(ctx, tx, storageUsedSQL, name)
	if err != nil || used {
		return "a reception in progress archives to it", err
	}
	return "", nil
}

// RemoveStorage implements tenant.Pipeline.
func (t *Tenant) RemoveStorage(ctx context.Context, name string, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	if err := tenant.ValidateName("storage", name); err != nil {
		return tenant.Storage{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, err
	}
	var out tenant.Storage
	err := t.write(ctx, "the storage cannot be removed", func(tx *sql.Tx, st *tenantStatements) error {
		stored, err := storageInTx(ctx, tx, name)
		if err != nil {
			return err
		}
		if err := t.sending().CheckStorageRemoval(ctx, sendingTx{tx: tx}, stored); err != nil {
			return err
		}
		var receiver string
		err = tx.QueryRowContext(ctx, receiverOfStorageSQL, name).Scan(&receiver)
		if err == nil {
			return tenant.Precondition("the receiver " + receiver + " archives to this storage")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if receiver, err = sqlkit.ReceiverRoutingTo(ctx, sendingTx{tx: tx}, name); err != nil || receiver != "" {
			if err != nil {
				return err
			}
			return tenant.Precondition("a route of the receiver " + receiver + " archives to this storage")
		}
		if why, err := storageInUse(ctx, tx, name); err != nil || why != "" {
			if err != nil {
				return err
			}
			return tenant.Precondition("the storage cannot be removed while " + why)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM storages WHERE name = ?`, name); err != nil {
			return err
		}
		out = stored
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, err
	}
	return out, nil
}

// --- Receivers ---

const receiverColumns = `name, type, enabled, storage, port, bind_address, config, naming, quiet_us, max_open_us, max_file_bytes, created, updated`

// receiverArgs are a receiver's values in the order of receiverColumns.
func receiverArgs(r tenant.Receiver) ([]any, error) {
	config, namingText, err := sqlkit.ReceiverConfigText(r)
	if err != nil {
		return nil, err
	}
	return []any{r.Name, string(r.Type), boolInt(r.Enabled), r.Storage, r.DIMSE.Port, r.DIMSE.BindAddress, config, namingText,
		r.QuietPeriod.Microseconds(), r.MaxOpenTime.Microseconds(), r.MaxFileBytes, micros(r.Created), micros(r.Updated)}, nil
}

// scanReceiver reads one row of receiverColumns.
func scanReceiver(row interface{ Scan(...any) error }) (tenant.Receiver, error) {
	var r tenant.Receiver
	var typ, config, namingText string
	var quiet, maxOpen, created, updated int64
	if err := row.Scan(&r.Name, &typ, &r.Enabled, &r.Storage, &r.DIMSE.Port, &r.DIMSE.BindAddress, &config, &namingText,
		&quiet, &maxOpen, &r.MaxFileBytes, &created, &updated); err != nil {
		return r, err
	}
	r.Type = tenant.ReceiverType(typ)
	r.QuietPeriod, r.MaxOpenTime = time.Duration(quiet)*time.Microsecond, time.Duration(maxOpen)*time.Microsecond
	r.Created, r.Updated = fromMicros(created), fromMicros(updated)
	return r, sqlkit.ReadReceiverConfig(&r, config, namingText)
}

// receiverInTx reads a receiver inside a change's transaction, or NotFound.
func receiverInTx(ctx context.Context, tx *sql.Tx, name string) (tenant.Receiver, error) {
	r, err := scanReceiver(tx.QueryRowContext(ctx, `SELECT `+receiverColumns+` FROM receivers WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fault.New(fault.NotFound, "the tenant has no receiver named "+name)
	}
	return r, err
}

// checkReceiver refuses r when its storage is not the tenant's or another
// receiver clashes with it (tenant.ReceiversClash): a DIMSE receiver on its
// port and address, or a stow receiver, all of which are on port 0, that
// lists one of its keys.
func checkReceiver(ctx context.Context, tx *sql.Tx, r tenant.Receiver) error {
	// A storage named must be the tenant's, and neither a replica nor of the
	// archive tier: nothing is archived to one of those first.
	if err := sqlkit.CheckArchiveTarget(ctx, sendingTx{tx: tx}, "", r.Storage); err != nil {
		return err
	}
	for _, s := range r.Routing.Storages() {
		if err := sqlkit.CheckArchiveTarget(ctx, sendingTx{tx: tx}, "receiver.routing", s); err != nil {
			return err
		}
	}
	if err := checkForwardTo(ctx, tx, r.ForwardTo); err != nil {
		return err
	}
	if err := checkForwardTo(ctx, tx, r.Routing.Senders()); err != nil {
		return fault.Wrap(fault.KindOf(err), err, "receiver.routing: "+fault.Message(err))
	}
	rows, err := tx.QueryContext(ctx, receiversOnPortSQL, r.DIMSE.Port, r.Name)
	others, err := collect(rows, err, scanReceiver)
	if err != nil {
		return err
	}
	for _, o := range others {
		if tenant.ReceiversClash(r, o) {
			return tenant.ClashError(o)
		}
	}
	return nil
}

// AddReceiver implements tenant.Pipeline.
func (t *Tenant) AddReceiver(ctx context.Context, r tenant.Receiver, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Receiver, error) {
	if err := r.Validate(); err != nil {
		return tenant.Receiver{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Receiver{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Receiver{}, err
	}
	r.Created, r.Updated = now, now
	args, err := receiverArgs(r)
	if err != nil {
		return tenant.Receiver{}, err
	}
	var out tenant.Receiver
	err = t.write(ctx, "the receiver cannot be added", func(tx *sql.Tx, st *tenantStatements) error {
		taken, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = ?`, r.Name)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "the tenant has a receiver named "+r.Name)
		}
		held, err := exists(ctx, tx, receptionNamesReceiverSQL, r.Name)
		if err == nil && !held {
			held, err = exists(ctx, tx, quarantineNamesReceiverSQL, r.Name)
		}
		if err != nil {
			return err
		}
		if held {
			return fault.New(fault.Conflict, "the name "+r.Name+" is still named by receptions or quarantined files of a receiver removed")
		}
		// A job's receiver names one receiver for the job's whole life, so
		// the name of a removed import receiver stays held while a job,
		// ended or not, names it.
		if held, err = exists(ctx, tx, importJobNamesReceiverSQL, r.Name); err != nil {
			return err
		}
		if held {
			return fault.New(fault.Conflict, "the name "+r.Name+" is still named by an import job of a receiver removed")
		}
		if err := checkReceiver(ctx, tx, r); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO receivers (`+receiverColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, err, "the tenant has a receiver named "+r.Name)
		}
		if err != nil {
			return err
		}
		if out, err = receiverInTx(ctx, tx, r.Name); err != nil {
			return err
		}
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Receiver{}, err
	}
	return out, nil
}

// Receiver implements tenant.Pipeline.
func (t *Tenant) Receiver(ctx context.Context, name string) (tenant.Receiver, error) {
	if err := tenant.ValidateName("receiver", name); err != nil {
		return tenant.Receiver{}, err
	}
	return readOne(ctx, t, "the tenant has no receiver named "+name, "the receiver cannot be read",
		`SELECT `+receiverColumns+` FROM receivers WHERE name = ?`, []any{name}, scanReceiver)
}

// ListReceivers implements tenant.Pipeline.
func (t *Tenant) ListReceivers(ctx context.Context, page master.Page) ([]tenant.Receiver, string, error) {
	after, err := t.pageAfterName(page, listReceivers)
	if err != nil {
		return nil, "", err
	}
	out, err := readRows(ctx, t, "the receivers cannot be listed",
		listReceiversSQL, []any{after, page.Limit + 1}, scanReceiver)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listReceivers, t.binding(), last.Name), nil
}

// SetReceiver implements tenant.Pipeline.
func (t *Tenant) SetReceiver(ctx context.Context, r tenant.Receiver, fields []tenant.ReceiverField, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Receiver, error) {
	if err := tenant.ValidateName("receiver", r.Name); err != nil {
		return tenant.Receiver{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Receiver{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Receiver{}, err
	}
	var out tenant.Receiver
	err := t.write(ctx, "the receiver cannot be changed", func(tx *sql.Tx, st *tenantStatements) error {
		stored, err := receiverInTx(ctx, tx, r.Name)
		if err != nil {
			return err
		}
		next, err := tenant.ApplyReceiver(stored, r, fields)
		if err != nil {
			return err
		}
		if tenant.NamespaceChanges(stored, next) {
			var used bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM receptions WHERE receiver = ?)
				OR EXISTS (SELECT 1 FROM quarantine WHERE receiver = ?)`, r.Name, r.Name).Scan(&used); err != nil {
				return err
			}
			if used {
				return tenant.NamespaceFixed(r.Name)
			}
		}
		next.Updated = now
		if err := checkReceiver(ctx, tx, next); err != nil {
			return err
		}
		args, err := receiverArgs(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE receivers SET enabled = ?, storage = ?, port = ?, bind_address = ?, config = ?,
			naming = ?, quiet_us = ?, max_open_us = ?, max_file_bytes = ?, updated = ? WHERE name = ?`,
			args[2], args[3], args[4], args[5], args[6], args[7], args[8], args[9], args[10], args[12], r.Name)
		if err != nil {
			return err
		}
		if out, err = receiverInTx(ctx, tx, r.Name); err != nil {
			return err
		}
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Receiver{}, err
	}
	return out, nil
}

// RemoveReceiver implements tenant.Pipeline.
func (t *Tenant) RemoveReceiver(ctx context.Context, name string, audit master.AuditEntry, guard tenant.Guard) (tenant.Receiver, error) {
	if err := tenant.ValidateName("receiver", name); err != nil {
		return tenant.Receiver{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Receiver{}, err
	}
	var out tenant.Receiver
	err := t.write(ctx, "the receiver cannot be removed", func(tx *sql.Tx, st *tenantStatements) error {
		stored, err := receiverInTx(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM receivers WHERE name = ?`, name); err != nil {
			return err
		}
		out = stored
		return audited(ctx, tx, st, audit, guard)
	})
	if err != nil {
		return tenant.Receiver{}, err
	}
	return out, nil
}
