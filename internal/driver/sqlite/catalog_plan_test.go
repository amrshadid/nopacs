package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// catalogIndexes are the names SQLite gives the catalog's unique
// constraints: the automatic index of each, in the order the table states
// them.
var catalogIndexes = testkit.CatalogIndexes{SubjectsFolded: "sqlite_autoindex_subjects_1",
	SessionsFolded: "sqlite_autoindex_sessions_2", ScansFolded: "sqlite_autoindex_scans_2", InstancesKey: "instances_by_uid",
	InstancesByScan: "PRIMARY KEY"}

// fillCatalog writes a catalog of subjects subjects, five sessions each and
// six scans a session with two instances each, straight through SQL into a
// migrated tenant database, with the values the reads filter by spread as
// a real tenant's are, the rare values of testkit given to the oldest rows,
// and runs ANALYZE.
func fillCatalog(tb testing.TB, db *sql.DB, subjects int) {
	tb.Helper()
	sessions, scans := subjects*5, subjects*30
	subject := `'sub-' || printf('%06d', (i - 1) / 5 + 1)`
	scanSubject := `'sub-' || printf('%06d', (i - 1) / 30 + 1)`
	scanSession := `'ses-' || (((i - 1) / 6) % 5)`
	for _, stmt := range []string{
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		 INSERT INTO subjects (id, label, folded, created, updated, session_count, instance_count, bytes)
		 SELECT i, 'sub-' || printf('%%06d', i), 'sub-' || printf('%%06d', i), i * 5000000, i * 5000000, 5, 60, 60000 FROM n`, subjects),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		 INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded, study_date, receiver,
		   scan_count, instance_count, bytes, updated)
		 SELECT i, '2.25.1.' || i, (i - 1) / 5 + 1, 'ses-' || ((i - 1) %% 5), 'ses-' || ((i - 1) %% 5), 'store', i * 1000000, %s, %s,
		   CASE WHEN i %% 50 = 0 THEN '' ELSE printf('%%04d%%02d%%02d', 2010 + i %% 15, i %% 12 + 1, i %% 28 + 1) END,
		   'recv' || (i %% 3), 6, 12, 12000, i * 1000000 FROM n`, sessions, subject, subject),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		 INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
		   subject_label, subject_folded, session_label, session_folded, storage, modality, series_description, description_folded,
		   series_number, receiver)
		 SELECT i, (i - 1) / 6 + 1, '2.25.2.' || i, 'scan-' || ((i - 1) %% 6), 'scan-' || ((i - 1) %% 6), 2, 2000, NULL,
		   ((i - 1) / 6 + 1) * 1000000 + i, ((i - 1) / 6 + 1) * 1000000 + i, %s, %s, %s, %s, 'store',
		   CASE i %% 4 WHEN 0 THEN 'MR' WHEN 1 THEN 'CT' WHEN 2 THEN 'PT' ELSE 'SR' END,
		   CASE i %% 5 WHEN 0 THEN 'T1 MPRAGE ' || i WHEN 1 THEN 'Localizer' WHEN 2 THEN 'DWI b1000' WHEN 3 THEN '' ELSE 'Scout ' || i END,
		   lower(CASE i %% 5 WHEN 0 THEN 'T1 MPRAGE ' || i WHEN 1 THEN 'Localizer' WHEN 2 THEN 'DWI b1000' WHEN 3 THEN '' ELSE 'Scout ' || i END),
		   (i - 1) %% 6 + 1, 'recv' || (((i - 1) / 6 + 1) %% 3) FROM n`, scans, scanSubject, scanSubject, scanSession, scanSession),
		`UPDATE sessions SET receiver = '` + testkit.RareReceiver + `' WHERE id <= 10`,
		`UPDATE scans SET receiver = '` + testkit.RareReceiver + `' WHERE session_id <= 10`,
		`UPDATE scans SET modality = '` + testkit.RareModality + `' WHERE id <= 10`,
		`INSERT INTO session_modalities (session_id, modality, created)
		 SELECT DISTINCT c.session_id, c.modality, s.created FROM scans AS c JOIN sessions AS s ON s.id = c.session_id`,
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		 INSERT INTO instances SELECT (i - 1) / 2 + 1, '2.25.3.' || i, 'rc', randomblob(32), NULL, '', 0, 1000, 1000, 0, NULL, 0 FROM n`, scans*2),
		`ANALYZE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			tb.Fatalf("%s: %v", stmt, err)
		}
	}
}

// sampleCatalog reads the plan checks' arguments from the catalog: the
// rows of the scan in the middle of its table, and of its session and
// subject.
func sampleCatalog(tb testing.TB, db *sql.DB) testkit.CatalogSample {
	tb.Helper()
	var v testkit.CatalogSample
	var created, sesCreated, subCreated int64
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM scans`).Scan(&n); err != nil {
		tb.Fatal(err)
	}
	err := db.QueryRow(`SELECT c.id, c.subject_label, c.session_label, c.label, c.subject_folded, c.session_folded, c.folded,
		c.description_folded, c.created, c.modality, c.receiver, s.id, s.created, s.subject_id, s.study_date, s.folded, sub.created,
		(SELECT min(sop_instance_uid) FROM instances WHERE scan_id = c.id)
		FROM scans AS c JOIN sessions AS s ON s.id = c.session_id JOIN subjects AS sub ON sub.id = s.subject_id
		WHERE c.id >= ? AND c.modality <> '' AND s.study_date <> '' ORDER BY c.id LIMIT 1`, n/2).Scan(
		&v.ScanID, &v.Scan.Subject, &v.Scan.Session, &v.Scan.Scan, &v.ScanKey.SubjectFolded, &v.ScanKey.SessionFolded, &v.ScanKey.Folded,
		&v.ScanKey.DescriptionFolded, &created, &v.Modality, &v.Receiver, &v.SessionID, &sesCreated, &v.SubjectID, &v.StudyDate,
		&v.SessionKey.Folded, &subCreated, &v.UID)
	if err != nil {
		tb.Fatal(err)
	}
	v.Subject, v.Session = v.Scan.Subject, [2]string{v.Scan.Subject, v.Scan.Session}
	v.ScanKey.ID, v.ScanKey.Created = v.ScanID, fromMicros(created)
	v.SessionKey.ID, v.SessionKey.Created, v.SessionKey.SubjectFolded, v.SessionKey.StudyDate = v.SessionID, fromMicros(sesCreated),
		label.Fold(v.Subject), v.StudyDate
	v.SubjectKey = sqlkit.CatalogKey{ID: v.SubjectID, Created: fromMicros(subCreated), Folded: label.Fold(v.Subject)}
	return v
}

