package mastertest

import (
	"errors"
	"fmt"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// testGrants guards setting a grant (new, then replaced, returning the old
// role and auditing both), the lists by key and by scope, removal, and each
// failure kind.
func testGrants(e *env) {
	t := e.mustTenant("lab")
	k := e.mustKey()
	scope := master.TenantScope(t.ID)

	first := e.audit()
	first.Details = map[string]string{"scope": scope.String()}
	prev, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: scope, Role: master.RoleReader}, first, master.LocalOperator())
	e.ok("SetGrant", err)
	if prev != "" {
		e.Errorf("SetGrant on no grant returned %q, want the empty role", prev)
	}
	second := e.audit()
	prev, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: scope, Role: master.RoleAdmin}, second, master.LocalOperator())
	e.ok("SetGrant again", err)
	if prev != master.RoleReader {
		e.Errorf("SetGrant on a reader grant returned %q, want reader", prev)
	}
	grants, next, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
	e.ok("GrantsByKey", err)
	if len(grants) != 1 || grants[0].Role != master.RoleAdmin || grants[0].Scope != scope || next != "" {
		e.Errorf("grants = %+v (next %q), want one admin grant on %s", grants, next, scope)
	}

	byID := map[string]master.AuditEntry{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		byID[a.ID] = a
	}
	if d := byID[first.ID].Details; d["old_role"] != "none" || d["new_role"] != "reader" || d["scope"] != scope.String() {
		e.Errorf("first set's audit details = %v, want old none, new reader and the caller's scope", d)
	}
	if d := byID[second.ID].Details; d["old_role"] != "reader" || d["new_role"] != "admin" {
		e.Errorf("second set's audit details = %v, want old reader, new admin", d)
	}

	_, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: "nope", Scope: master.Installation, Role: master.RoleReader}, e.audit(), master.LocalOperator())
	e.wantKind("SetGrant, unknown key", err, fault.NotFound)
	_, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.TenantScope("nope"), Role: master.RoleReader}, e.audit(), master.LocalOperator())
	e.wantKind("SetGrant, unknown tenant", err, fault.NotFound)
	_, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: "owner"}, e.audit(), master.LocalOperator())
	e.wantKind("SetGrant, bad role", err, fault.Invalid)

	removal := e.audit()
	prev, err = e.s.RemoveGrant(e.ctx, k.ID, scope, removal, master.LocalOperator())
	e.ok("RemoveGrant", err)
	if prev != master.RoleAdmin {
		e.Errorf("RemoveGrant returned %q, want admin", prev)
	}
	grants, _, err = e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
	e.ok("GrantsByKey", err)
	if len(grants) != 0 {
		e.Errorf("grants after RemoveGrant = %+v, want none", grants)
	}
	again := e.audit()
	prev, err = e.s.RemoveGrant(e.ctx, k.ID, scope, again, master.LocalOperator())
	if err != nil || prev != "" {
		e.Errorf("RemoveGrant of no grant = %q, %v; want the empty role and success", prev, err)
	}
	byID = map[string]master.AuditEntry{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		byID[a.ID] = a
	}
	if d := byID[removal.ID].Details; d["old_role"] != "admin" || d["new_role"] != "none" {
		e.Errorf("removal's audit details = %v, want old admin, new none", d)
	}
	if d := byID[again.ID].Details; d["old_role"] != "none" || d["new_role"] != "none" {
		e.Errorf("second removal's audit details = %v, want old none, new none", d)
	}
	_, err = e.s.RemoveGrant(e.ctx, "nope", scope, e.audit(), master.LocalOperator())
	e.wantKind("RemoveGrant, unknown key", err, fault.NotFound)
	_, err = e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope("nope"), e.audit(), master.LocalOperator())
	e.wantKind("RemoveGrant, unknown tenant", err, fault.NotFound)
	grants, _, err = e.s.GrantsByKey(e.ctx, "nope", master.Page{Limit: 10})
	if err != nil || len(grants) != 0 {
		e.Errorf("GrantsByKey(unknown) = %v, %v; want none, nil", grants, err)
	}
}

