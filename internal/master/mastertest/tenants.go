package mastertest

import (
	"fmt"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// testTenants guards each tenant operation: the normal case read back
// exactly, and each failure kind.
func testTenants(e *env) {
	t := e.tenant("lab")
	t.StateReason = "first"
	e.ok("CreateTenant", e.s.CreateTenant(e.ctx, t, e.audit(), master.LocalOperator()))
	got, err := e.s.Tenant(e.ctx, t.ID)
	e.ok("Tenant", err)
	if d := diffTenant(got, t); d != "" {
		e.Errorf("Tenant: %s", d)
	}
	got, err = e.s.TenantByName(e.ctx, "lab")
	e.ok("TenantByName", err)
	if d := diffTenant(got, t); d != "" {
		e.Errorf("TenantByName: %s", d)
	}

	sameName := e.tenant("lab")
	e.wantKind("CreateTenant, name taken", e.s.CreateTenant(e.ctx, sameName, e.audit(), master.LocalOperator()), fault.Conflict)
	sameID := e.tenant("other")
	sameID.ID = t.ID
	e.wantKind("CreateTenant, id taken", e.s.CreateTenant(e.ctx, sameID, e.audit(), master.LocalOperator()), fault.Conflict)
	bad := e.tenant("Bad Name")
	e.wantKind("CreateTenant, bad name", e.s.CreateTenant(e.ctx, bad, e.audit(), master.LocalOperator()), fault.Invalid)
	for _, device := range []string{"con", "nul", "com1", "lpt9"} {
		e.wantKind("CreateTenant, a name Windows reserves ("+device+")", e.s.CreateTenant(e.ctx, e.tenant(device), e.audit(), master.LocalOperator()), fault.Invalid)
	}
	badAudit := e.audit()
	badAudit.Outcome = "maybe"
	e.wantKind("CreateTenant, bad audit entry", e.s.CreateTenant(e.ctx, e.tenant("fine"), badAudit, master.LocalOperator()), fault.Invalid)

	_, err = e.s.Tenant(e.ctx, "nope")
	e.wantKind("Tenant, unknown", err, fault.NotFound)
	_, err = e.s.TenantByName(e.ctx, "nope")
	e.wantKind("TenantByName, unknown", err, fault.NotFound)
	_, err = e.s.TenantByName(e.ctx, "No/Pe")
	e.wantKind("TenantByName, bad name", err, fault.Invalid)
	_, err = e.s.Tenant(e.ctx, "a/b")
	e.wantKind("Tenant, bad id", err, fault.Invalid)

	e.ok("SetTenantState", e.s.SetTenantState(e.ctx, t.ID, master.StateFailed, "upgrade to 2 failed: disk full", e.audit(), master.LocalOperator()))
	got, err = e.s.Tenant(e.ctx, t.ID)
	e.ok("Tenant", err)
	if got.State != master.StateFailed || got.StateReason != "upgrade to 2 failed: disk full" {
		e.Errorf("state = %s %q after SetTenantState", got.State, got.StateReason)
	}
	e.wantKind("SetTenantState, unknown", e.s.SetTenantState(e.ctx, "nope", master.StateReady, "", e.audit(), master.LocalOperator()), fault.NotFound)
	e.wantKind("SetTenantState, bad state", e.s.SetTenantState(e.ctx, t.ID, "gone", "", e.audit(), master.LocalOperator()), fault.Invalid)

	k := e.mustKey()
	e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleReader)
	e.mustGrant(k.ID, master.Installation, master.RoleReader)
	e.ok("RemoveTenant", e.s.RemoveTenant(e.ctx, t.ID, e.audit(), master.LocalOperator()))
	_, err = e.s.Tenant(e.ctx, t.ID)
	e.wantKind("Tenant after RemoveTenant", err, fault.NotFound)
	grants, _, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
	e.ok("GrantsByKey", err)
	if len(grants) != 1 || !grants[0].Scope.IsInstallation() {
		e.Errorf("grants after the tenant's removal = %+v, want the installation grant only", grants)
	}
	e.wantKind("RemoveTenant, unknown", e.s.RemoveTenant(e.ctx, t.ID, e.audit(), master.LocalOperator()), fault.NotFound)
	// The name is free again once the tenant is gone.
	e.mustTenant("lab")
}

