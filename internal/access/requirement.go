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
	// originals says the answer holds values that are not in their
	// archived, de-identified form, which a masked key is refused.
	originals bool
	// destination says the request chooses where archived data goes,
	// which a masked key is refused.
	destination bool
	// writes says the request writes values of its own into archived
	// files, which a masked key is refused.
	writes bool
	// bulk says the request deletes many targets at once, which a masked
	// key is refused.
	bulk bool
}

// DeletesInBulk returns r for a request that deletes many targets of a
// tenant at once, or lets such a deletion go on (a batch delete and its
// resume). Check refuses it to a masked key, once the key's grant is found
// to suffice, as Denied with MessageMaskedBulk, and records the refusal: a
// masked key is held by a service outside the site, and how much of the
// archive goes at once is a person's choice, never that service's. For any
// other caller it is r.
func (r Requirement) DeletesInBulk() Requirement {
	r.bulk = true
	return r
}

// WritesValues returns r for a request that writes values the caller
// chooses into archived files (an edit of header values). Check refuses it
// to a masked key, once the key's grant is found to suffice, as Denied
// with MessageMaskedWrite, and records the refusal: the service that holds
// such a key is outside the site, and what the archive's files say is a
// person's choice, never that service's. For any other caller it is r.
func (r Requirement) WritesValues() Requirement {
	r.writes = true
	return r
}

// ChoosesDestination returns r for a request that decides where the
// archive's bytes go or who receives them: a send, a replica, a storage, a
// sender, a receiver's forwarding. Check refuses it to a masked key, once
// the key's grant is found to suffice, as Denied with
// MessageMaskedDestination, and records the refusal: a masked key is held
// by a service outside the site, and where data goes is a person's choice,
// never that service's, whatever the data's form. For any other caller it
// is r.
func (r Requirement) ChoosesDestination() Requirement {
	r.destination = true
	return r
}

// AnswersOriginals returns r for a request whose answer holds values that
// are not in their archived, de-identified form, whatever it names: a held
// header, a value's code, a trial's values. Check refuses it to a masked key
// (master.Key.Masked), once the key's grant is found to suffice, as Denied
// with MessageMasked, and records the refusal; for any other caller it is r.
func (r Requirement) AnswersOriginals() Requirement {
	r.originals = true
	return r
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
// details a change of grant adds (master.AuditEntry.WithRoleChange), the
// one the audit writer adds to an entry it moves to the master's log, and
// the one every entry of a masked key carries (DetailMasked).
const MaxRequestDetails = master.MaxAuditDetails - 4

// DetailMasked is the detail every audit entry a masked key makes carries,
// "true", so the log shows what an outside service read and was refused.
const DetailMasked = "masked"
