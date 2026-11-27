package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
)

// The keys of the details this package puts in audit entries.
const (
	detailScope   = "scope"
	detailKeyID   = "key_id"
	detailOutcome = "outcome"
	detailStart   = "start_time"
	detailEnd     = "end_time"
)

// Methods is the method-to-requirement table of the services: for each
// method, what a request needs of its caller and the facts of its audit
// entry, each line from the Role line of the method's comment. The server
// asks it about every request before the handler runs, and makes the access
// check with what it answers. Its zero value is ready to use.
//
// A requirement that depends on the request (a role on the tenant or the
// scope the request names) reads the request only for that scope. A request
// whose scope or tenant name is malformed, or that could not be read at all,
// is checked for the same role on the installation, and the handler then
// answers it as invalid (doc.go).
type Methods struct{}

// RequestFor reports whether procedure, a full procedure name such as
// "/nopacs.v1.TenantService/GetTenant", is a method of the table, and
// returns a new empty request message of its input type when its
// requirement depends on the request, or nil when the requirement is fixed,
// so the server need not read the body of a request it may refuse.
func (Methods) RequestFor(procedure string) (msg proto.Message, known bool) {
	return requestFor(rules, procedure)
}

// Requirement returns what a request to procedure needs of its caller, and
// its audit facts: Object and Details (the server fills Method,
// RemoteAddress and RequestID). msg is the request read into the message
// RequestFor returned, nil for a fixed requirement, or an empty message when
// the request could not be read, which is held to the role on the
// installation. A message of another type is read as an empty one, so a
// mistake in the server's wiring is held to the strictest requirement and
// never panics. An unknown procedure gets the zero Requirement, which
// nobody meets.
func (Methods) Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request) {
	return requirementIn(rules, procedure, msg)
}

// Pending reports whether procedure is a method the contract defines and no
// handler serves yet (pendingRules). The server does not mount such a
// method even when the rest of its service is served, so a call to it is
// answered as one to any method the server does not serve.
func (Methods) Pending(procedure string) bool {
	_, ok := pendingRules[procedure]
	return ok
}

// requestFor is RequestFor over table.
func requestFor(table map[string]rule, procedure string) (proto.Message, bool) {
	r, ok := table[procedure]
	if !ok {
		return nil, false
	}
	if r.newMsg == nil {
		return nil, true
	}
	return r.newMsg(), true
}

// requirementIn is Requirement over table.
func requirementIn(table map[string]rule, procedure string, msg proto.Message) (access.Requirement, access.Request) {
	r, ok := table[procedure]
	if !ok {
		return access.Requirement{}, access.Request{}
	}
	return r.need(msg)
}

// rule is one line of the table. newMsg is nil when the requirement does
// not depend on the request; need takes the request, or nil.
type rule struct {
	newMsg func() proto.Message
	need   func(msg proto.Message) (access.Requirement, access.Request)
}

// fixed is a line whose requirement does not depend on the request. Its
// audit facts are none: the request is not read before the check, and the
// handler, or package access, adds the object and details to the entries it
// writes.
func fixed(need access.Requirement) rule {
	return rule{need: func(proto.Message) (access.Requirement, access.Request) { return need, access.Request{} }}
}

// byMessage is a line whose requirement is read from the request, of type
// *M. A nil message, or one of another type, is read as the empty message.
func byMessage[M any, P interface {
	*M
	proto.Message
}](f func(P) (access.Requirement, access.Request)) rule {
	return rule{
		newMsg: func() proto.Message { return P(new(M)) },
		need: func(msg proto.Message) (access.Requirement, access.Request) {
			m, ok := msg.(P)
			if !ok || m == nil {
				m = P(new(M))
			}
			return f(m)
		},
	}
}

// installationAdmin is the requirement of every method that needs admin on
// the installation.
var installationAdmin = access.OnInstallation(master.RoleAdmin)

// onScope returns the requirement of role on a scope a request names: on the
// tenant when it names a valid one, and otherwise on the installation, which
// is also the requirement for a malformed scope (doc.go).
func onScope(s *v1.Scope, role master.Role) access.Requirement {
	if t, ok := s.GetKind().(*v1.Scope_Tenant); ok && master.ValidateTenantName(t.Tenant) == nil {
		return access.OnTenant(t.Tenant, role)
	}
	return access.OnInstallation(role)
}

// grantsOn is the requirement of a change or listing of the grants on the
// tenant named name: admin on the installation or on that tenant; admin on
// the installation for an empty or malformed name (the handler then answers
// a malformed one as invalid).
func grantsOn(name string) access.Requirement {
	if name != "" && master.ValidateTenantName(name) == nil {
		return access.TenantGrants(name)
	}
	return installationAdmin
}

// grantsOnScope is grantsOn for a scope a request names: a tenant's, or the
// installation's, whose grants only its admin changes.
func grantsOnScope(s *v1.Scope) access.Requirement {
	if t, ok := s.GetKind().(*v1.Scope_Tenant); ok {
		return grantsOn(t.Tenant)
	}
	return installationAdmin
}

// onMonitorScope is the requirement of a Monitor method: reader on the
// tenant the scope names, operator on the installation for every tenant (a
// malformed scope is held to the installation's, as onScope does).
func onMonitorScope(s *v1.Scope) access.Requirement {
	if t, ok := s.GetKind().(*v1.Scope_Tenant); ok && master.ValidateTenantName(t.Tenant) == nil {
		return access.OnTenant(t.Tenant, master.RoleReader)
	}
	return access.OnInstallation(master.RoleOperator)
}

// scopeDetail returns a scope as an audit entry's details write it,
// "installation" or "tenant/<name>", and "none" for a malformed one.
func scopeDetail(s *v1.Scope) string {
	switch k := s.GetKind().(type) {
	case *v1.Scope_Installation:
		return access.InstallationScope.String()
	case *v1.Scope_Tenant:
		return access.TenantNamed(k.Tenant).String()
	}
	return "none"
}

