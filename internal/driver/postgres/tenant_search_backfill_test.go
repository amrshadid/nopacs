package postgres

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestSearchBackfillContract runs the contract of the search backfill, and of
// the search rows through corrections, on Postgres, with the statements the
// port cannot make: an attribute put in state building (what the migration
// that adds a column does for a tenant holding scans), a statement run as
// written, and a count of the rows of every table that hold a text.
func TestSearchBackfillContract(t *testing.T) {
	ctx := context.Background()
	tenanttest.RunSearchBackfill(t,
		func(tb testing.TB, id string) tenant.Pipeline { return openTenantTest(tb, id).(*Tenant) },
		tenanttest.SearchHooks{
			Building: func(tb testing.TB, p tenant.Pipeline, field string) {
				if _, err := p.(*Tenant).pool.Exec(ctx, `UPDATE search_attributes SET state = 'building' WHERE field = $1`, field); err != nil {
					tb.Fatal(err)
				}
			},
			Exec: func(tb testing.TB, p tenant.Pipeline, query string, args ...any) {
				// The statement is written with "?", as every driver's is.
				var b strings.Builder
				n := 0
				for _, r := range query {
					if r == '?' {
						n++
						b.WriteString("$" + strconv.Itoa(n))
						continue
					}
					b.WriteRune(r)
				}
				if _, err := p.(*Tenant).pool.Exec(ctx, b.String(), args...); err != nil {
					tb.Fatal(err)
				}
			},
			CountText: func(tb testing.TB, p tenant.Pipeline, needle string) int {
				pool := p.(*Tenant).pool
				rows, err := pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = current_schema()`)
				if err != nil {
					tb.Fatal(err)
				}
				cols := map[string][]string{}
				for rows.Next() {
					var table, col string
					if err := rows.Scan(&table, &col); err != nil {
						tb.Fatal(err)
					}
					cols[table] = append(cols[table], col)
				}
				rows.Close()
				n := 0
				for table, cs := range cols {
					var where []string
					for _, c := range cs {
						where = append(where, `"`+c+`"::text LIKE $1`)
					}
					var c int
					// #nosec G202 -- the names come from the database's own catalog.
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM "`+table+`" WHERE `+strings.Join(where, " OR "), "%"+needle+"%").Scan(&c); err != nil {
						tb.Fatal(err)
					}
					n += c
				}
				return n
			},
		})
}
