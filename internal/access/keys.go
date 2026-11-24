package access

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The limits of the keys this package makes (access.proto, CreateKeyRequest).
const (
	// DefaultKeyLifetime is how long a key lives when its maker names no
	// expiry: a key nobody remembers still stops working on its own.
	DefaultKeyLifetime = 90 * 24 * time.Hour
	// MaxKeyLifetime is the longest expiry a key may be given, ten years. A
	// key meant to outlive that is made with NeverExpire, in so many words.
	MaxKeyLifetime = 3650 * 24 * time.Hour
	// DefaultPageSize is the size of a page when the request asks for none.
	DefaultPageSize = 100
	// MaxPageTokenLength bounds a page token, as the contract states.
	MaxPageTokenLength = 1 << 10
)

// The keys of the details this package writes in audit entries.
const (
	detailHolder  = "holder"
	detailExpires = "expires"
	detailScope   = "scope"
	detailFilter  = "filter"
)

// Expiry is when a new key stops working. The zero Expiry is the default,
// DefaultKeyLifetime after the key is made.
type Expiry struct {
	after time.Duration
	never bool
	set   bool
}

// ExpireAfter is an expiry d after the key is made. d must be positive and
// at most MaxKeyLifetime, or CreateKey refuses it as Invalid.
func ExpireAfter(d time.Duration) Expiry { return Expiry{after: d, set: true} }

// NeverExpire is a key that never expires. It is its own constructor, so a
// key without an expiry is always asked for in so many words.
func NeverExpire() Expiry { return Expiry{never: true, set: true} }

// expires returns the expiry time of a key made at now, the zero time for
// never, or an Invalid error.
func (e Expiry) expires(now time.Time) (time.Time, error) {
	switch {
	case !e.set:
		return now.Add(DefaultKeyLifetime), nil
	case e.never:
		return time.Time{}, nil
	case e.after <= 0 || e.after > MaxKeyLifetime:
		return time.Time{}, fault.New(fault.Invalid, "a key's expiry is positive and at most 10 years (3650 days)")
	}
	return now.Add(e.after), nil
}

// Scope is a scope as a request names it: one tenant by its name, or the
// installation. The zero Scope is the installation.
type Scope struct {
	// Tenant is the tenant's name, or empty for the installation.
	Tenant string
}

// InstallationScope is the installation.
var InstallationScope = Scope{}

// TenantNamed returns the scope of the tenant with this name.
func TenantNamed(name string) Scope { return Scope{Tenant: name} }

// IsInstallation reports whether s is the installation.
func (s Scope) IsInstallation() bool { return s.Tenant == "" }

// String returns "installation" or "tenant/<name>", as the scope is written
// in an audit entry's details. A name that could not be a tenant's is quoted.
func (s Scope) String() string {
	if s.IsInstallation() {
		return "installation"
	}
	return "tenant/" + clipTenantName(s.Tenant)
}

// Grant is a grant as a request reads it: its scope by tenant name.
type Grant struct {
	KeyID string
	Scope Scope
	Role  master.Role
}

// CreatedKey is a key just made. Secret holds the whole key, to be shown to
// its holder once; it prints as its id only, and Secret.Key() is the one
// call that reads it. Key has no hash.
type CreatedKey struct {
	Key    master.Key
	Secret secret.NewAPIKey
}

// GrantFilter says whose grants ListGrants lists: one key's, or those on one
// scope. The zero GrantFilter is refused as Invalid: there is no listing of
// every grant at once.
type GrantFilter struct {
	keyID   string
	scope   Scope
	byKey   bool
	byScope bool
}

// GrantsOfKey lists the grants of one key, on every scope.
func GrantsOfKey(keyID string) GrantFilter { return GrantFilter{keyID: keyID, byKey: true} }

// GrantsOnScope lists the grants on one scope, of every key.
func GrantsOnScope(s Scope) GrantFilter { return GrantFilter{scope: s, byScope: true} }

// The audit reasons of a management call refused for the Decision it was
// given, or for its key at the change. A zero Decision is refused without an
// entry: it carries none.
const (
	reasonNotAdmin = "the decision does not carry admin on the installation"
	sinceCheck     = " since the check"
)

