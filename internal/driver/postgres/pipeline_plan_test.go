package postgres

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// scaleEnv is the variable that makes the scale tests run (docs/testing.md).
const scaleEnv = "NOPACS_TEST_SCALE"

// skipScale skips a test at millions of rows except in the run made for it
// (go test -run with the test's name, without -short and without the race
// detector): under -short, under the race detector (which it would measure
// instead of the database), on 32 bits and on Windows, where nothing in
// Postgres's planner differs.
func skipScale(t *testing.T) {
	t.Helper()
	switch {
	case testing.Short():
		t.Skip("millions of rows: skipped under -short")
	case os.Getenv(scaleEnv) == "":
		t.Skip("millions of rows: run by make test-scale, which sets " + scaleEnv)
	case raceDetector:
		t.Skip("millions of rows: run without the race detector")
	case runtime.GOARCH == "386":
		t.Skip("millions of rows: run on 64 bits")
	case runtime.GOOS == "windows":
		t.Skip("millions of rows: measured on Linux and macOS, not Windows")
	}
}

// querier is what a pgx connection and a transaction share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Conn() *pgx.Conn
}

// planOf returns the plan Postgres makes for statement. With generic false
// it is run for real (EXPLAIN ANALYZE) with the values given; with generic
// true Postgres plans it for any value of its parameters (EXPLAIN
// GENERIC_PLAN, Postgres 16 and later), the plan a prepared statement
// settles on after a few runs, which is what pgx's statement cache keeps.
// That text is sent as it is, by the simple protocol of the connection
// beneath pgx, since pgx would refuse parameters it is given no values for.
func planOf(t *testing.T, q querier, generic bool, statement string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	if generic {
		results, err := q.Conn().PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+statement).ReadAll()
		if err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
		var lines []string
		for _, r := range results {
			for _, row := range r.Rows {
				lines = append(lines, string(row[0]))
			}
		}
		return strings.Join(lines, "\n")
	}
	rows, err := q.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) `+statement, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, statement)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// connQuerier is a connection as a querier.
type connQuerier struct{ c *pgx.Conn }

// Query runs a query on the connection.
func (q connQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.c.Query(ctx, sql, args...)
}

// Conn returns the connection.
func (q connQuerier) Conn() *pgx.Conn { return q.c }

// scaleListBound is the longest a page of a list may take at scale. A page
// read from the index its filters need takes milliseconds; one that tests
// each row of a wider index, or sorts the rows it kept, takes the better
// part of a second at these sizes.
const scaleListBound = 250 * time.Millisecond

// TestScalePipelinePlans guards that every list, claim and lookup of the
// pipeline is answered from an index at the size of a tenant that has
// worked for years: two million receptions (nearly all ended, at one
// receiver), a million quarantine records (nearly all released), two
// million instances held and a hundred thousand claimed by one reception,
// a million received files, and a million finished units of work. For each
// statement the driver sends, Postgres's plan for the values used and its
// generic plan (the one a prepared statement keeps) read no large table
// whole and sort no rows but those of a merge of parts each cut to its
// limit; every combination of filters the API contract allows is listed, a
// page of 50 in less than scaleListBound. The plans are logged.
func TestScalePipelinePlans(t *testing.T) {
	skipScale(t)
	ctx := context.Background()
	address := newSchema(t)
	s := openTenantAt(t, address, "scale")
	conn := admin(t, address)
	start := time.Now()
	mustExec(t, conn,
		`SET maintenance_work_mem = '256MB'`,
		`INSERT INTO storages SELECT 'store', 'directory', 'fast', '', '', '', '', '', false, '', NULL, 0, now(), now()`,
		`INSERT INTO receivers SELECT 'r' || n, 'dimse', true, 'store', 11000 + n, '', '{}', '', 60000000, 3600000000, 1024, now(), now()
		 FROM generate_series(1, 3) n`,
		`INSERT INTO receptions SELECT 'rc' || i, CASE WHEN i % 100 = 0 THEN 'other' ELSE 'busy' END, '2.25.1.' || (i / 4), i % 4 + 1,
		   CASE WHEN i % 100000 = 0 THEN 'receiving' WHEN i % 50000 = 1 THEN 'failed' WHEN i % 70000 = 2 THEN 'quarantined' ELSE 'done' END,
		   i % 100000 <> 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', CASE WHEN i % 100000 = 0 THEN '' ELSE 'store' END, '',
		   timestamptz 'epoch' + i * interval '1 second', timestamptz 'epoch' + i * interval '1 second', timestamptz 'epoch' + i * interval '1 second',
		   CASE WHEN i % 100000 = 0 THEN NULL ELSE timestamptz 'epoch' + i * interval '1 second' END, 0, '[]', false, '', '', '', NULL, ''
		 FROM generate_series(1, 2000000) i`,
		`INSERT INTO receptions SELECT 'claimer', 'busy', '2.25.1.900000000', 1, 'identified', false, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		   'sub-01', 'ses-1', 'store', '', now(), now(), now(), NULL, 0, '[]', false, '', '', '', NULL, ''`,
		`INSERT INTO quarantine SELECT 'q' || i, CASE WHEN i % 3 = 0 THEN 'not_dicom' ELSE 'different_file' END, '{}',
		   CASE WHEN i % 10000 = 0 THEN 'held' ELSE 'released' END, CASE WHEN i % 100 = 0 THEN 'other' ELSE 'busy' END, 'rc' || i,
		   '', '', '', 10, NULL, NULL, '', '2.25.3.' || i, '2.25.1.' || (i / 4), '', '', 'quarantine/q' || i, i % 10000 = 0,
		   timestamptz 'epoch' + i * interval '1 second', CASE WHEN i % 10000 = 0 THEN NULL ELSE now() END,
		   CASE WHEN i % 10000 = 0 THEN '' ELSE 'as_is' END, '', '', '', '', '', false, ''
		 FROM generate_series(1, 1000000) i`,
		`INSERT INTO subjects (label, folded, created) SELECT 'sub-' || n, 'sub-' || n, now() FROM generate_series(1, 1000) n`,
		`INSERT INTO sessions (study_uid, subject_id, label, folded, storage, created)
		 SELECT '2.25.1.' || n, n % 1000 + 1, 'ses-' || n, 'ses-' || n, 'store', now() FROM generate_series(1, 2000) n`,
		`INSERT INTO scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, session_label, session_folded,
		   storage, study_uid)
		 SELECT n, '2.25.2.' || n, 'scan-1', 'scan-1', 1000, 1000000, NULL, now(), now(), 'ses-' || n, 'ses-' || n, 'store', '2.25.1.' || n
		 FROM generate_series(1, 2000) n`,
		`INSERT INTO instances SELECT '2.25.3.' || i, i / 1000 + 1, 'rc' || (i / 1000 + 1), sha256(i::text::bytea), NULL, '', 0, 1000, 1000, now()
		 FROM generate_series(0, 1999999) i`,
		`INSERT INTO reception_scans SELECT 'rc' || n, n, 1000, 1000000 FROM generate_series(1, 2000) n`,
		`INSERT INTO instance_claims SELECT '2.25.4.' || i, 'claimer', '2.25.1.900000000', '2.25.2.900000000', sha256(i::text::bytea), now()
		 FROM generate_series(1, 100000) i`,
		`INSERT INTO instance_claims SELECT '2.25.5.' || i, 'busy' || (i % 900), '2.25.1.' || (i % 900), '2.25.2.' || (i % 900),
		   sha256(i::text::bytea), now() FROM generate_series(1, 900000) i`,
		`INSERT INTO received_files SELECT 'rc' || (i / 1000 + 1), '2.25.3.' || i, sha256(i::text::bytea), '2.25.2.1', '2.25.1.1',
		   '1.2.840.10008.5.1.4.1.1.4', '1.2.840.10008.1.2.1', 1000, 'in/' || i, '', '', '', '', false, now(), false FROM generate_series(0, 999999) i`,
		`INSERT INTO received_files SELECT 'claimer', '2.25.4.' || i, sha256(i::text::bytea), '2.25.2.900000000', '2.25.1.900000000',
		   '1.2.840.10008.5.1.4.1.1.4', '1.2.840.10008.1.2.1', 1000, 'claim/' || i, '', '', '', '', false, now(), false FROM generate_series(1, 100000) i`,
		`INSERT INTO work SELECT 'done' || i, 'archive', '', 'rc' || i, NULL, 'done', timestamptz 'epoch' + i * interval '1 second', 1, 10, '', '', now(), now()
		 FROM generate_series(1, 1000000) i`,
		`INSERT INTO work SELECT 'q' || i, 'archive', '', '', 'key' || i, 'queued', timestamptz 'epoch' + i * interval '1 second', 0, 10, '', '', now(), now()
		 FROM generate_series(1, 1000) i`,
		// A feed of two million events, nearly all of one type, pruned up
		// to its first hundred thousand, with a checkpoint an hour.
		`INSERT INTO events (type, at, payload, key) OVERRIDING SYSTEM VALUE
		 SELECT CASE WHEN i % 1000 = 0 THEN 'storage_space' ELSE 'reception_step' END, timestamptz 'epoch' + i * interval '1 second', '{}',
		   CASE WHEN i % 10 = 0 THEN 'k' || i END FROM generate_series(100001, 2100000) i`,
		`UPDATE event_marks SET pruned_through = 100000`,
		`INSERT INTO event_checkpoints SELECT timestamptz 'epoch' + h * interval '1 hour', h * 3600 FROM generate_series(28, 583) h`,
		`ANALYZE`)
	t.Logf("built in %v", time.Since(start))

	// The lookups inUse runs are planned for their values every time, so
	// only that plan is theirs.
	planned := map[string]bool{storageUsedSQL: true, receptionNamesReceiverSQL: true, quarantineNamesReceiverSQL: true,
		storageHoldsSQL: true, sessionsOnStorageSQL: true, receiverOfStorageSQL: true}
	// bad reports why a plan is not one read from an index: a sequential
	// scan of a table other than the few rows of a configuration table, or a
	// sort of many rows (more than sortBound, by the rows Postgres counted,
	// or for a generic plan the rows it expects) that is not a merge of parts
	// each cut to its limit. A sort of a few rows is a plan that read a
	// narrower index than the one in the list's order, which the planner may
	// rightly prefer (a study's receptions, the receptions in progress);
	// allowSort is a statement that reads every row of one reception, as a
	// count or a group does.
	const sortBound = 1000
	// event_marks is one row; event_checkpoints is one row an hour of the
	// retention period, those that can prune nothing more removed.
	smallTables := []string{"storages", "receivers", "catalog_totals", "quarantine_totals", "event_marks", "event_checkpoints"}
	rowsOf := regexp.MustCompile(`rows=([0-9]+)`)
	bad := func(plan string, allowSort bool) string {
		for _, line := range strings.Split(plan, "\n") {
			l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "->"))
			if _, table, found := strings.Cut(l, "Seq Scan on "); found && !slices.Contains(smallTables, strings.Fields(table)[0]) {
				return "it reads a table whole: " + l
			}
			if (strings.HasPrefix(l, "Sort (") || strings.HasPrefix(l, "Sort  (") || strings.HasPrefix(l, "Incremental Sort (")) &&
				!allowSort && !strings.Contains(plan, "Append") {
				m := rowsOf.FindStringSubmatch(l)
				if m == nil {
					return "it sorts rows it does not count: " + l
				}
				if n, _ := strconv.Atoi(m[1]); n > sortBound {
					return "it sorts many rows instead of reading them in order: " + l
				}
			}
		}
		return ""
	}
	check := func(what string, q querier, want, statement string, allowSort bool, args ...any) {
		t.Helper()
		plans := []bool{false, true}
		if planned[statement] {
			plans = plans[:1]
		}
		for _, generic := range plans {
			plan := planOf(t, q, generic, statement, args...)
			if generic {
				what += " (generic)"
			}
			t.Logf("%s:\n%s", what, plan)
			if why := bad(plan, allowSort); why != "" {
				t.Errorf("%s: %s\n%s", what, why, plan)
			}
			found := want == ""
			for _, alt := range strings.Split(want, "|") {
				found = found || strings.Contains(plan, alt)
			}
			if !found {
				t.Errorf("%s: the plan does not read %s\n%s", what, want, plan)
			}
		}
	}

	uids := make([]string, 1000)
	sums := make([][]byte, 1000)
	for i := range uids {
		uids[i] = fmt.Sprintf("2.25.4.%d", i*97%100000+1)
		sums[i] = make([]byte, 32)
	}
	when := time.Unix(1000000, 0).UTC()
	for _, c := range []struct {
		what, want, statement string
		args                  []any
		allowSort             bool
	}{
		{"a storage in use by a reception", "receptions_by_storage", storageUsedSQL, []any{"store"}, false},
		{"a receiver's name held by a reception", "receptions_by_receiver", receptionNamesReceiverSQL, []any{"gone"}, false},
		{"a receiver's name held by a record", "quarantine_by_receiver", quarantineNamesReceiverSQL, []any{"gone"}, false},
		{"the open reception of a study", "receptions_open", openReceptionSQL, []any{"busy", "2.25.1.4000"}, false},
		{"a study's next arrival", "receptions_arrival", nextArrivalSQL, []any{"2.25.1.4000"}, false},
		{"a study's arrivals", "receptions_arrival", arrivalsSQL, []any{"2.25.1.4000", 100}, false},
		{"a reception's received files", "received_files_pkey", listReceivedSQL, []any{"claimer", "", []byte{}, 1001}, false},
		{"the files a discard removes", "received_files_pkey", discardedSQL, []any{"claimer", "", []byte{}, 1001}, false},
		{"the count of what a discard removes", "received_files_pkey", discardCountSQL, []any{"rc500"}, true},
		{"a unit under its key", "work_by_key", workByKeySQL, []any{"key5"}, false},
		{"the next unit to claim", "work_claimable", claimNextSQL, []any{"archive", when}, false},
		{"a study's session", "sessions_study_uid_key", sessionLabelsSQL, []any{"2.25.1.1500"}, false},
		{"the scans of series", "scans_series_uid_key", scanLabelsSQL, []any{[]string{"2.25.2.5", "2.25.2.7"}}, false},
		{"the scans a reception confirmed", "reception_scans_pkey", receptionScansSQL, []any{"rc501", 10001}, true},
		{"a storage holding no instance", "scans_by_storage", storageHoldsSQL, []any{"other"}, false},
		{"a storage no session is on", "sessions_by_storage", sessionsOnStorageSQL, []any{"other"}, false},
		// The receptions not ended are few by nature; whichever partial index of
		// them is read, a sort of those few is allowed.
		{"receptions without work", "receptions_in_progress|receptions_by_storage", receptionsWithoutWorkSQL, []any{time.Unix(0, 0).UTC(), "", 1001}, true},
		{"a batch's instance rows, held", "instances_pkey", instanceRowsSQL, []any{uids}, false},
		{"a batch's instance rows, claimed", "instance_claims_pkey", instanceRowsSQL, []any{uids}, false},
		{"the received files a claim names", "received_files_pkey", receivedOfClaimSQL, []any{"claimer", uids, sums}, false},
		{"a page of events", "events_pkey", eventsAfterSQL, []any{int64(1500000), 1000}, false},
		{"a page of a rare type of events", "events_by_type", eventsOfType(tenant.EventStorageSpace), []any{int64(1500000), 1000}, false},
		{"a page of the common type of events", "events_by_type|events_pkey", eventsOfType(tenant.EventReceptionStep), []any{int64(1500000), 1000}, false},
		{"the feed's place", "events_pkey", eventHighSQL, nil, false},
		{"a step of pruning", "events_pkey", pruneStepSQL, []any{int64(1000000), 200}, false},
		{"the checkpoint pruning reads", "", pruneTargetSQL, []any{time.Unix(1000000, 0).UTC()}, false},
	} {
		check(c.what, connQuerier{conn}, c.want, c.statement, c.allowSort, c.args...)
	}
	// A discard's test of released places is one probe for each file. Turned
	// into a join, it may read every released record once instead, which
	// passes the checks above when it reads them through an index, and
	// grows with the records a tenant has ever released.
	for _, c := range []struct {
		what, statement string
		args            []any
	}{
		{"the files a discard removes, released places", discardedSQL, []any{"claimer", "", []byte{}, 1001}},
		{"the count of what a discard removes, released places", discardCountSQL, []any{"rc500"}},
	} {
		check(c.what, connQuerier{conn}, "SubPlan", c.statement, true, c.args...)
	}

	// A claim's insert and a commit's confirmation, and the release of the
	// claims of a reception with 100,000 of them, each run for real in a
	// transaction undone after.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newUIDs := make([]string, 1000)
	for i := range newUIDs {
		newUIDs[i] = fmt.Sprintf("2.25.9.%d", i)
	}
	studies, series := make([]string, 1000), make([]string, 1000)
	for i := range studies {
		studies[i], series[i] = "2.25.1.900000000", "2.25.2.900000000"
	}
	check("a claim's insert", tx, "", insertClaimsSQL, false, "claimer", when, newUIDs, studies, series, sums)
	check("a claim's check against what was held meanwhile", tx, "instances_pkey", heldAfterClaimSQL, false, newUIDs)
	check("the claims dropped for what was held meanwhile", tx, "instance_claims_pkey", dropClaimsSQL, false, newUIDs, "claimer")
	offs, lens := make([]int64, 1000), make([]int64, 1000)
	check("a commit's confirmation", tx, "instance_claims_pkey", confirmSQL, false, int64(1), when, uids, offs, lens, lens, sums,
		"claimer", "2.25.1.900000000", "2.25.2.900000000")
	check("a commit's confirmation of de-identified instances", tx, "instance_claims_pkey", confirmDeidentifiedSQL, false, int64(1), when,
		uids, offs, lens, lens, sums, "claimer", "2.25.1.900000000", "2.25.2.900000000", lens)
	began := time.Now()
	check("the release of 100,000 claims", tx, "instance_claims_by_reception", releaseClaimedSQL, false, "claimer")
	t.Logf("released 100,000 claims in %v", time.Since(began))
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
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
	since, until := time.Unix(500000, 0).UTC(), time.Unix(1500000, 0).UTC()
	after := &sqlkit.TimePosition{At: time.Unix(1200000, 0).UTC(), ID: "rc1200000"}
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
						for _, pos := range []*sqlkit.TimePosition{nil, after} {
							if q, a := receptionsQuery(f, pos, 51); q != "" {
								check(what, connQuerier{conn}, "receptions_", q, false, a...)
							}
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
		check(what, connQuerier{conn}, "quarantine_", q, false, a...)
		timed(what, func() (int, error) {
			got, _, err := s.ListQuarantined(ctx, f, master.Page{Limit: 50})
			return len(got), err
		})
		n++
	}
	t.Logf("%d combinations of filters checked", n)
}

// pipelineTables are the reception pipeline's tables of
// migrations/tenant/0001_baseline.sql.
var pipelineTables = []string{"catalog_totals", "instances", "quarantine", "quarantine_totals", "received_files", "reception_associations",
	"receivers", "receptions", "scans", "sessions", "storages", "subjects", "work"}

// TestPipelineTextIsByteOrdered guards that every text column of the
// pipeline's tables that an index or a constraint compares has the C
// collation, so order and uniqueness are byte by byte whatever collation
// the database was made with, as the lists' cursors and the claims' sorted
// order assume.
func TestPipelineTextIsByteOrdered(t *testing.T) {
	address := newSchema(t)
	openTenantAt(t, address, "coll")
	rows, err := admin(t, address).Query(context.Background(), `
		SELECT DISTINCT c.relname || '.' || a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = current_schema()
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		WHERE c.relname = ANY($1) AND a.atttypid = 'text'::regtype AND a.attcollation <> (SELECT oid FROM pg_collation WHERE collname = 'C' AND collnamespace = 'pg_catalog'::regnamespace)
		ORDER BY 1`, pipelineTables)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("indexed text columns without the C collation: %v", bad)
	}
}
