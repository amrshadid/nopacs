package servertest

import (
	"slices"

	"google.golang.org/protobuf/proto"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// Entry is what one method needs in Table: a fixed requirement, or, when
// NewMsg is set, a requirement read from the request by Need.
type Entry struct {
	// Fixed is the requirement when it does not depend on the request.
	Fixed access.Requirement
	// NewMsg returns an empty request message; nil when Fixed applies.
	NewMsg func() proto.Message
	// Need maps a request to its requirement, when NewMsg is set.
	Need func(proto.Message) access.Requirement
}

// Table is a stand-in method-to-requirement table, keyed by full procedure
// name. It implements server.Methods for the server's tests and the
// stand-in program. It is test data: the table the binary uses belongs to
// package internal/api. Contract returns the contract's table, taken from
// the "Role:" line of each method in api/proto/nopacs/v1; its tests check
// it against the descriptors and those lines.
type Table map[string]Entry

// RequestFor returns an empty request message of procedure when its
// requirement depends on the request, nil when it does not, and false for a
// procedure the table does not have.
func (t Table) RequestFor(procedure string) (proto.Message, bool) {
	e, ok := t[procedure]
	if !ok {
		return nil, false
	}
	if e.NewMsg == nil {
		return nil, true
	}
	return e.NewMsg(), true
}

// Requirement returns what procedure needs for the request msg (nil for a
// fixed requirement), and no audit facts: the stand-in records none.
func (t Table) Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request) {
	e, ok := t[procedure]
	if !ok {
		return access.Requirement{}, access.Request{}
	}
	if e.NewMsg == nil {
		return e.Fixed, access.Request{}
	}
	if msg == nil {
		msg = e.NewMsg()
	}
	return e.Need(msg), access.Request{}
}

// pendingMethods are the methods of services otherwise served that the
// contract defines and no handler serves yet: the method lines of
// api/not-served.txt, which a test compares with this list.
var pendingMethods = []string{
	nopacsv1connect.BackupServiceCreateBackupProcedure,
}

// Pending reports whether procedure is one of pendingMethods, so the server
// leaves it out of its routes as the binary's server does
// (server.PendingMethods).
func (Table) Pending(procedure string) bool {
	return slices.Contains(pendingMethods, procedure)
}

// PendingMethods returns pendingMethods, for the test that compares it with
// api/not-served.txt.
func PendingMethods() []string { return slices.Clone(pendingMethods) }

// fixed is an entry with a requirement that does not depend on the request.
func fixed(req access.Requirement) Entry { return Entry{Fixed: req} }

// byMessage is an entry whose requirement is read from the request, typed as
// the method's request message M.
func byMessage[M proto.Message](newMsg func() M, need func(M) access.Requirement) Entry {
	return Entry{
		NewMsg: func() proto.Message { return newMsg() },
		Need: func(m proto.Message) access.Requirement {
			typed, ok := m.(M)
			if !ok {
				// The zero Requirement, which nobody meets, fails closed.
				return access.Requirement{}
			}
			return need(typed)
		},
	}
}

// onScope is the requirement of a role on a Scope as a request gives it. A
// request with no scope is held to the role on the installation: the check
// runs before validation, so a caller without that role is refused before it
// learns its request was malformed.
func onScope(s *nopacsv1.Scope, role master.Role) access.Requirement {
	if t, ok := s.GetKind().(*nopacsv1.Scope_Tenant); ok {
		return access.OnTenant(t.Tenant, role)
	}
	return access.OnInstallation(role)
}