// actFor verifies that d is a Decision Check made and that its principal, as
// the check read it, holds admin on the installation when needAdmin (every
// operation on keys and grants needs it in this version: access.proto,
// AccessService), so a Decision made for a lesser requirement by a wrong
// entry in the server's table of methods opens nothing. It reads nothing.
//
// It returns the request's entry, with a fresh id, and with object and
// details in place of those the server gave Check when they are given, so
// the entry stays within the master's bounds once the master adds its own
// details; and the Actor the change is made by, admin on the installation,
// or for WhoAmI only a valid key, which the master checks again inside the
// change's transaction (master.Store), so a key revoked, expired or stripped
// of its grant since the check makes no change.
func (a *Access) actFor(d Decision, needAdmin bool, object string, details map[string]string) (master.AuditEntry, master.Actor, error) {
	if !d.checked() {
		return master.AuditEntry{}, master.Actor{}, fault.New(fault.Denied, MessageDenied)
	}
	e := d.Entry(master.OutcomeDone, "", Facts{})
	if object != "" {
		e.Object = object
	}
	if details != nil {
		e.Details = details
	}
	role := master.Role("")
	if needAdmin {
		role = master.RoleAdmin
		if !atLeast(d.principal.Role(master.Installation), role) {
			a.record(e, master.OutcomeRefused, reasonNotAdmin)
			return master.AuditEntry{}, master.Actor{}, fault.New(fault.Denied, MessageDenied)
		}
	}
	return e, d.Actor(master.Installation, role), nil
}

// change is actFor for an operation that changes the master: the actor goes
// into the change, whose transaction checks it. Before anything else reads
// what the request names (the key to revoke, the tenant of a scope) or
// validates it, the key is checked as a read checks it, unless Confirm
// already did for this request: so a key that may no longer act is told so,
// and is not told whether what it names exists or whether its request is
// valid.
func (a *Access) change(ctx context.Context, d Decision, object string, details map[string]string) (master.AuditEntry, master.Actor, error) {
	e, by, err := a.actFor(d, true, object, details)
	if err != nil || d.confirmed {
		return e, by, err
	}
	if err := a.store.CheckActor(ctx, by); err != nil {
		return master.AuditEntry{}, master.Actor{}, a.failAfter(e, err)
	}
	return e, by, nil
}

// Confirm checks d's key again against the requirement Check allowed, by the
// check every change makes in its own transaction (master.Store.CheckActor),
// and returns d marked as confirmed. A handler that changes something calls
// it first, before it validates the request or reads what the request
// names, so the order of the API contract (the credential, then the role,
// then whether what is named exists, then whether the request is valid) holds
// for a key that may have lapsed since the check: such a key is refused
// whatever it asked, and the request is recorded as refused, in the
// installation's log, as the check records its own refusals. A store's
// failure is recorded and answered as Unavailable or Internal. The local
// operator needs no check. The change itself still checks the key in its
// transaction; the mark only spares this package's operations a second read
// before it.
func (a *Access) Confirm(ctx context.Context, d Decision) (Decision, error) {
	if !d.checked() {
		return d, fault.New(fault.Denied, MessageDenied)
	}
	var by master.Actor
	switch d.req.kind {
	case needInstallation:
		by = d.Actor(master.Installation, d.req.role)
	case needTenant:
		by = d.Actor(master.TenantScope(d.tenant.ID), d.req.role)
	default:
		by = d.Actor(master.Installation, "")
	}
	if err := a.store.CheckActor(ctx, by); err != nil {
		return d, a.failAfter(d.Entry(master.OutcomeDone, "", Facts{}), err)
	}
	d.confirmed = true
	return d, nil
}

// read is actFor for an operation that only reads, which has no transaction
// to check its actor in: the master checks it first, by the same check a
// change makes (master.Store.CheckActor), so a key revoked since the check
// reads nothing. A revocation in the moment between that check and the read
// is not seen by that one read.
func (a *Access) read(ctx context.Context, d Decision, needAdmin bool, object string, details map[string]string) (master.AuditEntry, error) {
	e, by, err := a.actFor(d, needAdmin, object, details)
	if err != nil {
		return master.AuditEntry{}, err
	}
	if err := a.store.CheckActor(ctx, by); err != nil {
		return master.AuditEntry{}, a.failAfter(e, err)
	}
	return e, nil
}

