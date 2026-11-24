package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Every change here takes the tenant's configuration lock first (lockConfig),
// so the checks across rows it makes see no other change of configuration in
// flight. A storage's row is then locked by the change that changes or
// removes it, before what holds it is read (storageInUse).

// --- Storages ---

// storageColumns are a storage's columns as a read returns them: the secret
// only as whether one is set, so no read can carry it out.
const storageColumns = `name, type, dir_root, dir_path, s3_endpoint, s3_region, s3_bucket, s3_prefix,
	s3_path_style, s3_access_key_id, s3_secret IS NOT NULL, capacity_bytes, created, updated`

// scanStorage reads one row of storageColumns.
func scanStorage(row scanner) (tenant.Storage, error) {
	var s tenant.Storage
	var typ string
	var created, updated time.Time
	err := row.Scan(&s.Name, &typ, &s.Directory.Root, &s.Directory.Path, &s.S3.Endpoint, &s.S3.Region, &s.S3.Bucket,
		&s.S3.Prefix, &s.S3.PathStyle, &s.S3.AccessKeyID, &s.S3.SecretSet, &s.CapacityBytes, &created, &updated)
	s.Type = tenant.StorageType(typ)
	s.Created, s.Updated = created.UTC(), updated.UTC()
	return s, err
}

// storageInTx reads a storage inside a change's transaction, with the lock
// given ("" for none, or a locking clause such as "FOR UPDATE"), or
// NotFound.
func storageInTx(ctx context.Context, tx pgx.Tx, name, lock string) (tenant.Storage, error) {
	s, err := scanStorage(tx.QueryRow(ctx, `SELECT `+storageColumns+` FROM storages WHERE name = $1 `+lock, name))
	if isNoRow(err) {
		return s, fault.New(fault.NotFound, "the tenant has no storage named "+name)
	}
	return s, err
}