// tenantObject is the object of an entry about a tenant itself, as the
// registry writes it on its own entries.
func tenantObject(name string) string { return "tenant/" + name }

// rules is the table. A method of the contract missing here is unknown to
// RequestFor, so the server refuses to start, and the descriptor test fails
// until it is added.
var rules = map[string]rule{
	// AccessService: admin on the installation, but WhoAmI. Package access
	// writes each entry's object and details itself.
	nopacsv1connect.AccessServiceCreateKeyProcedure:   fixed(installationAdmin),
	nopacsv1connect.AccessServiceGetKeyProcedure:      fixed(installationAdmin),
	nopacsv1connect.AccessServiceRevokeKeyProcedure:   fixed(installationAdmin),
	nopacsv1connect.AccessServiceSetKeyRulesProcedure: fixed(installationAdmin),
	// The grants on a tenant, and the keys that hold them: admin on the
	// installation or on that tenant (access.TenantGrants), a tenant's admin
	// held to its bounds by package access and the master. Every key, and a
	// key's grants on every scope, stay the installation's admin's.
	nopacsv1connect.AccessServiceListKeysProcedure: byMessage(func(r *v1.ListKeysRequest) (access.Requirement, access.Request) {
		return grantsOn(r.GetTenant()), access.Request{}
	}),
	nopacsv1connect.AccessServiceSetGrantProcedure: byMessage(func(r *v1.SetGrantRequest) (access.Requirement, access.Request) {
		return grantsOnScope(r.GetScope()), access.Request{}
	}),
	nopacsv1connect.AccessServiceListGrantsProcedure: byMessage(func(r *v1.ListGrantsRequest) (access.Requirement, access.Request) {
		return grantsOnScope(r.GetScope()), access.Request{}
	}),
	nopacsv1connect.AccessServiceRemoveGrantProcedure: byMessage(func(r *v1.RemoveGrantRequest) (access.Requirement, access.Request) {
		return grantsOnScope(r.GetScope()), access.Request{}
	}),
	// ReinstateKeys: admin on the installation, further kept to the local
	// operator by package access, which refuses every key.
	nopacsv1connect.AccessServiceReinstateKeysProcedure: fixed(installationAdmin),
	// RotateKey: any authenticated caller reaches the handler, and package
	// access lets through only the installation's admin or the key being
	// rotated, deciding before it reads or validates anything else.
	nopacsv1connect.AccessServiceRotateKeyProcedure: fixed(access.Authenticated()),
	// "Role: none; any caller the server authenticates."
	nopacsv1connect.AccessServiceWhoAmIProcedure: fixed(access.Authenticated()),

	// TenantService.
	nopacsv1connect.TenantServiceCreateTenantProcedure: fixed(installationAdmin),
	// "Role: reader on that tenant, or reader on the installation."
	nopacsv1connect.TenantServiceGetTenantProcedure: byMessage(func(r *v1.GetTenantRequest) (access.Requirement, access.Request) {
		if master.ValidateTenantName(r.GetName()) != nil {
			return access.OnInstallation(master.RoleReader), access.Request{}
		}
		return access.TenantRegistration(r.GetName()), access.Request{}
	}),
	// "Role: none beyond an authenticated caller."
	nopacsv1connect.TenantServiceListTenantsProcedure:  fixed(access.Authenticated()),
	nopacsv1connect.TenantServiceRemoveTenantProcedure: fixed(installationAdmin),
	// Admin on the installation by the table; the handler then refuses
	// every caller but the local operator, as the contract says, and
	// names the tenant in its entries.
	nopacsv1connect.TenantServiceReclaimTenantProcedure: fixed(installationAdmin),

	// BackupService: admin on the installation. An export's handler names
	// the tenant it hands off in its entries; a backup's names the tenants
	// it holds. Either carries whatever the archive holds, data archived as
	// received and held files included, so a masked key is refused both.
	nopacsv1connect.BackupServiceExportTenantProcedure: fixed(installationAdmin.AnswersOriginals()),
	nopacsv1connect.BackupServiceCreateBackupProcedure: fixed(installationAdmin.AnswersOriginals()),

	// SettingsService: reader to read, admin to change, on the scope asked
	// for. The object is the setting's name, never its value.
	nopacsv1connect.SettingsServiceGetSettingProcedure: byMessage(func(r *v1.GetSettingRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleReader), settingFacts(r.GetName(), r.GetScope())
	}),
	// "Role: admin on the scope set; for a setting set on a tenant by the
	// installation's admin only, admin on the installation, whatever the
	// scope."
	nopacsv1connect.SettingsServiceSetSettingProcedure: byMessage(func(r *v1.SetSettingRequest) (access.Requirement, access.Request) {
		s := r.GetSetting()
		return settingChange(s.GetName(), s.GetScope()), settingFacts(s.GetName(), s.GetScope())
	}),
	nopacsv1connect.SettingsServiceUnsetSettingProcedure: byMessage(func(r *v1.UnsetSettingRequest) (access.Requirement, access.Request) {
		return settingChange(r.GetName(), r.GetScope()), settingFacts(r.GetName(), r.GetScope())
	}),
	nopacsv1connect.SettingsServiceListSettingsProcedure: byMessage(func(r *v1.ListSettingsRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleReader), access.Request{Details: map[string]string{detailScope: scopeDetail(r.GetScope())}}
	}),

	// AuditService: admin on the scope asked for.
	nopacsv1connect.AuditServiceListAuditEntriesProcedure: byMessage(func(r *v1.ListAuditEntriesRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleAdmin), access.Request{Details: auditFilterDetails(r)}
	}),

	// AuditSealService: admin on the log's scope, but the public keys,
	// which any authenticated caller may read: they check a signature and
	// cannot make one. The details are the scope and the range of seq as
	// asked; a digest kept outside is recorded only as given.
	nopacsv1connect.AuditSealServiceVerifyAuditProcedure: byMessage(func(r *v1.VerifyAuditRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleAdmin), access.Request{Details: verifyDetails(r)}
	}),
	nopacsv1connect.AuditSealServiceListAuditCheckpointsProcedure: byMessage(func(r *v1.ListAuditCheckpointsRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleAdmin), access.Request{Details: map[string]string{detailScope: scopeDetail(r.GetScope())}}
	}),
	nopacsv1connect.AuditSealServiceExportAuditProcedure: byMessage(func(r *v1.ExportAuditRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleAdmin), access.Request{Details: sealRangeDetails(r.GetScope(), r.GetFromSeq(), r.ToSeq)}
	}),
	// "Role: none; any caller the server authenticates."
	nopacsv1connect.AuditSealServiceGetAuditPublicKeyProcedure: fixed(access.Authenticated()),

	// MonitorService: reader on the tenant asked for, operator on the
	// installation for every tenant.
	nopacsv1connect.MonitorServiceGetStatusProcedure: byMessage(func(r *v1.GetStatusRequest) (access.Requirement, access.Request) {
		return onMonitorScope(r.GetScope()), access.Request{Details: map[string]string{detailScope: scopeDetail(r.GetScope())}}
	}),
	nopacsv1connect.MonitorServiceWatchProcedure: byMessage(func(r *v1.WatchRequest) (access.Requirement, access.Request) {
		return onMonitorScope(r.GetScope()), access.Request{Details: watchDetails(r)}
	}),

	// StorageService: admin on the tenant to change, reader to read. The
	// object is the storage's name. Adding or changing a storage decides
	// where the archive's bytes are written, which a masked key may not
	// choose (access.Requirement.ChoosesDestination).
	nopacsv1connect.StorageServiceAddStorageProcedure: byMessage(func(r *v1.AddStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).ChoosesDestination(), access.Request{Object: r.GetStorage().GetName(), Details: map[string]string{detailType: storageType(r.GetStorage())}}
	}),
	nopacsv1connect.StorageServiceGetStorageProcedure: byMessage(func(r *v1.GetStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: r.GetName()}
	}),
	nopacsv1connect.StorageServiceListStoragesProcedure: byMessage(func(r *v1.ListStoragesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.StorageServiceSetStorageProcedure: byMessage(func(r *v1.SetStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).ChoosesDestination(), access.Request{Object: r.GetStorage().GetName(), Details: maskDetails(r.GetUpdateMask().GetPaths())}
	}),
	nopacsv1connect.StorageServiceRemoveStorageProcedure: byMessage(func(r *v1.RemoveStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetName()}
	}),
	nopacsv1connect.StorageServiceTestStorageProcedure: byMessage(func(r *v1.TestStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetName()}
	}),

	// ReceiverService: admin on the tenant to change, reader to read. The
	// object is the receiver's name. A receiver that forwards, by forward_to
	// or a route's senders, sends what it receives to a peer, which a masked
	// key may not choose (access.Requirement.ChoosesDestination); a change
	// that stops forwarding is not refused it.
	nopacsv1connect.ReceiverServiceAddReceiverProcedure: byMessage(func(r *v1.AddReceiverRequest) (access.Requirement, access.Request) {
		return receiverChangeNeed(r.GetTenant(), r.GetReceiver(), nil), access.Request{Object: r.GetReceiver().GetName(), Details: requestReceiverDetails(r.GetReceiver())}
	}),
	nopacsv1connect.ReceiverServiceGetReceiverProcedure: byMessage(func(r *v1.GetReceiverRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: r.GetName()}
	}),
	nopacsv1connect.ReceiverServiceListReceiversProcedure: byMessage(func(r *v1.ListReceiversRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.ReceiverServiceSetReceiverProcedure: byMessage(func(r *v1.SetReceiverRequest) (access.Requirement, access.Request) {
		return receiverChangeNeed(r.GetTenant(), r.GetReceiver(), r.GetUpdateMask().GetPaths()), access.Request{Object: r.GetReceiver().GetName(), Details: maskDetails(r.GetUpdateMask().GetPaths())}
	}),
	nopacsv1connect.ReceiverServiceRemoveReceiverProcedure: byMessage(func(r *v1.RemoveReceiverRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetName()}
	}),

	// ReceptionService: reader to read, operator to settle a failed
	// reception, and operator to look a study up by its UID. A reception's
	// Study Instance UID is the one received, so a search by it tells
	// whether an original value is in the archive, which a masked key is
	// refused.
	nopacsv1connect.ReceptionServiceListReceptionsProcedure: byMessage(func(r *v1.ListReceptionsRequest) (access.Requirement, access.Request) {
		if r.GetStudyInstanceUid() != "" {
			return onTenantNamed(r.GetTenant(), master.RoleOperator).AnswersOriginals(), access.Request{Details: receptionFilterDetails(r)}
		}
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: receptionFilterDetails(r)}
	}),
	nopacsv1connect.ReceptionServiceGetReceptionProcedure: byMessage(func(r *v1.GetReceptionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: r.GetId()}
	}),
	nopacsv1connect.ReceptionServiceRetryReceptionProcedure: byMessage(func(r *v1.RetryReceptionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: r.GetId()}
	}),
	nopacsv1connect.ReceptionServiceDiscardReceptionProcedure: byMessage(func(r *v1.DiscardReceptionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: r.GetId()}
	}),

	// QuarantineService: reader to read the records; operator for a file's
	// header, a search by UID, and a release or a rejection.
	// A search by a UID as received and a held header hold originals, which
	// a masked key is refused (access.Requirement.AnswersOriginals).
	nopacsv1connect.QuarantineServiceListQuarantinedFilesProcedure: byMessage(func(r *v1.ListQuarantinedFilesRequest) (access.Requirement, access.Request) {
		if r.GetStudyInstanceUid() != "" || r.GetSopInstanceUid() != "" {
			return onTenantNamed(r.GetTenant(), master.RoleOperator).AnswersOriginals(), access.Request{Details: quarantineFilterDetails(r)}
		}
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: quarantineFilterDetails(r)}
	}),
	nopacsv1connect.QuarantineServiceGetQuarantinedFileProcedure: byMessage(func(r *v1.GetQuarantinedFileRequest) (access.Requirement, access.Request) {
		facts := access.Request{Object: r.GetId(), Details: headerDetails(r)}
		if r.GetIncludeHeader() {
			return onTenantNamed(r.GetTenant(), master.RoleOperator).AnswersOriginals(), facts
		}
		return onTenantNamed(r.GetTenant(), master.RoleReader), facts
	}),
	nopacsv1connect.QuarantineServiceReleaseQuarantinedFileProcedure: byMessage(func(r *v1.ReleaseQuarantinedFileRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), releaseRole(r)), access.Request{Object: r.GetId(), Details: releaseDetails(r, "")}
	}),
	nopacsv1connect.QuarantineServiceRejectQuarantinedFileProcedure: byMessage(func(r *v1.RejectQuarantinedFileRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: r.GetId()}
	}),

	// MetadataService: reader on the tenant. The object is the scan's path
	// of labels, and an instance id is recorded only as present, since it
	// holds a SOP Instance UID as received in this version.
	nopacsv1connect.MetadataServiceGetScanMetadataProcedure: byMessage(func(r *v1.GetScanMetadataRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel(), r.GetScanLabel())}
	}),
	nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure: byMessage(func(r *v1.GetInstanceMetadataRequest) (access.Requirement, access.Request) {
		d := map[string]string{}
		if r.GetInstanceId() != "" {
			d[detailInstance] = presentValue
		}
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel(), r.GetScanLabel()), Details: d}
	}),

	// CatalogService: reader on the tenant. The object is the path of
	// labels the request names, and the filters are in the details.
	nopacsv1connect.CatalogServiceListSubjectsProcedure: byMessage(func(r *v1.ListSubjectsRequest) (access.Requirement, access.Request) {
		d := catalogFilterDetails(r.GetLabelPrefix(), r.GetOrder())
		timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: d}
	}),
	nopacsv1connect.CatalogServiceGetSubjectProcedure: byMessage(func(r *v1.GetSubjectRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel())}
	}),
	nopacsv1connect.CatalogServiceListSessionsProcedure: byMessage(func(r *v1.ListSessionsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel()), Details: sessionFilterDetails(r)}
	}),
	nopacsv1connect.CatalogServiceGetSessionProcedure: byMessage(func(r *v1.GetSessionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel())}
	}),
	nopacsv1connect.CatalogServiceListScansProcedure: byMessage(func(r *v1.ListScansRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel()), Details: scanFilterDetails(r)}
	}),
	nopacsv1connect.CatalogServiceGetScanProcedure: byMessage(func(r *v1.GetScanRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel(), r.GetScanLabel())}
	}),

	// DataService: reader on the tenant. The object is the path of labels
	// the request names; an instance id and a position are recorded only
	// as present.
	nopacsv1connect.DataServiceReadFilesProcedure: byMessage(func(r *v1.ReadFilesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel(), r.GetScanLabel()), Details: readFilesDetails(r)}
	}),

	// SenderService: admin on the tenant to change or test a sender, reader
	// to read one; the object is the sender's name, recorded only when it
	// passes the rule of a sender's name. JobService: operator on the tenant
	// to make, cancel and retry a job, reader to read jobs and their
	// failures; the object is the job's id, recorded only when it has the
	// form of one. Adding or changing a sender decides where it sends, and a
	// send job what is sent: neither is a masked key's to choose
	// (access.Requirement.ChoosesDestination).
	nopacsv1connect.SenderServiceAddSenderProcedure: byMessage(func(r *v1.AddSenderRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).ChoosesDestination(), access.Request{Object: senderObject(r.GetSender().GetName()), Details: map[string]string{detailType: senderType(r.GetSender())}}
	}),
	nopacsv1connect.SenderServiceGetSenderProcedure: byMessage(func(r *v1.GetSenderRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: senderObject(r.GetName())}
	}),
	nopacsv1connect.SenderServiceListSendersProcedure: byMessage(func(r *v1.ListSendersRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.SenderServiceSetSenderProcedure: byMessage(func(r *v1.SetSenderRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).ChoosesDestination(), access.Request{Object: senderObject(r.GetSender().GetName()), Details: maskDetails(r.GetUpdateMask().GetPaths())}
	}),
	nopacsv1connect.SenderServiceRemoveSenderProcedure: byMessage(func(r *v1.RemoveSenderRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: senderObject(r.GetName()), Details: removeSenderDetails(r)}
	}),
	nopacsv1connect.SenderServiceTestSenderProcedure: byMessage(func(r *v1.TestSenderRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: senderObject(r.GetName())}
	}),

	nopacsv1connect.JobServiceCreateJobProcedure: byMessage(func(r *v1.CreateJobRequest) (access.Requirement, access.Request) {
		need := onTenantNamed(r.GetTenant(), master.RoleOperator)
		if r.GetSend() != nil {
			need = need.ChoosesDestination()
		}
		return need, access.Request{Details: createJobDetails(r)}
	}),
	nopacsv1connect.JobServiceGetJobProcedure: byMessage(func(r *v1.GetJobRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.JobServiceListJobsProcedure: byMessage(func(r *v1.ListJobsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: jobFilterDetails(r)}
	}),
	nopacsv1connect.JobServiceCancelJobProcedure: byMessage(func(r *v1.CancelJobRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetId())}
	}),
	// A pause stops work, which an operator may ask as a cancel; a resume of
	// a batch delete deletes again, so it needs what the batch needed:
	// admin, and never a masked key.
	nopacsv1connect.JobServicePauseJobProcedure: byMessage(func(r *v1.PauseJobRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.JobServiceResumeJobProcedure: byMessage(func(r *v1.ResumeJobRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).DeletesInBulk(), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.JobServiceRetryJobProcedure: byMessage(func(r *v1.RetryJobRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.JobServiceListJobFailuresProcedure: byMessage(func(r *v1.ListJobFailuresRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: jobObject(r.GetId())}
	}),
	// ListImportFiles and ListImportUnits: operator on the tenant, since
	// they answer paths, which can name a person; the object is the job's
	// id, and no path is recorded. A path is an original value, refused to
	// a masked key.
	nopacsv1connect.JobServiceListImportFilesProcedure: byMessage(func(r *v1.ListImportFilesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator).AnswersOriginals(), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.JobServiceListImportUnitsProcedure: byMessage(func(r *v1.ListImportUnitsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator).AnswersOriginals(), access.Request{Object: jobObject(r.GetId())}
	}),

	// ProfileService: operator on the tenant to read profiles, to check a
	// document and to try, explain and compare versions (trying reads held
	// files, as reading a held header does); admin to make a version. The
	// list of bases is the server's and needs only an authenticated caller.
	// The object is the profile's name, recorded only when it passes the
	// name rule, since what a caller sends in its place can be anything.
	nopacsv1connect.ProfileServiceCreateProfileVersionProcedure: byMessage(func(r *v1.CreateProfileVersionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: profileObject(r.GetName())}
	}),
	nopacsv1connect.ProfileServiceGetProfileVersionProcedure: byMessage(func(r *v1.GetProfileVersionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: profileObject(r.GetName()), Details: map[string]string{detailVersion: strconv.Itoa(int(r.GetVersion()))}}
	}),
	nopacsv1connect.ProfileServiceListProfilesProcedure: byMessage(func(r *v1.ListProfilesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{}
	}),
	nopacsv1connect.ProfileServiceListProfileVersionsProcedure: byMessage(func(r *v1.ListProfileVersionsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: profileObject(r.GetName())}
	}),
	nopacsv1connect.ProfileServiceValidateProfileProcedure: byMessage(func(r *v1.ValidateProfileRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{}
	}),
	// "Role: none; any caller the server authenticates."
	nopacsv1connect.ProfileServiceListProfileBasesProcedure: fixed(access.Authenticated()),
	// A trial's values are answered only when asked for, and never to a
	// masked key.
	// A trial or an explanation of a held file is refused it too, values
	// hidden or not: a held file is the sender's original, and what a run
	// answers of it (which rule matched, the pattern matches, whether a
	// value changed, its lengths) answers one question about each value per
	// run, so a caller who chooses the rules reads the value back. A masked
	// key tries only files it sends, which it already holds.
	nopacsv1connect.ProfileServiceTryProfileProcedure: byMessage(func(r *v1.TryProfileRequest) (access.Requirement, access.Request) {
		need := onTenantNamed(r.GetTenant(), master.RoleOperator)
		if r.GetShowValues() || len(r.GetQuarantineRecords()) > 0 {
			need = need.AnswersOriginals()
		}
		return need, access.Request{Object: trialObject(r.GetProfile()), Details: showValuesDetails(r.GetShowValues(), nil)}
	}),
	nopacsv1connect.ProfileServiceExplainProfileProcedure: byMessage(func(r *v1.ExplainProfileRequest) (access.Requirement, access.Request) {
		need := onTenantNamed(r.GetTenant(), master.RoleOperator)
		if r.GetQuarantineRecord() != "" {
			need = need.AnswersOriginals()
		}
		return need, access.Request{Object: trialObject(r.GetProfile())}
	}),
	nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure: byMessage(func(r *v1.DiffProfileVersionsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: profileObject(r.GetName()), Details: map[string]string{
			detailFromVersion: strconv.Itoa(int(r.GetFromVersion())), detailToVersion: strconv.Itoa(int(r.GetToVersion())), detailToName: profileOrEmpty(r.GetToName())}}
	}),

	// DeidService: admin on the tenant for the forward lookup and for an
	// import, each joining a value to its codes; operator to list the
	// tables' sizes. The object is the attribute's tag, or the table's
	// name, never a value. Both are refused to a masked key: a code joins an
	// identity to its pseudonym, and an import of one entry tells whether a
	// table holds a value.
	nopacsv1connect.DeidServiceCodeProcedure: byMessage(func(r *v1.CodeRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).AnswersOriginals(), access.Request{Object: attributeObject(r.GetAttribute())}
	}),
	nopacsv1connect.DeidServiceImportLookupCodesProcedure: byMessage(func(r *v1.ImportLookupCodesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).AnswersOriginals(), access.Request{Object: profileObject(r.GetTable())}
	}),
	nopacsv1connect.DeidServiceListLookupTablesProcedure: byMessage(func(r *v1.ListLookupTablesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{}
	}),
	nopacsv1connect.ProfileServiceDescribeProfileProcedure: byMessage(func(r *v1.DescribeProfileRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: trialObject(r.GetProfile())}
	}),

	// CorrectionService: admin on the tenant for every change (delete,
	// relabel, replace, undo, purge, allowing arrivals), operator to read the
	// corrections, whose reasons are a person's words about a subject, and
	// reader for the scrub findings and to resolve revisions. A change to a
	// target records its path of labels in the details, never an instance
	// id, which holds a SOP Instance UID as received; its object is the
	// correction's id, which the store sets once the correction is made
	// (none for a delete's preview). The reason is not recorded by the
	// requirement: the handler writes it once it has passed its bounds.
	nopacsv1connect.CorrectionServiceDeleteProcedure: byMessage(func(r *v1.DeleteRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Details: deleteDetails(r, nil)}
	}),
	// A batch delete deletes many targets at once, which a masked key may
	// not (access.Requirement.DeletesInBulk); its preview is the same
	// request, so it is refused the preview too. Its targets are counted,
	// never listed: a list of ten thousand labels is no audit detail.
	nopacsv1connect.CorrectionServiceDeleteBatchProcedure: byMessage(func(r *v1.DeleteBatchRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).DeletesInBulk(), access.Request{Details: deleteBatchDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceListDeleteBatchTargetsProcedure: byMessage(func(r *v1.ListDeleteBatchTargetsRequest) (access.Requirement,
		access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetJobId())}
	}),
	nopacsv1connect.CorrectionServiceRelabelProcedure: byMessage(func(r *v1.RelabelRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Details: relabelDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceReplaceProcedure: byMessage(func(r *v1.ReplaceRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Details: map[string]string{detailQuarantined: jobObject(r.GetQuarantinedFile())}}
	}),
	// An edit writes values a person types into archived files, which a
	// masked key may not (access.Requirement.WritesValues); its preview
	// is the same request, so it is refused the preview too.
	nopacsv1connect.CorrectionServiceEditProcedure: byMessage(func(r *v1.EditRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).WritesValues(), access.Request{Details: editDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceListCorrectionsProcedure: byMessage(func(r *v1.ListCorrectionsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Details: correctionFilterDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceGetCorrectionProcedure: byMessage(func(r *v1.GetCorrectionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.CorrectionServiceUndoCorrectionProcedure: byMessage(func(r *v1.UndoCorrectionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.CorrectionServicePurgeCorrectionProcedure: byMessage(func(r *v1.PurgeCorrectionRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.CorrectionServiceAllowArrivalsProcedure: byMessage(func(r *v1.AllowArrivalsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: jobObject(r.GetId()), Details: allowDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceListScrubFindingsProcedure: byMessage(func(r *v1.ListScrubFindingsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: findingFilterDetails(r)}
	}),
	nopacsv1connect.CorrectionServiceResolveInstancesProcedure: byMessage(func(r *v1.ResolveInstancesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Details: map[string]string{detailMembers: strconv.Itoa(len(r.GetMembers()))}}
	}),

	// ReplicaService: admin to attach, detach, repair and switch, since each
	// decides where the archive's bytes are or writes to a primary; reader
	// to list replicas. RetentionService: admin for every change of rules
	// and for disposal, and admin on the scope (a tenant, or the
	// installation for every tenant) to confirm the clock; reader for rules
	// and due sessions; operator for holds, whose reasons are a person's
	// words about a subject. The object of a storage, rule or hold method is
	// its name or id. Attaching a replica decides where copies of the
	// archive's bytes go, which a masked key may not choose
	// (access.Requirement.ChoosesDestination).
	nopacsv1connect.ReplicaServiceListReplicasProcedure: byMessage(func(r *v1.ListReplicasRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.ReplicaServiceAttachReplicaProcedure: byMessage(func(r *v1.AttachReplicaRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin).ChoosesDestination(), access.Request{Object: r.GetReplica()}
	}),
	nopacsv1connect.ReplicaServiceDetachReplicaProcedure: byMessage(func(r *v1.DetachReplicaRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetReplica(),
			Details: map[string]string{detailAbandonPurges: strconv.FormatBool(r.GetAbandonPurges())}}
	}),
	nopacsv1connect.ReplicaServiceRepairProcedure: byMessage(func(r *v1.RepairRequest) (access.Requirement, access.Request) {
		findings := strconv.Itoa(len(r.GetFindings()))
		if r.GetAll() {
			findings = "all"
		}
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetStorage(), Details: map[string]string{detailFindings: findings}}
	}),
	nopacsv1connect.ReplicaServiceSwitchStorageProcedure: byMessage(func(r *v1.SwitchStorageRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetPrimary(),
			Details: map[string]string{detailValidateOnly: strconv.FormatBool(r.GetValidateOnly()), detailSkipVerification: strconv.FormatBool(r.GetSkipVerification())}}
	}),
	// BackfillReplica: operator, as copying is the server's own work, and
	// refused to a masked key, since it fills the replica the request names
	// with the archive's bytes (access.Requirement.ChoosesDestination).
	// Recall: operator, since a recall costs the site money and is
	// previewed first; days above a storage's recall_days are an admin's,
	// judged by the handler once it knows the storages. VerifySample:
	// operator, since it answers keys that hold UIDs.
	nopacsv1connect.ReplicaServiceBackfillReplicaProcedure: byMessage(func(r *v1.BackfillReplicaRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator).ChoosesDestination(), access.Request{Object: r.GetReplica()}
	}),
	nopacsv1connect.ReplicaServiceRecallProcedure: byMessage(func(r *v1.RecallRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: labelPath(r.GetSubjectLabel(), r.GetSessionLabel(), r.GetScanLabel()),
			Details: map[string]string{detailValidateOnly: strconv.FormatBool(r.GetValidateOnly())}}
	}),
	nopacsv1connect.ReplicaServiceVerifySampleProcedure: byMessage(func(r *v1.VerifySampleRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: r.GetStorage(),
			Details: map[string]string{detailCount: strconv.Itoa(int(r.GetCount()))}}
	}),
	nopacsv1connect.RetentionServiceAddRetentionRuleProcedure: byMessage(func(r *v1.AddRetentionRuleRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetRule().GetName()}
	}),
	nopacsv1connect.RetentionServiceGetRetentionRuleProcedure: byMessage(func(r *v1.GetRetentionRuleRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{Object: r.GetName()}
	}),
	nopacsv1connect.RetentionServiceListRetentionRulesProcedure: byMessage(func(r *v1.ListRetentionRulesRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.RetentionServiceSetRetentionRuleProcedure: byMessage(func(r *v1.SetRetentionRuleRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetRule().GetName(), Details: maskDetails(r.GetUpdateMask().GetPaths())}
	}),
	nopacsv1connect.RetentionServiceRemoveRetentionRuleProcedure: byMessage(func(r *v1.RemoveRetentionRuleRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetName()}
	}),
	nopacsv1connect.RetentionServiceListDueSessionsProcedure: byMessage(func(r *v1.ListDueSessionsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleReader), access.Request{}
	}),
	nopacsv1connect.RetentionServiceDisposeProcedure: byMessage(func(r *v1.DisposeRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: r.GetRule(),
			Details: map[string]string{detailValidateOnly: strconv.FormatBool(r.GetValidateOnly())}}
	}),
	nopacsv1connect.RetentionServiceConfirmClockProcedure: byMessage(func(r *v1.ConfirmClockRequest) (access.Requirement, access.Request) {
		return onScope(r.GetScope(), master.RoleAdmin), access.Request{Details: map[string]string{detailScope: scopeDetail(r.GetScope())}}
	}),
	nopacsv1connect.RetentionServiceGetHoldProcedure: byMessage(func(r *v1.GetHoldRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{Object: jobObject(r.GetId())}
	}),
	nopacsv1connect.RetentionServiceListHoldsProcedure: byMessage(func(r *v1.ListHoldsRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleOperator), access.Request{}
	}),
	// PlaceHold and ReleaseHold: admin, a hold's target recorded as its path
	// of labels.
	nopacsv1connect.RetentionServicePlaceHoldProcedure: byMessage(func(r *v1.PlaceHoldRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Details: map[string]string{detailTarget: holdTargetDetail(r.GetTarget())}}
	}),
	nopacsv1connect.RetentionServiceReleaseHoldProcedure: byMessage(func(r *v1.ReleaseHoldRequest) (access.Requirement, access.Request) {
		return onTenantNamed(r.GetTenant(), master.RoleAdmin), access.Request{Object: jobObject(r.GetId())}
	}),
}

