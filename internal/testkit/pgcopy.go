package testkit

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostgresSchemaCopy is every row of every table of one Postgres schema, per
// table in COPY's text format, with each sequence's value: what a site's
// point-in-time recovery to the moment it was read brings back of that
// schema.
type PostgresSchemaCopy struct {
	tables map[string][]byte
	seqs   map[string]sequenceValue
}

// sequenceValue is a sequence's last value and whether it was given.
type sequenceValue struct {
	last   int64
	called bool
}

// connect opens one connection to the database at address (as
// PostgresSchema returned it), in its schema, without the pool's
// parameters, and returns it with the schema's name.
func connect(tb testing.TB, address string) (*pgx.Conn, string) {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the address is not a URL")
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(k, "pool_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		tb.Fatalf("cannot connect to the database %s names: %v", PostgresEnv, err)
	}
	return conn, q.Get("search_path")
}

// CopyPostgresSchema reads every row of every table of the schema at
// address, and each sequence's value, in one repeatable-read transaction,
// so the copy stands for one moment however the schema is being written.
func CopyPostgresSchema(tb testing.TB, address string) *PostgresSchemaCopy {
	tb.Helper()
	ctx := context.Background()
	conn, schema := connect(tb, address)
	defer func() { _ = conn.Close(ctx) }() // A failed close of a test's connection hides nothing.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // A read-only transaction has nothing to keep.
	out := &PostgresSchemaCopy{tables: map[string][]byte{}, seqs: map[string]sequenceValue{}}
	for _, name := range firstColumn(tb, tx, `SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE'`, schema) {
		var b bytes.Buffer
		if _, err := conn.PgConn().CopyTo(ctx, &b, "COPY "+pgx.Identifier{schema, name}.Sanitize()+" TO STDOUT"); err != nil {
			tb.Fatal(err)
		}
		out.tables[name] = b.Bytes()
	}
	for _, name := range firstColumn(tb, tx, `SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema = $1`, schema) {
		var v sequenceValue
		if err := tx.QueryRow(ctx, "SELECT last_value, is_called FROM "+pgx.Identifier{schema, name}.Sanitize()).Scan(&v.last, &v.called); err != nil {
			tb.Fatal(err)
		}
		out.seqs[name] = v
	}
	return out
}

// firstColumn returns the first column of query's rows, read in tx.
func firstColumn(tb testing.TB, tx pgx.Tx, query string, args ...any) []string {
	tb.Helper()
	rows, err := tx.Query(context.Background(), query, args...)
	if err != nil {
		tb.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// RestorePostgresSchema empties every table of the schema at address and
// loads the copy's rows and sequence values into it, in one transaction:
// the schema is then as it was when the copy was read, as after a site's
// point-in-time recovery. The foreign keys' triggers are off while the
// tables are loaded (session_replication_role), so the order of the tables
// does not matter; it needs a superuser, which the test database's is.
func RestorePostgresSchema(tb testing.TB, address string, c *PostgresSchemaCopy) {
	tb.Helper()
	ctx := context.Background()
	conn, schema := connect(tb, address)
	defer func() { _ = conn.Close(ctx) }() // A failed close of a test's connection hides nothing.
	tables := make([]string, 0, len(c.tables))
	for name := range c.tables {
		tables = append(tables, pgx.Identifier{schema, name}.Sanitize())
	}
	for _, s := range []string{"BEGIN", "SET LOCAL session_replication_role = replica", "TRUNCATE " + strings.Join(tables, ", ")} {
		if _, err := conn.Exec(ctx, s); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
	for name, b := range c.tables {
		if _, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(b), "COPY "+pgx.Identifier{schema, name}.Sanitize()+" FROM STDIN"); err != nil {
			tb.Fatalf("COPY %s: %v", name, err)
		}
	}
	for name, v := range c.seqs {
		if _, err := conn.Exec(ctx, "SELECT setval($1, $2, $3)", pgx.Identifier{schema, name}.Sanitize(), v.last, v.called); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
		tb.Fatal(err)
	}
}