// RefusedAtChange tells whether err is a change, or a CheckActor, refused
// because its key may no longer act (a master.Refusal in its chain). If so it
// returns the reason the request's audit entry records, which says the cause
// arose since the check, and the error the caller is shown: the one Check
// answers for the same cause, in kind and text (Unauthenticated with
// MessageUnauthenticated for a key that is no good, Denied with MessageDenied
// for one without the role). Otherwise refusal is nil. A caller that answers
// a request answers a refused change with it and records the request as
// refused, in the installation's log.
func RefusedAtChange(err error) (reason string, refusal error) {
	r, ok := master.RefusalOf(err)
	if !ok {
		return "", nil
	}
	reason = auditText(r.Reason+sinceCheck, master.MaxAuditReasonLength)
	if fault.KindOf(err) == fault.Denied {
		return reason, fault.New(fault.Denied, MessageDenied)
	}
	return reason, fault.New(fault.Unauthenticated, MessageUnauthenticated)
}

// failAfter records why an allowed request did not complete, and returns the
// error its caller is shown. A change refused because its key may no longer
// act is a refusal, recorded and answered as RefusedAtChange says. What the
// caller may know (Invalid, NotFound, Conflict) keeps its kind and message. A
// store's failure is Unavailable or Internal, with a message that says
// nothing of the cause. The entry is the one the failed change did not write
// (a change and its entry are both written or neither), so its id is still
// free.
func (a *Access) failAfter(e master.AuditEntry, err error) error {
	if reason, refusal := RefusedAtChange(err); refusal != nil {
		a.record(e, master.OutcomeRefused, reason)
		return refusal
	}
	if errors.Is(err, master.ErrDuplicateAuditID) {
		// The id source gave an id already used, so the failure is recorded
		// under a fresh one rather than refused again for the same reason.
		e.ID = a.newID()
	}
	e.Outcome = master.OutcomeFailed
	switch fault.KindOf(err) {
	case fault.Invalid, fault.NotFound, fault.Conflict:
		e.Reason = auditText(fault.Message(err), master.MaxAuditReasonLength)
		a.enqueueRecord(a.store, e)
		return err
	}
	e.Reason = auditText(reasonStoreFailure+": "+fault.KindOf(err).String(), master.MaxAuditReasonLength)
	a.enqueueRecord(a.store, e)
	return storeError(err)
}

// appendRead hands a read's audit entry to the audit writer. A read changes
// nothing, so its entry does not need its own synced commit on the request's
// path; it is written with the next batch (see the package documentation
// for what that means when the server is overwhelmed or killed).
func (a *Access) appendRead(e master.AuditEntry) {
	a.enqueueRecord(a.store, e)
}

// page returns a request's page with the contract's default and bounds
// applied: zero is DefaultPageSize; a negative size, one above
// master.MaxPageLimit, or a token over 1 KiB is Invalid.
func page(p master.Page) (master.Page, error) {
	if p.Limit == 0 {
		p.Limit = DefaultPageSize
	}
	if p.Limit < 0 || p.Limit > master.MaxPageLimit {
		return p, fault.New(fault.Invalid, "a page's size is 0 to "+strconv.Itoa(master.MaxPageLimit))
	}
	if len(p.Cursor) > MaxPageTokenLength {
		return p, fault.New(fault.Invalid, "a page token is at most 1 KiB")
	}
	return p, nil
}

// publicKey returns k without its hash: nothing this package returns
// carries one.
func publicKey(k master.Key) master.Key {
	k.Hash = nil
	return k
}

// CreateKey makes a key for holder, which expires as exp says and may be
// used only as rules allow. It needs the installation's admin, and so does a
// key that never expires: no other caller can ask for one. The holder is
// required, at most 256 bytes of UTF-8. The key and its audit entry, which
// names the key, its holder and its rules and never its secret, are written
// in one transaction. The whole key is in the result's Secret, once: only
// its hash is stored.
func (a *Access) CreateKey(ctx context.Context, d Decision, holder string, exp Expiry, rules master.KeyRules) (CreatedKey, error) {
	entry, by, err := a.change(ctx, d, "", map[string]string{detailHolder: auditText(holder, master.MaxAuditDetailValueLength)})
	if err != nil {
		return CreatedKey{}, err
	}
	now := entry.Time
	expires, err := exp.expires(now)
	if err != nil {
		return CreatedKey{}, a.failAfter(entry, err)
	}
	made, err := secret.MakeAPIKey()
	if err != nil {
		return CreatedKey{}, a.failAfter(entry, err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: holder, Created: now, Expires: expires, Rules: rules}
	entry.Object = k.ID
	addRuleDetails(entry.Details, rules)
	if expires.IsZero() {
		entry.Details[detailExpires] = "never"
	} else {
		entry.Details[detailExpires] = expires.UTC().Format(time.RFC3339)
	}
	if err := k.Validate(); err != nil {
		return CreatedKey{}, a.failAfter(entry, err)
	}
	if err := a.store.CreateKey(ctx, k, entry, by); err != nil {
		return CreatedKey{}, a.failAfter(entry, err)
	}
	return CreatedKey{Key: publicKey(k), Secret: made}, nil
}

// GetKey returns one key, revoked and expired keys included, without its
// hash. It needs the installation's admin. An unknown id is NotFound.
func (a *Access) GetKey(ctx context.Context, d Decision, id string) (master.Key, error) {
	entry, err := a.read(ctx, d, true, auditText(id, master.MaxAuditFieldLength), nil)
	if err != nil {
		return master.Key{}, err
	}
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, a.failAfter(entry, err)
	}
	k, err := a.store.Key(ctx, id)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return master.Key{}, a.failAfter(entry, err)
	}
	a.appendRead(entry)
	return publicKey(k), nil
}