// pendingRules is the table of the methods the contract defines and no
// handler serves yet: those of the services api/not-served.txt names, and
// the single methods it names of services otherwise served. Methods does
// not consult it for a requirement, and the server does not mount a method
// it holds (Pending), so the server answers each of these methods as one
// it does not serve, after the caller is authenticated, and none can be
// served by mistake with no handler behind it. Each line is written with
// its method's contract and checked against its Role line now; the change
// that serves a method moves its line into rules and its name off the
// list, and the tests fail until both are done.
var pendingRules = map[string]rule{}

// The keys of the details of the correction methods' entries.
const (
	// detailValidateOnly says whether a delete asked for a preview only.
	detailValidateOnly = "validate_only"
	// detailToLabel is the new label a relabel names.
	detailToLabel = "to_label"
	// detailMembers is how many (instance, revision) pairs a resolve asks
	// about; never the instance ids, which hold UIDs as received.
	detailMembers = "members"
	// detailAbandonPurges says whether a replica's detach abandons the
	// purges that still count it.
	detailAbandonPurges = "abandon_purges"
	// detailOverrideRetention says whether a delete overrides a retention
	// rule's keep_at_least; the reason is the handler's to record.
	detailOverrideRetention = "override_retention"
	// detailSkipVerification says whether a switch passes over the checks
	// of its replica; the reason is the handler's to record.
	detailSkipVerification = "skip_verification"
	// detailFindings is how many scrub findings a repair names, or "all";
	// never their ids.
	detailFindings = "findings"
)

