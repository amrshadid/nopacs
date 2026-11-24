package postgres

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// studyInstancesSQL is the first page of a study's instances, scan by scan
// in order of id, each scan's in order of SOP Instance UID: scans_by_study,
// then instances_by_scan for each scan, so the page stops at its limit.
const studyInstancesSQL = `SELECT s.series_uid, i.sop_instance_uid, i.storage_key, s.subject_label, s.session_label, s.label, i.size
	FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
	WHERE s.study_uid = $1 ORDER BY s.id, i.sop_instance_uid LIMIT 1001`

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

// largeTable is the rows past which a sequential scan of the scans or
// sessions table fails a plan.
const largeTable = 10000

// TestScaleCatalog builds a synthetic tenant of NOPACS_SCALE_INSTANCES
// instances through the port (testkit.GenerateTenant) and measures what a
// catalog that large costs on Postgres, in one table per kind as the
// schema has it: the rate of reception into an empty tenant and into the
// large one, the size of each table and index, both plans of every
// statement of the catalog's reads (checkCatalogPlans) and the time of each
// read through the port (testkit.TimeCatalogReads), the plan Postgres makes
// for the values used (EXPLAIN ANALYZE) and its generic plan, and the time
// (median, 99th percentile) of each read the catalog's services will make
// (an instance by its UID alone, a scan's instances paged, a study's
// instances, a session by study UID, a subject's sessions and a session's
// scans by label, a page of subjects), and the time of VACUUM ANALYZE of
// the large tables. A read that scans a large table whole fails the test;
// so does a page past scaleListBound. It is skipped unless
// NOPACS_SCALE_INSTANCES is set; with NOPACS_SCALE_KEEP the schema is kept
// and named, for psql.
func TestScaleCatalog(t *testing.T) {
	n := testkit.ScaleInstances(t)
	skipScale(t)
	ctx := context.Background()
	base := testAddress(t)
	var address string
	if testkit.ScaleKeep() {
		schema := randomName(t, "scale_")
		mustExec(t, admin(t, base), `CREATE SCHEMA `+schema)
		address = withParams(t, base, map[string]string{"search_path": schema, "pool_max_conns": "8"})
		t.Logf("the tenant is kept in schema %s", schema)
	} else {
		address = withParams(t, newSchema(t), map[string]string{"pool_max_conns": "8"})
	}
	s := openTenantAt(t, address, "scale")
	conn := admin(t, address)
	profiles := scaleProfiles(t)
	sum := testkit.NewSummary(t, fmt.Sprintf("Catalog on Postgres, %d instances", n))

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
		sum.Add("build", fmt.Sprintf("%d instances in %v (%.0f instances/s, 4 writers)", n, time.Since(began).Round(time.Second),
			float64(g.Instances)/time.Since(began).Seconds()))
	}
	for _, table := range []string{"instances", "received_files"} {
		began := time.Now()
		mustExec(t, conn, `VACUUM (ANALYZE) `+table)
		sum.Add("VACUUM (ANALYZE) "+table, time.Since(began).Round(time.Millisecond).String())
	}
	mustExec(t, conn, `ANALYZE`)
	logSizes(t, conn, sum)

	// The catalog's reads as its services make them: both plans of every
	// statement checked (checkCatalogPlans; a table under largeTable rows
	// may be read whole), and the time of each read through the port.
	checkCatalogPlans(t, conn, sampleCatalog(t, conn), largeTable)
	testkit.TimeCatalogReads(t, s, catalogProbes(t, conn), scaleProbes, sum, scaleListBound)

	scans := sampleScans(t, conn)
	instances := sampleInstances(t, conn, scans)
	pick := func(r *rand.Rand) scanRow { return scans[r.IntN(len(scans))] }
	queries := []catalogQuery{
		{"an instance by its UID alone", `SELECT s.storage, i.storage_key, s.subject_label, s.session_label, s.label, i.byte_offset,
			i.byte_length, i.size, coalesce(i.archived_sha256, i.sha256), s.series_uid, s.study_uid
			FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid = $1`,
			func(r *rand.Rand) []any { return []any{instances[r.IntN(len(instances))]} }},
		{"a scan's first page of instances", scanInstancesSQL, func(r *rand.Rand) []any { return []any{pick(r).series, "", 1001} }},
		{"a scan's page after a cursor", scanInstancesSQL, func(r *rand.Rand) []any { return []any{pick(r).series, "2.25.5", 1001} }},
		{"a study's instances, first page", studyInstancesSQL,
			func(r *rand.Rand) []any { return []any{pick(r).study} }},
		{"a session by study UID", sessionLabelsSQL, func(r *rand.Rand) []any { return []any{pick(r).study} }},
		{"a scan by series UID", `SELECT label, instances, bytes FROM scans WHERE series_uid = $1`,
			func(r *rand.Rand) []any { return []any{pick(r).series} }},
		{"a subject's sessions by label", `SELECT se.label, se.study_uid FROM subjects AS su JOIN sessions AS se ON se.subject_id = su.id
			WHERE su.folded = $1 ORDER BY se.folded LIMIT 51`,
			func(r *rand.Rand) []any { return []any{pick(r).subject} }},
		{"a session's scans by labels", `SELECT s.label, s.series_uid, s.instances FROM subjects AS su
			JOIN sessions AS se ON se.subject_id = su.id JOIN scans AS s ON s.session_id = se.id
			WHERE su.folded = $1 AND se.folded = $2 ORDER BY s.folded LIMIT 1001`,
			func(r *rand.Rand) []any { p := pick(r); return []any{p.subject, p.session} }},
		{"a page of subjects by label", `SELECT label FROM subjects WHERE folded > $1 ORDER BY folded LIMIT 51`,
			func(r *rand.Rand) []any { n := r.IntN(26); return []any{"sub-" + letters[n:n+1]} }},
	}
	// The instance and received file tables are large at any size; the
	// scans and sessions grow four hundred and a few thousand times more
	// slowly, and a sequential scan of a few thousand rows is the planner's
	// right choice, so they count as large only past largeTable rows.
	largeNames := []string{"instances", "received_files"}
	for _, table := range []string{"scans", "sessions"} {
		var rows int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows > largeTable {
			largeNames = append(largeNames, table)
		}
	}
	large := regexp.MustCompile(`Seq Scan on (` + strings.Join(largeNames, "|") + `)\b`)
	r := rand.New(rand.NewPCG(2, 3)) // #nosec G404 -- a seeded generator, so a run probes the same rows as the last.
	for _, q := range queries {
		args := q.args(r)
		for _, generic := range []bool{false, true} {
			plan := planOf(t, connQuerier{conn}, generic, q.sql, args...)
			what := q.what
			if generic {
				what += " (generic)"
			}
			t.Logf("%s, plan:\n%s", what, plan)
			if m := large.FindString(plan); m != "" {
				t.Errorf("%s reads a large table whole: %s", what, m)
			}
		}
		d := make([]time.Duration, 0, scaleProbes)
		for range scaleProbes {
			args := q.args(r)
			began := time.Now()
			rows, err := s.pool.Query(ctx, q.sql, args...)
			if err != nil {
				t.Fatalf("%s: %v", q.what, err)
			}
			for rows.Next() {
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
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
	began := time.Now()
	mustExec(t, conn, `VACUUM (ANALYZE) instances`)
	sum.Add("VACUUM (ANALYZE) instances after the last slice", time.Since(began).Round(time.Millisecond).String())
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
func sampleScans(t *testing.T, conn *pgx.Conn) []scanRow {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT s.series_uid, se.study_uid, su.folded, se.folded FROM scans AS s
		JOIN sessions AS se ON se.id = s.session_id JOIN subjects AS su ON su.id = se.subject_id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (scanRow, error) {
		var r scanRow
		err := row.Scan(&r.series, &r.study, &r.subject, &r.session)
		return r, err
	})
	if err != nil {
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
func sampleInstances(t *testing.T, conn *pgx.Conn, scans []scanRow) []string {
	t.Helper()
	var out []string
	for i := 0; i < len(scans); i += max(1, len(scans)/1000) {
		rows, err := conn.Query(context.Background(), `SELECT i.sop_instance_uid FROM scans AS s JOIN instances AS i ON i.scan_id = s.id
			WHERE s.series_uid = $1 ORDER BY i.sop_instance_uid LIMIT 2`, scans[i].series)
		if err != nil {
			t.Fatal(err)
		}
		uids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, uids...)
	}
	return out
}

// logSizes logs, and adds to sum past a megabyte, the bytes of each table and index of the schema.
func logSizes(t *testing.T, conn *pgx.Conn, sum *testkit.Summary) {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT c.relname, c.relkind::text, pg_relation_size(c.oid)
		FROM pg_class AS c WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind IN ('r', 'i')
		ORDER BY 3 DESC LIMIT 20`)
	if err != nil {
		t.Fatal(err)
	}
	type size struct {
		name, kind string
		bytes      int64
	}
	sizes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (size, error) {
		var s size
		err := row.Scan(&s.name, &s.kind, &s.bytes)
		return s, err
	})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range sizes {
		lines = append(lines, fmt.Sprintf("%-32s %s %8.1f MB", s.name, s.kind, float64(s.bytes)/1e6))
		if s.bytes > 1e6 {
			sum.Add("size of "+s.name, fmt.Sprintf("%.1f MB", float64(s.bytes)/1e6))
		}
	}
	t.Logf("sizes (r table, i index):\n  %s", strings.Join(lines, "\n  "))
}

// catalogProbes returns the labels of about a thousand scans spread over
// the tenant, each with one instance it holds, for the timings.
func catalogProbes(t *testing.T, conn *pgx.Conn) []testkit.CatalogProbe {
	t.Helper()
	ctx := context.Background()
	var n int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM scans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT c.subject_label, c.session_label, c.label,
		(SELECT min(sop_instance_uid) FROM instances WHERE scan_id = c.id) FROM scans AS c WHERE c.id % $1 = 0`,
		max(1, n/1000))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (testkit.CatalogProbe, error) {
		var p testkit.CatalogProbe
		err := row.Scan(&p.Path.Subject, &p.Path.Session, &p.Path.Scan, &p.UID)
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
