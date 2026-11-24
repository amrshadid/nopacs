package api

import (
	"strconv"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The limits of the contract that this package checks before acting
// (access.proto, "Limits" and "Rules of the whole package"; tenant.proto,
// CreateTenantRequest).
const (
	// DefaultPageSize is the size of a page when page_size is zero.
	DefaultPageSize = access.DefaultPageSize
	// MaxPageSize is the largest page_size; a larger one is invalid.
	MaxPageSize = master.MaxPageLimit
	// MaxPageTokenLength bounds page_token, in bytes.
	MaxPageTokenLength = access.MaxPageTokenLength
	// MaxDatabaseAddressLength bounds CreateTenantRequest.database_address,
	// in bytes.
	MaxDatabaseAddressLength = 4 << 10
	// MaxHolderLength bounds CreateKeyRequest.holder, in bytes.
	MaxHolderLength = master.MaxHolderLength
	// MaxKeyLifetime is the longest CreateKeyRequest.expire_after.
	MaxKeyLifetime = access.MaxKeyLifetime
)

// invalid returns an Invalid error whose message starts with the field it is
// about, as the contract's order of checks asks a handler to name it.
func invalid(field, rule string) error {
	return fault.New(fault.Invalid, field+": "+rule)
}

// pageOf returns the page a list request asks for: page_size 0 is
// DefaultPageSize, a negative one or one above MaxPageSize is invalid, and a
// token over MaxPageTokenLength is invalid. The token itself is checked by the
// store that made it, with the same arguments as the list it came from.
func pageOf(size int32, token string) (master.Page, error) {
	if size < 0 || size > MaxPageSize {
		return master.Page{}, invalid("page_size", "is 0 to "+strconv.Itoa(MaxPageSize)+"; 0 asks for "+strconv.Itoa(DefaultPageSize))
	}
	if size == 0 {
		size = DefaultPageSize
	}
	if len(token) > MaxPageTokenLength {
		return master.Page{}, invalid("page_token", "is at most 1 KiB")
	}
	return master.Page{Cursor: token, Limit: int(size)}, nil
}

// listError returns the error of a store's list. A store answers a token it
// did not make for these arguments as Invalid; that is the request's
// page_token, so the field is named.
func listError(err error, token string) error {
	if token != "" && fault.KindOf(err) == fault.Invalid {
		return fault.Wrap(fault.Invalid, err, "page_token: is not one this list returned for these arguments")
	}
	return err
}

// tenantName returns an Invalid error naming field unless name is a valid
// tenant name (master.ValidateTenantName; tenant.proto,
// CreateTenantRequest.name).
func tenantName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	if master.ValidateTenantName(name) != nil {
		return invalid(field, "is 1 to 63 of a-z, 0-9 and -, starting with a letter, and not a device name Windows reserves (con, prn, aux, nul, com0-9, lpt0-9)")
	}
	return nil
}

// settingName returns an Invalid error naming field unless name is a valid
// setting name.
func settingName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	if master.ValidateSettingName(name) != nil {
		return invalid(field, "is 1 to 128 of a-z, 0-9, ., _ and -")
	}
	return nil
}

// keyID returns an Invalid error naming field unless id could be a key's id
// (master.ValidateKeyRef), never repeating it: a whole key given as an id
// is a secret, and is refused here as not a key id.
func keyID(field, id string) error {
	if id == "" {
		return invalid(field, "is required")
	}
	if master.ValidateKeyRef("key id", id) != nil {
		return invalid(field, "is not a key id: give the key's id, never the whole key")
	}
	return nil
}

// text returns an Invalid error naming field unless s is UTF-8 of min to max
// bytes with no NUL byte, which both database types can store.
func text(field, s string, minLen, maxLen int) error {
	switch {
	case len(s) < minLen && minLen == 1:
		return invalid(field, "is required")
	case len(s) < minLen || len(s) > maxLen:
		return invalid(field, "is at most "+strconv.Itoa(maxLen)+" bytes")
	case !utf8.ValidString(s):
		return invalid(field, "is UTF-8")
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return invalid(field, "holds no NUL byte")
		}
	}
	return nil
}

// scopeOf returns the scope a request names, or an Invalid error naming
// field: a scope with neither kind set, or a tenant name that is not valid.
func scopeOf(field string, s *v1.Scope) (access.Scope, error) {
	switch k := s.GetKind().(type) {
	case *v1.Scope_Installation:
		return access.InstallationScope, nil
	case *v1.Scope_Tenant:
		if err := tenantName(field+".tenant", k.Tenant); err != nil {
			return access.Scope{}, err
		}
		return access.TenantNamed(k.Tenant), nil
	}
	return access.Scope{}, invalid(field, "is required: a tenant or the installation")
}

// scopeMessage returns s as the contract writes it.
func scopeMessage(s access.Scope) *v1.Scope {
	if s.IsInstallation() {
		return &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
	}
	return &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: s.Tenant}}
}

// timestamp returns t as a UTC timestamp, or nil for the zero time: a time
// that is not set is an absent field, never the year 1.
func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

// roleOf returns the contract's role for a master role; the empty role is
// ROLE_UNSPECIFIED, "there was none".
func roleOf(r master.Role) v1.Role {
	switch r {
	case master.RoleReader:
		return v1.Role_ROLE_READER
	case master.RoleWriter:
		return v1.Role_ROLE_WRITER
	case master.RoleOperator:
		return v1.Role_ROLE_OPERATOR
	case master.RoleAdmin:
		return v1.Role_ROLE_ADMIN
	}
	return v1.Role_ROLE_UNSPECIFIED
}