// The keys of the details of the profile methods' entries.
const (
	// detailVersion is the version a request names.
	detailVersion = "version"
	// detailFromVersion and detailToVersion are the versions a comparison
	// names; detailToName the other profile it names, when it names one.
	detailFromVersion = "from_version"
	detailToVersion   = "to_version"
	detailToName      = "to_name"
)

// profileOrEmpty is profileObject that leaves an empty name empty.
func profileOrEmpty(name string) string {
	if name == "" {
		return ""
	}
	return profileObject(name)
}

// trialObject returns the profile a trial run, an explanation or a check
// names, as an audit entry's object: "name@version" ("name@latest" for
// version 0) when the name passes the name rule, "document" for an unsaved
// document (whose checksum the handler adds), "invalid" otherwise.
func trialObject(p *v1.TrialProfile) string {
	switch k := p.GetKind().(type) {
	case *v1.TrialProfile_Version:
		name, version := k.Version.GetName(), k.Version.GetVersion()
		if !profile.ValidName(name) || version < 0 {
			return invalidValue
		}
		if version == 0 {
			return name + "@latest"
		}
		return name + "@" + strconv.Itoa(int(version))
	case *v1.TrialProfile_Document:
		return "document"
	}
	return invalidValue
}

// profileObject returns a profile's name as an audit entry's object: as
// given when it passes the name rule, and "invalid" otherwise.
func profileObject(name string) string {
	if profile.ValidName(name) {
		return name
	}
	return invalidValue
}