// checkStorageOverlap refuses s when another storage of the tenant would
// share its files (tenant.StoragesOverlap). It reads only the storages of s's
// root or of its endpoint and bucket, through their partial indexes.
func checkStorageOverlap(ctx context.Context, tx pgx.Tx, s tenant.Storage) error {
	var rows pgx.Rows
	var err error
	if s.Type == tenant.StorageDirectory {
		rows, err = tx.Query(ctx, `SELECT `+storageColumns+` FROM storages
			WHERE type = 'directory' AND dir_root = $1 AND name <> $2`, s.Directory.Root, s.Name)
	} else {
		rows, err = tx.Query(ctx, `SELECT `+storageColumns+` FROM storages
			WHERE type = 's3' AND s3_endpoint = $1 AND s3_bucket = $2 AND name <> $3`, s.S3.Endpoint, s.S3.Bucket, s.Name)
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

// secretArg is a storage's sealed secret for a statement: NULL for none.
func secretArg(s tenant.Storage) any {
	if len(s.S3.SealedSecret) > 0 {
		return s.S3.SealedSecret
	}
	return nil
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
	err = t.pipe(ctx, "the storage cannot be added", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = $1`, s.Name)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "the tenant has a storage named "+s.Name)
		}
		if err := checkStorageOverlap(ctx, tx, s); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO storages (name, type, dir_root, dir_path, s3_endpoint, s3_region, s3_bucket,
			s3_prefix, s3_path_style, s3_access_key_id, s3_secret, capacity_bytes, created, updated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)`,
			s.Name, string(s.Type), s.Directory.Root, s.Directory.Path, s.S3.Endpoint, s.S3.Region, s.S3.Bucket,
			s.S3.Prefix, s.S3.PathStyle, s.S3.AccessKeyID, secretArg(s), s.CapacityBytes, micros(now))
		if constraintViolation(err, "storages_pkey") {
			return fault.Wrap(fault.Conflict, t.red.scrub(err), "the tenant has a storage named "+s.Name)
		}
		if err != nil {
			return err
		}
		if out, err = storageInTx(ctx, tx, s.Name, ""); err != nil {
			return err
		}
		return t.audited(ctx, tx, audit, guard)
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
		`SELECT `+storageColumns+` FROM storages WHERE name = $1`, []any{name}, scanStorage)
}

// StorageSecret implements tenant.StorageSecrets. The sealed bytes are
// read as stored and handed back as they are; nothing here opens or logs
// them.
func (t *Tenant) StorageSecret(ctx context.Context, name string) ([]byte, error) {
	if err := tenant.ValidateName("storage", name); err != nil {
		return nil, err
	}
	return readOne(ctx, t, "the tenant has no S3 storage named "+name, "the storage's secret cannot be read",
		`SELECT s3_secret FROM storages WHERE name = $1 AND type = 's3' AND s3_secret IS NOT NULL`, []any{name},
		func(row scanner) ([]byte, error) {
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
	out, err := readRows(ctx, t, "the storages cannot be listed", listStoragesSQL, []any{after, page.Limit + 1}, scanStorage)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listStorages, t.binding(), last.Name), nil
}

// SetStorage implements tenant.Pipeline. The storage's row is locked FOR NO
// KEY UPDATE when it is read, before anything is checked: a commit or an
// identify holds it FOR SHARE, so a change of where its files are waits for
// those in flight, and those that come later wait for it and then see it.
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
	err := t.pipe(ctx, "the storage cannot be changed", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		stored, err := storageInTx(ctx, tx, s.Name, "FOR NO KEY UPDATE")
		if err != nil {
			return err
		}
		next, location, err := tenant.ApplyStorage(stored, s, fields)
		if err != nil {
			return err
		}
		if location && (next.Directory != stored.Directory || next.S3.Endpoint != stored.S3.Endpoint ||
			next.S3.Bucket != stored.S3.Bucket || next.S3.Prefix != stored.S3.Prefix) {
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
		// The secret is written only when the change names it (coalesce
		// keeps the stored one otherwise).
		if _, err := tx.Exec(ctx, `UPDATE storages SET dir_root = $1, dir_path = $2, s3_endpoint = $3, s3_region = $4,
			s3_bucket = $5, s3_prefix = $6, s3_path_style = $7, s3_access_key_id = $8, s3_secret = coalesce($9, s3_secret),
			capacity_bytes = $10, updated = $11 WHERE name = $12`,
			next.Directory.Root, next.Directory.Path, next.S3.Endpoint, next.S3.Region, next.S3.Bucket, next.S3.Prefix,
			next.S3.PathStyle, next.S3.AccessKeyID, secretArg(next), next.CapacityBytes, micros(now), s.Name); err != nil {
			return err
		}
		if out, err = storageInTx(ctx, tx, s.Name, ""); err != nil {
			return err
		}
		return t.audited(ctx, tx, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, err
	}
	return out, nil
}

// storageInUse says why a storage's files may not move or its configuration
// go, or "" when nothing holds it: it holds an instance, a session names it,
// or a reception that has not ended names it. The caller has locked the
// storage's row, so each read here, a statement of its own under read
// committed, sees every commit and identify that held the row before.
func storageInUse(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	holds, err := inUse(ctx, tx, storageHoldsSQL, name)
	if err != nil || holds {
		return "it holds archived files", err
	}
	named, err := inUse(ctx, tx, sessionsOnStorageSQL, name)
	if err != nil || named {
		return "a session's files are on it, where every arrival of the session is archived", err
	}
	used, err := inUse(ctx, tx, storageUsedSQL, name)
	if err != nil || used {
		return "a reception in progress archives to it", err
	}
	return "", nil
}

// RemoveStorage implements tenant.Pipeline. The row is locked FOR UPDATE,
// the lock its DELETE takes, before anything is checked (SetStorage says
// why).
func (t *Tenant) RemoveStorage(ctx context.Context, name string, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	if err := tenant.ValidateName("storage", name); err != nil {
		return tenant.Storage{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Storage{}, err
	}
	var out tenant.Storage
	err := t.pipe(ctx, "the storage cannot be removed", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		stored, err := storageInTx(ctx, tx, name, "FOR UPDATE")
		if err != nil {
			return err
		}
		var receiver string
		err = tx.QueryRow(ctx, receiverOfStorageSQL, pgx.QueryExecModeExec, name).Scan(&receiver)
		if err == nil {
			return tenant.Precondition("the receiver " + receiver + " archives to this storage")
		}
		if !isNoRow(err) {
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
		if _, err := tx.Exec(ctx, `DELETE FROM storages WHERE name = $1`, name); err != nil {
			return err
		}
		out = stored
		return t.audited(ctx, tx, audit, guard)
	})
	if err != nil {
		return tenant.Storage{}, err
	}
	return out, nil
}

// lockStorageShared holds a storage's row FOR SHARE to the end of the
// transaction, for a commit or an identify that names it: its removal (FOR
// UPDATE) and a change of where its files are (FOR NO KEY UPDATE) wait for
// it, or it waits for them and then sees what they did. A storage not there
// is a precondition failure.
func lockStorageShared(ctx context.Context, tx pgx.Tx, name string) error {
	found, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = $1 FOR SHARE`, name)
	if err != nil {
		return err
	}
	if !found {
		return tenant.Precondition("the tenant has no storage named " + name)
	}
	return nil
}

// --- Receivers ---

const receiverColumns = `name, type, enabled, storage, port, bind_address, config, naming, quiet_us, max_open_us, max_file_bytes, created, updated`

// receiverArgs are a receiver's values in the order of receiverColumns.
func receiverArgs(r tenant.Receiver) ([]any, error) {
	config, namingText, err := sqlkit.ReceiverConfigText(r)
	if err != nil {
		return nil, err
	}
	return []any{r.Name, string(r.Type), r.Enabled, r.Storage, r.DIMSE.Port, r.DIMSE.BindAddress, config, namingText,
		r.QuietPeriod.Microseconds(), r.MaxOpenTime.Microseconds(), r.MaxFileBytes, micros(r.Created), micros(r.Updated)}, nil
}

// scanReceiver reads one row of receiverColumns.
func scanReceiver(row scanner) (tenant.Receiver, error) {
	var r tenant.Receiver
	var typ, config, namingText string
	var quiet, maxOpen int64
	var created, updated time.Time
	if err := row.Scan(&r.Name, &typ, &r.Enabled, &r.Storage, &r.DIMSE.Port, &r.DIMSE.BindAddress, &config, &namingText,
		&quiet, &maxOpen, &r.MaxFileBytes, &created, &updated); err != nil {
		return r, err
	}
	r.Type = tenant.ReceiverType(typ)
	r.QuietPeriod, r.MaxOpenTime = time.Duration(quiet)*time.Microsecond, time.Duration(maxOpen)*time.Microsecond
	r.Created, r.Updated = created.UTC(), updated.UTC()
	return r, sqlkit.ReadReceiverConfig(&r, config, namingText)
}

// receiverInTx reads a receiver inside a change's transaction, with the lock
// given, or NotFound.
func receiverInTx(ctx context.Context, tx pgx.Tx, name, lock string) (tenant.Receiver, error) {
	r, err := scanReceiver(tx.QueryRow(ctx, `SELECT `+receiverColumns+` FROM receivers WHERE name = $1 `+lock, name))
	if isNoRow(err) {
		return r, fault.New(fault.NotFound, "the tenant has no receiver named "+name)
	}
	return r, err
}

// receiverExists reports whether the tenant has the receiver. A plain read
// is enough for a change that writes the receiver's name into a new record:
// were the receiver removed and a receiver of the same name added while
// that change is in flight, the rows it leaves are those of the change made
// after the receiver was added, at the new receiver, which is a valid order,
// so no lock on the receiver's row would change what any reader can see.
func receiverExists(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	return exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = $1`, name)
}

// checkReceiver refuses r when its storage is not the tenant's or another
// receiver clashes with it (tenant.ReceiversClash): a DIMSE receiver on its
// port and address, or a stow receiver, all of which are on port 0, that
// lists one of its keys.
func checkReceiver(ctx context.Context, tx pgx.Tx, r tenant.Receiver) error {
	has, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = $1`, r.Storage)
	if err != nil {
		return err
	}
	if !has {
		return tenant.Precondition("the tenant has no storage named " + r.Storage)
	}
	for _, s := range r.Routing.Storages() {
		has, err := exists(ctx, tx, `SELECT 1 FROM storages WHERE name = $1`, s)
		if err != nil {
			return err
		}
		if !has {
			return tenant.Precondition("receiver.routing: the tenant has no storage named " + s)
		}
	}
	if err := checkForwardTo(ctx, tx, r.ForwardTo); err != nil {
		return err
	}
	if err := checkForwardTo(ctx, tx, r.Routing.Senders()); err != nil {
		return fault.Wrap(fault.KindOf(err), err, "receiver.routing: "+fault.Message(err))
	}
	rows, err := tx.Query(ctx, receiversOnPortSQL, r.DIMSE.Port, r.Name)
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
	err = t.pipe(ctx, "the receiver cannot be added", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = $1`, r.Name)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "the tenant has a receiver named "+r.Name)
		}
		held, err := inUse(ctx, tx, receptionNamesReceiverSQL, r.Name)
		if err == nil && !held {
			held, err = inUse(ctx, tx, quarantineNamesReceiverSQL, r.Name)
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
		if held, err = inUse(ctx, tx, importJobNamesReceiverSQL, r.Name); err != nil {
			return err
		}
		if held {
			return fault.New(fault.Conflict, "the name "+r.Name+" is still named by an import job of a receiver removed")
		}
		if err := checkReceiver(ctx, tx, r); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO receivers (`+receiverColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, args...)
		if constraintViolation(err, "receivers_pkey") {
			return fault.Wrap(fault.Conflict, t.red.scrub(err), "the tenant has a receiver named "+r.Name)
		}
		if err != nil {
			return err
		}
		if out, err = receiverInTx(ctx, tx, r.Name, ""); err != nil {
			return err
		}
		return t.audited(ctx, tx, audit, guard)
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
		`SELECT `+receiverColumns+` FROM receivers WHERE name = $1`, []any{name}, scanReceiver)
}

// ListReceivers implements tenant.Pipeline.
func (t *Tenant) ListReceivers(ctx context.Context, page master.Page) ([]tenant.Receiver, string, error) {
	after, err := t.pageAfterName(page, listReceivers)
	if err != nil {
		return nil, "", err
	}
	out, err := readRows(ctx, t, "the receivers cannot be listed", listReceiversSQL, []any{after, page.Limit + 1}, scanReceiver)
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
	err := t.pipe(ctx, "the receiver cannot be changed", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		stored, err := receiverInTx(ctx, tx, r.Name, "FOR NO KEY UPDATE")
		if err != nil {
			return err
		}
		next, err := tenant.ApplyReceiver(stored, r, fields)
		if err != nil {
			return err
		}
		next.Updated = now
		if err := checkReceiver(ctx, tx, next); err != nil {
			return err
		}
		args, err := receiverArgs(next)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE receivers SET enabled = $1, storage = $2, port = $3, bind_address = $4, config = $5,
			naming = $6, quiet_us = $7, max_open_us = $8, max_file_bytes = $9, updated = $10 WHERE name = $11`,
			args[2], args[3], args[4], args[5], args[6], args[7], args[8], args[9], args[10], args[12], r.Name); err != nil {
			return err
		}
		if out, err = receiverInTx(ctx, tx, r.Name, ""); err != nil {
			return err
		}
		return t.audited(ctx, tx, audit, guard)
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
	err := t.pipe(ctx, "the receiver cannot be removed", func(tx pgx.Tx) error {
		if err := lockConfig(ctx, tx); err != nil {
			return err
		}
		stored, err := receiverInTx(ctx, tx, name, "FOR UPDATE")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM receivers WHERE name = $1`, name); err != nil {
			return err
		}
		out = stored
		return t.audited(ctx, tx, audit, guard)
	})
	if err != nil {
		return tenant.Receiver{}, err
	}
	return out, nil
}