// sqlitePlanFault returns why SQLite's plan of a catalog read is not one
// range of the index named, read in the list's order, or "": a line that
// scans a table, a temporary B-tree that sorts or groups, a read of
// another index, or for a page after a cursor (paged) a walk of the index
// from one end (SCAN) rather than a search of a range in it.
func sqlitePlanFault(plan []string, index string, paged bool) string {
	text := strings.Join(plan, "\n")
	for _, line := range plan {
		// The sqlite3 tool draws the plan as a tree.
		line = strings.TrimLeft(line, "|`- ")
		switch {
		case strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "USING ") && !strings.Contains(line, "json_each"):
			return "it reads a table whole: " + line
		case strings.Contains(line, "TEMP B-TREE"):
			return "it sorts or groups rows: " + line
		case paged && strings.HasPrefix(line, "SCAN ") && strings.Contains(line, " "+index):
			return "a page after a cursor walks its index from one end: " + line
		}
	}
	if !strings.Contains(text, " "+index) {
		return "it does not read " + index
	}
	return ""
}

// TestCatalogPlans guards that every read of the catalog is a range of the
// index named for it in the order the list answers, at a size where
// SQLite's planner chooses as it would at millions of rows: a catalog of
// thousands of subjects and tens of thousands of scans, analyzed, and every
// line of SQLite's plan of the very statement the driver sends checked
// (sqlitePlanFault). The statements of a modality's sessions read by id
// and of a page's modalities are checked too. Under the race detector it
// is skipped: a plan does not change with it, and filling the tables under
// it takes minutes of the package's five.
func TestCatalogPlans(t *testing.T) {
	if raceDetector {
		t.Skip("plans do not change under the race detector; run without it")
	}
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
	fillCatalog(t, db, 4000)
	checkCatalogPlans(t, db, sampleCatalog(t, db), "", 0)
}

