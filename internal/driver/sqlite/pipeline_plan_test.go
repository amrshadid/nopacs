package sqlite

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fillPipeline writes many rows into every pipeline table of a migrated
// tenant database at path, straight through SQL, and runs ANALYZE, so the
// planner chooses as it would on a tenant that has worked for a while.
func fillPipeline(tb testing.TB, path string) {
	tb.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200)
		 INSERT INTO storages SELECT 'store' || i, 'directory', 'fast', 'd' || i, '', '', '', '', 0, '', NULL, 0, 0, 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200)
		 INSERT INTO receivers SELECT 'recv' || i, 'dimse', 1, 'store' || i, 11000 + i, '', '{}', '', 60000000, 3600000000, 4294967296, 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10000)
		 INSERT INTO receptions SELECT 'rc' || i, 'recv' || (i % 200 + 1),
		   CASE i % 50 WHEN 0 THEN '2.25.9.' || i ELSE '2.25.1.' || (i % 5000 + 1) END, i / 5000 + 1,
		   CASE i % 50 WHEN 0 THEN 'receiving' WHEN 1 THEN 'complete' WHEN 2 THEN 'archived' WHEN 3 THEN 'failed' ELSE 'done' END,
		   CASE WHEN i % 50 >= 3 THEN 1 ELSE 0 END,
		   0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', CASE WHEN i % 50 >= 2 THEN 'store' || (i % 200 + 1) ELSE '' END, '',
		   i * 1000000, i * 1000000, i * 1000000, NULL, 0, '[]', 0, '', '', '', NULL, '' FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO received_files SELECT 'rc' || (i % 20000 + 1), '2.25.3.' || i, randomblob(32), '2.25.2.1', '2.25.1.1',
		   '1.2.840.10008.5.1.4.1.1.4', '1.2.840.10008.1.2.1', 1000, 'x/' || i, '', '', '', '', 0, 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10000)
		 INSERT INTO quarantine SELECT 'q' || i, CASE i % 3 WHEN 0 THEN 'not_dicom' WHEN 1 THEN 'bad_uid' ELSE 'different_file' END,
		   '{}', CASE i % 4 WHEN 0 THEN 'held' WHEN 1 THEN 'released' ELSE 'rejected' END, 'recv' || (i % 200 + 1), 'rc' || i,
		   '', '', '', 10, NULL, NULL, '', '2.25.3.' || i, '2.25.1.' || (i % 5000 + 1), '', '', '', 0, i * 1000000, NULL,
		   '', '', '', '', '', '', 0, '', '', '', '' FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 500)
		 INSERT INTO subjects (id, label, folded, created) SELECT i, 'sub-' || i, 'sub-' || i, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 5000)
		 INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created)
		 SELECT i, '2.25.1.' || i, i % 500 + 1, 'ses-' || i, 'ses-' || i, 'store' || (i % 200 + 1), 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, storage, study_uid)
		 SELECT i, i % 5000 + 1, '2.25.2.' || i, 'scan-' || i, 'scan-' || i, 5, 5000, NULL, 0, 0, 'store' || (i % 200 + 1),
		   '2.25.1.' || (i % 5000 + 1) FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO instances SELECT i % 20000 + 1, '2.25.3.' || i, 'rc' || (i % 10000 + 1), randomblob(32), NULL, '', 0, 1000, 1000, 0, NULL
		 FROM n WHERE i % 100 <> 0`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO instance_claims SELECT '2.25.3.' || i, 'rc' || (i % 10000 + 1), '2.25.1.' || (i % 5000 + 1),
		   '2.25.2.' || (i % 20000 + 1), randomblob(32), 0 FROM n WHERE i % 100 = 0`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO reception_scans SELECT 'rc' || (i % 10000 + 1), i % 20000 + 1, 1, 1000 FROM n WHERE i % 100 <> 0
		 ON CONFLICT DO NOTHING`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO work SELECT 'w' || i, 'reception', 'rc' || i, 'rc' || i, NULL, 'done', i, 1, 10, '', '', 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100)
		 INSERT INTO work SELECT 'live' || i, 'reception', 'rc' || i, 'rc' || i, 'key' || i, 'queued', i, 0, 10, '', '', 0, 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO events (type, at, payload, key) SELECT CASE i % 10 WHEN 0 THEN 'receiver_health' ELSE 'reception_step' END,
		   i, '{}', NULL FROM n`,
		`ANALYZE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			tb.Fatalf("%s: %v", stmt, err)
		}
	}
}

// queryPlan returns SQLite's plan of query, one line per step.
func queryPlan(tb testing.TB, db *sql.DB, query string, args ...any) []string {
	tb.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		tb.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			tb.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return plan
}

// TestPipelinePlans guards that every list, claim and lookup of the
// pipeline, and every read of the event feed with its pruning, is answered
// from an index, never by reading a whole table: it
// fills every table, runs ANALYZE, and checks SQLite's plan of the very
// statement the driver runs for a step that reads no table without an
// index, and that the index named for it is the one used.
func TestPipelinePlans(t *testing.T) {
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	after := []any{int64(5000000), "rc5"}
	severalTypes, severalArgs := eventsQuery([]tenant.EventType{tenant.EventReceiverHealth, tenant.EventStorageSpace}, 5000, 1000)
	rq := func(f tenant.ReceptionFilter) (string, []any) { return receptionsQuery(f, after, 101) }
	qq := func(f tenant.QuarantineFilter) (string, []any) { return quarantineQuery(f, after, 101) }
	cases := []struct {
		name, index string
		query       string
		args        []any
	}{
		{"ListStorages", "sqlite_autoindex_storages_1", listStoragesSQL, []any{"store5", 101}},
		{"ListReceivers", "sqlite_autoindex_receivers_1", listReceiversSQL, []any{"recv5", 101}},
		{"a storage in use", "receptions_by_storage", storageUsedSQL, []any{"store5"}},
		{"a receiver of a storage", "receivers_by_storage", receiverOfStorageSQL, []any{"store5"}},
		{"the receivers on a port", "receivers_by_port", receiversOnPortSQL, []any{11005, "recv5"}},
		{"a receiver named by a reception", "receptions_by_receiver", receptionNamesReceiverSQL, []any{"recv5"}},
		{"a receiver named by a quarantined file", "quarantine_by_receiver", quarantineNamesReceiverSQL, []any{"recv5"}},
		{"OpenReception", "receptions_open", openReceptionSQL, []any{"recv5", "2.25.1.5"}},
		{"the next arrival", "receptions_arrival", nextArrivalSQL, []any{"2.25.1.5"}},
		{"a study's arrivals", "receptions_arrival", arrivalsSQL, []any{"2.25.1.5", 100}},
		{"ReceivedFiles", "PRIMARY KEY", listReceivedSQL, []any{"rc5", "", []byte{}, 101}},
		{"CheckDuplicates, the held instances", "instances_by_uid", duplicatesSQL, []any{`["2.25.3.5","2.25.3.6"]`}},
		{"CheckDuplicates, the claims", "PRIMARY KEY", duplicatesSQL, []any{`["2.25.3.5","2.25.3.6"]`}},
		{"an instance's row, for a claim", "instances_by_uid", instanceRowSQL, []any{"2.25.3.5"}},
		{"an instance's claim, for a claim", "PRIMARY KEY", instanceRowSQL, []any{"2.25.3.5"}},
		{"a claim confirmed", "PRIMARY KEY", takeClaimSQL, []any{"2.25.3.5", "rc5", "2.25.1.5", "2.25.2.5"}},
		{"a reception's claims released", "instance_claims_by_reception", releaseClaimedSQL, []any{"rc5"}},
		{"a storage a session names", "sessions_by_storage", sessionsOnStorageSQL, []any{"store5"}},
		{"ReceptionsWithoutWork", "receptions_in_progress", receptionsWithoutWorkSQL, []any{int64(-1), "", 101}},
		{"ReceptionsWithoutWork, a reception's live unit", "work_by_reception", receptionsWithoutWorkSQL, []any{int64(-1), "", 101}},
		{"a storage that holds data", "scans_by_storage", storageHoldsSQL, []any{"store5"}},
		{"ReceptionScans", "PRIMARY KEY", receptionScansSQL, []any{"rc5", 10001}},
		{"CatalogLabels, the session", "sqlite_autoindex_sessions_1", sessionLabelsSQL, []any{"2.25.1.5"}},
		{"CatalogLabels, the scans", "sqlite_autoindex_scans_1", scanLabelsSQL, []any{`["2.25.2.5","2.25.2.6"]`}},
		{"DiscardedFiles", "quarantine_kept", discardedSQL, []any{"rc5", "", []byte{}, 101}},
		{"a discard's count", "quarantine_kept", discardCountSQL, []any{"rc5"}},
		{"ClaimWork", "work_claimable", claimNextSQL, []any{"reception", int64(50), claimCandidates}},
		{"a unit by its key", "work_by_key", workByKeySQL, []any{"key5"}},
		// The event feed (pipeline_events.go).
		{"a read of events after a cursor", "INTEGER PRIMARY KEY", eventsAfterSQL, []any{5000, 1000}},
		{"a read of events of one type", "events_by_type", eventsOfTypeSQL, []any{"receiver_health", 5000, 1000}},
		{"a read of events of several types", "events_by_type", severalTypes, severalArgs},
		{"the feed's place", "event_marks", eventHighSQL, nil},
		{"the pruning", "INTEGER PRIMARY KEY", pruneSQL, []any{100}},
		{"the incoming bytes", "receptions_holding", incomingBytesSQL, nil},
	}
	for _, c := range []struct {
		name, index string
		f           tenant.ReceptionFilter
	}{
		{"ListReceptions", "receptions_by_start", tenant.ReceptionFilter{}},
		{"ListReceptions by time", "receptions_by_start", tenant.ReceptionFilter{Since: fromMicros(1000000), Until: fromMicros(9000000000)}},
		{"ListReceptions by receiver", "receptions_by_receiver", tenant.ReceptionFilter{Receiver: "recv5"}},
		{"ListReceptions by study", "receptions_by_study", tenant.ReceptionFilter{StudyUID: "2.25.1.5"}},
		{"ListReceptions by step", "receptions_by_state", tenant.ReceptionFilter{States: []reception.State{reception.Failed}}},
		{"ListReceptions by steps", "receptions_by_state", tenant.ReceptionFilter{States: []reception.State{reception.Failed, reception.Archived}}},
		{"ListReceptions in progress", "receptions_in_progress", tenant.ReceptionFilter{InProgress: true}},
	} {
		q, a := rq(c.f)
		cases = append(cases, struct {
			name, index string
			query       string
			args        []any
		}{c.name, c.index, q, a})
	}
	for _, c := range []struct {
		name, index string
		f           tenant.QuarantineFilter
	}{
		{"ListQuarantined", "quarantine_by_time", tenant.QuarantineFilter{}},
		{"ListQuarantined by state", "quarantine_by_state", tenant.QuarantineFilter{State: tenant.QuarantineHeld}},
		{"ListQuarantined by reason", "quarantine_by_reason", tenant.QuarantineFilter{Reason: tenant.QuarantineBadUID}},
		{"ListQuarantined by receiver", "quarantine_by_receiver", tenant.QuarantineFilter{Receiver: "recv5"}},
		{"ListQuarantined by reception", "quarantine_by_reception", tenant.QuarantineFilter{ReceptionID: "rc5"}},
		{"ListQuarantined by study", "quarantine_by_study", tenant.QuarantineFilter{StudyUID: "2.25.1.5"}},
		{"ListQuarantined by instance", "quarantine_by_instance", tenant.QuarantineFilter{SOPInstanceUID: "2.25.3.5"}},
	} {
		q, a := qq(c.f)
		cases = append(cases, struct {
			name, index string
			query       string
			args        []any
		}{c.name, c.index, q, a})
	}
	for _, c := range cases {
		plan := queryPlan(t, db, c.query, c.args...)
		text := strings.Join(plan, "\n")
		t.Logf("%s:\n%s", c.name, text)
		for _, line := range plan {
			// A scan of a subquery's rows (each bounded by its own LIMIT) or
			// of json_each's array is not a scan of a table.
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "USING ") && !strings.Contains(line, "json_each") &&
				!strings.HasPrefix(line, "SCAN (subquery") && !strings.Contains(line, "CONSTANT ROW") {
				t.Errorf("%s reads a whole table: %q", c.name, line)
			}
		}
		if !strings.Contains(text, " "+c.index) {
			t.Errorf("%s does not use %s", c.name, c.index)
		}
	}
}

// TestPipelineSchemaWithSQLite guards the pipeline's schema as SQLite's own
// command line tool, not this package, reads it from a migrated tenant
// database: every table is there, STRICT, with the indexes the listings and
// claims rely on, and the file passes SQLite's integrity check; the
// triggers keep the receptions per step as the tool writes, and the feed's
// ids never go back. It is
// skipped without the tool (SQLITE3 names one, or it is looked up on PATH),
// unless NOPACS_SQLITE3_REQUIRED is set.
func TestPipelineSchemaWithSQLite(t *testing.T) {
	tool := os.Getenv("SQLITE3")
	if tool == "" {
		tool = "sqlite3"
	}
	tool, err := exec.LookPath(tool)
	if err != nil {
		if os.Getenv("NOPACS_SQLITE3_REQUIRED") != "" {
			t.Fatalf("the sqlite3 tool is required: %v", err)
		}
		t.Skipf("the sqlite3 tool is not here: %v", err)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(query string) string {
		t.Helper()
		out, err := exec.Command(tool, "-batch", "-noheader", path, query).CombinedOutput() //nolint:gosec // G204, G702: the tool and the database are this test's own.
		if err != nil {
			t.Fatalf("sqlite3 %q: %v: %s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Logf("schema:\n%s", run(".schema"))
	if got := run("PRAGMA integrity_check"); got != "ok" {
		t.Errorf("integrity check: %s", got)
	}
	tables := run(`SELECT name || ' ' || strict FROM pragma_table_list WHERE schema = 'main' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	for _, want := range []string{"storages 1", "receivers 1", "receptions 1", "reception_associations 1", "received_files 1",
		"quarantine 1", "quarantine_totals 1", "work 1", "subjects 1", "sessions 1", "scans 1", "instances 1", "catalog_totals 1",
		"reception_steps 1", "events 1", "event_marks 1"} {
		if !strings.Contains("\n"+tables+"\n", "\n"+want+"\n") {
			t.Errorf("sqlite3 shows no STRICT table %q among:\n%s", want, tables)
		}
	}
	indexes := run(`SELECT name FROM sqlite_schema WHERE type = 'index' AND sql IS NOT NULL ORDER BY name`)
	for _, want := range []string{"storages_by_root", "storages_by_bucket", "receivers_by_storage", "receivers_by_port",
		"receptions_arrival", "receptions_open", "receptions_by_start", "receptions_by_receiver", "receptions_by_study",
		"receptions_by_state", "receptions_in_progress", "receptions_by_storage", "quarantine_by_time",
		"quarantine_by_state", "quarantine_by_reason", "quarantine_by_receiver", "quarantine_by_reception",
		"quarantine_by_study", "quarantine_by_instance", "work_by_key", "work_claimable",
		"receptions_receiver_in_progress", "receptions_by_receiver_state", "receptions_by_study_state",
		"quarantine_by_state_reason", "quarantine_by_state_receiver", "quarantine_by_receiver_reason",
		"quarantine_by_state_receiver_reason", "quarantine_kept", "instance_claims_by_reception", "instances_by_uid",
		"scans_by_storage", "scans_by_session_created", "sessions_by_storage", "work_by_reception", "receptions_holding",
		"events_by_type", "events_by_key"} {
		if !strings.Contains("\n"+indexes+"\n", "\n"+want+"\n") {
			t.Errorf("sqlite3 shows no index %s", want)
		}
	}
	if got := run(`SELECT files || ' ' || bytes FROM quarantine_totals`); got != "0 0" {
		t.Errorf("the quarantine's totals start at %q, want 0 0", got)
	}
	if got := run(`SELECT subjects || ' ' || sessions || ' ' || scans || ' ' || instances FROM catalog_totals`); got != "0 0 0 0" {
		t.Errorf("the catalog's totals start at %q, want 0 0 0 0", got)
	}
	// The CHECK constraints refuse what the driver never writes: a
	// negative count, and the instance rows below.
	for _, bad := range []string{
		`UPDATE catalog_totals SET instances = -1`,
		`UPDATE quarantine_totals SET files = -1`,
		// A claim with no reception, a held row with no reception, a
		// checksum of the wrong length in either, an archived checksum of
		// the wrong length.
		`INSERT INTO instance_claims VALUES ('1.2', '', '1.2', '1.2', zeroblob(32), 0)`,
		`INSERT INTO instance_claims VALUES ('1.2', 'rc', '1.2', '1.2', x'00', 0)`,
		`INSERT INTO instances VALUES (1, '1.2', '', zeroblob(32), NULL, '', 0, 0, 0, 0, NULL)`,
		`INSERT INTO instances VALUES (1, '1.2', 'rc', x'00', NULL, '', 0, 0, 0, 0, NULL)`,
		`INSERT INTO instances VALUES (1, '1.2', 'rc', zeroblob(32), x'00', '', 0, 0, 0, 0, NULL)`,
	} {
		out, err := exec.Command(tool, "-batch", path, bad).CombinedOutput() //nolint:gosec // G204, G702: the tool and the database are this test's own.
		if err == nil || !strings.Contains(string(out), "CHECK constraint failed") {
			t.Errorf("sqlite3 let %q through: %v %s", bad, err, out)
		}
	}
	// The receptions per step, kept by the schema's triggers as the tool
	// itself writes: an insert counts its state, a change of state moves
	// the count, a delete removes it.
	run(`INSERT INTO receptions VALUES ('rc1', 'r', '2.25.1.1', 1, 'receiving', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '',
		0, 0, 0, NULL, 0, '[]', 0, '', '', '', NULL, '')`)
	run(`INSERT INTO receptions VALUES ('rc2', 'r', '2.25.1.2', 1, 'receiving', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '',
		0, 0, 0, NULL, 0, '[]', 0, '', '', '', NULL, '')`)
	run(`UPDATE receptions SET state = 'complete' WHERE id = 'rc1'`)
	steps := `SELECT group_concat(state || ' ' || receptions, ', ') FROM (SELECT * FROM reception_steps ORDER BY state)`
	if got := run(steps); got != "complete 1, receiving 1" {
		t.Errorf("after two opens and a step the steps read %q", got)
	}
	run(`DELETE FROM receptions WHERE id = 'rc2'`)
	if got := run(steps); got != "complete 1, receiving 0" {
		t.Errorf("after a delete the steps read %q", got)
	}
	// The feed's ids only grow: emptied, it goes on after the last id.
	run(`INSERT INTO events (type, at, payload) VALUES ('receiver_health', 1, '{}'), ('receiver_health', 2, '{}')`)
	run(`DELETE FROM events`)
	run(`INSERT INTO events (type, at, payload) VALUES ('receiver_health', 3, '{}')`)
	if got := run(`SELECT id FROM events`); got != "3" {
		t.Errorf("an event written after the feed was emptied has id %s, want 3", got)
	}
	if got := run(`SELECT pruned_through FROM event_marks`); got != "0" {
		t.Errorf("the pruning's mark starts at %q, want 0", got)
	}
	plan := run(`EXPLAIN QUERY PLAN ` + strings.ReplaceAll(strings.ReplaceAll(claimNextSQL, "?", "1"), "kind = 1", "kind = 'reception'"))
	if !strings.Contains(plan, "work_claimable") {
		t.Errorf("sqlite3's plan of a claim does not use work_claimable:\n%s", plan)
	}
}
