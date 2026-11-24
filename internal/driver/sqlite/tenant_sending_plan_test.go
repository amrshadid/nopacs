package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// fillSending writes many senders' and jobs' rows into a migrated tenant
// database at path, beside fillPipeline's, and runs ANALYZE: 20,000 jobs,
// made at i seconds, one in a hundred of sender s1 and one in a hundred
// failed, a tenth running; 20,000
// scans of jobs; 20,000 failures, half of them of job j1, one in seven
// refused; 20,000 instances owed; 20,000 tallies of 200 jobs.
func fillSending(tb testing.TB, path string) {
	tb.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO jobs SELECT 'j' || i, 'send', CASE i % 100 WHEN 7 THEN 's1' ELSE 's' || (i % 50 + 2) END, 'sub-1', 'ses-1', '', '', '',
		   NULL, CASE WHEN i % 100 = 5 THEN 'failed' WHEN i % 100 = 9 THEN 'queued' WHEN i % 10 = 3 THEN 'running' ELSE 'done' END,
		   CASE WHEN i % 100 <> 9 AND i % 10 <> 3 THEN 1 ELSE 0 END, '', '', i * 1000000, NULL, i * 1000000, NULL,
		   i * 1000000, '', '', 'l' || i, 1, 1, 1, 0, '', '', 0, 1, 1, 10, 100, 10, 100, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO job_tallies SELECT 'j' || (i % 200 + 1), 'rule|r' || i || '|remove', i FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO job_scans SELECT 'j' || (i % 2000 + 1), i, 'u' || i, '', '', 10, 100, 0, 0, 0, 0, 0, 0, 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO job_failures SELECT CASE i % 2 WHEN 0 THEN 'j1' ELSE 'j' || (i % 200 + 2) END, 'sub-1', 'ses-1', 'scan-' || (i % 50), '2.25.3.' || i, i % 50,
		   CASE i % 7 WHEN 0 THEN 0 ELSE 1 END, 'peer_refused', 49152, '', '', '', 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO job_resends SELECT 'j' || (i % 2000 + 1), i % 2000, '2.25.3.' || i FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO job_owed SELECT 'j' || (i % 2000 + 1), i % 2000, '2.25.3.' || i FROM n`,
		`ANALYZE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			tb.Fatalf("%s: %v", stmt, err)
		}
	}
}

// TestSendingPlans guards that every statement of senders' and jobs' work
// that reads a table that grows is answered from an index, never by reading
// a whole table: it fills the pipeline's and the jobs' tables, runs
// ANALYZE, and checks SQLite's plan of the very statements package sqlkit
// runs (SendingDialect.Plans), each using the index named for it.
func TestSendingPlans(t *testing.T) {
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
	fillPipeline(t, path)
	fillSending(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, c := range sendingDialect.Plans() {
		plan := queryPlan(t, db, c.SQL, c.Args...)
		text := strings.Join(plan, "\n")
		t.Logf("%s:\n%s", c.Name, text)
		coroutines := map[string]bool{}
		for _, line := range plan {
			if name, ok := strings.CutPrefix(line, "CO-ROUTINE "); ok {
				coroutines[name] = true
			}
			// A scan of a co-routine's rows, each bounded by its own LIMIT,
			// is not a scan of a table.
			if name, ok := strings.CutPrefix(line, "SCAN "); ok && !strings.Contains(line, "USING ") && !coroutines[name] &&
				!strings.Contains(line, "CONSTANT ROW") {
				t.Errorf("%s reads a whole table: %q", c.Name, line)
			}
		}
		want := c.Index
		if c.Index == "" {
			want = "PRIMARY KEY"
			if !strings.Contains(text, want) && strings.Contains(text, "sqlite_autoindex") {
				want = "sqlite_autoindex"
			}
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s does not use %s", c.Name, want)
		}
	}
}