// The keys of the details the reading methods put in audit entries.
const (
	detailLabelPrefix       = "label_prefix"
	detailOrder             = "order"
	detailStudyDateStart    = "study_date_start"
	detailStudyDateEnd      = "study_date_end"
	detailModality          = "modality"
	detailDescriptionPrefix = "series_description_prefix"
	detailInstance          = "instance"
	detailStartAfter        = "start_after"
	detailAnnounceOnly      = "announce_only"
	detailRanged            = "range"
	detailWindow            = "window"
)

// invalidValue is what an audit entry records in place of a value the
// request gave that does not pass its rule: the entry still says the
// field was given, and nothing malformed, of any length or content,
// reaches the log.
const invalidValue = "invalid"

// labelPath returns the path of labels a request names, "<subject>",
// "<subject>/<session>" or "<subject>/<session>/<scan>", as an audit
// entry's object writes it: the labels given from the top, up to the first
// empty one, so a level named without the one above it (which the handler
// refuses as invalid) is not recorded as if it were placed. A label that
// does not pass the label rule is written "invalid": what a caller sent
// that is not a label can be anything, a patient's name included. Three
// valid labels fit an entry's field, which holds 256 bytes, only up to
// about 84 bytes each, so the path is clipped.
func labelPath(labels ...string) string {
	var parts []string
	for _, l := range labels {
		if l == "" {
			break
		}
		if label.Check(l) != nil {
			l = invalidValue
		}
		parts = append(parts, l)
	}
	return clip(strings.Join(parts, "/"), master.MaxAuditFieldLength-8)
}

