package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// The snapshot of a Postgres database (package snapshot), read and loaded
// by package sqlkit's one implementation, with the information schema of
// the store's own schema (current_schema()) to find what to read.

// snapshotDialect is how a snapshot reads and writes the database of b. A
// snapshot reads in one REPEATABLE READ, read-only transaction, which sees
// one moment of the database throughout (postgresql.org/docs/current/
// transaction-iso.html).
func snapshotDialect(b *base) sqlkit.SnapshotDialect {
	return sqlkit.SnapshotDialect{
		Tables: `SELECT table_name::text FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
			ORDER BY table_name`,
		Columns: `SELECT column_name::text, data_type::text FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`,
		References: `SELECT c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_constraint AS c
			WHERE c.contype = 'f' AND c.connamespace = current_schema()::regnamespace`,
		Kind: func(declared string) (snapshot.Kind, bool) {
			switch declared {
			case "bigint", "integer", "smallint":
				return snapshot.KindInt, true
			case "text", "character varying", "character", "json", "jsonb":
				return snapshot.KindText, true
			case "bytea":
				return snapshot.KindBytes, true
			case "boolean":
				return snapshot.KindBool, true
			case "timestamp with time zone":
				return snapshot.KindTime, true
			case "double precision", "real":
				return snapshot.KindFloat, true
			}
			return "", false
		},
		Select: func(column, declared string) string {
			q := `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
			if declared == "json" || declared == "jsonb" {
				// The document's text, which loads back as the same document.
				return q + "::text"
			}
			return q
		},
		Param: func(i int, declared string) string {
			p := "$" + strconv.Itoa(i)
			if declared == "json" || declared == "jsonb" {
				return p + "::" + declared
			}
			return p
		},
		// The ids of an identity column always generated are written as
		// the snapshot holds them (postgresql.org/docs/current/sql-insert.html).
		Overriding: " OVERRIDING SYSTEM VALUE",
		// The tenant's identity is written by Migrate, for the tenant id the
		// store is opened with, and checked by every open.
		Skip: []string{"tenant_identity"},
		BeginRead: func(ctx context.Context) (*sql.Tx, error) {
			return b.mig.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		},
		BeginWrite: func(ctx context.Context) (*sql.Tx, error) {
			return b.mig.BeginTx(ctx, nil)
		},
		ReadSequences:  readSequences,
		RaiseSequences: raiseSequences,
	}
}

// identityColumns returns, by table, the identity column of each table of
// the schema that has one.
func identityColumns(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT table_name::text, column_name::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND is_identity = 'YES'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		out[t] = c
	}
	return out, rows.Err()
}

// readSequences reads the last value each identity column's sequence gave,
// 0 for one that never gave any.
func readSequences(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	cols, err := identityColumns(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for t, c := range cols {
		var v int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(pg_sequence_last_value(pg_get_serial_sequence($1, $2)::regclass), 0)`,
			t, c).Scan(&v); err != nil {
			return nil, err
		}
		out[t] = v
	}
	return out, nil
}

// raiseSequences sets each identity column's sequence past both the value
// seqs holds for its table and every id the table holds: a row written
// with its own id does not move the sequence.
func raiseSequences(ctx context.Context, tx *sql.Tx, seqs map[string]int64) error {
	cols, err := identityColumns(ctx, tx)
	if err != nil {
		return err
	}
	for t, c := range cols {
		var held int64
		q := `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(`+q+`), 0) FROM "`+strings.ReplaceAll(t, `"`, `""`)+`"`).Scan(&held); err != nil {
			return err
		}
		v := max(held, seqs[t])
		if v < 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence($1, $2), $3, true)`, t, c, v); err != nil {
			return err
		}
	}
	return nil
}

// migrateTo applies the first version migrations of all, refusing a
// database already past them.
func (b *base) migrateTo(ctx context.Context, all []sqlkit.Migration, version int) error {
	if version < 1 || version > len(all) {
		return fault.Wrap(fault.Conflict, sqlkit.ErrNewerSchema, "the snapshot is of schema version "+strconv.Itoa(version)+
			", and this binary knows versions 1 to "+strconv.Itoa(len(all)))
	}
	got, err := b.version(ctx)
	if err != nil {
		return err
	}
	if got > version {
		return fault.New(fault.Conflict, "the database is at schema version "+strconv.Itoa(got)+", past the snapshot's "+strconv.Itoa(version))
	}
	_, err = b.migrate(ctx, all[:version])
	return err
}

var (
	_ snapshot.Database = (*Master)(nil)
	_ snapshot.Database = (*Tenant)(nil)
)

// Snapshot implements snapshot.Database.
func (m *Master) Snapshot(ctx context.Context, sink snapshot.Sink) (snapshot.Point, error) {
	if err := m.open(); err != nil {
		return snapshot.Point{}, err
	}
	p, err := sqlkit.Snapshot(ctx, snapshotDialect(&m.base), false, sink)
	return p, m.runnerErr(ctx, err, "the master database cannot be read")
}

// SchemaVersions returns the newest schema version of the master and of a
// tenant this binary migrates to: what a restore checks every section of a
// backup against before it writes anything.
func SchemaVersions() (masterVersion, tenantVersion int, err error) {
	if masterMigrationsErr != nil {
		return 0, 0, fault.Wrap(fault.Internal, masterMigrationsErr, "the master's migrations cannot be read")
	}
	if tenantMigrationsErr != nil {
		return 0, 0, fault.Wrap(fault.Internal, tenantMigrationsErr, "a tenant's migrations cannot be read")
	}
	return len(masterMigrations), len(tenantMigrations), nil
}

// MigrateTo implements snapshot.Database.
func (m *Master) MigrateTo(ctx context.Context, version int) error {
	if masterMigrationsErr != nil {
		return fault.Wrap(fault.Internal, masterMigrationsErr, "the master's migrations cannot be read")
	}
	return m.migrateTo(ctx, masterMigrations, version)
}

// Load implements snapshot.Database.
func (m *Master) Load(ctx context.Context, version int, src snapshot.Source) error {
	if err := m.open(); err != nil {
		return err
	}
	return m.runnerErr(ctx, sqlkit.LoadSnapshot(ctx, snapshotDialect(&m.base), version, src), "the master database cannot be loaded")
}

// Snapshot implements snapshot.Database.
func (t *Tenant) Snapshot(ctx context.Context, sink snapshot.Sink) (snapshot.Point, error) {
	if err := t.open(); err != nil {
		return snapshot.Point{}, err
	}
	p, err := sqlkit.Snapshot(ctx, snapshotDialect(&t.base), true, sink)
	return p, t.runnerErr(ctx, err, "the tenant database cannot be read")
}

// MigrateTo implements snapshot.Database: the schema to version, then the
// tenant's identity, as Migrate writes it.
func (t *Tenant) MigrateTo(ctx context.Context, version int) error {
	if tenantMigrationsErr != nil {
		return fault.Wrap(fault.Internal, tenantMigrationsErr, "a tenant's migrations cannot be read")
	}
	if err := t.migrateTo(ctx, tenantMigrations, version); err != nil {
		return err
	}
	return t.claim(ctx)
}

// Load implements snapshot.Database.
func (t *Tenant) Load(ctx context.Context, version int, src snapshot.Source) error {
	if err := t.open(); err != nil {
		return err
	}
	return t.runnerErr(ctx, sqlkit.LoadSnapshot(ctx, snapshotDialect(&t.base), version, src), "the tenant database cannot be loaded")
}
