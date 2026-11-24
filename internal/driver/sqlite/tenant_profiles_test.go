package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestTamperedProfileVersion guards that a profile version whose row was
// changed outside the store is refused on read on SQLite
// (tenanttest.CheckTamperedProfile).
func TestTamperedProfileVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "tampered")
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
	tenanttest.CheckTamperedProfile(t, s, func(stmt string) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	})
}