// testTenantAdminGrants guards the bounds of a tenant's admin inside the
// change's transaction (master.CheckGrantChange): acting by its admin grant
// on a tenant, a key sets and removes a reader grant there, and is refused,
// changing nothing and writing no entry, when it gives admin, lowers or
// removes an admin's grant (an admin set by another change before it),
// gives, changes or removes a grant of a key that is admin on the
// installation (which outranks it), changes its own key's grant, touches
// the installation's scope, or touches another tenant.
func testTenantAdminGrants(e *env) {
	lab, other := e.mustTenant("lab"), e.mustTenant("other")
	scope := master.TenantScope(lab.ID)
	admin, peer, person, chief := e.mustKey(), e.mustKey(), e.mustKey(), e.mustKey()
	e.mustGrant(admin.ID, scope, master.RoleAdmin)
	e.mustGrant(peer.ID, scope, master.RoleAdmin)
	e.mustGrant(chief.ID, scope, master.RoleOperator)
	e.mustGrant(chief.ID, master.Installation, master.RoleAdmin)
	by := master.ActingKey(admin.ID, scope, master.RoleAdmin, at(T0))
	newcomer := e.mustKey()
	e.mustGrant(newcomer.ID, master.Installation, master.RoleAdmin)

	_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: person.ID, Scope: scope, Role: master.RoleReader}, e.audit(), by)
	e.ok("SetGrant of a reader by a tenant's admin", err)
	before := len(e.auditIDs())
	for _, c := range []struct {
		what string
		run  func() error
	}{
		{"giving admin", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: person.ID, Scope: scope, Role: master.RoleAdmin}, e.audit(), by)
			return err
		}},
		{"lowering an admin", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: peer.ID, Scope: scope, Role: master.RoleReader}, e.audit(), by)
			return err
		}},
		{"removing an admin", func() error {
			_, err := e.s.RemoveGrant(e.ctx, peer.ID, scope, e.audit(), by)
			return err
		}},
		{"another tenant", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: person.ID, Scope: master.TenantScope(other.ID), Role: master.RoleReader}, e.audit(), by)
			return err
		}},
		{"lowering an installation's admin", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: chief.ID, Scope: scope, Role: master.RoleReader}, e.audit(), by)
			return err
		}},
		{"removing an installation's admin", func() error {
			_, err := e.s.RemoveGrant(e.ctx, chief.ID, scope, e.audit(), by)
			return err
		}},
		{"giving an installation's admin a grant", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: newcomer.ID, Scope: scope, Role: master.RoleReader}, e.audit(), by)
			return err
		}},
		{"its own key", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: admin.ID, Scope: scope, Role: master.RoleOperator}, e.audit(), by)
			return err
		}},
		{"removing its own key's grant", func() error {
			_, err := e.s.RemoveGrant(e.ctx, admin.ID, scope, e.audit(), by)
			return err
		}},
		{"a grant on the installation", func() error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: person.ID, Scope: master.Installation, Role: master.RoleReader}, e.audit(), by)
			return err
		}},
		{"removing a grant on the installation", func() error {
			_, err := e.s.RemoveGrant(e.ctx, chief.ID, master.Installation, e.audit(), by)
			return err
		}},
	} {
		if err := c.run(); fault.KindOf(err) != fault.Denied || !errors.Is(err, master.ErrGrantBound) {
			e.Errorf("%s by a tenant's admin: %v, want a refusal with ErrGrantBound", c.what, err)
		}
	}
	if after := len(e.auditIDs()); after != before {
		e.Errorf("the refused changes wrote %d audit entries, want none", after-before)
	}
	r, err := e.s.RolesFor(e.ctx, peer.ID, "lab")
	e.ok("RolesFor", err)
	if r.Tenant != master.RoleAdmin {
		e.Errorf("the other admin's role is %q after the refusals, want admin", r.Tenant)
	}
	for _, k := range []struct {
		name        string
		id          string
		tenant, ins master.Role
	}{
		{"the installation's admin", chief.ID, master.RoleOperator, master.RoleAdmin},
		{"the newcomer", newcomer.ID, "", master.RoleAdmin},
		{"the acting admin", admin.ID, master.RoleAdmin, ""},
		{"the person", person.ID, master.RoleReader, ""},
	} {
		r, err := e.s.RolesFor(e.ctx, k.id, "lab")
		e.ok("RolesFor", err)
		if r.Tenant != k.tenant || r.Installation != k.ins {
			e.Errorf("%s holds %q on the tenant and %q on the installation after the refusals, want %q and %q", k.name, r.Tenant, r.Installation, k.tenant, k.ins)
		}
	}
	prev, err := e.s.RemoveGrant(e.ctx, person.ID, scope, e.audit(), by)
	if err != nil || prev != master.RoleReader {
		e.Errorf("RemoveGrant of a reader by a tenant's admin = %q, %v; want reader", prev, err)
	}
}

