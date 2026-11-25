package postgres

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
)

// TestSendingPlans guards that every statement of senders' and jobs' work
// that reads a table that grows is answered from an index, never by reading
// a whole table, at a moderate size: 20,000 jobs made at i seconds, one in a
// hundred of sender s1 and one in a hundred failed, a tenth running; 20,000
// scans of jobs; 20,000 failures, half of job j1, one in seven refused;
// 20,000 instances owed; 20,000 tallies of 200 jobs; 2,000 scans of the catalog with 200,000 instances,
// half of them recording one of 5,000 profile versions, 2,000 of them marked
// damaged by a scrub; 50,000 finished
// units of work; 20,000 scans of corrections, of 200 subjects, and
// 50,000 instances they hide; 40,000 rows of a replica's queue; 40,000
// sessions' retention times. For the
// very statements package sqlkit runs (SendingDialect.Plans), Postgres's
// plan for the values used and its generic plan (the one a prepared
// statement keeps) read no such table whole, the first uses the index named
// for it, and neither sorts the rows of a statement that must read them in
// order. The plans are logged.
func TestSendingPlans(t *testing.T) {
	address := newSchema(t)
	openTenantAt(t, address, "plans")
	conn := admin(t, address)
	mustExec(t, conn,
		`INSERT INTO subjects (label, folded, created) SELECT 'sub-' || n, 'sub-' || n, now() FROM generate_series(1, 200) n`,
		`INSERT INTO sessions (study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded)
		 SELECT '2.25.1.' || n, n % 200 + 1, 'ses-' || n, 'ses-' || n, 'store', now(), 'sub-' || (n % 200 + 1), 'sub-' || (n % 200 + 1)
		 FROM generate_series(1, 1000) n`,
		`INSERT INTO scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, subject_label,
		   subject_folded, session_label, session_folded, storage, study_uid)
		 SELECT n % 1000 + 1, '2.25.2.' || n, 'scan-' || n, 'scan-' || n, 100, 100000, NULL, now(), now(),
		   'sub-' || (n % 200 + 1), 'sub-' || (n % 200 + 1), 'ses-' || (n % 1000 + 1), 'ses-' || (n % 1000 + 1), 'store', '2.25.1.' || n
		 FROM generate_series(1, 2000) n`,
		`INSERT INTO instances SELECT '2.25.3.' || i, i / 100 + 1, 'rc' || (i / 1000 + 1), sha256(i::text::bytea), NULL, '', 0, 1000, 1000, now()
		 FROM generate_series(0, 199999) i`,
		`INSERT INTO profile_versions (name, version, checksum, note, created_by_key, created, source, expanded)
		 SELECT 'p' || (n % 100), n / 100 + 1, repeat('a', 64), '', '', now(), 'x', 'x' FROM generate_series(1, 5000) n`,
		`UPDATE instances SET profile_version_id = abs(hashtext(sop_instance_uid) % 5000) + 1 WHERE scan_id % 2 = 0`,
		`INSERT INTO reception_scans SELECT 'rc' || n, n, 100, 100000 FROM generate_series(1, 2000) n`,
		`INSERT INTO work SELECT 'done' || i, 'send-s1', '', '', NULL, 'done', timestamptz 'epoch' + i * interval '1 second', 1, 30, '', '',
		   now(), now() FROM generate_series(1, 50000) i`,
		`INSERT INTO jobs SELECT 'j' || i, 'send', CASE i % 100 WHEN 7 THEN 's1' ELSE 's' || (i % 50 + 2) END, 'sub-1', 'ses-1', '', '', '',
		   NULL, CASE WHEN i % 100 = 5 THEN 'failed' WHEN i % 100 = 9 THEN 'queued' WHEN i % 10 = 3 THEN 'running' ELSE 'done' END,
		   i % 100 <> 9 AND i % 10 <> 3, '', '', timestamptz 'epoch' + i * interval '1 second', NULL,
		   timestamptz 'epoch' + i * interval '1 second', NULL, timestamptz 'epoch' + i * interval '1 second', '', '', 'l' || i,
		   true, true, true, 0, '', '', 0, 1, 1, 10, 100, 10, 100, 0, 0, 0, 0, 0, 0, NULL, 0 FROM generate_series(1, 20000) i`,
		`INSERT INTO job_scans SELECT 'j' || (i % 2000 + 1), i, 'u' || i, '', '', 10, 100, 0, 0, 0, 0, 0, 0, 0, false
		 FROM generate_series(1, 20000) i`,
		`INSERT INTO job_failures SELECT CASE i % 2 WHEN 0 THEN 'j1' ELSE 'j' || (i % 200 + 2) END, 'sub-1', 'ses-1', 'scan-' || (i % 50), '2.25.3.' || i, i % 50,
		   i % 7 <> 0, 'peer_refused', 49152, '', '', '', now() FROM generate_series(1, 20000) i`,
		`INSERT INTO job_resends SELECT 'j' || (i % 2000 + 1), i % 2000, '2.25.3.' || i FROM generate_series(1, 20000) i`,
		`INSERT INTO job_tallies SELECT 'j' || (i % 200 + 1), 'rule|r' || i || '|remove', i FROM generate_series(1, 20000) i`,
		`INSERT INTO job_owed SELECT 'j' || (i % 2000 + 1), i % 2000, '2.25.3.' || i FROM generate_series(1, 20000) i`,
		`INSERT INTO instance_damage SELECT '2.25.3.' || (i * 97), 'sf-' || i, now() FROM generate_series(1, 2000) i`,
		`INSERT INTO correction_scans (correction_id, series_uid, study_uid, storage, subject_label, session_label, scan_label,
		   new_subject, new_session, new_scan, instances, bytes)
		 SELECT 'c' || n, '2.25.2.' || n, '2.25.1.' || n, 'store', 'sub-' || (n % 200 + 1), 'ses-' || n, 'scan-' || n, '', '', '', 0, 0
		 FROM generate_series(1, 20000) n`,
		`INSERT INTO hidden_instances (correction_id, sop_instance_uid, scan_id, reception_id, sha256, storage_key, byte_offset,
		   byte_length, size, held_at)
		 SELECT 'c' || (i % 20000 + 1), '2.25.3.' || i, i % 20000 + 1, 'rc1', sha256(i::text::bytea), '', 0, 1000, 1000, now()
		 FROM generate_series(1, 50000) i`,
		// A replica's queue: one row in four pending, one in four copying
		// with its lease ended, the rest in step; due within a thousand
		// seconds, so many rows share a time and the claim's order goes on
		// to the replica and the series.
		`INSERT INTO replica_scans SELECT 'copy', '2.25.2.' || i, s, NULL,
		   CASE s WHEN 'in_step' THEN NULL ELSE timestamptz 'epoch' END, timestamptz 'epoch' + (i % 1000) * interval '1 second',
		   1, 0, 0, '', CASE s WHEN 'copying' THEN timestamptz 'epoch' + (i % 1000) * interval '1 second' END, NULL, false, ''
		 FROM (SELECT i, CASE WHEN i % 4 = 0 THEN 'pending' WHEN i % 4 = 1 THEN 'copying' ELSE 'in_step' END AS s
		   FROM generate_series(1, 40000) i) AS x`,
		// The sessions' retention times: under four rules, due within a
		// thousand seconds, one in ten due under none.
		`INSERT INTO retention_sessions SELECT '2.25.1.' || i, timestamptz 'epoch', timestamptz 'epoch', NULL, NULL,
		   CASE WHEN i % 10 = 0 THEN NULL ELSE timestamptz 'epoch' + (i % 1000) * interval '1 second' END,
		   CASE WHEN i % 10 = 0 THEN '' ELSE 'r' || (i % 4) END, CASE WHEN i % 10 = 0 THEN '' ELSE 'review' END, ''
		 FROM generate_series(1, 40000) i`,
		`ANALYZE`)
	for _, c := range sendingDialect.Plans() {
		statement := sqlkit.Rebind(c.SQL)
		custom := planOf(t, connQuerier{conn}, false, statement, c.Args...)
		generic := planOf(t, connQuerier{conn}, true, statement)
		t.Logf("%s:\n%s\ngeneric:\n%s", c.Name, custom, generic)
		for _, plan := range []string{custom, generic} {
			for _, line := range strings.Split(plan, "\n") {
				if _, table, found := strings.Cut(line, "Seq Scan on "); found {
					t.Errorf("%s reads %s whole", c.Name, strings.Fields(table)[0])
				}
			}
		}
		want := c.Index
		if want == "" {
			want = "Index" // the primary key or another index of the table's key
		}
		if !strings.Contains(custom, want) {
			t.Errorf("%s does not use %s", c.Name, want)
		}
		if c.Ordered {
			for _, plan := range []string{custom, generic} {
				if strings.Contains(plan, "Sort") {
					t.Errorf("%s sorts the rows it reads instead of reading them in order:\n%s", c.Name, plan)
				}
			}
		}
	}
}
