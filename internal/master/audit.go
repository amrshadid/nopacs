package master

import (
	"strconv"
	"time"
)

// AuditEntry records one request, or one change, made to the installation. It
// never holds a key's secret or a database password; the caller that builds
// it is responsible for that, and the bounds below keep an entry small.
type AuditEntry struct {
	// ID is the entry's opaque id, given by the caller, unique among entries.
	ID string
	// Time is when the request was made.
	Time time.Time
	// KeyID is the id of the key that made the request; empty when it was
	// made on the local socket or with no key at all.
	KeyID string
	// LocalUser is the operating system user that made the request on the
	// local socket; empty otherwise. At most one of KeyID and LocalUser is
	// set.
	LocalUser string
	// RemoteAddress is where the request came from.
	RemoteAddress string
	// Method is the API method or command that was called.
	Method string
	// TenantName is the tenant the request named, as it named it: not a
	// tenant id, so the entry still reads right after the tenant is removed,
	// and can record a request for a tenant that never existed.
	TenantName string
	// Object is what inside the tenant or installation the request was about.
	Object string
	// Outcome is how the request ended.
	Outcome Outcome
	// Reason is the real cause of a refusal or a failure, which the caller
	// may not have been told; it may be empty.
	Reason string
	// Details holds what else the entry needs to be read: a database type, a
	// scope, an old and a new role, a setting's name, the filters of a list.
	// Never a secret.
	Details map[string]string
	// RequestID is the request's id, which also appears in the log.
	RequestID string
}

// Validate returns an Invalid error when e cannot be stored.
func (e AuditEntry) Validate() error {
	if err := ValidateID("audit entry id", e.ID); err != nil {
		return err
	}
	if err := ValidateTime("audit time", e.Time); err != nil {
		return err
	}
	if e.KeyID != "" && e.LocalUser != "" {
		return invalid("an audit entry names a key or a local user, not both")
	}
	if e.KeyID != "" {
		if err := ValidateID("key id", e.KeyID); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"local user", e.LocalUser},
		{"remote address", e.RemoteAddress},
		{"tenant name", e.TenantName},
		{"object", e.Object},
		{"request id", e.RequestID},
	} {
		if err := validateText(f.name, f.value, 0, MaxAuditFieldLength); err != nil {
			return err
		}
	}
	if err := validateText("method", e.Method, 1, MaxAuditFieldLength); err != nil {
		return err
	}
	if !e.Outcome.Valid() {
		return invalid("an outcome is done, refused or failed")
	}
	if err := validateText("reason", e.Reason, 0, MaxAuditReasonLength); err != nil {
		return err
	}
	if len(e.Details) > MaxAuditDetails {
		return invalid("an audit entry has at most " + strconv.Itoa(MaxAuditDetails) + " details")
	}
	for k, v := range e.Details {
		if err := validateText("detail key", k, 1, MaxAuditDetailKeyLength); err != nil {
			return err
		}
		if err := validateText("detail value", v, 0, MaxAuditDetailValueLength); err != nil {
			return err
		}
	}
	return nil
}

// WithRoleChange returns a copy of e whose details also say the role a grant
// had before a change and the role it has after, "none" for no grant. A
// driver calls it inside the change, where the old role is known, so the
// entry it writes is exact even when two changes race. The copy has its own
// map, so the caller's entry is not changed.
func (e AuditEntry) WithRoleChange(old, next Role) AuditEntry {
	return e.WithDetails(DetailOldRole, old.detail(), DetailNewRole, next.detail())
}

// ValidateRoleChange returns an Invalid error when e would be past its bounds
// once WithRoleChange has added its two details, so a driver can refuse it
// before it changes anything.
func (e AuditEntry) ValidateRoleChange() error {
	return e.WithRoleChange(RoleAdmin, RoleAdmin).Validate()
}

// WithStateChange returns a copy of e whose details also say a tenant's
// state before and after SetTenantState, read inside the change.
func (e AuditEntry) WithStateChange(old, next TenantState) AuditEntry {
	return e.WithDetails(DetailOldState, string(old), DetailNewState, string(next))
}

// WithRevocation returns a copy of e whose details also say whether the key
// was already revoked before RevokeKey, read inside the change.
func (e AuditEntry) WithRevocation(alreadyRevoked bool) AuditEntry {
	return e.WithDetails(DetailAlreadyRevoked, strconv.FormatBool(alreadyRevoked))
}

// WithDetails returns a copy of e with the key and value pairs added to its
// details, replacing a key the caller set. The copy has its own map, so the
// caller's entry is not changed. An odd last key is ignored.
func (e AuditEntry) WithDetails(pairs ...string) AuditEntry {
	details := make(map[string]string, len(e.Details)+len(pairs)/2)
	for k, v := range e.Details {
		details[k] = v
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		details[pairs[i]] = pairs[i+1]
	}
	e.Details = details
	return e
}

// ValidateWith returns an Invalid error when e would be past its bounds once
// the detail keys named have been added (with values of the longest a store
// writes, "unreachable"), so a driver can refuse it before it changes
// anything.
func (e AuditEntry) ValidateWith(keys ...string) error {
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, string(StateUnreachable))
	}
	return e.WithDetails(pairs...).Validate()
}

// AuditFilter narrows ListAudit. A zero field matches every entry.
type AuditFilter struct {
	// TenantName keeps entries that named this tenant.
	TenantName string
	// KeyID keeps entries made with this key.
	KeyID string
	// Outcome keeps entries with this outcome.
	Outcome Outcome
	// Since keeps entries at or after this time.
	Since time.Time
	// Until keeps entries before this time.
	Until time.Time
}

// Validate returns an Invalid error when f cannot be applied.
func (f AuditFilter) Validate() error {
	if err := validateText("tenant name", f.TenantName, 0, MaxAuditFieldLength); err != nil {
		return err
	}
	if f.KeyID != "" {
		if err := ValidateID("key id", f.KeyID); err != nil {
			return err
		}
	}
	if f.Outcome != "" && !f.Outcome.Valid() {
		return invalid("an outcome is done, refused or failed")
	}
	for _, t := range []struct {
		name string
		t    time.Time
	}{{"start time", f.Since}, {"end time", f.Until}} {
		if !t.t.IsZero() {
			if err := ValidateTime(t.name, t.t); err != nil {
				return err
			}
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return invalid("an end time is after the start time")
	}
	return nil
}
