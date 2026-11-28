package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// scaleSearchRounds is how many times each search shape is timed.
const scaleSearchRounds = 60

// TestScaleSearch builds a synthetic tenant of NOPACS_SCALE_INSTANCES scans
// (each of one instance, so the variable counts scans, with the row a
// search table holds of each) through the port, as reception writes one,
// and measures what a search of it costs on Postgres: the build's rate, the
// size of each table and index, and, for each shape of search a research
// archive's users make (equality and range pages, contains, counts, a
// facet, and searches across the levels), the time (median, 99th
// percentile, longest) over rounds with other values drawn each time. It
// is skipped unless NOPACS_SCALE_INSTANCES is set.
func TestScaleSearch(t *testing.T) {
	n := testkit.ScaleInstances(t)
	skipScale(t)
	ctx := context.Background()
	address := withParams(t, newSchema(t), map[string]string{"pool_max_conns": "16"})
	s := openTenantAt(t, address, "scale")
	conn := admin(t, address)
	sum := testkit.NewSummary(t, fmt.Sprintf("Search on Postgres, %d scans", n))
	began := time.Now()
	g, err := testkit.GenerateTenant(ctx, s, testkit.TenantShape{Instances: n, ScanInstances: 1, SearchRows: true, Seed: 1,
		Profiles: scaleProfiles(t), Workers: 8})
	if err != nil {
		t.Fatal(err)
	}
	sum.Add("build", fmt.Sprintf("%d scans in %d sessions in %v (%.0f scans/s, 8 writers)", g.Scans, g.Studies,
		time.Since(began).Round(time.Second), float64(g.Scans)/time.Since(began).Seconds()))
	began = time.Now()
	mustExec(t, conn, `VACUUM (ANALYZE) scan_search`, `ANALYZE`)
	sum.Add("VACUUM (ANALYZE) scan_search and ANALYZE", time.Since(began).Round(time.Millisecond).String())
	logSizes(t, conn, sum)
	testkit.TimeSearches(t, s, scaleSearchRounds, sum)
}