// labelPrefix returns a label prefix as an audit entry records it: as
// given when it is a label or the start of one, and "invalid" otherwise. A
// start of a label may end where a whole label may not (in a "."), or be
// a name reserved on Windows that a longer label is not, so a prefix
// passes when it, or it with one more character that is valid anywhere
// in a label, passes the label rule.
func labelPrefix(p string) string {
	if label.Check(p) == nil || label.Check(p+"_") == nil {
		return p
	}
	return invalidValue
}

// studyDate returns a study date bound as an audit entry records it: as
// given when it has the form of a DICOM date, "YYYYMMDD", and "invalid"
// otherwise.
func studyDate(s string) string {
	if len(s) != 8 || strings.Trim(s, "0123456789") != "" {
		return invalidValue
	}
	return s
}

// modality returns a Modality filter as an audit entry records it: as
// given when it is a value of the CS value representation (PS3.5 6.2), 1
// to 16 of A-Z, 0-9, space and "_", and "invalid" otherwise.
func modality(s string) string {
	if s == "" || len(s) > 16 || strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 _") != "" {
		return invalidValue
	}
	return s
}

// receiverName returns a receiver filter as an audit entry records it: as
// given when it follows the rule of a receiver's name, and "invalid"
// otherwise.
func receiverName(s string) string {
	if master.ValidateTenantName(s) != nil {
		return invalidValue
	}
	return s
}

