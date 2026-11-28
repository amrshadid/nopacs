package postgres

import (
	"context"
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
	address := newSchema(t)
	openTenantAt(t, address, "t1")
	conn := admin(t, address)
	body := `{"version":1,"archive":[{"name":"a","storage":"store"}]}`
	if _, err := conn.Exec(ctx, `INSERT INTO route_rule_sets (receiver, version, body, created, created_by) VALUES ('r', 1, $1, now(), '')`,
		body); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Exec(ctx, `UPDATE route_rule_sets SET body = '{}' WHERE receiver = 'r' AND version = 1`)
	if err == nil || !strings.Contains(err.Error(), "never changed") {
		t.Fatalf("an update of a stored version: %v, want it refused", err)
	}
	var got string
	if err := conn.QueryRow(ctx, `SELECT body FROM route_rule_sets WHERE receiver = 'r' AND version = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Errorf("the version reads %s after a refused update, want %s", got, body)
	}
	_, err = conn.Exec(ctx, `DELETE FROM route_rule_sets WHERE receiver = 'r' AND version = 1`)
	if err == nil || !strings.Contains(err.Error(), "never changed") {
		t.Fatalf("a delete of a stored version: %v, want it refused", err)
	}
	if err := conn.QueryRow(ctx, `SELECT body FROM route_rule_sets WHERE receiver = 'r' AND version = 1`).Scan(&got); err != nil || got != body {
		t.Errorf("the version reads %q (%v) after a refused delete, want %s", got, err, body)
	}
}
