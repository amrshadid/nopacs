package access

import (
	"maps"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Principal is who made a request, with the roles the check read. Only
// Access.Check makes one; the zero Principal is nobody and holds nothing. A
// key's principal knows its role on the installation and, when the
// requirement named an existing tenant, its role there; not its other
// grants, which the check does not read (WhoAmI does).
type Principal struct {
	key        master.Key
	local      string
	isKey      bool
	isLoc      bool
	instRole   master.Role
	tenantID   string
	tenantRole master.Role
}

// IsLocalOperator reports whether the caller is the local operator.
func (p Principal) IsLocalOperator() bool { return p.isLoc }

// LocalUser returns the local operator's operating system user id, or "".
func (p Principal) LocalUser() string { return p.local }

// Key returns the caller's key, without its hash, as the check read it, and
// whether the caller is a key.
func (p Principal) Key() (master.Key, bool) {
	k := p.key
	k.Hash = nil
	return k, p.isKey
}

// KeyID returns the caller's key id, or "" for the local operator.
func (p Principal) KeyID() string { return p.key.ID }

// Role returns the role the caller holds on scope, or "" for none or for a
// tenant the check did not read. The local operator holds admin on every
// scope. A grant on the installation is not a role on a tenant.
func (p Principal) Role(scope master.Scope) master.Role {
	switch {
	case p.isLoc:
		return master.RoleAdmin
	case !p.isKey:
		return ""
	case scope.IsInstallation():
		return p.instRole
	case p.tenantID != "" && scope.TenantID == p.tenantID:
		return p.tenantRole
	}
	return ""
}

// CanListTenants reports whether the caller may list every tenant: the local
// operator, or a key with reader or more on the installation. Such a caller
// is told NotFound for a tenant that does not exist: it could list the
// tenants anyway, so the answer reveals nothing (access.proto, "A tenant that
// does not exist").
func (p Principal) CanListTenants() bool {
	return atLeast(p.Role(master.Installation), master.RoleReader)
}

// atLeast reports whether have includes need, by the one order of roles
// the master states (master.Role.Includes), the order every change's own
// check of its actor uses too. No role includes an unknown one, and the
// empty role includes nothing.
func atLeast(have, need master.Role) bool {
	return have.Includes(need)
}

// TenantRef names the tenant a requirement named: its id and its name, as
// the check read them, and nothing else. A handler that needs the tenant's
// state, database or any other field reads the tenant itself.
type TenantRef struct {
	// ID is the tenant's id at the time of the check.
	ID string
	// Name is the tenant's name, as the request gave it.
	Name string
}

// Decision is what Check allowed: the caller, the tenant the requirement
// named, and the facts of the request for its audit entries. Only Check
// makes one, and no other package can make or alter one: the fields are
// unexported and the zero Decision carries no caller, so a handler that
// skipped the check can neither call a function of this package that takes
// a Decision nor make an audit entry the master takes.
//
// A Decision is for one request. It is not a session: every change made
// for a key carries the key as its Actor, and the master checks it again in
// the change's transaction; every read this package makes for a key checks
// it again first. So a key revoked since the check is refused even with a
// Decision in hand.
type Decision struct {
	principal Principal
	tenant    TenantRef
	hasTenant bool
	entry     master.AuditEntry
	newID     func() string
	clock     func() time.Time
	// req is the requirement Check allowed, which Confirm checks the key
	// against again.
	req Requirement
	// confirmed records that Confirm found the key still good at the start
	// of the handler, so a change need not ask the master a second time
	// before its own transaction checks the key again.
	confirmed bool
	// entered records that an entry of the request was made from this
	// Decision (Entry, which Record, RecordTo and every change call), shared
	// by every copy, so the server can tell a request that ended without
	// one (Entered).
	entered *atomic.Bool
}

// Entered reports whether an audit entry of the request was made from d or
// a copy of it: by Entry, so by Record, RecordTo or a change. A request whose
// check allowed it and that ends without one (its message could not be read,
// its handler failed or panicked before recording, it was canceled) has no
// record of what it did; the server records one for it. The zero Decision
// reports false.
func (d Decision) Entered() bool { return d.entered != nil && d.entered.Load() }

// Actor returns who makes a change for d, as the master's changes take it
// (master.Actor), needing role on scope: the caller's key, with the server's
// clock, which the store reads inside the change's transaction to judge the
// key's expiry, or the local operator, whom
// no store refuses. The zero Decision gives the zero Actor, which every
// change refuses as Invalid, so a Decision that did not come from Check
// opens nothing here either. A grant on the installation is not a role on a
// tenant: scope is the scope the change needs, as the requirement Check
// allowed named it.
func (d Decision) Actor(scope master.Scope, role master.Role) master.Actor {
	switch {
	case !d.checked() || d.clock == nil:
		return master.Actor{}
	case d.principal.isLoc:
		return master.LocalOperator()
	}
	return master.ActingKey(d.principal.key.ID, scope, role, d.clock)
}

// Principal returns who made the request.
func (d Decision) Principal() Principal { return d.principal }

// Method returns the method the request asked for, as its audit entry names
// it: a full procedure name, or a plain HTTP route's pattern.
func (d Decision) Method() string { return d.entry.Method }

// checked reports whether d is a Decision Check made: one with a caller and
// an id source. The zero Decision, all another package can make, is not.
func (d Decision) checked() bool {
	return d.newID != nil && (d.principal.isKey || d.principal.isLoc)
}

// Tenant returns the tenant the requirement named, its id and name only, and
// whether there was one. Its state is not checked here: whether a tenant
// that is not ready can serve a request is the handler's to decide, from the
// tenant it reads.
func (d Decision) Tenant() (TenantRef, bool) { return d.tenant, d.hasTenant }

// Entry returns an audit entry of the request with its outcome and reason,
// for the caller to write with its change (a change and its entry are one
// transaction), in the log the change concerns: the tenant's for a change
// inside a tenant, the master's for the installation. facts adds what the
// handler learned after the check (Facts); the identity of the request is the
// check's. Each call makes a new entry with its own fresh id and its own
// details map, so a handler that reads and then changes writes two entries
// and loses neither; all of a request's entries share its request id. A
// read's entry goes to Record or RecordTo instead.
func (d Decision) Entry(outcome master.Outcome, reason string, facts Facts) master.AuditEntry {
	e := d.entry
	if d.newID != nil {
		e.ID = d.newID()
	}
	if d.entered != nil {
		d.entered.Store(true)
	}
	e.Details = maps.Clone(d.entry.Details)
	e.Outcome = outcome
	e.Reason = auditText(reason, master.MaxAuditReasonLength)
	return facts.apply(e)
}
