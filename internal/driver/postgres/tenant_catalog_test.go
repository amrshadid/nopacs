package postgres

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestForgedCatalogCursors guards that a catalog list on Postgres refuses
// as a bad page token a cursor written by someone other than the list,
// whose key is outside its filter or holds bytes Postgres refuses, rather
// than answering rows outside the filter or failing as the server's fault
// (testkit.CheckForgedCatalogCursors).
func TestForgedCatalogCursors(t *testing.T) {
	address := newSchema(t)
	tn := openTenantAt(t, address, "forged")
	conn := admin(t, address)
	fillCatalog(t, conn, 30)
	testkit.CheckForgedCatalogCursors(t, tn, sampleCatalog(t, conn))
}
