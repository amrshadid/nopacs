package access

import (
	"github.com/amrshadid/nopacs/internal/master"
)

// requirementKind is what a Requirement asks for.
type requirementKind int

const (
	// needInvalid is the zero Requirement, which no caller meets: a
	// handler that forgot to set one fails closed.
	needInvalid requirementKind = iota
	needAuthenticated
	needInstallation
	needTenant
	needTenantRegistration
	needTenantGrants
)

// Requirement is what a request needs of its caller. The zero Requirement is
// met by nobody.
type Requirement struct {
	kind   requirementKind
	role   master.Role
	tenant string
}

// Authenticated requires only a good credential: WhoAmI and ListTenants,
// whose answers the caller's grants then shape.
func Authenticated() Requirement {
	return Requirement{kind: needAuthenticated}
}

// OnInstallation requires at least role on the installation. Only a grant
// on the installation meets it, or the local operator.
func OnInstallation(role master.Role) Requirement {
	return Requirement{kind: needInstallation, role: role}
}

// OnTenant requires at least role on the tenant named tenantName, as the
// request named it. Only a grant on that tenant meets it, or the local
// operator: a role on the installation gives nothing inside a tenant
// (access.proto, Role).
func OnTenant(tenantName string, role master.Role) Requirement {
	return Requirement{kind: needTenant, role: role, tenant: tenantName}
}

// TenantRegistration requires the right to read one tenant's registration
// and state, as GetTenant does: reader or more on that tenant, or reader or
// more on the installation, or the local operator (tenant.proto,
// GetTenant).
func TenantRegistration(tenantName string) Requirement {
	return Requirement{kind: needTenantRegistration, role: master.RoleReader, tenant: tenantName}
}

// TenantGrants requires the right to hand out grants on one tenant: admin
// on the installation, or admin on that tenant, or the local operator. A
// tenant's admin is then held to what a tenant's admin may do: grants on
// that tenant only, below admin, never its own key's or an admin's
// (master.CheckGrantChange), and a listing of that tenant's keys alone.
// Which keys may use a tenant is the tenant's business, so its admin lets a
// key in; making keys and admins stays with the installation.
func TenantGrants(tenantName string) Requirement {
	return Requirement{kind: needTenantGrants, role: master.RoleAdmin, tenant: tenantName}
}

// namesTenant reports whether r names a tenant the check must look up.
func (r Requirement) namesTenant() bool {
	return r.kind == needTenant || r.kind == needTenantRegistration || r.kind == needTenantGrants
}

// valid reports whether r is one a constructor above made.
func (r Requirement) valid() bool {
	switch r.kind {
	case needAuthenticated:
		return true
	case needInstallation, needTenant, needTenantRegistration, needTenantGrants:
		return r.role.Valid()
	}
	return false
}

// Request is what the check needs to know of the request for its audit
// entry. The server fills it from the request; none of it is trusted, so each
// text is escaped and cut to what an audit entry holds (see auditText).
// Method is required.
type Request struct {
	// Method is the API method or command, such as
	// "/nopacs.v1.AccessService/CreateKey".
	Method string
	// RemoteAddress is where the request came from: the caller's address,
	// "ip:port" from the connection or "ip" from a trusted proxy's
	// forwarded header; empty on the local socket. The networks of a key and
	// of a tenant are judged against it.
	RemoteAddress string
	// Local reports that the request came on the installation's local
	// socket, which no network rule applies to: its caller is a user of the
	// machine the server runs on.
	Local bool
	// ClientCert is the SHA-256 of the TLS client certificate the request's
	// connection presented, in lowercase hexadecimal; empty for none.
	ClientCert string
	// RequestID is the id the server made for the request.
	RequestID string
	// Object is what the request is about, such as a key's id.
	Object string
	// Details are what else the entry needs, never a secret. At most
	// MaxRequestDetails pairs, so the master can still add a grant's old and
	// new role, and the audit writer a note that a tenant's log did not
	// answer.
	Details map[string]string
}

// MaxRequestDetails bounds Request.Details: the master's bound less the two
// details a change of grant adds (master.AuditEntry.WithRoleChange) and the
// one the audit writer adds to an entry it moves to the master's log.
const MaxRequestDetails = master.MaxAuditDetails - 3
