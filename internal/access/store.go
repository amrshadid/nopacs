package access

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Store is the part of the master database this package uses: the
// operations it calls and no others. master.Store satisfies it, so a server
// passes its master store as it is; stating it here keeps a change of the
// master port to this one file, and lets a test wrap the store to count what
// a check reads.
type Store interface {
	// Key returns the key with this id, or NotFound.
	Key(ctx context.Context, id string) (master.Key, error)
	// RolesFor returns the key's role on the installation, whether the
	// tenant named tenantName exists and its id, and the key's role on it,
	// in one statement; an empty tenantName asks for the installation only,
	// and a name that is not valid finds no tenant. It is the check's one
	// read after the key, on the path of every request made with a key.
	RolesFor(ctx context.Context, keyID, tenantName string) (master.Roles, error)
	// GrantsByKey returns a page of a key's grants.
	GrantsByKey(ctx context.Context, keyID string, page master.Page) ([]master.Grant, string, error)
	// GrantsByScope returns a page of the grants on one scope.
	GrantsByScope(ctx context.Context, scope master.Scope, page master.Page) ([]master.Grant, string, error)
	// TenantByName returns the tenant with this name, or NotFound.
	TenantByName(ctx context.Context, name string) (master.Tenant, error)
	// MarkKeyUsed records a key's use, at most once a minute per key.
	MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error)
	// AppendAudit writes audit entries, all or none.
	AppendAudit(ctx context.Context, entries ...master.AuditEntry) error
	// CheckActor makes the check of an actor that every change makes in
	// its transaction, on its own: nil, or the error with a
	// master.Refusal a change by it would return now.
	CheckActor(ctx context.Context, by master.Actor) error
	// CreateKey adds a key with its audit entry, if by is still allowed to.
	CreateKey(ctx context.Context, k master.Key, audit master.AuditEntry, by master.Actor) error
	// ListKeys returns a page of keys.
	ListKeys(ctx context.Context, page master.Page) ([]master.Key, string, error)
	// RevokeKey revokes a key with its audit entry, if by is still allowed
	// to, and returns its revoke time.
	RevokeKey(ctx context.Context, id string, at time.Time, audit master.AuditEntry, by master.Actor) (time.Time, error)
	// SetKeyRules replaces a key's rules with its audit entry, if by is
	// still allowed to.
	SetKeyRules(ctx context.Context, id string, rules master.KeyRules, audit master.AuditEntry, by master.Actor) error
	// RotateKey replaces a key with a successor holding its grants and
	// rules, with its audit entry, if by is still allowed to.
	RotateKey(ctx context.Context, id string, successor master.Key, oldEnds time.Time, audit master.AuditEntry, by master.Actor) (master.Key, error)
	// SetGrant sets a key's role on a scope with its audit entry, if by is
	// still allowed to, and returns the role it replaced.
	SetGrant(ctx context.Context, g master.Grant, audit master.AuditEntry, by master.Actor) (master.Role, error)
	// RemoveGrant removes a key's grant on a scope with its audit entry, if
	// by is still allowed to, and returns the role it had.
	RemoveGrant(ctx context.Context, keyID string, scope master.Scope, audit master.AuditEntry, by master.Actor) (master.Role, error)
}

// The master port must keep satisfying Store; if it changes, the build fails
// here, in the one file to change.
var _ Store = master.Store(nil)
