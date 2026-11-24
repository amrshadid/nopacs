package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// studyInstancesSQL is the first page of a study's instances, scan by scan
// in order of id, each scan's in order of SOP Instance UID: scans_by_study,
// then the instances' primary key, with no sort.
const studyInstancesSQL = `SELECT s.series_uid, i.sop_instance_uid, i.storage_key, s.subject_label, s.session_label, s.label, i.size
	FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
	WHERE s.study_uid = ? ORDER BY s.id, i.sop_instance_uid LIMIT 1001`

// catalogQuery is one read the catalog's services will make of a large
// tenant, with how to draw its arguments from the tenant.
type catalogQuery struct {
	what, sql string
	args      func(r *rand.Rand) []any
}

// letters are where a page of subjects starts from: a label's first
// letter after "sub-", one of the default rule's hash characters.
const letters = "abcdefghijklmnopqrstuvwxyz"

// scaleProbes is how many times each catalog read is timed.
const scaleProbes = 300

// scaleStrictRows is the rows past which a catalog read of a table must be
// a range of an index at scale (checkCatalogPlans).
const scaleStrictRows = 10000

// TestScaleCatalog builds a synthetic tenant of NOPACS_SCALE_INSTANCES
// instances through the port (testkit.GenerateTenant) and measures what a
// catalog that large costs on SQLite: the rate of reception into an empty
// tenant and into the large one, the size of each table and index, the
// plan of every statement of the catalog's reads, every line checked
// (checkCatalogPlans), and the time of each read through the port
// (testkit.TimeCatalogReads), and the plan and the time (median, 99th
// percentile) of each read the catalog's services will make (an instance by its UID alone, a scan's instances
// paged, a study's instances, a session by study UID, a subject's
// sessions and a session's scans by label, a page of subjects), and the
// time of ANALYZE and VACUUM. A read that scans a large table whole fails
// the test; so does a page past scaleListBound. The plans are read again
// by the sqlite3 tool when it is here, so they are not only this driver's
// reading of them. It is skipped unless NOPACS_SCALE_INSTANCES is set.
func TestScaleCatalog(t *testing.T) {
	n := testkit.ScaleInstances(t)
	skipScale(t)
	ctx := context.Background()
	dir := t.TempDir()
	if testkit.ScaleKeep() {
		var err error
		if dir, err = os.MkdirTemp("", "nopacs-scale-"); err != nil {
			t.Fatal(err)
		}
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
	if testkit.ScaleKeep() {
		t.Logf("the tenant is kept at %s", path)
	}
	profiles := scaleProfiles(t)
	sum := testkit.NewSummary(t, fmt.Sprintf("Catalog on SQLite, %d instances", n))

	// The rate of reception: a slice into the empty tenant, the bulk, then
	// a slice into the large one, each slice one writer as one receiver's
	// pipeline is.
	const slice = 50000
	rate := func(first int, count int64, workers int) (float64, int) {
		t.Helper()
		began := time.Now()
		g, err := testkit.GenerateTenant(ctx, s, testkit.TenantShape{Instances: count, Seed: 1, FirstStudy: first, Profiles: profiles, Workers: workers})
		if err != nil {
			t.Fatal(err)
		}
		return float64(g.Instances) / time.Since(began).Seconds(), g.NextStudy
	}
	emptyRate, next := rate(0, min(slice, n), 1)
	sum.Add("reception into an empty tenant", fmt.Sprintf("%.0f instances/s", emptyRate))
	if n > slice {
		// The bulk is written with commits not synced and a large page
		// cache (setBulkWrites), and the settings openPools gives a server
		// are back before anything is measured: the bulk only makes the
		// tenant large, and a build synced commit by commit through a 2 MB
		// cache takes hours at ten million instances.
		if err := s.setBulkWrites(ctx, true, 1024); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		last := time.Now()
		g, err := testkit.GenerateTenant(ctx, s, testkit.TenantShape{Instances: n - slice, Seed: 1, FirstStudy: next, Profiles: profiles, Workers: 4,
			Progress: func(done int64) {
				if time.Since(last) > time.Minute {
					last = time.Now()
					t.Logf("built %d of %d instances in %v, %.0f instances/s", done+slice, n, time.Since(began).Round(time.Second),
						float64(done)/time.Since(began).Seconds())
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		next = g.NextStudy
		if err := s.setBulkWrites(ctx, false, 0); err != nil {
			t.Fatal(err)
		}
		sum.Add("build (commits not synced, 1 GiB cache)", fmt.Sprintf("%d instances in %v (%.0f instances/s, 4 writers)", n, time.Since(began).Round(time.Second),
			float64(g.Instances)/time.Since(began).Seconds()))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	began := time.Now()
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	sum.Add("ANALYZE", time.Since(began).Round(time.Millisecond).String())
	logSizes(t, db, sum)

	// The catalog's reads as its services make them: the plan of every
	// statement checked line by line, as this driver reads it and as the
	// sqlite3 tool does, and the time of each read through the port.
	toolAndPath := ""
	if tool := sqlite3Tool(); tool != "" {
		toolAndPath = tool + "\x00" + path
	}
	checkCatalogPlans(t, db, sampleCatalog(t, db), toolAndPath, scaleStrictRows)
	testkit.TimeCatalogReads(t, s, catalogProbes(t, db), scaleProbes, sum, scaleListBound)

	scans := sampleScans(t, db)
	instances := sampleInstances(t, db, scans)
	pick := func(r *rand.Rand) scanRow { return scans[r.IntN(len(scans))] }
	queries := []catalogQuery{
		{"an instance by its UID alone", `SELECT s.storage, i.storage_key, s.subject_label, s.session_label, s.label, i.byte_offset,
			i.byte_length, i.size, coalesce(i.archived_sha256, i.sha256), s.series_uid, s.study_uid
			FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid = ?`,
			func(r *rand.Rand) []any { return []any{instances[r.IntN(len(instances))]} }},
		{"a scan's first page of instances", scanInstancesSQL, func(r *rand.Rand) []any { return []any{pick(r).series, "", 1001} }},
		{"a scan's page after a cursor", scanInstancesSQL, func(r *rand.Rand) []any { return []any{pick(r).series, "2.25.5", 1001} }},
		{"a study's instances, first page", studyInstancesSQL,
			func(r *rand.Rand) []any { return []any{pick(r).study} }},
		{"a session by study UID", sessionLabelsSQL, func(r *rand.Rand) []any { return []any{pick(r).study} }},
		{"a scan by series UID", `SELECT label, instances, bytes FROM scans WHERE series_uid = ?`,
			func(r *rand.Rand) []any { return []any{pick(r).series} }},
		{"a subject's sessions by label", `SELECT se.label, se.study_uid FROM subjects AS su JOIN sessions AS se ON se.subject_id = su.id
			WHERE su.folded = ? ORDER BY se.folded LIMIT 51`,
			func(r *rand.Rand) []any { return []any{pick(r).subject} }},
		{"a session's scans by labels", `SELECT s.label, s.series_uid, s.instances FROM subjects AS su
			JOIN sessions AS se ON se.subject_id = su.id JOIN scans AS s ON s.session_id = se.id
			WHERE su.folded = ? AND se.folded = ? ORDER BY s.folded LIMIT 1001`,
			func(r *rand.Rand) []any { p := pick(r); return []any{p.subject, p.session} }},
		{"a page of subjects by label", `SELECT label FROM subjects WHERE folded > ? ORDER BY folded LIMIT 51`,
			func(r *rand.Rand) []any { n := r.IntN(26); return []any{"sub-" + letters[n:n+1]} }},
	}
	tool := sqlite3Tool()
	r := rand.New(rand.NewPCG(2, 3)) // #nosec G404 -- a seeded generator, so a run probes the same rows as the last.
	for _, q := range queries {
		args := q.args(r)
		plan := queryPlan(t, db, q.sql, args...)
		t.Logf("%s, plan:\n  %s", q.what, strings.Join(plan, "\n  "))
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "USING ") {
				t.Errorf("%s reads a table whole: %q", q.what, line)
			}
		}
		if tool != "" {
			out := runSQLite3(t, tool, path, "EXPLAIN QUERY PLAN "+literal(q.sql, args))
			t.Logf("%s, plan by sqlite3:\n%s", q.what, out)
			if strings.Contains(out, "SCAN ") && !strings.Contains(out, "USING ") {
				t.Errorf("%s: sqlite3 plans a scan of a whole table:\n%s", q.what, out)
			}
		}
		d := make([]time.Duration, 0, scaleProbes)
		for range scaleProbes {
			args := q.args(r)
			began := time.Now()
			rows, err := db.QueryContext(ctx, q.sql, args...)
			if err != nil {
				t.Fatalf("%s: %v", q.what, err)
			}
			got := 0
			for rows.Next() {
				got++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			d = append(d, time.Since(began))
		}
		sp := testkit.SpreadOf(d)
		sum.Add(q.what, sp.String())
		if sp.P99 > scaleListBound {
			t.Errorf("%s: the 99th percentile %v is past %v", q.what, sp.P99, scaleListBound)
		}
	}

	if n > slice {
		largeRate, _ := rate(next, slice, 1)
		sum.Add(fmt.Sprintf("reception into the tenant of %d instances", n), fmt.Sprintf("%.0f instances/s", largeRate))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatal(err)
	}
	sum.Add("VACUUM", time.Since(began).Round(time.Millisecond).String())
	if fi, err := os.Stat(path); err == nil {
		sum.Add("file after VACUUM", fmt.Sprintf("%.2f GB", float64(fi.Size())/1e9))
	}
}

// scaleProfiles reads the public test files the synthetic tenant's
// instances are made from.
func scaleProfiles(t *testing.T) []testkit.FileProfile {
	t.Helper()
	var out []testkit.FileProfile
	for _, name := range []string{"MR_small.dcm", "ct5n-2062.dcm"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "receive", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, testkit.ProfileOf(t, b))
	}
	return out
}

// scanRow is one scan of the tenant with what places it.
type scanRow struct {
	series, study, subject, session string
}

// sampleScans returns every scan of the tenant (a few tens of thousands at
// ten million instances) with its study and folded labels.
func sampleScans(t *testing.T, db *sql.DB) []scanRow {
	t.Helper()
	rows, err := db.Query(`SELECT s.series_uid, se.study_uid, su.folded, se.folded FROM scans AS s
		JOIN sessions AS se ON se.id = s.session_id JOIN subjects AS su ON su.id = se.subject_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []scanRow
	for rows.Next() {
		var r scanRow
		if err := rows.Scan(&r.series, &r.study, &r.subject, &r.session); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the tenant has no scan")
	}
	return out
}

// sampleInstances returns the UIDs of up to two instances of each of a
// thousand scans, read by the scan's index, so a probe by UID is spread
// over the whole primary key.
func sampleInstances(t *testing.T, db *sql.DB, scans []scanRow) []string {
	t.Helper()
	var out []string
	for i := 0; i < len(scans); i += max(1, len(scans)/1000) {
		rows, err := db.Query(scanInstancesSQL, scans[i].series, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var uid, rid, storage, key, subject, session, scan string
			var off, length, size int64
			var sum, archived []byte
			var held int64
			if err := rows.Scan(&uid, &rid, &storage, &key, &subject, &session, &scan, &off, &length, &size, &sum, &archived, &held); err != nil {
				t.Fatal(err)
			}
			out = append(out, uid)
		}
		_ = rows.Close()
	}
	return out
}

// logSizes logs, and adds to sum past a megabyte, the bytes of each table and index, from SQLite's dbstat
// table, and of the file.
func logSizes(t *testing.T, db *sql.DB, sum *testkit.Summary) {
	t.Helper()
	rows, err := db.Query(`SELECT name, SUM(pgsize) FROM dbstat GROUP BY name ORDER BY 2 DESC LIMIT 20`)
	if err != nil {
		t.Logf("sizes are not known: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%-32s %8.1f MB", name, float64(size)/1e6))
		if size > 1e6 {
			sum.Add("size of "+name, fmt.Sprintf("%.1f MB", float64(size)/1e6))
		}
	}
	t.Logf("sizes:\n  %s", strings.Join(lines, "\n  "))
}

// sqlite3Tool returns the path of the sqlite3 tool, SQLITE3 or the one on
// PATH, or empty when there is none.
func sqlite3Tool() string {
	tool := os.Getenv("SQLITE3")
	if tool == "" {
		tool = "sqlite3"
	}
	p, err := exec.LookPath(tool)
	if err != nil {
		return ""
	}
	return p
}

// runSQLite3 runs one statement with the sqlite3 tool on the file at path.
func runSQLite3(t *testing.T, tool, path, statement string) string {
	t.Helper()
	out, err := exec.Command(tool, "-batch", "-readonly", path, statement).CombinedOutput() //nolint:gosec // G204: the tool and the database are this test's own.
	if err != nil {
		t.Fatalf("sqlite3 %q: %v: %s", statement, err, out)
	}
	return strings.TrimSpace(string(out))
}

// literal returns query with each ? replaced by its argument as an SQL
// literal, for a tool that takes no parameters. The arguments are this
// test's own UIDs, labels and numbers.
func literal(query string, args []any) string {
	var b strings.Builder
	for _, c := range query {
		if c == '?' && len(args) > 0 {
			switch v := args[0].(type) {
			case string:
				b.WriteString("'" + strings.ReplaceAll(v, "'", "''") + "'")
			default:
				fmt.Fprint(&b, v)
			}
			args = args[1:]
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// catalogProbes returns the labels of about a thousand scans spread over
// the tenant, each with one instance it holds, for the timings.
func catalogProbes(t *testing.T, db *sql.DB) []testkit.CatalogProbe {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM scans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT c.subject_label, c.session_label, c.label,
		(SELECT min(sop_instance_uid) FROM instances WHERE scan_id = c.id) FROM scans AS c WHERE c.id % ? = 0`,
		max(1, n/1000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []testkit.CatalogProbe
	for rows.Next() {
		var p testkit.CatalogProbe
		if err := rows.Scan(&p.Path.Subject, &p.Path.Session, &p.Path.Scan, &p.UID); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
