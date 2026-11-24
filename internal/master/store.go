package master

import (
	"context"
	"time"
)

// Store is the master database, stated as operations. A driver package
// implements it for one database type and proves it with the contract suite
// in package mastertest.
//
// Every method takes a context, and fails with an error from package fault.
// Every method that changes the master, except MarkKeyUsed, takes the audit
// entry for that change and writes both or neither: if the entry cannot be
// written (its id is taken, the database fails), the change is not made, and
// if the change cannot be made (NotFound, Conflict, any failure), the entry
// is not written. A refusal or a failure that changes nothing is recorded
// with AppendAudit. An audit entry whose id is taken fails with
// ErrDuplicateAuditID in the chain, of kind Internal.
//
// Every method that changes the master, except MarkKeyUsed, also takes who
// makes the change, an Actor, and checks it inside the change's transaction:
// a key must exist, not be revoked, not have expired at the time its clock
// gives inside the transaction, and hold the actor's role or more on the
// actor's scope. When it does not, nothing is written, neither the change
// nor its entry, and the call fails with a Refusal in the chain, of kind
// Unauthenticated (the key is no good) or Denied (it lacks the role). A
// change that could meet NotFound or Conflict checks its actor before it
// reads or writes anything else, so a refused caller learns nothing about
// what it named; SetSetting and UnsetSetting, which meet neither, check it
// last, after their writes and before the commit. The check holds against
// every change that would undo it until the transaction ends, so a
// revocation, an expiry or a grant removed or lowered is ordered before the
// change, which is then refused, or after it, never in between. The local
// operator and the server hold no key, so this check never refuses them;
// the one change that asks for a particular caller, ReinstateKeys, which
// only the local operator may make, refuses every other Actor, the server
// included, as Denied. A zero Actor is Invalid.
//
// A call that returns an error has committed nothing. A context that ends
// during a call, a call on a closed store, a database that is read-only, and
// a file that is not a database of this type are Unavailable; the context's
// cause stays in the chain for errors.Is.
//
// A value is validated with its Validate method before anything is written,
// and refused as Invalid. A list's limit outside 1 to 1000 is Invalid. A
// cursor is bound to the list that made it, including that list's key,
// scope or filter: one used with another list, another key, scope or
// filter, or altered or cut short, is Invalid.
//
// Ids are compared whole and case-sensitively, and lists order text by its
// bytes, so "K1" and "k1" are two ids and "A" sorts before "_" before "a".
type Store interface {
	// Migrate brings the database's schema to the version this binary
	// knows, applying what is missing, in order, under a lock that holds
	// across processes. It refuses, changing nothing, a database whose
	// applied migrations differ from this binary's or that is at a version
	// this binary does not know.
	Migrate(ctx context.Context) error
	// Version returns the schema version the database is at: 0 before the
	// first migration.
	Version(ctx context.Context) (int, error)
	// Ping reports whether the database answers, as Unavailable if not.
	Ping(ctx context.Context) error
	// Close releases the database. Calls made after it, and calls in flight
	// when it is called, fail as Unavailable. A second Close does nothing.
	Close() error

	// CreateTenant adds a tenant. A name or id already taken is Conflict.
	CreateTenant(ctx context.Context, t Tenant, audit AuditEntry, by Actor) error
	// Tenant returns the tenant with this id, or NotFound.
	Tenant(ctx context.Context, id string) (Tenant, error)
	// TenantByName returns the tenant with this name, or NotFound.
	TenantByName(ctx context.Context, name string) (Tenant, error)
	// ListTenants returns a page of tenants in order of name, and the cursor
	// of the next page, empty after the last.
	ListTenants(ctx context.Context, page Page) ([]Tenant, string, error)
	// SetTenantState sets a tenant's state and its reason, or is NotFound.
	// The audit entry's details get the state before and after, read inside
	// the change (AuditEntry.WithStateChange).
	SetTenantState(ctx context.Context, id string, state TenantState, reason string, audit AuditEntry, by Actor) error
	// RemoveTenant removes a tenant and every grant on it, or is NotFound.
	RemoveTenant(ctx context.Context, id string, audit AuditEntry, by Actor) error

	// CreateKey adds a key. An id already taken is Conflict.
	CreateKey(ctx context.Context, k Key, audit AuditEntry, by Actor) error
	// Key returns the key with this id, revoked or expired or not, or
	// NotFound. It is on the path of every API request: one read of the
	// primary key.
	Key(ctx context.Context, id string) (Key, error)
	// ListKeys returns a page of keys in order of creation, then id.
	ListKeys(ctx context.Context, page Page) ([]Key, string, error)
	// MarkKeyUsed sets a key's last-used time to at, only when the time it
	// holds is unset or more than a minute before at, so a busy key costs one
	// write a minute, not one a request. The stored time never moves
	// backward: an at older than it writes nothing. It reports whether it
	// wrote. It is one statement with no read first, and takes no audit
	// entry: the request that used the key writes its own. An unknown key
	// writes nothing and is not an error, since the caller has just read the
	// key.
	//
	// A caller that has just read the key may skip the call when the key's
	// LastUsed is within the minute before at, which saves the statement on
	// nearly every request of a busy key; the store's own throttle still
	// holds for callers that do not.
	//
	// address is the caller's address at that use, at most MaxAddressLength
	// bytes of UTF-8, empty for a use on the local socket; it is written
	// with the time and under the same throttle, so it is the address of a
	// use at most a minute old, never one per request. An empty address
	// keeps the one stored: the last address is the last known on the
	// network.
	MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error)
	// RevokeKey sets a key's revoked time to at and returns it, or is
	// NotFound. A key already revoked keeps the time it was first revoked,
	// which is returned; nothing changes, and the audit entry, which records
	// the request, is still written (the API contract: revoking a revoked
	// key succeeds with its first revoke time). The entry's details say
	// whether the key was already revoked (AuditEntry.WithRevocation).
	RevokeKey(ctx context.Context, id string, at time.Time, audit AuditEntry, by Actor) (time.Time, error)

	// SetKeyRules replaces where a key may be used from (KeyRules), or is
	// NotFound. A revoked key is Conflict, decided in the change's
	// transaction.
	SetKeyRules(ctx context.Context, id string, rules KeyRules, audit AuditEntry, by Actor) error
	// RotateKey replaces the key id with successor, in one transaction: the
	// successor is added with the rotated key's holder and rules (read
	// inside the change; successor's own are ignored) and a copy of every
	// grant the rotated key holds, and the rotated key is set to stop
	// working at oldEnds, or at its own expiry when that is sooner
	// (RotatedExpiry), with Successor naming the new key. It returns the
	// successor as stored. An unknown key is NotFound; a key revoked,
	// expired at the successor's creation time, or already rotated is
	// Conflict, so a key makes at most one successor; a successor id taken
	// is Conflict. The audit entry's details get the successor's id and the
	// rotated key's new end (DetailSuccessor, DetailOldExpires).
	RotateKey(ctx context.Context, id string, successor Key, oldEnds time.Time, audit AuditEntry, by Actor) (Key, error)
	// RotatedFrom returns the key a rotation replaced with the key id (the
	// key whose Successor is id), revoked or expired or not, or NotFound
	// when id was not made by a rotation or does not exist. A key has at
	// most one: a key makes at most one successor, and a successor's id is
	// new. It is one read of an index.
	RotatedFrom(ctx context.Context, id string) (Key, error)

	// SetGrant gives a key a role on a scope. When the key already has a
	// grant on that scope, its role is replaced in one step, so a role is
	// changed with no moment of no access, which removing and adding the
	// grant would leave. It returns the role the grant had before, or the
	// empty Role when there was none, and the audit entry it writes has the
	// old and new role in its details (AuditEntry.WithRoleChange), both taken
	// inside the change, so two concurrent sets each see what the other left.
	// An unknown key or tenant is NotFound. A revoked key is Conflict,
	// decided in the same transaction as the write, so no grant is ever
	// written after the key's revocation (the API contract).
	SetGrant(ctx context.Context, g Grant, audit AuditEntry, by Actor) (Role, error)
	// GrantsByKey returns a page of a key's grants, the installation first,
	// then by tenant id, each with its tenant's name at the time of the
	// read. An unknown key has none. It reads a range of the grants'
	// primary key, as long as the key has grants on the page; the access
	// check, which runs on every request, uses RolesFor instead.
	GrantsByKey(ctx context.Context, keyID string, page Page) ([]Grant, string, error)
	// GrantsByScope returns a page of the grants on one scope, by key id,
	// each with its tenant's name at the time of the read.
	GrantsByScope(ctx context.Context, scope Scope, page Page) ([]Grant, string, error)
	// RolesFor returns a key's role on the installation, whether a tenant
	// is named tenantName and its id, and the key's role on that tenant,
	// each role the empty Role when the key has none (Roles). An empty
	// tenantName asks for the installation only. A name that is not a
	// valid tenant name is not an error: no tenant has it, so the tenant is
	// not found, and the same statement runs as for any other name, so a
	// missing tenant and a tenant without a grant cost the caller the same
	// work. A key that does not exist has no roles and is not an error
	// either. It is the access check's one read of grants and tenant on
	// every request: one statement of point lookups.
	RolesFor(ctx context.Context, keyID, tenantName string) (Roles, error)
	// CheckActor makes the check every change makes of its actor (see
	// Store), on its own, with nothing written: nil when by may act, or the
	// error with a Refusal that a change by it would return now. It is for
	// a caller about to change what this store does not hold (a tenant's
	// own database, which holds no keys) and for a read that must not
	// answer a key revoked since its request was checked: as late as it is
	// called, a revocation can still fall between it and what follows,
	// which a change's own check leaves no room for. A zero Actor is
	// Invalid. It is one statement of point lookups.
	CheckActor(ctx context.Context, by Actor) error
	// RemoveGrant removes a key's grant on a scope and returns the role it
	// had, or the empty Role when there was none: removing a grant the key
	// does not hold changes nothing and succeeds (the API contract). The
	// audit entry it writes has the old role in its details and "none" as
	// the new. An unknown key or tenant is NotFound.
	RemoveGrant(ctx context.Context, keyID string, scope Scope, audit AuditEntry, by Actor) (Role, error)

	// Setting returns a setting's value, or NotFound.
	Setting(ctx context.Context, name string) (string, error)
	// SetSetting sets a setting's value, adding the setting or replacing it.
	SetSetting(ctx context.Context, name, value string, audit AuditEntry, by Actor) error
	// UnsetSetting removes a setting's value and reports whether there was
	// one: removing a value that is not set changes nothing and succeeds, as
	// the API contract's UnsetSetting does. The audit entry is written
	// either way.
	UnsetSetting(ctx context.Context, name string, audit AuditEntry, by Actor) (bool, error)
	// ListSettings returns a page of settings in order of name.
	ListSettings(ctx context.Context, page Page) ([]Setting, string, error)

	// AppendAudit writes up to 10,000 entries, all or none. An id already
	// taken is ErrDuplicateAuditID (Internal). No entries is no write.
	AppendAudit(ctx context.Context, entries ...AuditEntry) error
	// ListAudit returns a page of the entries the filter keeps, newest
	// first: by time, then by id, both descending. The tenant name is
	// matched exactly, byte for byte.
	ListAudit(ctx context.Context, filter AuditFilter, page Page) ([]AuditEntry, string, error)

	// AuditSeals is the seal of the master's audit log (auditseal.go). Its
	// changes are the server's own and take no Actor.
	AuditSeals

	// KeyReinstatement lets suspended keys work again (suspend.go). It is
	// the local operator's alone and refuses every other Actor.
	KeyReinstatement
}