// Contract returns the contract's method-to-requirement table, a new copy
// each call so a test may add to it.
func Contract() Table {
	admin := fixed(access.OnInstallation(master.RoleAdmin))
	return Table{
		nopacsv1connect.AccessServiceCreateKeyProcedure:   admin,
		nopacsv1connect.AccessServiceGetKeyProcedure:      admin,
		nopacsv1connect.AccessServiceListKeysProcedure:    admin,
		nopacsv1connect.AccessServiceRevokeKeyProcedure:   admin,
		nopacsv1connect.AccessServiceSetGrantProcedure:    admin,
		nopacsv1connect.AccessServiceListGrantsProcedure:  admin,
		nopacsv1connect.AccessServiceRemoveGrantProcedure: admin,
		nopacsv1connect.AccessServiceSetKeyRulesProcedure: admin,
		// "Role: admin on the installation." The handler also asks for the
		// local operator, after the check.
		nopacsv1connect.AccessServiceReinstateKeysProcedure: admin,
		// "Role: admin on the installation; or the key being rotated
		// itself": the handler decides which, after the check.
		nopacsv1connect.AccessServiceRotateKeyProcedure: fixed(access.Authenticated()),
		// "Role: none; any caller the server authenticates."
		nopacsv1connect.AccessServiceWhoAmIProcedure: fixed(access.Authenticated()),

		nopacsv1connect.TenantServiceCreateTenantProcedure: admin,
		// "Role: reader on that tenant, or reader on the installation."
		nopacsv1connect.TenantServiceGetTenantProcedure: byMessage(
			func() *nopacsv1.GetTenantRequest { return &nopacsv1.GetTenantRequest{} },
			func(m *nopacsv1.GetTenantRequest) access.Requirement { return access.TenantRegistration(m.GetName()) },
		),
		// "Role: none beyond an authenticated caller."
		nopacsv1connect.TenantServiceListTenantsProcedure:  fixed(access.Authenticated()),
		nopacsv1connect.TenantServiceRemoveTenantProcedure: admin,
		// "Role: admin on the installation." The handler also asks for the
		// local operator, after the check.
		nopacsv1connect.TenantServiceReclaimTenantProcedure: admin,
		// BackupService: "Role: admin on the installation."
		nopacsv1connect.BackupServiceExportTenantProcedure: admin,

		nopacsv1connect.SettingsServiceGetSettingProcedure: byMessage(
			func() *nopacsv1.GetSettingRequest { return &nopacsv1.GetSettingRequest{} },
			func(m *nopacsv1.GetSettingRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleReader)
			},
		),
		nopacsv1connect.SettingsServiceSetSettingProcedure: byMessage(
			func() *nopacsv1.SetSettingRequest { return &nopacsv1.SetSettingRequest{} },
			func(m *nopacsv1.SetSettingRequest) access.Requirement {
				return settingChange(m.GetSetting().GetName(), m.GetSetting().GetScope())
			},
		),
		nopacsv1connect.SettingsServiceUnsetSettingProcedure: byMessage(
			func() *nopacsv1.UnsetSettingRequest { return &nopacsv1.UnsetSettingRequest{} },
			func(m *nopacsv1.UnsetSettingRequest) access.Requirement {
				return settingChange(m.GetName(), m.GetScope())
			},
		),
		nopacsv1connect.SettingsServiceListSettingsProcedure: byMessage(
			func() *nopacsv1.ListSettingsRequest { return &nopacsv1.ListSettingsRequest{} },
			func(m *nopacsv1.ListSettingsRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleReader)
			},
		),
		nopacsv1connect.AuditServiceListAuditEntriesProcedure: byMessage(
			func() *nopacsv1.ListAuditEntriesRequest { return &nopacsv1.ListAuditEntriesRequest{} },
			func(m *nopacsv1.ListAuditEntriesRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleAdmin)
			},
		),

		// AuditSealService: "admin on the scope asked for.", but the public
		// keys: "none; any caller the server authenticates."
		nopacsv1connect.AuditSealServiceVerifyAuditProcedure: byMessage(
			func() *nopacsv1.VerifyAuditRequest { return &nopacsv1.VerifyAuditRequest{} },
			func(m *nopacsv1.VerifyAuditRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleAdmin)
			},
		),
		nopacsv1connect.AuditSealServiceListAuditCheckpointsProcedure: byMessage(
			func() *nopacsv1.ListAuditCheckpointsRequest { return &nopacsv1.ListAuditCheckpointsRequest{} },
			func(m *nopacsv1.ListAuditCheckpointsRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleAdmin)
			},
		),
		nopacsv1connect.AuditSealServiceExportAuditProcedure: byMessage(
			func() *nopacsv1.ExportAuditRequest { return &nopacsv1.ExportAuditRequest{} },
			func(m *nopacsv1.ExportAuditRequest) access.Requirement {
				return onScope(m.GetScope(), master.RoleAdmin)
			},
		),
		nopacsv1connect.AuditSealServiceGetAuditPublicKeyProcedure: fixed(access.Authenticated()),

		// StorageService: "admin on the tenant." to change, "reader on the
		// tenant." to read.
		nopacsv1connect.StorageServiceAddStorageProcedure:    onTenant(func() *nopacsv1.AddStorageRequest { return &nopacsv1.AddStorageRequest{} }, master.RoleAdmin),
		nopacsv1connect.StorageServiceGetStorageProcedure:    onTenant(func() *nopacsv1.GetStorageRequest { return &nopacsv1.GetStorageRequest{} }, master.RoleReader),
		nopacsv1connect.StorageServiceListStoragesProcedure:  onTenant(func() *nopacsv1.ListStoragesRequest { return &nopacsv1.ListStoragesRequest{} }, master.RoleReader),
		nopacsv1connect.StorageServiceSetStorageProcedure:    onTenant(func() *nopacsv1.SetStorageRequest { return &nopacsv1.SetStorageRequest{} }, master.RoleAdmin),
		nopacsv1connect.StorageServiceRemoveStorageProcedure: onTenant(func() *nopacsv1.RemoveStorageRequest { return &nopacsv1.RemoveStorageRequest{} }, master.RoleAdmin),
		nopacsv1connect.StorageServiceTestStorageProcedure:   onTenant(func() *nopacsv1.TestStorageRequest { return &nopacsv1.TestStorageRequest{} }, master.RoleAdmin),

		// ReceiverService: the same lines.
		nopacsv1connect.ReceiverServiceAddReceiverProcedure:    onTenant(func() *nopacsv1.AddReceiverRequest { return &nopacsv1.AddReceiverRequest{} }, master.RoleAdmin),
		nopacsv1connect.ReceiverServiceGetReceiverProcedure:    onTenant(func() *nopacsv1.GetReceiverRequest { return &nopacsv1.GetReceiverRequest{} }, master.RoleReader),
		nopacsv1connect.ReceiverServiceListReceiversProcedure:  onTenant(func() *nopacsv1.ListReceiversRequest { return &nopacsv1.ListReceiversRequest{} }, master.RoleReader),
		nopacsv1connect.ReceiverServiceSetReceiverProcedure:    onTenant(func() *nopacsv1.SetReceiverRequest { return &nopacsv1.SetReceiverRequest{} }, master.RoleAdmin),
		nopacsv1connect.ReceiverServiceRemoveReceiverProcedure: onTenant(func() *nopacsv1.RemoveReceiverRequest { return &nopacsv1.RemoveReceiverRequest{} }, master.RoleAdmin),

		// ReceptionService: reader to read, operator to filter by Study
		// Instance UID and to settle a failed reception.
		nopacsv1connect.ReceptionServiceListReceptionsProcedure: byMessage(
			func() *nopacsv1.ListReceptionsRequest { return &nopacsv1.ListReceptionsRequest{} },
			func(m *nopacsv1.ListReceptionsRequest) access.Requirement {
				return tenantRole(m.GetTenant(), master.RoleReader, m.GetStudyInstanceUid() != "")
			},
		),
		nopacsv1connect.ReceptionServiceGetReceptionProcedure:     onTenant(func() *nopacsv1.GetReceptionRequest { return &nopacsv1.GetReceptionRequest{} }, master.RoleReader),
		nopacsv1connect.ReceptionServiceRetryReceptionProcedure:   onTenant(func() *nopacsv1.RetryReceptionRequest { return &nopacsv1.RetryReceptionRequest{} }, master.RoleOperator),
		nopacsv1connect.ReceptionServiceDiscardReceptionProcedure: onTenant(func() *nopacsv1.DiscardReceptionRequest { return &nopacsv1.DiscardReceptionRequest{} }, master.RoleOperator),

		// QuarantineService: reader to read, operator for a UID filter, a
		// header, a release and a rejection.
		nopacsv1connect.QuarantineServiceListQuarantinedFilesProcedure: byMessage(
			func() *nopacsv1.ListQuarantinedFilesRequest { return &nopacsv1.ListQuarantinedFilesRequest{} },
			func(m *nopacsv1.ListQuarantinedFilesRequest) access.Requirement {
				return tenantRole(m.GetTenant(), master.RoleReader, m.GetStudyInstanceUid() != "" || m.GetSopInstanceUid() != "")
			},
		),
		nopacsv1connect.QuarantineServiceGetQuarantinedFileProcedure: byMessage(
			func() *nopacsv1.GetQuarantinedFileRequest { return &nopacsv1.GetQuarantinedFileRequest{} },
			func(m *nopacsv1.GetQuarantinedFileRequest) access.Requirement {
				return tenantRole(m.GetTenant(), master.RoleReader, m.GetIncludeHeader())
			},
		),
		nopacsv1connect.QuarantineServiceReleaseQuarantinedFileProcedure: byMessage(
			func() *nopacsv1.ReleaseQuarantinedFileRequest { return &nopacsv1.ReleaseQuarantinedFileRequest{} },
			func(m *nopacsv1.ReleaseQuarantinedFileRequest) access.Requirement {
				role := master.RoleOperator
				if m.GetProfile() != nil {
					role = master.RoleAdmin
				}
				if m.GetTenant() == "" {
					return access.OnInstallation(role)
				}
				return access.OnTenant(m.GetTenant(), role)
			},
		),
		nopacsv1connect.QuarantineServiceRejectQuarantinedFileProcedure: onTenant(func() *nopacsv1.RejectQuarantinedFileRequest { return &nopacsv1.RejectQuarantinedFileRequest{} }, master.RoleOperator),

		// MetadataService: reader on the tenant for a scan's or an
		// instance's header.
		nopacsv1connect.MetadataServiceGetScanMetadataProcedure:     onTenant(func() *nopacsv1.GetScanMetadataRequest { return &nopacsv1.GetScanMetadataRequest{} }, master.RoleReader),
		nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure: onTenant(func() *nopacsv1.GetInstanceMetadataRequest { return &nopacsv1.GetInstanceMetadataRequest{} }, master.RoleReader),

		// CatalogService: reader on the tenant for every list and get.
		nopacsv1connect.CatalogServiceListSubjectsProcedure: onTenant(func() *nopacsv1.ListSubjectsRequest { return &nopacsv1.ListSubjectsRequest{} }, master.RoleReader),
		nopacsv1connect.CatalogServiceGetSubjectProcedure:   onTenant(func() *nopacsv1.GetSubjectRequest { return &nopacsv1.GetSubjectRequest{} }, master.RoleReader),
		nopacsv1connect.CatalogServiceListSessionsProcedure: onTenant(func() *nopacsv1.ListSessionsRequest { return &nopacsv1.ListSessionsRequest{} }, master.RoleReader),
		nopacsv1connect.CatalogServiceGetSessionProcedure:   onTenant(func() *nopacsv1.GetSessionRequest { return &nopacsv1.GetSessionRequest{} }, master.RoleReader),
		nopacsv1connect.CatalogServiceListScansProcedure:    onTenant(func() *nopacsv1.ListScansRequest { return &nopacsv1.ListScansRequest{} }, master.RoleReader),
		nopacsv1connect.CatalogServiceGetScanProcedure:      onTenant(func() *nopacsv1.GetScanRequest { return &nopacsv1.GetScanRequest{} }, master.RoleReader),

		// DataService: reader on the tenant for a stream of files.
		nopacsv1connect.DataServiceReadFilesProcedure: onTenant(func() *nopacsv1.ReadFilesRequest { return &nopacsv1.ReadFilesRequest{} }, master.RoleReader),

		// SenderService: "admin on the tenant." to change or test a sender,
		// "reader on the tenant." to read one. JobService: "operator on the
		// tenant." to make, cancel and retry a job, "reader on the tenant."
		// to read jobs and their failures.
		nopacsv1connect.SenderServiceAddSenderProcedure:    onTenant(func() *nopacsv1.AddSenderRequest { return &nopacsv1.AddSenderRequest{} }, master.RoleAdmin),
		nopacsv1connect.SenderServiceGetSenderProcedure:    onTenant(func() *nopacsv1.GetSenderRequest { return &nopacsv1.GetSenderRequest{} }, master.RoleReader),
		nopacsv1connect.SenderServiceListSendersProcedure:  onTenant(func() *nopacsv1.ListSendersRequest { return &nopacsv1.ListSendersRequest{} }, master.RoleReader),
		nopacsv1connect.SenderServiceSetSenderProcedure:    onTenant(func() *nopacsv1.SetSenderRequest { return &nopacsv1.SetSenderRequest{} }, master.RoleAdmin),
		nopacsv1connect.SenderServiceRemoveSenderProcedure: onTenant(func() *nopacsv1.RemoveSenderRequest { return &nopacsv1.RemoveSenderRequest{} }, master.RoleAdmin),
		nopacsv1connect.SenderServiceTestSenderProcedure:   onTenant(func() *nopacsv1.TestSenderRequest { return &nopacsv1.TestSenderRequest{} }, master.RoleAdmin),
		nopacsv1connect.JobServiceCreateJobProcedure:       onTenant(func() *nopacsv1.CreateJobRequest { return &nopacsv1.CreateJobRequest{} }, master.RoleOperator),
		nopacsv1connect.JobServiceGetJobProcedure:          onTenant(func() *nopacsv1.GetJobRequest { return &nopacsv1.GetJobRequest{} }, master.RoleReader),
		nopacsv1connect.JobServiceListJobsProcedure:        onTenant(func() *nopacsv1.ListJobsRequest { return &nopacsv1.ListJobsRequest{} }, master.RoleReader),
		nopacsv1connect.JobServiceCancelJobProcedure:       onTenant(func() *nopacsv1.CancelJobRequest { return &nopacsv1.CancelJobRequest{} }, master.RoleOperator),
		nopacsv1connect.JobServiceRetryJobProcedure:        onTenant(func() *nopacsv1.RetryJobRequest { return &nopacsv1.RetryJobRequest{} }, master.RoleOperator),
		nopacsv1connect.JobServiceListJobFailuresProcedure: onTenant(func() *nopacsv1.ListJobFailuresRequest { return &nopacsv1.ListJobFailuresRequest{} }, master.RoleReader),
		nopacsv1connect.JobServiceListImportFilesProcedure: onTenant(func() *nopacsv1.ListImportFilesRequest { return &nopacsv1.ListImportFilesRequest{} }, master.RoleOperator),
		nopacsv1connect.JobServiceListImportUnitsProcedure: onTenant(func() *nopacsv1.ListImportUnitsRequest { return &nopacsv1.ListImportUnitsRequest{} }, master.RoleOperator),

		// ProfileService: "admin on the tenant." to make a version,
		// "operator on the tenant." to read, check, try, explain and
		// compare; the bases need only an authenticated caller.
		nopacsv1connect.ProfileServiceCreateProfileVersionProcedure: onTenant(func() *nopacsv1.CreateProfileVersionRequest { return &nopacsv1.CreateProfileVersionRequest{} }, master.RoleAdmin),
		nopacsv1connect.ProfileServiceGetProfileVersionProcedure:    onTenant(func() *nopacsv1.GetProfileVersionRequest { return &nopacsv1.GetProfileVersionRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceListProfilesProcedure:         onTenant(func() *nopacsv1.ListProfilesRequest { return &nopacsv1.ListProfilesRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceListProfileVersionsProcedure:  onTenant(func() *nopacsv1.ListProfileVersionsRequest { return &nopacsv1.ListProfileVersionsRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceValidateProfileProcedure:      onTenant(func() *nopacsv1.ValidateProfileRequest { return &nopacsv1.ValidateProfileRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceListProfileBasesProcedure:     fixed(access.Authenticated()),
		nopacsv1connect.ProfileServiceTryProfileProcedure:           onTenant(func() *nopacsv1.TryProfileRequest { return &nopacsv1.TryProfileRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceExplainProfileProcedure:       onTenant(func() *nopacsv1.ExplainProfileRequest { return &nopacsv1.ExplainProfileRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure:  onTenant(func() *nopacsv1.DiffProfileVersionsRequest { return &nopacsv1.DiffProfileVersionsRequest{} }, master.RoleOperator),
		nopacsv1connect.ProfileServiceDescribeProfileProcedure:      onTenant(func() *nopacsv1.DescribeProfileRequest { return &nopacsv1.DescribeProfileRequest{} }, master.RoleOperator),
		// DeidService: "admin on the tenant." for the forward lookup and an
		// import, "operator on the tenant." to list the tables.
		nopacsv1connect.DeidServiceCodeProcedure:              onTenant(func() *nopacsv1.CodeRequest { return &nopacsv1.CodeRequest{} }, master.RoleAdmin),
		nopacsv1connect.DeidServiceImportLookupCodesProcedure: onTenant(func() *nopacsv1.ImportLookupCodesRequest { return &nopacsv1.ImportLookupCodesRequest{} }, master.RoleAdmin),
		nopacsv1connect.DeidServiceListLookupTablesProcedure:  onTenant(func() *nopacsv1.ListLookupTablesRequest { return &nopacsv1.ListLookupTablesRequest{} }, master.RoleOperator),

		// CorrectionService: "admin on the tenant." for every change,
		// "operator on the tenant." to read corrections, "reader on the
		// tenant." for scrub findings and to resolve revisions.
		nopacsv1connect.CorrectionServiceDeleteProcedure:            onTenant(func() *nopacsv1.DeleteRequest { return &nopacsv1.DeleteRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServiceRelabelProcedure:           onTenant(func() *nopacsv1.RelabelRequest { return &nopacsv1.RelabelRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServiceReplaceProcedure:           onTenant(func() *nopacsv1.ReplaceRequest { return &nopacsv1.ReplaceRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServiceListCorrectionsProcedure:   onTenant(func() *nopacsv1.ListCorrectionsRequest { return &nopacsv1.ListCorrectionsRequest{} }, master.RoleOperator),
		nopacsv1connect.CorrectionServiceGetCorrectionProcedure:     onTenant(func() *nopacsv1.GetCorrectionRequest { return &nopacsv1.GetCorrectionRequest{} }, master.RoleOperator),
		nopacsv1connect.CorrectionServiceUndoCorrectionProcedure:    onTenant(func() *nopacsv1.UndoCorrectionRequest { return &nopacsv1.UndoCorrectionRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServicePurgeCorrectionProcedure:   onTenant(func() *nopacsv1.PurgeCorrectionRequest { return &nopacsv1.PurgeCorrectionRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServiceAllowArrivalsProcedure:     onTenant(func() *nopacsv1.AllowArrivalsRequest { return &nopacsv1.AllowArrivalsRequest{} }, master.RoleAdmin),
		nopacsv1connect.CorrectionServiceListScrubFindingsProcedure: onTenant(func() *nopacsv1.ListScrubFindingsRequest { return &nopacsv1.ListScrubFindingsRequest{} }, master.RoleReader),
		nopacsv1connect.CorrectionServiceResolveInstancesProcedure:  onTenant(func() *nopacsv1.ResolveInstancesRequest { return &nopacsv1.ResolveInstancesRequest{} }, master.RoleReader),

		// MonitorService: "reader on the tenant asked for; operator on the
		// installation for every tenant."
		nopacsv1connect.MonitorServiceGetStatusProcedure: byMessage(
			func() *nopacsv1.GetStatusRequest { return &nopacsv1.GetStatusRequest{} },
			func(m *nopacsv1.GetStatusRequest) access.Requirement { return onMonitorScope(m.GetScope()) },
		),
		nopacsv1connect.MonitorServiceWatchProcedure: byMessage(
			func() *nopacsv1.WatchRequest { return &nopacsv1.WatchRequest{} },
			func(m *nopacsv1.WatchRequest) access.Requirement { return onMonitorScope(m.GetScope()) },
		),
	}
}

// onMonitorScope is the requirement of a Monitor method: reader on the
// tenant the scope names, operator on the installation for every tenant
// or a malformed scope.
func onMonitorScope(s *nopacsv1.Scope) access.Requirement {
	if t, ok := s.GetKind().(*nopacsv1.Scope_Tenant); ok && master.ValidateTenantName(t.Tenant) == nil {
		return access.OnTenant(t.Tenant, master.RoleReader)
	}
	return access.OnInstallation(master.RoleOperator)
}

// tenanted is a request into one tenant, named by its field "tenant".
type tenanted interface {
	proto.Message
	GetTenant() string
}

// onTenant is an entry whose requirement is role on the tenant the request
// names by its field "tenant".
func onTenant[M tenanted](newMsg func() M, role master.Role) Entry {
	return byMessage(newMsg, func(m M) access.Requirement { return tenantRole(m.GetTenant(), role, false) })
}

// tenantRole is the requirement of role on the tenant named name, or of
// operator when operator is set. A request with no tenant is held to the
// role on the installation, as onScope holds one with no scope.
func tenantRole(name string, role master.Role, operator bool) access.Requirement {
	if operator {
		role = master.RoleOperator
	}
	if name == "" {
		return access.OnInstallation(role)
	}
	return access.OnTenant(name, role)
}

// installationSet is the list of settings an admin on the installation
// alone sets on a tenant: SetSetting and UnsetSetting of one need admin on
// the installation, whatever the scope. Restated from the server's list of
// settings, so the table stays test data and its test compares the two.
var installationSet = map[string]bool{"receiver.ports": true, "incoming.limit": true, "sender.tenant-max-associations": true,
	"receiver.allow-as-received": true, "deid.pixel-limit": true,
	"import.allowed-roots": true, "import.max-units": true}

// settingChange is the requirement of a change of the setting named name
// on scope.
func settingChange(name string, scope *nopacsv1.Scope) access.Requirement {
	if installationSet[name] {
		return access.OnInstallation(master.RoleAdmin)
	}
	return onScope(scope, master.RoleAdmin)
}
