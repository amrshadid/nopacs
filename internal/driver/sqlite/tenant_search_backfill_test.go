package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestSearchBackfillContract runs the contract of the search backfill, and of
// the search rows through corrections, on SQLite, with the statements the
// port cannot make: an attribute put in state building (what the migration
// that adds a column does for a tenant holding scans), a statement run as
// written, and a count of the rows of every table that hold a text.
func TestSearchBackfillContract(t *testing.T) {
	write := func(tb testing.TB, p tenant.Pipeline, fn func(ctx context.Context, tx *sql.Tx) error) {
		tb.Helper()
		ctx := context.Background()
		if err := p.(*Tenant).writeShared(ctx, "test", func(tx *sql.Tx, _ *sharedStatements) error { return fn(ctx, tx) }); err != nil {
			tb.Fatal(err)
		}
	}
	tenanttest.RunSearchBackfill(t,
		func(tb testing.TB, id string) tenant.Pipeline { return openTenantTest(tb, id).(*Tenant) },
		tenanttest.SearchHooks{
			Building: func(tb testing.TB, p tenant.Pipeline, field string) {
				write(tb, p, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE search_attributes SET state = 'building' WHERE field = ?`, field)
					return err
				})
			},
			Exec: func(tb testing.TB, p tenant.Pipeline, query string, args ...any) {
				write(tb, p, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, query, args...)
					return err
				})
			},
			CountText: func(tb testing.TB, p tenant.Pipeline, needle string) int {
				n := 0
				write(tb, p, func(ctx context.Context, tx *sql.Tx) error {
					cols := map[string][]string{}
					rows, err := tx.QueryContext(ctx, `SELECT m.name, c.name FROM sqlite_schema AS m, pragma_table_info(m.name) AS c WHERE m.type = 'table'`)
					if err != nil {
						return err
					}
					for rows.Next() {
						var table, col string
						if err := rows.Scan(&table, &col); err != nil {
							return err
						}
						cols[table] = append(cols[table], col)
					}
					if err := rows.Close(); err != nil {
						return err
					}
					for table, cs := range cols {
						var where []string
						var args []any
						for _, c := range cs {
							where = append(where, `CAST("`+c+`" AS TEXT) LIKE ?`)
							args = append(args, "%"+needle+"%")
						}
						var c int
						// #nosec G202 -- the names come from the database's own catalog.
						if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`" WHERE `+strings.Join(where, " OR "), args...).Scan(&c); err != nil {
							return err
						}
						n += c
					}
					return nil
				})
				return n
			},
		})
}