// ListKeys returns a page of every key, revoked and expired keys included,
// oldest first, without their hashes, and the token of the next page. It
// needs the installation's admin.
func (a *Access) ListKeys(ctx context.Context, d Decision, p master.Page) ([]master.Key, string, error) {
	entry, err := a.read(ctx, d, true, "", nil)
	if err != nil {
		return nil, "", err
	}
	p, err = page(p)
	if err != nil {
		return nil, "", a.failAfter(entry, err)
	}
	keys, next, err := a.store.ListKeys(ctx, p)
	if err != nil {
		return nil, "", a.failAfter(entry, err)
	}
	a.appendRead(entry)
	for i := range keys {
		keys[i] = publicKey(keys[i])
	}
	return keys, next, nil
}

// RevokeKey ends a key for good and returns it with its revoke time. It
// needs the installation's admin. Revoking a revoked key changes nothing and
// returns its first revoke time; the request is still recorded. An unknown
// id is NotFound. Because every check reads the key, the key's next request
// is refused. The key is read before the revocation, and the answer is that
// read with the revoke time the revocation returned: a revocation that
// happened is never answered as a failure.
func (a *Access) RevokeKey(ctx context.Context, d Decision, id string) (master.Key, error) {
	entry, by, err := a.change(ctx, d, auditText(id, master.MaxAuditFieldLength), nil)
	if err != nil {
		return master.Key{}, err
	}
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, a.failAfter(entry, err)
	}
	k, err := a.store.Key(ctx, id)
	if err == nil {
		k.Revoked, err = a.store.RevokeKey(ctx, id, a.clock(), entry, by)
	}
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return master.Key{}, a.failAfter(entry, err)
	}
	return publicKey(k), nil
}

// resolve returns the master's scope for s, reading the tenant by name. A
// tenant that does not exist is NotFound: the caller has passed the
// installation admin's check, so it may list every tenant and learns nothing
// from being told a name is unknown.
func (a *Access) resolve(ctx context.Context, s Scope) (master.Scope, error) {
	if s.IsInstallation() {
		return master.Installation, nil
	}
	if err := master.ValidateTenantName(s.Tenant); err != nil {
		return master.Scope{}, err
	}
	t, err := a.store.TenantByName(ctx, s.Tenant)
	if fault.KindOf(err) == fault.NotFound {
		return master.Scope{}, fault.New(fault.NotFound, MessageNoTenant)
	}
	if err != nil {
		return master.Scope{}, err
	}
	return master.TenantScope(t.ID), nil
}

// SetGrant gives the key keyID role on scope, replacing in one step the
// role it held there, and returns the grant and the role it replaced (the
// empty Role for none). It needs the installation's admin. Writer is not a
// role on the installation and is Invalid there; an unknown key or tenant is
// NotFound; a revoked key is Conflict. The audit entry has the scope and,
// from the master, the old and the new role.
func (a *Access) SetGrant(ctx context.Context, d Decision, keyID string, scope Scope, role master.Role) (Grant, master.Role, error) {
	entry, by, err := a.change(ctx, d, auditText(keyID, master.MaxAuditFieldLength), map[string]string{detailScope: scope.String()})
	if err != nil {
		return Grant{}, "", err
	}
	if err := master.ValidateID("key id", keyID); err != nil {
		return Grant{}, "", a.failAfter(entry, err)
	}
	if !role.Valid() {
		return Grant{}, "", a.failAfter(entry, fault.New(fault.Invalid, "a role is reader, writer, operator or admin"))
	}
	if role == master.RoleWriter && scope.IsInstallation() {
		return Grant{}, "", a.failAfter(entry, fault.New(fault.Invalid, "writer is not a role on the installation"))
	}
	ms, err := a.resolve(ctx, scope)
	if err != nil {
		return Grant{}, "", a.failAfter(entry, err)
	}
	// The port refuses a revoked key as Conflict in the transaction that
	// would write the grant, so no grant follows a revocation.
	previous, err := a.store.SetGrant(ctx, master.Grant{KeyID: keyID, Scope: ms, Role: role}, entry, by)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key or tenant has this id")
		}
		return Grant{}, "", a.failAfter(entry, err)
	}
	return Grant{KeyID: keyID, Scope: scope, Role: role}, previous, nil
}