// catalogFilterDetails returns the details every catalog listing records:
// its label prefix and its order, when given.
func catalogFilterDetails(prefix string, order v1.CatalogOrder) map[string]string {
	d := map[string]string{}
	if prefix != "" {
		d[detailLabelPrefix] = labelPrefix(prefix)
	}
	if order != v1.CatalogOrder_CATALOG_ORDER_UNSPECIFIED {
		d[detailOrder] = clip(order.String(), 64)
	}
	return d
}

// sessionFilterDetails returns the filters of a listing of sessions, as
// its entry's details record them (catalog.proto, ListSessions).
func sessionFilterDetails(r *v1.ListSessionsRequest) map[string]string {
	d := catalogFilterDetails(r.GetLabelPrefix(), r.GetOrder())
	if r.GetStudyDateStart() != "" {
		d[detailStudyDateStart] = studyDate(r.GetStudyDateStart())
	}
	if r.GetStudyDateEnd() != "" {
		d[detailStudyDateEnd] = studyDate(r.GetStudyDateEnd())
	}
	if r.GetModality() != "" {
		d[detailModality] = modality(r.GetModality())
	}
	if r.GetReceiver() != "" {
		d[detailReceiver] = receiverName(r.GetReceiver())
	}
	timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
	return d
}

// scanFilterDetails returns the filters of a listing of scans, as its
// entry's details record them (catalog.proto, ListScans). A series
// description is recorded only as present: it is text the sender wrote,
// which can hold anything, a name included.
func scanFilterDetails(r *v1.ListScansRequest) map[string]string {
	d := catalogFilterDetails(r.GetLabelPrefix(), r.GetOrder())
	if r.GetModality() != "" {
		d[detailModality] = modality(r.GetModality())
	}
	if r.GetSeriesDescriptionPrefix() != "" {
		d[detailDescriptionPrefix] = presentValue
	}
	if r.GetReceiver() != "" {
		d[detailReceiver] = receiverName(r.GetReceiver())
	}
	timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
	return d
}