// testTenantNameRace guards the unique tenant name under concurrency: many
// creations of one name at once, with different ids, end with exactly one
// tenant, one success, every other Conflict, and an audit entry for the
// success only.
func testTenantNameRace(e *env) {
	const rounds, racers = 30, 8
	for round := range rounds {
		name := fmt.Sprintf("race%02d", round)
		tenants := make([]master.Tenant, racers)
		entries := make([]master.AuditEntry, racers)
		for i := range racers {
			tenants[i] = e.tenant(name)
			entries[i] = e.audit()
		}
		errs := make([]error, racers)
		start := make(chan struct{})
		done := make(chan struct{}, racers)
		for i := range racers {
			go func() {
				<-start
				errs[i] = e.s.CreateTenant(e.ctx, tenants[i], entries[i], master.LocalOperator())
				done <- struct{}{}
			}()
		}
		close(start)
		for range racers {
			<-done
		}
		winners := 0
		var winner int
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				winner = i
			case fault.KindOf(err) != fault.Conflict:
				e.Errorf("round %d: creation %d failed with %v, want Conflict", round, i, err)
			}
		}
		if winners != 1 {
			e.Fatalf("round %d: %d creations of one name succeeded, want 1", round, winners)
		}
		got, err := e.s.TenantByName(e.ctx, name)
		e.ok("TenantByName", err)
		if got.ID != tenants[winner].ID {
			e.Errorf("round %d: the tenant is %s, want the winner %s", round, got.ID, tenants[winner].ID)
		}
		ids := e.auditIDs()
		for i := range racers {
			if ids[entries[i].ID] != (i == winner) {
				e.Errorf("round %d: audit entry of creation %d present = %v, want %v", round, i, ids[entries[i].ID], i == winner)
			}
		}
	}
}

// testTenantsListed guards the tenant list: by name, paged across inserts.
func testTenantsListed(e *env) {
	for i := range 40 {
		e.mustTenant(fmt.Sprintf("m%02d", i))
	}
	n := 0
	checkPaging(e, "ListTenants", func(p master.Page) ([]master.Tenant, string, error) { return e.s.ListTenants(e.ctx, p) },
		func(t master.Tenant) string { return t.Name },
		func() {
			n++
			e.mustTenant(fmt.Sprintf("a%02d", n)) // before the position reached
			e.mustTenant(fmt.Sprintf("z%02d", n)) // after it
		},
		40, 6)
}

