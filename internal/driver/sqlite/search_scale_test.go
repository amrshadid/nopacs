package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// scaleSearchRounds is how many times each search shape is timed.
const scaleSearchRounds = 60

// TestScaleSearch builds a synthetic tenant of NOPACS_SCALE_INSTANCES scans
// (each of one instance, so the variable counts scans, with the row a
// search table holds of each) through the port, as reception writes one,
// and measures what a search of it costs on SQLite: the build's rate, the
// size of each table and index, and, for each shape of search a research
// archive's users make (equality and range pages, contains, counts, a
// facet, and searches across the levels), the time (median, 99th
// percentile, longest) over rounds with other values drawn each time. It
// is skipped unless NOPACS_SCALE_INSTANCES is set.
func TestScaleSearch(t *testing.T) {
	n := testkit.ScaleInstances(t)
	skipScale(t)
	ctx := context.Background()
	dir := t.TempDir()
	if testkit.ScaleKeep() {
		var err error
		if dir, err = os.MkdirTemp("", "nopacs-scale-"); err != nil {
			t.Fatal(err)
		}
		t.Logf("the tenant is kept at %s", dir)
	}
	path := filepath.Join(dir, "tenant.db")
	s, err := OpenTenant(ctx, path, "scale")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sum := testkit.NewSummary(t, fmt.Sprintf("Search on SQLite, %d scans", n))
	// The bulk is written with commits not synced and a large page cache,
	// and the settings a server has are back before anything is measured.
	if err := s.setBulkWrites(ctx, true, 1024); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	g, err := testkit.GenerateTenant(ctx, s, testkit.TenantShape{Instances: n, ScanInstances: 1, SearchRows: true, Seed: 1,
		Profiles: scaleProfiles(t), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setBulkWrites(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	sum.Add("build (commits not synced, 1 GiB cache)", fmt.Sprintf("%d scans in %d sessions in %v (%.0f scans/s, 4 writers)", g.Scans, g.Studies,
		time.Since(began).Round(time.Second), float64(g.Scans)/time.Since(began).Seconds()))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	began = time.Now()
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	sum.Add("ANALYZE", time.Since(began).Round(time.Millisecond).String())
	logSizes(t, db, sum)
	testkit.TimeSearches(t, s, scaleSearchRounds, sum)
}