// RemoveGrant takes back the key keyID's grant on scope and returns the
// role it had, or the empty Role when it held none, which changes nothing
// and succeeds. It needs the installation's admin. An unknown key or tenant
// is NotFound.
func (a *Access) RemoveGrant(ctx context.Context, d Decision, keyID string, scope Scope) (master.Role, error) {
	entry, by, err := a.change(ctx, d, auditText(keyID, master.MaxAuditFieldLength), map[string]string{detailScope: scope.String()})
	if err != nil {
		return "", err
	}
	if err := master.ValidateID("key id", keyID); err != nil {
		return "", a.failAfter(entry, err)
	}
	ms, err := a.resolve(ctx, scope)
	if err != nil {
		return "", a.failAfter(entry, err)
	}
	previous, err := a.store.RemoveGrant(ctx, keyID, ms, entry, by)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return "", a.failAfter(entry, err)
	}
	return previous, nil
}

// ListGrants returns a page of the grants filter keeps, with each scope by
// its tenant's name, and the token of the next page. It needs the
// installation's admin. A scope's tenant that does not exist is NotFound; a
// key that does not exist has no grants.
func (a *Access) ListGrants(ctx context.Context, d Decision, filter GrantFilter, p master.Page) ([]Grant, string, error) {
	desc := "none"
	switch {
	case filter.byKey:
		desc = "key/" + auditText(filter.keyID, master.MaxIDLength)
	case filter.byScope:
		desc = filter.scope.String()
	}
	entry, err := a.read(ctx, d, true, "", map[string]string{detailFilter: desc})
	if err != nil {
		return nil, "", err
	}
	p, err = page(p)
	if err != nil {
		return nil, "", a.failAfter(entry, err)
	}
	var grants []master.Grant
	var next string
	switch {
	case filter.byKey:
		if err := master.ValidateID("key id", filter.keyID); err != nil {
			return nil, "", a.failAfter(entry, err)
		}
		grants, next, err = a.store.GrantsByKey(ctx, filter.keyID, p)
	case filter.byScope:
		var ms master.Scope
		ms, err = a.resolve(ctx, filter.scope)
		if err == nil {
			grants, next, err = a.store.GrantsByScope(ctx, ms, p)
		}
	default:
		err = fault.New(fault.Invalid, "grants are listed by key or by scope")
	}
	if err != nil {
		return nil, "", a.failAfter(entry, err)
	}
	out := named(grants)
	a.appendRead(entry)
	return out, next, nil
}

// named returns grants with each scope by its tenant's name, which the
// master's lists fill in at the time of the read.
func named(grants []master.Grant) []Grant {
	out := make([]Grant, 0, len(grants))
	for _, g := range grants {
		s := InstallationScope
		if !g.Scope.IsInstallation() {
			s = TenantNamed(g.TenantName)
		}
		out = append(out, Grant{KeyID: g.KeyID, Scope: s, Role: g.Role})
	}
	return out
}

// WhoAmI returns the caller of d and every grant of its key, each scope by
// its tenant's name, and records the request. Any caller Check
// authenticated may ask, while its key is still good; a zero Decision is
// Denied. The local operator has
// no grants: it is admin by being the user that runs the server.
func (a *Access) WhoAmI(ctx context.Context, d Decision) (Principal, []Grant, error) {
	entry, err := a.read(ctx, d, false, "", nil)
	if err != nil {
		return Principal{}, nil, err
	}
	var all []master.Grant
	if d.principal.isKey {
		all, err = a.allGrants(ctx, d.principal.key.ID)
		if err != nil {
			return Principal{}, nil, a.failAfter(entry, err)
		}
	}
	grants := named(all)
	a.appendRead(entry)
	return d.principal, grants, nil
}
