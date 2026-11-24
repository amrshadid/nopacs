package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// The snapshot of a SQLite database (package snapshot), read and loaded by
// package sqlkit's one implementation, with SQLite's own catalog of tables
// (sqlite.org/schematab.html) and pragma functions
// (sqlite.org/pragma.html#pragfunc) to find what to read.

// snapshotDialect is how a snapshot reads and writes the database in p.
// Reads use the read pool, where a transaction reads one moment of the
// file once it has read once (write-ahead mode, sqlite.org/wal.html);
// the load uses the one write connection, whose transactions begin
// IMMEDIATE.
func snapshotDialect(p *pools) sqlkit.SnapshotDialect {
	return sqlkit.SnapshotDialect{
		Tables:     `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`,
		Columns:    `SELECT name, type FROM pragma_table_info(?) ORDER BY cid`,
		References: `SELECT m.name, f."table" FROM sqlite_schema AS m, pragma_foreign_key_list(m.name) AS f WHERE m.type = 'table'`,
		Kind: func(declared string) (snapshot.Kind, bool) {
			// Every table is STRICT, so each column is one of these four
			// (sqlite.org/stricttables.html).
			switch strings.ToUpper(declared) {
			case "INTEGER":
				return snapshot.KindInt, true
			case "TEXT":
				return snapshot.KindText, true
			case "BLOB":
				return snapshot.KindBytes, true
			case "REAL":
				return snapshot.KindFloat, true
			}
			return "", false
		},
		Select: func(column, _ string) string { return `"` + strings.ReplaceAll(column, `"`, `""`) + `"` },
		Param:  func(int, string) string { return "?" },
		BeginRead: func(ctx context.Context) (*sql.Tx, error) {
			return p.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		},
		BeginWrite: func(ctx context.Context) (*sql.Tx, error) {
			return p.write.BeginTx(ctx, nil)
		},
		ReadSequences:  readSequences,
		RaiseSequences: raiseSequences,
	}
}

// hasSequences reports whether the database has sqlite_sequence, which
// SQLite makes with the first AUTOINCREMENT table (sqlite.org/autoinc.html).
func hasSequences(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&n)
	return n > 0, err
}

// readSequences reads the largest id each AUTOINCREMENT table ever gave.
func readSequences(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	out := map[string]int64{}
	ok, err := hasSequences(ctx, tx)
	if err != nil || !ok {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, rows.Err()
}

// raiseSequences raises the largest id each AUTOINCREMENT table gave to at
// least seqs says. A row written with its id already raised it past every
// id loaded (sqlite.org/autoinc.html); this keeps also the ids given and
// since removed, such as a catalog's, which a rebuild then never gives
// again.
func raiseSequences(ctx context.Context, tx *sql.Tx, seqs map[string]int64) error {
	ok, err := hasSequences(ctx, tx)
	if err != nil || !ok {
		return err
	}
	for name, n := range seqs {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = max(seq, ?) WHERE name = ?`, n, name)
		if err != nil {
			return err
		}
		if changed, err := res.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, name, n); err != nil {
				return err
			}
		}
	}
	return nil
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

// migrateTo applies the first version migrations of all to the database on
// p's write connection, refusing one already past them.
func migrateTo(ctx context.Context, p *pools, all []sqlkit.Migration, version int) error {
	if version < 1 || version > len(all) {
		return fault.Wrap(fault.Conflict, sqlkit.ErrNewerSchema, "the snapshot is of schema version "+strconv.Itoa(version)+
			", and this binary knows versions 1 to "+strconv.Itoa(len(all)))
	}
	got, err := sqlkit.Version(ctx, p.read, dialect)
	if err != nil {
		return err
	}
	if got > version {
		return fault.New(fault.Conflict, "the database is at schema version "+strconv.Itoa(got)+", past the snapshot's "+strconv.Itoa(version))
	}
	_, err = sqlkit.Migrate(ctx, p.write, dialect, all[:version])
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
	p, err := sqlkit.Snapshot(ctx, snapshotDialect(m.p), false, sink)
	return p, m.fail(ctx, err, "the master database cannot be read")
}

// MigrateTo implements snapshot.Database.
func (m *Master) MigrateTo(ctx context.Context, version int) error {
	if err := m.open(); err != nil {
		return err
	}
	if masterMigrationsErr != nil {
		return fault.Wrap(fault.Internal, masterMigrationsErr, "the master's migrations cannot be read")
	}
	return m.fail(ctx, migrateTo(ctx, m.p, masterMigrations, version), "the master database cannot be migrated")
}

// Load implements snapshot.Database.
func (m *Master) Load(ctx context.Context, version int, src snapshot.Source) error {
	if err := m.open(); err != nil {
		return err
	}
	return m.fail(ctx, sqlkit.LoadSnapshot(ctx, snapshotDialect(m.p), version, src), "the master database cannot be loaded")
}

// Snapshot implements snapshot.Database.
func (t *Tenant) Snapshot(ctx context.Context, sink snapshot.Sink) (snapshot.Point, error) {
	if err := t.open(); err != nil {
		return snapshot.Point{}, err
	}
	p, err := sqlkit.Snapshot(ctx, snapshotDialect(t.p), true, sink)
	return p, t.fail(ctx, err, "the tenant database cannot be read")
}

// MigrateTo implements snapshot.Database.
func (t *Tenant) MigrateTo(ctx context.Context, version int) error {
	if err := t.open(); err != nil {
		return err
	}
	if tenantMigrationsErr != nil {
		return fault.Wrap(fault.Internal, tenantMigrationsErr, "a tenant's migrations cannot be read")
	}
	return t.fail(ctx, migrateTo(ctx, t.p, tenantMigrations, version), "the tenant database cannot be migrated")
}

// Load implements snapshot.Database.
func (t *Tenant) Load(ctx context.Context, version int, src snapshot.Source) error {
	if err := t.open(); err != nil {
		return err
	}
	return t.fail(ctx, sqlkit.LoadSnapshot(ctx, snapshotDialect(t.p), version, src), "the tenant database cannot be loaded")
}
