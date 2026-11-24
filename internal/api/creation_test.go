package api_test

import (
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/master"
)

// ownEntries returns the entries of the installation's log that are the
// request's own CreateTenant entries: not the registry's system entry for
// the row, which carries the request's id too.
func ownEntries(e *env, requestID string) []master.AuditEntry {
	var out []master.AuditEntry
	for _, en := range entriesOf(e.masterLog(), requestID) {
		if en.Method == nopacsv1connect.TenantServiceCreateTenantProcedure {
			out = append(out, en)
		}
	}
	return out
}

// TestEveryCreationPathLeavesOneEntry guards the audit of CreateTenant along
// each way the registry can end a creation that this test can bring about:
// whether the registry wrote the request's entry or wrote nothing, the
// master's log holds exactly one entry of the request, failed. The ways a
// test cannot bring about without reaching into the registry (an entry the
// master would refuse, a name being replaced or removed during the
// creation) end through the same lookup.
func TestEveryCreationPathLeavesOneEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(e *env)
		req   *v1.CreateTenantRequest
		code  connect.Code
	}{
		{"the driver refuses the address before anything is written", nil,
			&v1.CreateTenantRequest{Name: "loc", DatabaseType: pgType, DatabaseAddress: pgAddress + "?location=refused"}, connect.CodeInvalidArgument},
		{"a name taken (a conflict found before writing)", func(e *env) { e.tenant("taken") },
			&v1.CreateTenantRequest{Name: "taken", DatabaseType: sqliteType}, connect.CodeAlreadyExists},
		{"the database refuses the address after the row (undone)", nil,
			&v1.CreateTenantRequest{Name: "refused", DatabaseType: pgType, DatabaseAddress: pgAddress + "?sslmode=refused"}, connect.CodeInvalidArgument},
		{"the database does not answer (undone)", func(e *env) { e.pg.setDown("down", true) },
			&v1.CreateTenantRequest{Name: "down", DatabaseType: pgType, DatabaseAddress: pgAddress}, connect.CodeUnavailable},
		{"the undoing itself fails", func(e *env) { e.pg.setDown("stuck", true); e.faulty.failRemoveTenant.Store(true) },
			&v1.CreateTenantRequest{Name: "stuck", DatabaseType: pgType, DatabaseAddress: pgAddress}, 0},
		{"the registry is closing", func(e *env) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.registry.Close(ctx); err != nil {
				e.t.Fatal(err)
			}
		}, &v1.CreateTenantRequest{Name: "late", DatabaseType: sqliteType}, connect.CodeUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if c.setup != nil {
				c.setup(e)
			}
			o := call(e.as(local).tenants.CreateTenant, c.req)
			if c.code != 0 && o.code() != c.code {
				t.Errorf("code %v (%v), want %v", o.code(), o.err, c.code)
			}
			if o.err == nil {
				t.Fatalf("the creation succeeded: %v", o.res)
			}
			got := ownEntries(e, o.requestID)
			if len(got) != 1 || got[0].Outcome != master.OutcomeFailed {
				t.Errorf("the request's entries: %+v, want one, failed", got)
			}
		})
	}
}