// testGrantsListed guards both grant lists: by key, the installation first
// and then by tenant id; by scope, by key id; each paged across inserts.
func testGrantsListed(e *env) {
	k := e.mustKey()
	e.mustGrant(k.ID, master.Installation, master.RoleAdmin)
	var tenants []master.Tenant
	for i := range 23 {
		t := e.mustTenant(fmt.Sprintf("t%02d", i))
		tenants = append(tenants, t)
		e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleReader)
	}
	inserted := 0
	checkPaging(e, "GrantsByKey",
		func(p master.Page) ([]master.Grant, string, error) { return e.s.GrantsByKey(e.ctx, k.ID, p) },
		func(g master.Grant) string { return g.Scope.TenantID },
		func() {
			inserted++
			t := e.mustTenant(fmt.Sprintf("new%02d", inserted))
			e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleWriter)
		},
		24, 5)

	for range 21 {
		other := e.mustKey()
		e.mustGrant(other.ID, master.TenantScope(tenants[0].ID), master.RoleWriter)
	}
	checkPaging(e, "GrantsByScope",
		func(p master.Page) ([]master.Grant, string, error) {
			return e.s.GrantsByScope(e.ctx, master.TenantScope(tenants[0].ID), p)
		},
		func(g master.Grant) string { return g.KeyID },
		func() { e.mustGrant(e.mustKey().ID, master.TenantScope(tenants[0].ID), master.RoleReader) },
		22, 4)
	install, _, err := e.s.GrantsByScope(e.ctx, master.Installation, master.Page{Limit: 10})
	e.ok("GrantsByScope(installation)", err)
	if len(install) != 1 || install[0].KeyID != k.ID || !install[0].Scope.IsInstallation() {
		e.Errorf("installation grants = %+v, want the one admin grant", install)
	}
}

// testSetGrantRace guards that two concurrent sets on the same key and scope
// are ordered: exactly one sees no grant before it, the other sees the
// first one's role, and each one's audit entry says what it saw.
func testSetGrantRace(e *env) {
	t := e.mustTenant("lab")
	for round := range 25 {
		k := e.mustKey()
		scope := master.TenantScope(t.ID)
		if round%2 == 1 {
			scope = master.Installation
		}
		roles := []master.Role{master.RoleReader, master.RoleWriter}
		entries := []master.AuditEntry{e.audit(), e.audit()}
		prev := make([]master.Role, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		done := make(chan int, 2)
		for i := range 2 {
			go func() {
				<-start
				prev[i], errs[i] = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: scope, Role: roles[i]}, entries[i], master.LocalOperator())
				done <- i
			}()
		}
		close(start)
		<-done
		<-done
		if errs[0] != nil || errs[1] != nil {
			e.Fatalf("round %d: SetGrant: %v, %v", round, errs[0], errs[1])
		}
		var firstIdx int
		switch {
		case prev[0] == "" && prev[1] == roles[0]:
			firstIdx = 0
		case prev[1] == "" && prev[0] == roles[1]:
			firstIdx = 1
		default:
			e.Fatalf("round %d: previous roles %q and %q; want exactly one none and the other the first one's role", round, prev[0], prev[1])
		}
		lastIdx := 1 - firstIdx
		grants, _, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
		e.ok("GrantsByKey", err)
		if len(grants) != 1 || grants[0].Role != roles[lastIdx] {
			e.Fatalf("round %d: grants = %+v, want one with the last set's role %s", round, grants, roles[lastIdx])
		}
		byID := map[string]master.AuditEntry{}
		for _, a := range e.allAudit(master.AuditFilter{}) {
			byID[a.ID] = a
		}
		if d := byID[entries[firstIdx].ID].Details; d["old_role"] != "none" || d["new_role"] != string(roles[firstIdx]) {
			e.Errorf("round %d: first set's audit = %v", round, d)
		}
		if d := byID[entries[lastIdx].ID].Details; d["old_role"] != string(roles[firstIdx]) || d["new_role"] != string(roles[lastIdx]) {
			e.Errorf("round %d: second set's audit = %v", round, d)
		}
	}
}

