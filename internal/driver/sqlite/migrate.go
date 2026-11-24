package sqlite

import (
	"context"
	"database/sql"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
)

// dialect is how the migration runner works on SQLite. BeginLocked begins on
// the write pool, whose transactions are BEGIN IMMEDIATE, so the lock is the
// database's write lock and holds across processes.
var dialect = sqlkit.Dialect{
	HasTable:      `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'`,
	CreateTable:   `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, checksum BLOB NOT NULL, applied INTEGER NOT NULL) STRICT`,
	SelectApplied: `SELECT version, name, checksum FROM schema_migrations ORDER BY version`,
	InsertApplied: `INSERT INTO schema_migrations (version, name, checksum, applied) VALUES (?, ?, ?, ?)`,
	BeginLocked: func(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
		return db.BeginTx(ctx, nil)
	},
}