// fromTable matches the table a catalog read reads.
var fromTable = regexp.MustCompile(` FROM ([a-z_]+)`)

// tableRows returns the rows of the table a statement reads.
func tableRows(t *testing.T, db *sql.DB, statement string) int64 {
	t.Helper()
	m := fromTable.FindStringSubmatch(statement)
	if m == nil {
		t.Fatalf("no table in %s", statement)
	}
	var rows int64
	if err := db.QueryRow(`SELECT count(*) FROM ` + m[1]).Scan(&rows); err != nil { //nolint:gosec // G202: a table name of this package's own statements.
		t.Fatal(err)
	}
	return rows
}

// checkCatalogPlans checks SQLite's plan of every catalog read with
// arguments from v (testkit.CatalogPlanCases and the two reads by a list
// of ids) and logs it; toolAndPath, when not empty, is the
// sqlite3 tool and the database's path, joined by a NUL, and the tool
// plans each one again. A read of a table of fewer than strictRows rows
// has its faults logged, not failed: reading a few thousand rows whole is
// the planner's right choice, as a tenant of ten million instances has
// only that many subjects and sessions; TestCatalogPlans checks those
// reads at a size where it is not.
func checkCatalogPlans(t *testing.T, db *sql.DB, v testkit.CatalogSample, toolAndPath string, strictRows int64) {
	t.Helper()
	cases := testkit.CatalogPlanCases(catalogSQL, v, catalogIndexes, 101)
	ids := []int64{v.SessionID, v.SessionID + 1, v.SessionID + 2}
	byID, err := catalogSQL.SessionsByID(ids)
	if err != nil {
		t.Fatal(err)
	}
	modalities, err := catalogSQL.ModalitiesOf(ids)
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases,
		testkit.CatalogPlanCase{Name: "a modality's sessions by id", Read: byID, Index: "INTEGER PRIMARY KEY", Paged: true},
		testkit.CatalogPlanCase{Name: "a page's modalities", Read: modalities, Index: "PRIMARY KEY", Paged: true})
	for _, c := range cases {
		plan := queryPlan(t, db, c.Read.SQL, c.Read.Args...)
		t.Logf("%s:\n  %s", c.Name, strings.Join(plan, "\n  "))
		fail := t.Errorf
		if rows := tableRows(t, db, c.Read.SQL); rows < strictRows {
			fail = func(format string, args ...any) {
				t.Logf("a table of %d rows: "+format, append([]any{rows}, args...)...)
			}
		}
		index := c.Index
		if c.Also != "" && !strings.Contains(strings.Join(plan, "\n"), " "+index) {
			index = c.Also
		}
		if why := sqlitePlanFault(plan, index, c.Paged); why != "" {
			fail("%s: %s", c.Name, why)
		}
		if tool, path, ok := strings.Cut(toolAndPath, "\x00"); ok {
			out := runSQLite3(t, tool, path, "EXPLAIN QUERY PLAN "+literal(c.Read.SQL, c.Read.Args))
			if why := sqlitePlanFault(strings.Split(out, "\n"), index, c.Paged); why != "" {
				fail("%s, planned by sqlite3: %s\n%s", c.Name, why, out)
			}
		}
	}
}
