package search_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
)

// TestEveryInsideEveryStaysWithinTheBounds guards the compiler against a
// condition inside every parse bound that repeats its child at each nested
// "every": the SQL and the values a compiled search makes never pass
// query.MaxSQLBytes and query.MaxParams, a condition that would is refused
// as invalid, and doing either takes a bounded amount of memory, for both
// databases.
func TestEveryInsideEveryStaysWithinTheBounds(t *testing.T) {
	levels := []string{"session", "scan"}
	for _, d := range []struct {
		name string
		d    query.Dialect
	}{{"sqlite", query.SQLite}, {"postgres", query.Postgres}} {
		refused := 0
		for _, depth := range []int{1, 2, 4, 8, 12, 14, 16} {
			var b strings.Builder
			for i := 0; i < depth; i++ {
				b.WriteString("every " + levels[i%2] + " (")
			}
			if levels[(depth-1)%2] == "session" {
				b.WriteString(`session.receiver == "x"`)
			} else {
				b.WriteString(strings.TrimSuffix(strings.Repeat(`Modality == "MR" and `, 10), " and "))
			}
			b.WriteString(strings.Repeat(")", depth))
			n, err := query.Parse(b.String())
			if err != nil {
				continue // beyond the grammar's own bound
			}
			c := query.Compiler{Registry: search.Registry(), Dialect: d.d, Now: time.Now()}
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			q, err := c.Compile(n, query.LevelScan)
			runtime.ReadMemStats(&m1)
			if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb > 16 {
				t.Errorf("%s depth %d: compiling allocated %d MB", d.name, depth, mb)
			}
			if err != nil {
				refused++
				if !strings.Contains(err.Error(), "too complex") {
					t.Errorf("%s depth %d refused with %v, want the query is too complex", d.name, depth, err)
				}
				continue
			}
			if len(q.Where) > query.MaxSQLBytes || len(q.Args) > query.MaxParams {
				t.Errorf("%s depth %d: %d bytes of SQL and %d values pass the bounds", d.name, depth, len(q.Where), len(q.Args))
			}
		}
		if refused == 0 {
			t.Errorf("%s: no nesting of every was refused; the bound proves nothing", d.name)
		}
	}
}