// testTenantHandoff guards the read-only states: a tenant is made moved by
// a key and read back so, with its reason, by id, by name and in the list;
// the server's own checks then cannot make it ready, unreachable, upgrading
// or failed (precondition, nothing written, no audit entry), but may make it
// fenced and set a fenced tenant's reason again; a key or the local
// operator takes it out of either, with the states before and after in the
// audit entry. The server giving back a failed export takes a moved tenant
// to ready though the key that moved it is revoked, and makes no other
// change: not out of ready, not to fenced or failed, not out of fenced.
func testTenantHandoff(e *env) {
	t := e.mustTenant("lab")
	_, admin := e.adminKey()
	state := func(want master.TenantState, reason string) {
		e.Helper()
		got, err := e.s.Tenant(e.ctx, t.ID)
		e.ok("Tenant", err)
		byName, err := e.s.TenantByName(e.ctx, "lab")
		e.ok("TenantByName", err)
		listed, _, err := e.s.ListTenants(e.ctx, master.Page{Limit: 10})
		e.ok("ListTenants", err)
		if got.State != want || got.StateReason != reason || byName.State != want || len(listed) != 1 || listed[0].State != want {
			e.Fatalf("state %q (%q), by name %q, listed %+v; want %q (%q)", got.State, got.StateReason, byName.State, listed, want, reason)
		}
	}
	moved := e.audit()
	e.ok("SetTenantState moved", e.s.SetTenantState(e.ctx, t.ID, master.StateMoved, "handed off", moved, admin))
	state(master.StateMoved, "handed off")
	if d := e.detailsOf(moved.ID); d[master.DetailOldState] != "ready" || d[master.DetailNewState] != "moved" {
		e.Errorf("the moved entry's details are %v", d)
	}
	for _, next := range []master.TenantState{master.StateReady, master.StateUnreachable, master.StateUpgrading, master.StateFailed} {
		a := e.audit()
		e.wantKind("the server makes a moved tenant "+string(next), e.s.SetTenantState(e.ctx, t.ID, next, "", a, master.Server()), fault.Precondition)
		if e.auditIDs()[a.ID] {
			e.Errorf("a refused change of the state to %s wrote its audit entry", next)
		}
	}
	state(master.StateMoved, "handed off")
	e.ok("the server fences a moved tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateFenced, "storage main: another installation", e.audit(), master.Server()))
	state(master.StateFenced, "storage main: another installation")
	e.ok("the server sets a fenced tenant's reason again", e.s.SetTenantState(e.ctx, t.ID, master.StateFenced, "storage main: later generation", e.audit(), master.Server()))
	state(master.StateFenced, "storage main: later generation")
	for _, next := range []master.TenantState{master.StateReady, master.StateMoved} {
		e.wantKind("the server makes a fenced tenant "+string(next), e.s.SetTenantState(e.ctx, t.ID, next, "", e.audit(), master.Server()), fault.Precondition)
	}
	state(master.StateFenced, "storage main: later generation")
	e.ok("the local operator reclaims a fenced tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), master.LocalOperator()))
	state(master.StateReady, "")
	e.ok("the server's own checks of a ready tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateUnreachable, "down", e.audit(), master.Server()))
	e.ok("the server's own checks of a ready tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), master.Server()))
	e.ok("SetTenantState moved again", e.s.SetTenantState(e.ctx, t.ID, master.StateMoved, "", e.audit(), admin))
	e.ok("a key takes a moved tenant back", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), admin))
	state(master.StateReady, "")

	// The server giving back a failed export: a moved tenant made ready
	// though the key that moved it was revoked since, and no other change.
	for _, next := range []master.TenantState{master.StateReady, master.StateUnreachable, master.StateMoved} {
		e.wantKind("a give-back of a ready tenant to "+string(next), e.s.SetTenantState(e.ctx, t.ID, next, "", e.audit(), master.ExportGiveBack()), fault.Precondition)
	}
	k, mover := e.adminKey()
	e.ok("SetTenantState moved by a key", e.s.SetTenantState(e.ctx, t.ID, master.StateMoved, "handed off", e.audit(), mover))
	if _, err := e.s.RevokeKey(e.ctx, k.ID, T0, e.audit(), master.LocalOperator()); err != nil {
		e.Fatalf("RevokeKey: %v", err)
	}
	e.wantKind("the revoked key takes the moved tenant back", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), mover), fault.Unauthenticated)
	for _, next := range []master.TenantState{master.StateFenced, master.StateFailed} {
		e.wantKind("a give-back of a moved tenant to "+string(next), e.s.SetTenantState(e.ctx, t.ID, next, "x", e.audit(), master.ExportGiveBack()), fault.Precondition)
	}
	back := e.audit()
	e.ok("the server gives back a failed export", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", back, master.ExportGiveBack()))
	state(master.StateReady, "")
	if d := e.detailsOf(back.ID); d[master.DetailOldState] != "moved" || d[master.DetailNewState] != "ready" {
		e.Errorf("the give-back entry's details are %v", d)
	}
	e.ok("SetTenantState moved", e.s.SetTenantState(e.ctx, t.ID, master.StateMoved, "", e.audit(), admin))
	e.ok("the server fences a moved tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateFenced, "storage main: another installation", e.audit(), master.Server()))
	e.wantKind("a give-back of a fenced tenant", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), master.ExportGiveBack()), fault.Precondition)
	state(master.StateFenced, "storage main: another installation")
}
