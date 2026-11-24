package postgres

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestTamperedProfileVersion guards that a profile version whose row was
// changed outside the store is refused on read on Postgres
// (tenanttest.CheckTamperedProfile).
func TestTamperedProfileVersion(t *testing.T) {
	address := newSchema(t)
	tn := openTenantAt(t, address, "tampered")
	conn := admin(t, address)
	tenanttest.CheckTamperedProfile(t, tn, func(stmt string) {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	})
}
