package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// scaleEnv is the variable that makes the scale tests run (docs/develop/testing.md).
const scaleEnv = "NOPACS_TEST_SCALE"

// skipScale skips a test at millions of rows except in the run made for it
// (make test-scale): under -short, under the race detector (which it would
// measure instead of the database), on 32 bits and on Windows, whose CI
// minutes cost more and where nothing in SQLite's planner differs.
func skipScale(t *testing.T) {
	t.Helper()
	switch {
	case testing.Short():
		t.Skip("millions of rows: skipped under -short")
	case os.Getenv(scaleEnv) == "":
		t.Skip("millions of rows: run by make test-scale, which sets " + scaleEnv)
	case raceDetector:
		t.Skip("millions of rows: run without the race detector by make test-scale")
	case runtime.GOARCH == "386":
		t.Skip("millions of rows: run on 64 bits by make test-scale")
	case runtime.GOOS == "windows":
		t.Skip("millions of rows: measured on Linux (CI) and macOS, not Windows")
	}
}

// scaleListBound is the longest a page of either list may take at scale. A
// page read from the index its filters need takes milliseconds; one that
// tests each row of a wider index took the better part of a second at these
// sizes, so the bound tells the two apart with room for a slow machine.
const scaleListBound = 250 * time.Millisecond

// TestScaleListFilters guards the receptions and quarantine lists at two
// million receptions and one million records, shaped as a tenant that has
// worked for years: nearly all receptions ended and at one receiver,
// nearly all records released. Every combination of filters the API
// contract allows is listed, a page of 50 from the first page, and each
// must be answered from an index whose leading columns are its filters:
// the plan reads no table whole and sorts no rows outside a merge of
// bounded parts, and the page takes less than scaleListBound.
func TestScaleListFilters(t *testing.T) {
	skipScale(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	start := time.Now()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000000)
		 INSERT INTO receptions SELECT 'rc' || i, CASE WHEN i % 100 = 0 THEN 'other' ELSE 'busy' END, '2.25.1.' || (i / 4), i % 4 + 1,
		   CASE WHEN i % 100000 = 0 THEN 'receiving' WHEN i % 50000 = 1 THEN 'failed' WHEN i % 70000 = 2 THEN 'quarantined' ELSE 'done' END,
		   CASE WHEN i % 100000 = 0 THEN 0 ELSE 1 END,
		   0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '', i * 1000000, i * 1000000, i * 1000000, NULL, 0, '[]', 0, '', '', '', NULL, '' FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000000)
		 INSERT INTO quarantine SELECT 'q' || i, CASE WHEN i % 3 = 0 THEN 'not_dicom' ELSE 'different_file' END, '{}',
		   CASE WHEN i % 10000 = 0 THEN 'held' ELSE 'released' END, CASE WHEN i % 100 = 0 THEN 'other' ELSE 'busy' END, 'rc' || i,
		   '', '', '', 10, NULL, NULL, '', '2.25.3.' || i, '2.25.1.' || (i / 4), '', '', '', 0, i * 1000000, NULL,
		   '', '', '', '', '', '', 0, '', '', '', '' FROM n`,
		`ANALYZE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("built in %v", time.Since(start))

	checkPlan := func(what, query string, args []any) {
		t.Helper()
		plan := queryPlan(t, db, query, args...)
		union := strings.Contains(strings.Join(plan, "\n"), "UNION ALL")
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "USING ") && !strings.HasPrefix(line, "SCAN (subquery") &&
				!strings.Contains(line, "CONSTANT ROW") {
				t.Errorf("%s reads a table whole: %q", what, line)
			}
			if strings.Contains(line, "TEMP B-TREE FOR ORDER BY") && !union {
				t.Errorf("%s sorts its rows instead of reading them in order from an index:\n%s", what, strings.Join(plan, "\n"))
			}
		}
	}
	timed := func(what string, list func() (int, error)) {
		t.Helper()
		began := time.Now()
		n, err := list()
		took := time.Since(began)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if took > scaleListBound {
			t.Errorf("%s: %d rows in %v, past %v", what, n, took, scaleListBound)
		}
	}
	since, until := fromMicros(500000*1000000), fromMicros(1500000*1000000)
	stateSets := [][]reception.State{nil, {reception.Failed}, {reception.Failed, reception.Quarantined}, {reception.Receiving}}
	n := 0
	for _, receiver := range []string{"", "busy"} {
		for _, study := range []string{"", "2.25.1.12345"} {
			for _, states := range stateSets {
				for _, inProgress := range []bool{false, true} {
					for _, ranged := range []bool{false, true} {
						f := tenant.ReceptionFilter{Receiver: receiver, StudyUID: study, States: states, InProgress: inProgress}
						if ranged {
							f.Since, f.Until = since, until
						}
						what := fmt.Sprintf("receptions %+v", f)
						if q, a := receptionsQuery(f, nil, 51); q != "" {
							checkPlan(what, q, a)
						}
						timed(what, func() (int, error) {
							got, _, err := s.ListReceptions(ctx, f, master.Page{Limit: 50})
							return len(got), err
						})
						n++
					}
				}
			}
		}
	}
	for mask := range 1 << 7 {
		var f tenant.QuarantineFilter
		if mask&1 != 0 {
			f.State = tenant.QuarantineHeld
		}
		if mask&2 != 0 {
			f.Reason = tenant.QuarantineNotDICOM
		}
		if mask&4 != 0 {
			f.Receiver = "busy"
		}
		if mask&8 != 0 {
			f.ReceptionID = "rc12345"
		}
		if mask&16 != 0 {
			f.StudyUID = "2.25.1.3086"
		}
		if mask&32 != 0 {
			f.SOPInstanceUID = "2.25.3.12345"
		}
		if mask&64 != 0 {
			f.Since, f.Until = since, until
		}
		what := fmt.Sprintf("quarantine %+v", f)
		q, a := quarantineQuery(f, nil, 51)
		checkPlan(what, q, a)
		timed(what, func() (int, error) {
			got, _, err := s.ListQuarantined(ctx, f, master.Page{Limit: 50})
			return len(got), err
		})
		n++
	}
	t.Logf("%d combinations of filters checked", n)
}
