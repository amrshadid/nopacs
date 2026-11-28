package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuleSetVersionImmutable guards that a stored rule set version is
// never changed or removed, whatever writes to the table: the database
// refuses an UPDATE or a DELETE of a route_rule_sets row, so a reception's record of the version
// it was routed by always names the rules it was routed by, and the row
// reads as it was written.
func TestRuleSetVersionImmutable(t *testing.T) {
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
	body := `{"version":1,"archive":[{"name":"a","storage":"store"}]}`
	if _, err := db.ExecContext(ctx, `INSERT INTO route_rule_sets (receiver, version, body, created, created_by) VALUES ('r', 1, ?, 1, '')`,
		body); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE route_rule_sets SET body = '{}' WHERE receiver = 'r' AND version = 1`)
	if err == nil || !strings.Contains(err.Error(), "never changed") {
		t.Fatalf("an update of a stored version: %v, want it refused", err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT body FROM route_rule_sets WHERE receiver = 'r' AND version = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Errorf("the version reads %s after a refused update, want %s", got, body)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM route_rule_sets WHERE receiver = 'r' AND version = 1`)
	if err == nil || !strings.Contains(err.Error(), "never changed") {
		t.Fatalf("a delete of a stored version: %v, want it refused", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT body FROM route_rule_sets WHERE receiver = 'r' AND version = 1`).Scan(&got); err != nil || got != body {
		t.Errorf("the version reads %q (%v) after a refused delete, want %s", got, err, body)
	}
}
