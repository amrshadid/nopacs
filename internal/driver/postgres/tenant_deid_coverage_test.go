package postgres

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestDeidCoverageContract runs the contract of the coverage reports on
// Postgres.
func TestDeidCoverageContract(t *testing.T) {
	tenanttest.RunDeidCoverage(t, func(tb testing.TB, id string) tenant.Pipeline { return openTenantTest(tb, id).(*Tenant) })
}
