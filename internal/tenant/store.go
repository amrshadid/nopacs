package tenant

import (
	"context"

	"github.com/amrshadid/nopacs/internal/master"
)

// Store is one tenant's database, stated as operations. A driver package
// implements it for one database type and proves it with the contract suite
// in package tenanttest. A store belongs to one tenant, whose id it is
// opened with.
//
// Every method takes a context and fails with an error from package fault.
// Every method that changes the database takes the audit entry for that
// change and writes both or neither: if the entry cannot be written (its id
// is taken, the database fails), the change is not made, and if the change
// cannot be made (an invalid value, any failure), the entry is not written.
// A refusal or a failure that changes nothing is recorded with AppendAudit.
// An audit entry whose id is taken fails with master.ErrDuplicateAuditID in
// the chain, of kind Internal: ids are made by the caller, so a duplicate is
// a bug, never a Conflict.
//
// A call that returns an error has committed nothing. A context that ends
// during a call, a call on a closed store or in flight when it is closed, a
// database that is read-only, and a file that is not a database of this
// type are Unavailable; the context's cause stays in the chain for
// errors.Is.
//
// A value is validated with the master package's rules before anything is
// written, and refused as Invalid. A list's limit outside 1 to 1000 is
// Invalid. A cursor is bound to the list that made it, to that list's
// filter, and to the tenant: one used with the other list, another filter,
// another tenant's store, or altered or cut short, is Invalid.
//
// Every method that changes the database also takes a Guard: the check of
// who makes the change, which the store runs last in the change's
// transaction, after the change and its audit entry are written and before
// the commit. A Guard that returns an error undoes the change: nothing is
// committed, neither the change nor its entry, and the call returns that
// error as it is (its kind and chain kept). A change that fails before the
// Guard's turn (its entry's id taken, say) does not run it. A nil Guard is a change no key makes,
// the server's own. The package documentation says why a tenant's change
// takes a Guard and not the key itself, and what that leaves open.
//
// Text is kept and matched byte for byte, and lists order it by its bytes.
// No error a store returns holds its database's address or password; its
// message (fault.Message) is written to be shown, logged and kept in a
// tenant's state reason.
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
	// when it is called, fail as Unavailable; a second Close does nothing.
	Close() error

	// Setting returns the tenant's override of a setting, or NotFound when
	// the tenant has none and the installation's value applies.
	Setting(ctx context.Context, name string) (string, error)
	// SetSetting sets the tenant's override of a setting, adding it or
	// replacing it. The name and value are bounded as the master's are
	// (master.ValidateSettingName, master.ValidateSettingValue); the empty
	// string is a value.
	SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, guard Guard) error
	// UnsetSetting removes the tenant's override and reports whether there
	// was one: removing one that is not set changes nothing and succeeds, as
	// the API contract's UnsetSetting does. The audit entry is written
	// either way.
	UnsetSetting(ctx context.Context, name string, audit master.AuditEntry, guard Guard) (bool, error)
	// ListSettings returns a page of the tenant's overrides in order of
	// name, and the cursor of the next page, empty after the last. A setting
	// present throughout a listing is returned exactly once, whatever is set
	// or unset meanwhile.
	ListSettings(ctx context.Context, page master.Page) ([]master.Setting, string, error)

	// AppendAudit writes up to master.MaxAuditBatch entries to the tenant's
	// audit log, all or none. An id already taken is
	// master.ErrDuplicateAuditID (Internal). No entries is no write.
	AppendAudit(ctx context.Context, entries ...master.AuditEntry) error
	// ListAudit returns a page of the entries the filter keeps, newest
	// first: by time, then by id, both descending. The tenant name is
	// matched exactly, byte for byte. An entry present throughout a listing
	// is returned exactly once, whatever is appended meanwhile.
	ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error)

	// AuditSeals is the seal of the tenant's audit log, with the master's
	// rules (master.AuditSeals). Its changes are the server's own and take
	// no Guard.
	master.AuditSeals
}

// Guard is the check a change makes of who makes it, run by the store last
// in the change's transaction: after the change and its audit entry are
// written, so after any wait for the write lock or for the rows the change
// needs, and just before the commit (Store). Keys and grants live only in
// the master, so a tenant's store cannot read them in its own transaction;
// the caller hands it this check instead, which asks the master
// (master.Store.CheckActor).
//
// A Guard may be called more than once for one change: a driver that
// commits several changes in one transaction runs a change again, Guard
// included, when that transaction is undone by something else (as the
// SQLite driver does). It must be safe to repeat: it reads, and decides,
// and does nothing outside the change's transaction that a repeat would
// do twice. The change it allows is still made once.
type Guard func(ctx context.Context) error
