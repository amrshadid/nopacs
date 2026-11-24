package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestRolesForPlan guards that RolesFor's one statement, which runs on every
// request with a key, is point lookups of indexes: the tenants' unique index
// on name and the grants' primary key, never a scan of either table. It
// fills both tables, runs ANALYZE, and logs SQLite's plan of the very
// statement the driver prepares.
func TestRolesForPlan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	m, err := OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO api_keys (id, hash, holder, created) SELECT printf('Key%06d', i), zeroblob(32), 'plan', 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		 INSERT INTO tenants (id, name, driver, address, state, state_reason, created) SELECT 't' || i, 'tenant' || i, 'sqlite', x'01', 'ready', '', 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO grants (key_id, scope, tenant_id, role) SELECT printf('Key%06d', i), 't' || (i % 2000 + 1), 't' || (i % 2000 + 1), 'reader' FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		 INSERT INTO grants (key_id, scope, tenant_id, role) SELECT printf('Key%06d', i), '', NULL, 'admin' FROM n`,
		`ANALYZE`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+rolesForSQL, "Key010000", "tenant1000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	t.Logf("RolesFor plan:\n%s", text)
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN ") && !strings.HasPrefix(line, "SCAN one") && !strings.Contains(line, "CONSTANT ROW") {
			t.Errorf("RolesFor scans a table: %q", line)
		}
	}
	for _, want := range []string{"SEARCH t USING INDEX", "SEARCH grants USING INDEX"} {
		if !strings.Contains(text, want) {
			t.Errorf("RolesFor's plan has no %q", want)
		}
	}
}