// readFilesDetails returns the details of the entry a stream of files
// opens with (data.proto, ReadFiles): whether an instance and a position
// to resume from were given, never which, since an instance id is an
// identifier as received in this version; and whether only the
// announcements were asked for.
func readFilesDetails(r *v1.ReadFilesRequest) map[string]string {
	d := map[string]string{detailAnnounceOnly: strconv.FormatBool(r.GetAnnounceOnly())}
	if r.GetInstanceId() != "" {
		d[detailInstance] = presentValue
	}
	if r.GetStartAfter() != nil {
		d[detailStartAfter] = presentValue
	}
	return d
}

// builtinSettings is the list of settings the binary ships, which the
// table reads a setting's kind from: the table has no other list, and the
// handlers check the kind again against theirs (settingsService).
var builtinSettings = settings.MustBuiltin()

// settingChange returns the requirement of a change of the setting named
// name on scope: admin on the scope, except for a setting a tenant's value
// of which the installation's admin alone sets, which needs admin on the
// installation whatever the scope (settings.proto, SetSetting).
func settingChange(name string, scope *v1.Scope) access.Requirement {
	if def, ok := builtinSettings.Lookup(name); ok && def.TenantSetByInstallation {
		return installationAdmin
	}
	return onScope(scope, master.RoleAdmin)
}

// settingFacts returns the audit facts of a request about one setting.
func settingFacts(name string, scope *v1.Scope) access.Request {
	return access.Request{Object: name, Details: map[string]string{detailScope: scopeDetail(scope)}}
}

// auditFilterDetails returns the filters of a listing of the audit log, as
// its entry's details record them (audit.proto, ListAuditEntries).
func auditFilterDetails(r *v1.ListAuditEntriesRequest) map[string]string {
	d := map[string]string{detailScope: scopeDetail(r.GetScope())}
	if r.GetKeyId() != "" {
		d[detailKeyID] = master.KeyObject(r.GetKeyId())
	}
	if o, ok := masterOutcome(r.GetOutcome()); ok && o != "" {
		d[detailOutcome] = string(o)
	}
	if r.GetStartTime() != nil {
		d[detailStart] = r.GetStartTime().AsTime().UTC().Format(time.RFC3339Nano)
	}
	if r.GetEndTime() != nil {
		d[detailEnd] = r.GetEndTime().AsTime().UTC().Format(time.RFC3339Nano)
	}
	return d
}

// attributeObject returns the attribute a forward lookup names as an audit
// entry's object: its tag in eight digits when it names a standard
// element, "invalid" otherwise. The value is never an object.
func attributeObject(attr string) string {
	t, _, err := codeAttribute(attr)
	if err != nil {
		return invalidValue
	}
	return fmt.Sprintf("%08X", t)
}
