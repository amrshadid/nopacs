package tui

import (
	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// access is what the caller may do, read from the grants WhoAmI answers, so
// the menu and the keys of a screen can leave out what the server would refuse.
// It is a convenience and never the check: the server decides every call, and a
// screen opened by a key the menu did not offer would be refused there.
//
// A nil access means the caller is not known yet (the answer to who-am-I has
// not arrived, or failed): everything is offered, and the server says no.
type access struct {
	// operator is the local operator, who may do everything.
	operator bool
	// installation is the role on the installation, none when absent. An
	// installation role counts on every tenant too.
	installation v1.Role
	// tenants are the roles on single tenants.
	tenants map[string]v1.Role
	// masked says the caller's key is a masked key (Key.masked): never
	// answered an original value and refused what writes into archived files,
	// deletes in bulk or chooses where data goes. The menu leaves those out.
	masked bool
}

// newAccess reads the grants of a principal.
func newAccess(p *v1.Principal) *access {
	if p == nil {
		return nil
	}
	a := &access{tenants: map[string]v1.Role{}}
	if p.GetLocalOperator() != nil {
		a.operator = true
	}
	a.masked = p.GetKey().GetMasked()
	for _, g := range p.GetGrants() {
		switch sc := g.GetScope().GetKind().(type) {
		case *v1.Scope_Installation:
			a.installation = max(a.installation, g.GetRole())
		case *v1.Scope_Tenant:
			a.tenants[sc.Tenant] = max(a.tenants[sc.Tenant], g.GetRole())
		}
	}
	return a
}

// all says that nothing is left out: the local operator, or a caller not known.
func (a *access) all() bool { return a == nil || a.operator }

// onInstallation says whether the caller holds at least role on the
// installation.
func (a *access) onInstallation(role v1.Role) bool {
	return a.all() || a.installation >= role
}

// onTenant says whether the caller holds at least role on the tenant, by a
// grant on it or by one on the installation.
func (a *access) onTenant(name string, role v1.Role) bool {
	return a.all() || a.installation >= role || a.tenants[name] >= role
}

// anyGrant says whether the caller holds any grant at all: a key with none
// sees no tenant and no screen.
func (a *access) anyGrant() bool {
	return a.all() || a.installation > 0 || len(a.tenants) > 0
}

// maskedKey says whether the caller is known to hold a masked key.
func (a *access) maskedKey() bool { return a != nil && a.masked }

// onTenantPlain says whether the caller holds at least role on the tenant and
// is not a masked key: the check of what a masked key is refused outright.
func (a *access) onTenantPlain(name string, role v1.Role) bool {
	return !a.maskedKey() && a.onTenant(name, role)
}