// testRolesFor guards the access check's lookup: the installation's role,
// whether the tenant named exists and its id, and the key's role on it, for
// a key with grants on both, on one, on none, and an unknown key; a tenant
// found and not found; no name and a name that is not valid, Windows device
// names among them (not found, not an error); and after the tenant's removal and its recreation under the
// same name, when the old grant gives nothing and the new id is reported.
func testRolesFor(e *env) {
	lab, clinic := e.mustTenant("lab"), e.mustTenant("clinic")
	both, tenantOnly, none := e.mustKey(), e.mustKey(), e.mustKey()
	e.mustGrant(both.ID, master.Installation, master.RoleReader)
	e.mustGrant(both.ID, master.TenantScope(lab.ID), master.RoleWriter)
	e.mustGrant(tenantOnly.ID, master.TenantScope(lab.ID), master.RoleAdmin)
	found := func(t master.Tenant, inst, ten master.Role) master.Roles {
		return master.Roles{Installation: inst, Tenant: ten, TenantID: t.ID, TenantFound: true}
	}
	cases := []struct {
		key, tenant string
		want        master.Roles
	}{
		{both.ID, "lab", found(lab, master.RoleReader, master.RoleWriter)},
		{both.ID, "clinic", found(clinic, master.RoleReader, "")},
		{both.ID, "", master.Roles{Installation: master.RoleReader}},
		{both.ID, "nope", master.Roles{Installation: master.RoleReader}},
		{both.ID, "Bad Name", master.Roles{Installation: master.RoleReader}},
		// Windows device names are not valid tenant names on any system: no
		// tenant has one, and asking is not an error.
		{both.ID, "con", master.Roles{Installation: master.RoleReader}},
		{both.ID, "nul", master.Roles{Installation: master.RoleReader}},
		{both.ID, "com0", master.Roles{Installation: master.RoleReader}},
		{both.ID, "lpt9", master.Roles{Installation: master.RoleReader}},
		{both.ID, "bad\x00\xff", master.Roles{Installation: master.RoleReader}},
		{tenantOnly.ID, "lab", found(lab, "", master.RoleAdmin)},
		{tenantOnly.ID, "", master.Roles{}},
		{none.ID, "lab", found(lab, "", "")},
		{none.ID, "nope", master.Roles{}},
		{"unknown", "lab", found(lab, "", "")},
	}
	for _, c := range cases {
		got, err := e.s.RolesFor(e.ctx, c.key, c.tenant)
		if err != nil || got != c.want {
			e.Errorf("RolesFor(%s, %q) = %+v, %v; want %+v", c.key, c.tenant, got, err, c.want)
		}
	}
	_, err := e.s.RolesFor(e.ctx, "a b", "lab")
	e.wantKind("RolesFor, bad key id", err, fault.Invalid)
	e.ok("RemoveTenant", e.s.RemoveTenant(e.ctx, lab.ID, e.audit(), master.LocalOperator()))
	got, err := e.s.RolesFor(e.ctx, both.ID, "lab")
	if want := (master.Roles{Installation: master.RoleReader}); err != nil || got != want {
		e.Errorf("RolesFor after the tenant's removal = %+v, %v; want %+v", got, err, want)
	}
	// A tenant made again under the name is a new tenant, with a new id
	// and no grants: the removed tenant's grant gives nothing.
	again := e.mustTenant("lab")
	got, err = e.s.RolesFor(e.ctx, both.ID, "lab")
	if want := found(again, master.RoleReader, ""); err != nil || got != want || again.ID == lab.ID {
		e.Errorf("RolesFor for a new tenant under a removed tenant's name = %+v, %v; want %+v (old id %s)", got, err, want, lab.ID)
	}
}

// testGrantTenantNames guards that the grant lists carry each tenant's name
// as it is at the time of the read, and none for the installation.
func testGrantTenantNames(e *env) {
	lab, clinic := e.mustTenant("lab"), e.mustTenant("clinic")
	k := e.mustKey()
	e.mustGrant(k.ID, master.Installation, master.RoleAdmin)
	e.mustGrant(k.ID, master.TenantScope(lab.ID), master.RoleReader)
	e.mustGrant(k.ID, master.TenantScope(clinic.ID), master.RoleWriter)
	grants, _, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
	e.ok("GrantsByKey", err)
	want := map[string]string{"": "", lab.ID: "lab", clinic.ID: "clinic"}
	if len(grants) != 3 {
		e.Fatalf("%d grants, want 3", len(grants))
	}
	for _, g := range grants {
		if g.TenantName != want[g.Scope.TenantID] {
			e.Errorf("grant on %s carries the name %q, want %q", g.Scope, g.TenantName, want[g.Scope.TenantID])
		}
	}
	byScope, _, err := e.s.GrantsByScope(e.ctx, master.TenantScope(clinic.ID), master.Page{Limit: 10})
	e.ok("GrantsByScope", err)
	if len(byScope) != 1 || byScope[0].TenantName != "clinic" {
		e.Errorf("GrantsByScope = %+v, want one grant named clinic", byScope)
	}
	inst, _, err := e.s.GrantsByScope(e.ctx, master.Installation, master.Page{Limit: 10})
	e.ok("GrantsByScope(installation)", err)
	if len(inst) != 1 || inst[0].TenantName != "" {
		e.Errorf("installation grants = %+v, want one with no tenant name", inst)
	}
}

// roleDetail is a role as an audit entry's details write it.
func roleDetail(r master.Role) string {
	if r == "" {
		return master.NoRole
	}
	return string(r)
}
