package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestForgedCatalogCursors guards that a catalog list on SQLite refuses as
// a bad page token a cursor written by someone other than the list, whose
// key is outside its filter or holds bytes no key holds, rather than
// answering rows outside the filter (testkit.CheckForgedCatalogCursors).
func TestForgedCatalogCursors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "forged")
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
	fillCatalog(t, db, 30)
	testkit.CheckForgedCatalogCursors(t, s, sampleCatalog(t, db))
}