// masterRole returns the master's role for a role a request names, and false
// for ROLE_UNSPECIFIED or a number the contract does not define.
func masterRole(r v1.Role) (master.Role, bool) {
	switch r {
	case v1.Role_ROLE_READER:
		return master.RoleReader, true
	case v1.Role_ROLE_WRITER:
		return master.RoleWriter, true
	case v1.Role_ROLE_OPERATOR:
		return master.RoleOperator, true
	case v1.Role_ROLE_ADMIN:
		return master.RoleAdmin, true
	}
	return "", false
}

// keyMessage returns a key as the contract writes it, without its hash.
func keyMessage(k master.Key) *v1.Key {
	return &v1.Key{
		Id:          k.ID,
		Holder:      k.Holder,
		CreateTime:  timestamp(k.Created),
		ExpireTime:  timestamp(k.Expires),
		LastUseTime: timestamp(k.LastUsed),
		RevokeTime:  timestamp(k.Revoked),

		Networks:         networkTexts(k.Rules.Networks),
		ClientCertSha256: k.Rules.ClientCert,
		LastUseAddress:   k.LastAddress,
		SuccessorId:      k.Successor,
		Suspended:        !k.Suspended.IsZero(),
		SuspendTime:      timestamp(k.Suspended),
	}
}

// networkTexts returns each network as the contract writes it.
func networkTexts(n settings.Networks) []string {
	if len(n) == 0 {
		return nil
	}
	out := make([]string, len(n))
	for i, p := range n {
		out[i] = settings.FormatNetwork(p)
	}
	return out
}

// grantMessage returns a grant as the contract writes it.
func grantMessage(g access.Grant) *v1.Grant {
	return &v1.Grant{KeyId: g.KeyID, Scope: scopeMessage(g.Scope), Role: roleOf(g.Role)}
}

// databaseType returns the contract's type for the database type a tenant's
// row records (master.Tenant.Driver).
func databaseType(name string) v1.DatabaseType {
	switch name {
	case master.SQLiteDriver:
		return v1.DatabaseType_DATABASE_TYPE_SQLITE
	case master.PostgresDriver:
		return v1.DatabaseType_DATABASE_TYPE_POSTGRES
	}
	return v1.DatabaseType_DATABASE_TYPE_UNSPECIFIED
}

// tenantState returns the contract's state for a master state.
func tenantState(s master.TenantState) v1.TenantState {
	switch s {
	case master.StateReady:
		return v1.TenantState_TENANT_STATE_READY
	case master.StateUnreachable:
		return v1.TenantState_TENANT_STATE_UNREACHABLE
	case master.StateUpgrading:
		return v1.TenantState_TENANT_STATE_UPGRADING
	case master.StateFailed:
		return v1.TenantState_TENANT_STATE_FAILED
	case master.StateMoved:
		return v1.TenantState_TENANT_STATE_MOVED
	case master.StateFenced:
		return v1.TenantState_TENANT_STATE_FENCED
	}
	return v1.TenantState_TENANT_STATE_UNSPECIFIED
}

// tenantMessage returns a tenant as the contract writes it: never its
// address, sealed or not. A ready tenant has no reason (tenant.proto,
// Tenant.state_reason).
func tenantMessage(t master.Tenant) *v1.Tenant {
	reason := t.StateReason
	if t.State == master.StateReady {
		reason = ""
	}
	return &v1.Tenant{
		Name:         t.Name,
		DatabaseType: databaseType(t.Driver),
		State:        tenantState(t.State),
		StateReason:  reason,
		CreateTime:   timestamp(t.Created),
	}
}

// outcomeMessage returns the contract's outcome for a master outcome.
func outcomeMessage(o master.Outcome) v1.AuditOutcome {
	switch o {
	case master.OutcomeDone:
		return v1.AuditOutcome_AUDIT_OUTCOME_DONE
	case master.OutcomeRefused:
		return v1.AuditOutcome_AUDIT_OUTCOME_REFUSED
	case master.OutcomeFailed:
		return v1.AuditOutcome_AUDIT_OUTCOME_FAILED
	}
	return v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED
}

// masterOutcome returns the master's outcome for a filter: "" for
// AUDIT_OUTCOME_UNSPECIFIED (every outcome), and false for a number the
// contract does not define.
func masterOutcome(o v1.AuditOutcome) (master.Outcome, bool) {
	switch o {
	case v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED:
		return "", true
	case v1.AuditOutcome_AUDIT_OUTCOME_DONE:
		return master.OutcomeDone, true
	case v1.AuditOutcome_AUDIT_OUTCOME_REFUSED:
		return master.OutcomeRefused, true
	case v1.AuditOutcome_AUDIT_OUTCOME_FAILED:
		return master.OutcomeFailed, true
	}
	return "", false
}

// entryMessage returns an audit entry as the contract writes it.
func entryMessage(e master.AuditEntry) *v1.AuditEntry {
	return &v1.AuditEntry{
		Id:            e.ID,
		Time:          timestamp(e.Time),
		KeyId:         e.KeyID,
		LocalUserId:   e.LocalUser,
		ClientAddress: e.RemoteAddress,
		Method:        e.Method,
		Tenant:        e.TenantName,
		Object:        e.Object,
		Outcome:       outcomeMessage(e.Outcome),
		RequestId:     e.RequestID,
		Details:       e.Details,
		Reason:        e.Reason,
	}
}
