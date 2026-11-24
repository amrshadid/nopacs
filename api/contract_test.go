// Package api_test checks rules of the API contract that buf lint cannot
// express, by reading the descriptors compiled into the generated Go code.
package api_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	// Imported for its side effect: it registers the nopacs.v1 files in
	// protoregistry.GlobalFiles.
	_ "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The package whose messages are checked.
const contractPackage = "nopacs.v1"

// secretFields are the only fields through which a secret crosses the API: a
// secret never appears in an API answer, except a key's secret once, when it
// is made, so that a log or a client's history cannot leak one. Each must be
// marked debug_redact, which the server's redactor reads, and no other field
// may be.
var secretFields = []string{
	"nopacs.v1.CreateKeyResponse.secret",
	"nopacs.v1.CreateTenantRequest.database_address",
	"nopacs.v1.RotateKeyResponse.secret",
	"nopacs.v1.S3Storage.secret_access_key",
	"nopacs.v1.StowSender.credential",
	"nopacs.v1.TenantExportSummary.transfer_key",
}

// patientDataFields are the fields through which a file's values cross the
// API in a trial run of a profile: the caller's own data (a file it sent,
// or a held file it may read), answered only to it; and the paths of an
// import under its root and an import's inline map, since a folder or file
// name in a tree a site kept, or a subject's label in its map, can be a
// patient's name, and the contract answers them only to an operator. Each must be marked debug_redact, so the server's redactor
// removes them before anything is logged, as it removes a secret.
var patientDataFields = []string{
	"nopacs.v1.TrialElement.after",
	"nopacs.v1.TrialElement.before",
	"nopacs.v1.TrialFile.content",
	"nopacs.v1.TrialFile.name",
	"nopacs.v1.CodeRequest.value",
	"nopacs.v1.LookupEntry.value",
	"nopacs.v1.TrialResult.name",
	"nopacs.v1.ImportReceiver.map_file",
	"nopacs.v1.ImportJob.path",
	"nopacs.v1.ImportJob.map_file",
	"nopacs.v1.ImportJob.map_csv",
	"nopacs.v1.ImportFile.path",
	"nopacs.v1.ImportUnit.folder",
	"nopacs.v1.QuarantinedFile.import_path",
}

// textFields lists every field of the contract that carries free text: each
// string or bytes field, and each map with string or bytes keys or values.
// A secret can only travel in such a field. A field added to the contract
// that is not listed here fails the test, so adding one is a deliberate act:
// whoever lists it has decided it carries no secret, or that it is one of
// secretFields.
var textFields = []string{
	// scope.proto
	"nopacs.v1.Scope.tenant",

	// access.proto
	"nopacs.v1.Key.id",
	"nopacs.v1.Key.holder",
	"nopacs.v1.Grant.key_id",
	"nopacs.v1.LocalOperator.user_id",
	"nopacs.v1.CreateKeyRequest.holder",
	"nopacs.v1.CreateKeyResponse.secret",
	"nopacs.v1.GetKeyRequest.id",
	"nopacs.v1.ListKeysRequest.page_token",
	"nopacs.v1.ListKeysResponse.next_page_token",
	"nopacs.v1.RevokeKeyRequest.id",
	"nopacs.v1.SetGrantRequest.key_id",
	"nopacs.v1.ListGrantsRequest.key_id",
	"nopacs.v1.ListGrantsRequest.page_token",
	"nopacs.v1.ListGrantsResponse.next_page_token",
	"nopacs.v1.RemoveGrantRequest.key_id",
	"nopacs.v1.Key.networks",
	"nopacs.v1.Key.client_cert_sha256",
	"nopacs.v1.Key.last_use_address",
	"nopacs.v1.Key.successor_id",
	"nopacs.v1.CreateKeyRequest.networks",
	"nopacs.v1.CreateKeyRequest.client_cert_sha256",
	"nopacs.v1.RotateKeyRequest.id",
	"nopacs.v1.RotateKeyResponse.secret",
	"nopacs.v1.SetKeyRulesRequest.id",
	"nopacs.v1.SetKeyRulesRequest.networks",
	"nopacs.v1.SetKeyRulesRequest.client_cert_sha256",

	// audit.proto
	"nopacs.v1.AuditEntry.id",
	"nopacs.v1.AuditEntry.key_id",
	"nopacs.v1.AuditEntry.local_user_id",
	"nopacs.v1.AuditEntry.client_address",
	"nopacs.v1.AuditEntry.method",
	"nopacs.v1.AuditEntry.tenant",
	"nopacs.v1.AuditEntry.object",
	"nopacs.v1.AuditEntry.request_id",
	"nopacs.v1.AuditEntry.details",
	"nopacs.v1.AuditEntry.reason",
	"nopacs.v1.ListAuditEntriesRequest.key_id",
	"nopacs.v1.ListAuditEntriesRequest.page_token",
	"nopacs.v1.ListAuditEntriesResponse.next_page_token",

	// settings.proto
	"nopacs.v1.Setting.name",
	"nopacs.v1.Setting.value",
	"nopacs.v1.GetSettingRequest.name",
	"nopacs.v1.UnsetSettingRequest.name",
	"nopacs.v1.ListSettingsRequest.page_token",
	"nopacs.v1.ListSettingsResponse.next_page_token",

	// tenant.proto
	"nopacs.v1.Tenant.name",
	"nopacs.v1.Tenant.state_reason",
	"nopacs.v1.CreateTenantRequest.name",
	"nopacs.v1.CreateTenantRequest.database_address",
	"nopacs.v1.GetTenantRequest.name",
	"nopacs.v1.ListTenantsRequest.page_token",
	"nopacs.v1.ListTenantsResponse.next_page_token",
	"nopacs.v1.RemoveTenantRequest.name",
	"nopacs.v1.RemoveTenantRequest.confirm_name",
	"nopacs.v1.RemoveTenantResponse.data_location",

	// quarantine.proto
	"nopacs.v1.Caller.calling_ae_title",
	"nopacs.v1.Caller.called_ae_title",
	"nopacs.v1.Caller.address",
	"nopacs.v1.QuarantinedFile.id",
	"nopacs.v1.QuarantinedFile.details",
	"nopacs.v1.QuarantinedFile.receiver",
	"nopacs.v1.QuarantinedFile.reception",
	"nopacs.v1.QuarantinedFile.sha256",
	"nopacs.v1.QuarantinedFile.kept_sha256",
	"nopacs.v1.QuarantinedFile.sop_class_uid",
	"nopacs.v1.QuarantinedFile.sop_instance_uid",
	"nopacs.v1.QuarantinedFile.study_instance_uid",
	"nopacs.v1.QuarantinedFile.series_instance_uid",
	"nopacs.v1.QuarantinedFile.transfer_syntax_uid",
	"nopacs.v1.QuarantinedFile.release_reception",
	"nopacs.v1.QuarantinedFile.reject_reason",
	"nopacs.v1.QuarantinedFile.release_reason",
	"nopacs.v1.QuarantinedFile.released_from",
	"nopacs.v1.QuarantinedFile.release_receiver",
	"nopacs.v1.ListQuarantinedFilesRequest.tenant",
	"nopacs.v1.ListQuarantinedFilesRequest.receiver",
	"nopacs.v1.ListQuarantinedFilesRequest.reception",
	"nopacs.v1.ListQuarantinedFilesRequest.page_token",
	"nopacs.v1.ListQuarantinedFilesRequest.study_instance_uid",
	"nopacs.v1.ListQuarantinedFilesRequest.sop_instance_uid",
	"nopacs.v1.ListQuarantinedFilesResponse.next_page_token",
	"nopacs.v1.GetQuarantinedFileRequest.tenant",
	"nopacs.v1.GetQuarantinedFileRequest.id",
	"nopacs.v1.GetQuarantinedFileResponse.header_json",
	"nopacs.v1.ReleaseQuarantinedFileRequest.tenant",
	"nopacs.v1.ReleaseQuarantinedFileRequest.id",
	"nopacs.v1.ReleaseQuarantinedFileRequest.receiver",
	"nopacs.v1.RejectQuarantinedFileRequest.tenant",
	"nopacs.v1.RejectQuarantinedFileRequest.id",
	"nopacs.v1.RejectQuarantinedFileRequest.reason",
	"nopacs.v1.ReleaseQuarantinedFileRequest.reason",

	// reception.proto
	"nopacs.v1.Reception.id",
	"nopacs.v1.Reception.receiver",
	"nopacs.v1.Reception.study_instance_uid",
	"nopacs.v1.Reception.subject_label",
	"nopacs.v1.Reception.session_label",
	"nopacs.v1.Reception.storage",
	"nopacs.v1.Reception.failure",
	"nopacs.v1.Reception.calling_ae_titles",
	"nopacs.v1.Reception.retry_of",
	"nopacs.v1.Reception.retry_reception",
	"nopacs.v1.Reception.discard_reason",
	"nopacs.v1.ReceptionScan.label",
	"nopacs.v1.ReceptionScan.series_instance_uid",
	"nopacs.v1.ReceptionArrival.id",
	"nopacs.v1.ListReceptionsRequest.tenant",
	"nopacs.v1.ListReceptionsRequest.receiver",
	"nopacs.v1.ListReceptionsRequest.study_instance_uid",
	"nopacs.v1.ListReceptionsRequest.page_token",
	"nopacs.v1.ListReceptionsResponse.next_page_token",
	"nopacs.v1.GetReceptionRequest.tenant",
	"nopacs.v1.GetReceptionRequest.id",
	"nopacs.v1.RetryReceptionRequest.tenant",
	"nopacs.v1.RetryReceptionRequest.id",
	"nopacs.v1.RetryReceptionRequest.receiver",
	"nopacs.v1.DiscardReceptionRequest.tenant",
	"nopacs.v1.DiscardReceptionRequest.id",
	"nopacs.v1.DiscardReceptionRequest.reason",

	// receiver.proto
	"nopacs.v1.Receiver.name",
	"nopacs.v1.Receiver.storage",
	"nopacs.v1.DimseReceiver.bind_address",
	"nopacs.v1.DimseReceiver.ae_title",
	"nopacs.v1.DimseReceiver.sop_classes",
	"nopacs.v1.DimseReceiver.transfer_syntaxes",
	"nopacs.v1.StowReceiver.key_ids",
	"nopacs.v1.CallerRule.calling_ae_title",
	"nopacs.v1.CallerRule.address_range",
	"nopacs.v1.ReceiverTls.certificate_file",
	"nopacs.v1.ReceiverTls.private_key_file",
	"nopacs.v1.NamingRule.subject",
	"nopacs.v1.NamingRule.session",
	"nopacs.v1.NamingRule.scan",
	"nopacs.v1.NamingPattern.attribute",
	"nopacs.v1.NamingPattern.expression",
	"nopacs.v1.ReceiverHealth.detail",
	"nopacs.v1.AddReceiverRequest.tenant",
	"nopacs.v1.GetReceiverRequest.tenant",
	"nopacs.v1.GetReceiverRequest.name",
	"nopacs.v1.ListReceiversRequest.tenant",
	"nopacs.v1.ListReceiversRequest.page_token",
	"nopacs.v1.ListReceiversResponse.next_page_token",
	"nopacs.v1.SetReceiverRequest.tenant",
	"nopacs.v1.RemoveReceiverRequest.tenant",
	"nopacs.v1.RemoveReceiverRequest.name",

	// monitor.proto
	"nopacs.v1.StorageStatus.storage",
	"nopacs.v1.ReceiverStatus.receiver",
	"nopacs.v1.TenantStatus.tenant",
	"nopacs.v1.TenantStatus.event_cursor",
	"nopacs.v1.GetStatusRequest.page_token",
	"nopacs.v1.GetStatusResponse.next_page_token",
	"nopacs.v1.AssociationFailed.receiver",
	"nopacs.v1.ReceptionStepReached.reception",
	"nopacs.v1.ReceptionStepReached.receiver",
	"nopacs.v1.ReceptionStepReached.subject_label",
	"nopacs.v1.ReceptionStepReached.session_label",
	"nopacs.v1.ReceptionStepReached.retry_of",
	"nopacs.v1.FileQuarantined.file",
	"nopacs.v1.FileQuarantined.reception",
	"nopacs.v1.FileQuarantined.receiver",
	"nopacs.v1.QuarantineDecided.file",
	"nopacs.v1.QuarantineDecided.release_reception",
	"nopacs.v1.ReceiverHealthChanged.receiver",
	"nopacs.v1.StorageSpaceChanged.storage",
	"nopacs.v1.FramesAdded.subject_label",
	"nopacs.v1.FramesAdded.session_label",
	"nopacs.v1.FramesAdded.scan_label",
	"nopacs.v1.Event.cursor",
	"nopacs.v1.Event.tenant",
	"nopacs.v1.WatchKeepAlive.cursor",
	"nopacs.v1.WatchKeepAlive.tenant_cursors",
	"nopacs.v1.WatchKeepAlive.unanswered_tenants",
	"nopacs.v1.WatchRequest.cursor",
	"nopacs.v1.WatchRequest.tenant_cursors",

	// storage.proto
	"nopacs.v1.Storage.name",
	"nopacs.v1.DirectoryStorage.root",
	"nopacs.v1.DirectoryStorage.path",
	"nopacs.v1.S3Storage.endpoint",
	"nopacs.v1.S3Storage.region",
	"nopacs.v1.S3Storage.bucket",
	"nopacs.v1.S3Storage.prefix",
	"nopacs.v1.S3Storage.access_key_id",
	"nopacs.v1.S3Storage.secret_access_key",
	"nopacs.v1.AddStorageRequest.tenant",
	"nopacs.v1.GetStorageRequest.tenant",
	"nopacs.v1.GetStorageRequest.name",
	"nopacs.v1.ListStoragesRequest.tenant",
	"nopacs.v1.ListStoragesRequest.page_token",
	"nopacs.v1.ListStoragesResponse.next_page_token",
	"nopacs.v1.SetStorageRequest.tenant",
	"nopacs.v1.RemoveStorageRequest.tenant",
	"nopacs.v1.RemoveStorageRequest.name",
	"nopacs.v1.TestStorageRequest.tenant",
	"nopacs.v1.TestStorageRequest.name",

	// catalog.proto
	"nopacs.v1.Subject.label",
	"nopacs.v1.Session.subject_label",
	"nopacs.v1.Session.label",
	"nopacs.v1.Session.study_instance_uid",
	"nopacs.v1.Session.study_date",
	"nopacs.v1.Session.modalities",
	"nopacs.v1.Session.storage",
	"nopacs.v1.Session.receiver",
	"nopacs.v1.Scan.subject_label",
	"nopacs.v1.Scan.session_label",
	"nopacs.v1.Scan.label",
	"nopacs.v1.Scan.series_instance_uid",
	"nopacs.v1.Scan.modality",
	"nopacs.v1.Scan.series_description",
	"nopacs.v1.Scan.storage",
	"nopacs.v1.Scan.manifest_sha256",
	"nopacs.v1.Scan.receiver",
	"nopacs.v1.ListSubjectsRequest.tenant",
	"nopacs.v1.ListSubjectsRequest.label_prefix",
	"nopacs.v1.ListSubjectsRequest.page_token",
	"nopacs.v1.ListSubjectsResponse.next_page_token",
	"nopacs.v1.GetSubjectRequest.tenant",
	"nopacs.v1.GetSubjectRequest.subject_label",
	"nopacs.v1.ListSessionsRequest.tenant",
	"nopacs.v1.ListSessionsRequest.subject_label",
	"nopacs.v1.ListSessionsRequest.label_prefix",
	"nopacs.v1.ListSessionsRequest.study_date_start",
	"nopacs.v1.ListSessionsRequest.study_date_end",
	"nopacs.v1.ListSessionsRequest.modality",
	"nopacs.v1.ListSessionsRequest.receiver",
	"nopacs.v1.ListSessionsRequest.page_token",
	"nopacs.v1.ListSessionsResponse.next_page_token",
	"nopacs.v1.GetSessionRequest.tenant",
	"nopacs.v1.GetSessionRequest.subject_label",
	"nopacs.v1.GetSessionRequest.session_label",
	"nopacs.v1.ListScansRequest.tenant",
	"nopacs.v1.ListScansRequest.subject_label",
	"nopacs.v1.ListScansRequest.session_label",
	"nopacs.v1.ListScansRequest.label_prefix",
	"nopacs.v1.ListScansRequest.modality",
	"nopacs.v1.ListScansRequest.series_description_prefix",
	"nopacs.v1.ListScansRequest.receiver",
	"nopacs.v1.ListScansRequest.page_token",
	"nopacs.v1.ListScansResponse.next_page_token",
	"nopacs.v1.GetScanRequest.tenant",
	"nopacs.v1.GetScanRequest.subject_label",
	"nopacs.v1.GetScanRequest.session_label",
	"nopacs.v1.GetScanRequest.scan_label",

	// metadata.proto
	"nopacs.v1.InstanceMetadata.instance_id",
	"nopacs.v1.InstanceMetadata.sop_instance_uid",
	"nopacs.v1.InstanceMetadata.dicom_json",
	"nopacs.v1.InstanceMetadata.transfer_syntax_uid",
	"nopacs.v1.GetScanMetadataRequest.tenant",
	"nopacs.v1.GetScanMetadataRequest.subject_label",
	"nopacs.v1.GetScanMetadataRequest.session_label",
	"nopacs.v1.GetScanMetadataRequest.scan_label",
	"nopacs.v1.GetScanMetadataRequest.page_token",
	"nopacs.v1.GetScanMetadataResponse.next_page_token",
	"nopacs.v1.GetScanMetadataResponse.manifest_sha256",
	"nopacs.v1.GetInstanceMetadataRequest.tenant",
	"nopacs.v1.GetInstanceMetadataRequest.subject_label",
	"nopacs.v1.GetInstanceMetadataRequest.session_label",
	"nopacs.v1.GetInstanceMetadataRequest.scan_label",
	"nopacs.v1.GetInstanceMetadataRequest.instance_id",

	// data.proto
	"nopacs.v1.ScanMarker.subject_label",
	"nopacs.v1.ScanMarker.session_label",
	"nopacs.v1.ScanMarker.scan_label",
	"nopacs.v1.ScanMarker.manifest_sha256",
	"nopacs.v1.ArchivedFile.instance_id",
	"nopacs.v1.ReadPosition.session_label",
	"nopacs.v1.ReadPosition.scan_label",
	"nopacs.v1.ReadPosition.instance_id",
	"nopacs.v1.ReadFilesRequest.instance_id",
	"nopacs.v1.ArchivedFile.key",
	"nopacs.v1.ArchivedFile.sha256",
	"nopacs.v1.ArchivedFile.sop_instance_uid",
	"nopacs.v1.ArchivedFile.subject_label",
	"nopacs.v1.ArchivedFile.session_label",
	"nopacs.v1.ArchivedFile.scan_label",
	"nopacs.v1.FileChunk.data",
	"nopacs.v1.ReadFilesRequest.tenant",
	"nopacs.v1.ReadFilesRequest.subject_label",
	"nopacs.v1.ReadFilesRequest.session_label",
	"nopacs.v1.ReadFilesRequest.scan_label",

	// profile.proto: a profile is configuration a person writes; no field
	// carries a secret or a value read from a file.
	"nopacs.v1.ProfileVersionRef.name",
	"nopacs.v1.ProfileDocument.bases",
	"nopacs.v1.ProfileDocument.description",
	"nopacs.v1.ProfileRule.name",
	"nopacs.v1.ProfileRule.reason",
	"nopacs.v1.ProfileSelector.tag",
	"nopacs.v1.ProfileSelector.keyword",
	"nopacs.v1.ProfileSelector.group",
	"nopacs.v1.ProfileSelector.vr",
	"nopacs.v1.ProfileSelector.path",
	"nopacs.v1.CodeRequest.value",
	"nopacs.v1.LookupEntry.value",
	"nopacs.v1.CodeRequest.tenant",
	"nopacs.v1.CodeRequest.attribute",
	"nopacs.v1.CodeResponse.attribute",
	"nopacs.v1.CodeResponse.code",
	"nopacs.v1.SubjectLabelCode.receiver",
	"nopacs.v1.SubjectLabelCode.subject_label",
	"nopacs.v1.LookupTableCode.table",
	"nopacs.v1.LookupTableCode.code",
	"nopacs.v1.ImportLookupCodesRequest.tenant",
	"nopacs.v1.ImportLookupCodesRequest.table",
	"nopacs.v1.LookupEntry.code",
	"nopacs.v1.ImportLookupCodesResponse.table",
	"nopacs.v1.ListLookupTablesRequest.tenant",
	"nopacs.v1.ListLookupTablesRequest.page_token",
	"nopacs.v1.ListLookupTablesResponse.next_page_token",
	"nopacs.v1.LookupTable.name",
	"nopacs.v1.DescribeProfileRequest.tenant",
	"nopacs.v1.DescribeProfileResponse.checksum",
	"nopacs.v1.DescribeProfileResponse.notes",
	"nopacs.v1.ProfileClassEffect.action",
	"nopacs.v1.ProfileClassEffect.elements",
	"nopacs.v1.ProfileMethod.withheld",
	"nopacs.v1.ProfileMethod.method_values",
	"nopacs.v1.ProfileMethod.identity_removed",
	"nopacs.v1.ProfileMethod.temporal_modified",
	"nopacs.v1.MethodCode.value",
	"nopacs.v1.MethodCode.scheme",
	"nopacs.v1.MethodCode.meaning",
	"nopacs.v1.DeidJob.profile",
	"nopacs.v1.DeidJob.subject_label",
	"nopacs.v1.DeidJob.session_label",
	"nopacs.v1.DeidJob.scan_label",
	"nopacs.v1.JobTally.key",
	"nopacs.v1.ValuePatternSelector.pattern",
	"nopacs.v1.ValuePatternSelector.vr",
	"nopacs.v1.RedactAction.pattern",
	"nopacs.v1.RedactAction.replacement",
	"nopacs.v1.PrivateElement.creator",
	"nopacs.v1.PrivateElement.group",
	"nopacs.v1.PrivateElement.element",
	"nopacs.v1.ContentItemSelector.scheme",
	"nopacs.v1.ContentItemSelector.value",
	"nopacs.v1.ContentItemSelector.value_type",
	"nopacs.v1.ProfileCondition.equals",
	"nopacs.v1.ProfileCondition.prefix",
	"nopacs.v1.ProfileCondition.pattern",
	"nopacs.v1.ReplaceAction.constant",
	"nopacs.v1.ReplaceAction.template",
	"nopacs.v1.SetAction.constant",
	"nopacs.v1.SetAction.template",
	"nopacs.v1.LookupAction.table",
	"nopacs.v1.PixelRule.name",
	"nopacs.v1.PixelRule.reason",
	"nopacs.v1.ExpandedProfile.bases",
	"nopacs.v1.ExpandedRule.origin",
	"nopacs.v1.ExpandedRule.vrs",
	"nopacs.v1.ExpandedPixelRule.origin",
	"nopacs.v1.ProfileVersion.name",
	"nopacs.v1.ProfileVersion.checksum",
	"nopacs.v1.ProfileVersion.created_by_key_id",
	"nopacs.v1.ProfileVersion.note",
	"nopacs.v1.ProfileSummary.name",
	"nopacs.v1.ProfileProblem.rule_name",
	"nopacs.v1.ProfileProblem.field",
	"nopacs.v1.ProfileProblem.code",
	"nopacs.v1.ProfileProblem.message",
	"nopacs.v1.ProfileBase.name",
	"nopacs.v1.ProfileBase.unavailable_reason",
	"nopacs.v1.ProfileBase.description",
	"nopacs.v1.CreateProfileVersionRequest.tenant",
	"nopacs.v1.CreateProfileVersionRequest.name",
	"nopacs.v1.CreateProfileVersionRequest.note",
	"nopacs.v1.GetProfileVersionRequest.tenant",
	"nopacs.v1.GetProfileVersionRequest.name",
	"nopacs.v1.ListProfilesRequest.tenant",
	"nopacs.v1.ListProfilesRequest.page_token",
	"nopacs.v1.ListProfilesResponse.next_page_token",
	"nopacs.v1.ListProfileVersionsRequest.tenant",
	"nopacs.v1.ListProfileVersionsRequest.name",
	"nopacs.v1.ListProfileVersionsRequest.page_token",
	"nopacs.v1.ListProfileVersionsResponse.next_page_token",
	"nopacs.v1.ValidateProfileRequest.tenant",
	"nopacs.v1.ValidateProfileResponse.checksum",

	// profile.proto, trial runs and explanations: a file a caller sends,
	// its name, and the values before and after are the caller's own data,
	// listed in patientDataFields too; the rest is a profile's words and
	// the server's.
	"nopacs.v1.ProfileVersionName.name",
	"nopacs.v1.TrialFile.name",
	"nopacs.v1.TrialFile.content",
	"nopacs.v1.TrialReception.receiver",
	"nopacs.v1.TrialReception.calling_ae_title",
	"nopacs.v1.TrialReception.called_ae_title",
	"nopacs.v1.TryProfileRequest.tenant",
	"nopacs.v1.TryProfileRequest.quarantine_records",
	"nopacs.v1.TryProfileResponse.checksum",
	"nopacs.v1.TryProfileResponse.reception_refusal",
	"nopacs.v1.TrialResult.name",
	"nopacs.v1.TrialResult.quarantine_record",
	"nopacs.v1.TrialResult.reason",
	"nopacs.v1.TrialResult.cause",
	"nopacs.v1.TrialResult.path",
	"nopacs.v1.TrialResult.rule",
	"nopacs.v1.TrialResult.receiver",
	"nopacs.v1.TrialElement.path",
	"nopacs.v1.TrialElement.keyword",
	"nopacs.v1.TrialElement.vr",
	"nopacs.v1.TrialElement.action",
	"nopacs.v1.TrialElement.rule_name",
	"nopacs.v1.TrialElement.origin",
	"nopacs.v1.TrialElement.reason",
	"nopacs.v1.TrialElement.before",
	"nopacs.v1.TrialElement.after",
	// A file's pixel work: fixed words (risk cause, action, JPEG marker
	// names) and a profile's rule and base names, never a file's value.
	"nopacs.v1.TrialPixels.risk_cause",
	"nopacs.v1.TrialPixels.rule",
	"nopacs.v1.TrialPixels.origin",
	"nopacs.v1.TrialPixels.action",
	"nopacs.v1.TrialPixels.segments_removed",
	"nopacs.v1.TrialCount.key",
	"nopacs.v1.ExplainProfileRequest.tenant",
	"nopacs.v1.ExplainProfileRequest.quarantine_record",
	"nopacs.v1.ExplainProfileResponse.checksum",
	"nopacs.v1.ExplainProfileResponse.refusal",
	"nopacs.v1.ExplainProfileResponse.refusal_path",
	"nopacs.v1.ExplainedElement.path",
	"nopacs.v1.ExplainedElement.keyword",
	"nopacs.v1.ExplainedElement.vr",
	"nopacs.v1.ExplainedElement.action",
	"nopacs.v1.ExplainedElement.rule_name",
	"nopacs.v1.ExplainedElement.origin",
	"nopacs.v1.ExplainedElement.reason",
	"nopacs.v1.ExplainedElement.selected_by",
	"nopacs.v1.ExplainedCandidate.rule_name",
	"nopacs.v1.ExplainedCandidate.origin",
	"nopacs.v1.ExplainedCandidate.selected_by",
	"nopacs.v1.ExplainedCandidate.why_not",
	"nopacs.v1.DiffProfileVersionsRequest.tenant",
	"nopacs.v1.DiffProfileVersionsRequest.name",
	"nopacs.v1.DiffProfileVersionsRequest.to_name",
	"nopacs.v1.DiffProfileVersionsResponse.bases_added",
	"nopacs.v1.DiffProfileVersionsResponse.bases_removed",
	"nopacs.v1.ProfileRuleChange.origin",

	// sender.proto: a sender's configuration is what a person writes; the
	// credential is the one secret, listed in secretFields too.
	"nopacs.v1.Sender.name",
	"nopacs.v1.DimseSender.host",
	"nopacs.v1.DimseSender.called_ae_title",
	"nopacs.v1.DimseSender.calling_ae_title",
	"nopacs.v1.StowSender.url",
	"nopacs.v1.StowSender.username",
	"nopacs.v1.StowSender.header_name",
	"nopacs.v1.StowSender.credential",
	"nopacs.v1.SenderTls.ca_file",
	"nopacs.v1.SenderTls.certificate_file",
	"nopacs.v1.SenderTls.private_key_file",
	"nopacs.v1.SenderTls.server_name",
	"nopacs.v1.SenderHealth.detail",
	"nopacs.v1.SenderTestResult.detail",
	"nopacs.v1.AddSenderRequest.tenant",
	"nopacs.v1.GetSenderRequest.tenant",
	"nopacs.v1.GetSenderRequest.name",
	"nopacs.v1.ListSendersRequest.tenant",
	"nopacs.v1.ListSendersRequest.page_token",
	"nopacs.v1.ListSendersResponse.next_page_token",
	"nopacs.v1.SetSenderRequest.tenant",
	"nopacs.v1.RemoveSenderRequest.tenant",
	"nopacs.v1.RemoveSenderRequest.name",
	"nopacs.v1.TestSenderRequest.tenant",
	"nopacs.v1.TestSenderRequest.name",

	// job.proto: ids, labels, fixed phrases and the UIDs of a SOP class and
	// a transfer syntax, which name kinds of object, not a person.
	"nopacs.v1.SendJob.sender",
	"nopacs.v1.SendJob.subject_label",
	"nopacs.v1.SendJob.session_label",
	"nopacs.v1.SendJob.scan_label",
	"nopacs.v1.SendJob.reception",
	"nopacs.v1.SendJob.receiver",
	"nopacs.v1.Job.id",
	"nopacs.v1.Job.reason",
	"nopacs.v1.Job.created_by_key_id",
	"nopacs.v1.Job.retry_of",
	"nopacs.v1.Job.retry_job",
	"nopacs.v1.JobFailure.instance_id",
	"nopacs.v1.JobFailure.subject_label",
	"nopacs.v1.JobFailure.session_label",
	"nopacs.v1.JobFailure.scan_label",
	"nopacs.v1.JobFailure.sop_class_uid",
	"nopacs.v1.JobFailure.transfer_syntax_uid",
	"nopacs.v1.JobFailure.detail",
	"nopacs.v1.CreateJobRequest.tenant",
	"nopacs.v1.GetJobRequest.tenant",
	"nopacs.v1.GetJobRequest.id",
	"nopacs.v1.ListJobsRequest.tenant",
	"nopacs.v1.ListJobsRequest.sender",
	"nopacs.v1.ListJobsRequest.page_token",
	"nopacs.v1.ListJobsResponse.next_page_token",
	"nopacs.v1.CancelJobRequest.tenant",
	"nopacs.v1.CancelJobRequest.id",
	"nopacs.v1.RetryJobRequest.tenant",
	"nopacs.v1.RetryJobRequest.id",
	"nopacs.v1.ListJobFailuresRequest.tenant",
	"nopacs.v1.ListJobFailuresRequest.id",
	"nopacs.v1.ListJobFailuresRequest.page_token",
	"nopacs.v1.ListJobFailuresResponse.next_page_token",

	// receiver.proto: the senders a receiver forwards to, by name.
	"nopacs.v1.Receiver.forward_to",

	// receiver.proto and quarantine.proto: a receiver's routes name a
	// route, a storage and senders; a release names a storage. The
	// conditions' texts are ProfileCondition's, listed with profile.proto.
	"nopacs.v1.Route.name",
	"nopacs.v1.Route.storage",
	"nopacs.v1.RouteForwardTo.senders",
	"nopacs.v1.ReleaseQuarantinedFileRequest.storage",
	"nopacs.v1.QuarantinedFile.release_storage",
	"nopacs.v1.TrialResult.route",

	// monitor.proto: the events and status of jobs and senders carry ids,
	// names and fixed phrases.
	"nopacs.v1.SenderStatus.sender",
	"nopacs.v1.JobStepReached.job",
	"nopacs.v1.JobStepReached.sender",
	"nopacs.v1.JobStepReached.receiver",
	"nopacs.v1.JobStepReached.reception",
	"nopacs.v1.JobStepReached.retry_of",
	"nopacs.v1.JobStepReached.reason",
	"nopacs.v1.JobProgressed.job",
	"nopacs.v1.JobProgressed.sender",
	"nopacs.v1.SenderHealthChanged.sender",

	// job.proto, scrub and rebuild jobs: a storage's name and a schema's
	// name, written by a person.
	"nopacs.v1.ScrubJob.storage",
	"nopacs.v1.RebuildJob.scratch_schema",

	// correction.proto: labels, ids, keys, lowercase hexadecimal checksums,
	// names, fixed phrases and a person's reason, which the contract says
	// never holds a value read from a file. An instance id and a key hold
	// a SOP Instance UID as received, as they do in data.proto.
	"nopacs.v1.CorrectionTarget.subject_label",
	"nopacs.v1.CorrectionTarget.session_label",
	"nopacs.v1.CorrectionTarget.scan_label",
	"nopacs.v1.CorrectionTarget.instance_id",
	"nopacs.v1.PeerCopy.sender",
	"nopacs.v1.DeletePreview.storages",
	"nopacs.v1.DeletePreview.jobs",
	"nopacs.v1.DeletePreview.confirm",
	"nopacs.v1.DeletePreview.held_after_purge",
	"nopacs.v1.HeldValue.value",
	"nopacs.v1.Correction.id",
	"nopacs.v1.Correction.to_label",
	"nopacs.v1.Correction.quarantined_file",
	"nopacs.v1.Correction.reason",
	"nopacs.v1.Correction.created_by_key_id",
	"nopacs.v1.Correction.state_reason",
	"nopacs.v1.Correction.old_sha256",
	"nopacs.v1.Correction.new_sha256",
	"nopacs.v1.Correction.undo_reason",
	"nopacs.v1.DeleteRequest.tenant",
	"nopacs.v1.DeleteRequest.reason",
	"nopacs.v1.DeleteRequest.confirm",
	"nopacs.v1.RelabelRequest.tenant",
	"nopacs.v1.RelabelRequest.to_label",
	"nopacs.v1.RelabelRequest.reason",
	"nopacs.v1.ReplaceRequest.tenant",
	"nopacs.v1.ReplaceRequest.quarantined_file",
	"nopacs.v1.ReplaceRequest.reason",
	"nopacs.v1.ListCorrectionsRequest.tenant",
	"nopacs.v1.ListCorrectionsRequest.page_token",
	"nopacs.v1.ListCorrectionsResponse.next_page_token",
	"nopacs.v1.GetCorrectionRequest.tenant",
	"nopacs.v1.GetCorrectionRequest.id",
	"nopacs.v1.UndoCorrectionRequest.tenant",
	"nopacs.v1.UndoCorrectionRequest.id",
	"nopacs.v1.UndoCorrectionRequest.reason",
	"nopacs.v1.PurgeCorrectionRequest.tenant",
	"nopacs.v1.PurgeCorrectionRequest.id",
	"nopacs.v1.AllowArrivalsRequest.tenant",
	"nopacs.v1.AllowArrivalsRequest.id",
	"nopacs.v1.AllowArrivalsRequest.reason",
	"nopacs.v1.ScrubFinding.id",
	"nopacs.v1.ScrubFinding.storage",
	"nopacs.v1.ScrubFinding.key",
	"nopacs.v1.ScrubFinding.subject_label",
	"nopacs.v1.ScrubFinding.session_label",
	"nopacs.v1.ScrubFinding.scan_label",
	"nopacs.v1.ScrubFinding.instance_id",
	"nopacs.v1.ScrubFinding.expected_sha256",
	"nopacs.v1.ScrubFinding.found_sha256",
	"nopacs.v1.ScrubFinding.job",
	"nopacs.v1.ScrubFinding.clear_job",
	"nopacs.v1.ListScrubFindingsRequest.tenant",
	"nopacs.v1.ListScrubFindingsRequest.storage",
	"nopacs.v1.ListScrubFindingsRequest.page_token",
	"nopacs.v1.ListScrubFindingsResponse.next_page_token",
	"nopacs.v1.StorageDisposition.storage",
	"nopacs.v1.StoragePurge.storage",
	"nopacs.v1.StoragePurge.why",
	"nopacs.v1.InstanceRef.instance_id",
	"nopacs.v1.ResolvedInstance.sha256",
	"nopacs.v1.ResolvedInstance.correction",
	"nopacs.v1.ResolvedInstance.subject_label",
	"nopacs.v1.ResolvedInstance.session_label",
	"nopacs.v1.ResolvedInstance.scan_label",
	"nopacs.v1.ResolveInstancesRequest.tenant",

	// audit_seal.proto: digests, signatures, public keys and the encoded
	// form of entries that AuditService already answers; nothing here is a
	// secret, since a public key checks a signature and cannot make one.
	"nopacs.v1.AuditCheckpoint.tenant",
	"nopacs.v1.AuditCheckpoint.head",
	"nopacs.v1.AuditCheckpoint.previous_digest",
	"nopacs.v1.AuditCheckpoint.digest",
	"nopacs.v1.AuditCheckpoint.signature",
	"nopacs.v1.AuditCheckpoint.signed_text",
	"nopacs.v1.AuditPublicKey.public_key",
	"nopacs.v1.AuditPublicKey.public_key_pem",
	"nopacs.v1.AuditVerifyProblem.detail",
	"nopacs.v1.ListAuditCheckpointsRequest.page_token",
	"nopacs.v1.ListAuditCheckpointsResponse.next_page_token",
	"nopacs.v1.AuditExportHeader.installation_id",
	"nopacs.v1.AuditExportHeader.tenant",
	"nopacs.v1.AuditExportHeader.chain_before",
	"nopacs.v1.SealedAuditEntry.encoded",
	"nopacs.v1.SealedAuditEntry.chain",
	"nopacs.v1.GetAuditPublicKeyResponse.installation_id",
	"nopacs.v1.AuditSealStatus.last_digest",
	"nopacs.v1.VerifyAuditRequest.expected_checkpoint_digest",
	"nopacs.v1.AuditCheckpoint.installation_id",
	"nopacs.v1.AuditCheckpoint.log",
	"nopacs.v1.AuditPublicKey.installation_id",
	"nopacs.v1.AuditAnchor.log",
	"nopacs.v1.AuditAnchor.digest",
	"nopacs.v1.AuditExportHeader.log",

	// backup.proto: the encrypted bytes of a backup or an export (useless
	// without the installation key file or the transfer key), names,
	// formats and checksums. The transfer key is the one secret, listed in
	// secretFields too.
	"nopacs.v1.BackupSection.tenant",
	"nopacs.v1.BackupSection.sha256",
	"nopacs.v1.BackupSummary.format",
	"nopacs.v1.BackupSummary.installation_id",
	"nopacs.v1.BackupSummary.sha256",
	"nopacs.v1.CreateBackupResponse.data",
	"nopacs.v1.ExportTenantRequest.tenant",
	"nopacs.v1.TenantExportSummary.transfer_key",
	"nopacs.v1.ExportTenantResponse.data",
	"nopacs.v1.StorageOwner.storage",
	"nopacs.v1.BackupSection.event_history",
	"nopacs.v1.BackupSection.last_correction",
	"nopacs.v1.BackupSection.last_reception",
	"nopacs.v1.ReclaimTenantRequest.name",
	"nopacs.v1.ReinstateKeysRequest.key_ids",

	// monitor.proto: a tenant's scrub and seal status and the events of
	// scrubs and corrections carry ids, names, labels and digests.
	"nopacs.v1.ScrubSummary.job",
	"nopacs.v1.ScrubFindingChanged.finding",
	"nopacs.v1.ScrubFindingChanged.storage",
	"nopacs.v1.ScrubFindingChanged.subject_label",
	"nopacs.v1.ScrubFindingChanged.session_label",
	"nopacs.v1.ScrubFindingChanged.scan_label",
	"nopacs.v1.ScrubFindingChanged.job",
	"nopacs.v1.CatalogChanged.correction",
	"nopacs.v1.CatalogChanged.subject_label",
	"nopacs.v1.CatalogChanged.session_label",
	"nopacs.v1.CatalogChanged.scan_label",
	"nopacs.v1.CatalogChanged.to_label",
	"nopacs.v1.CatalogRebuilt.job",
	"nopacs.v1.TenantStatus.event_history",
	"nopacs.v1.WatchKeepAlive.event_history",
	"nopacs.v1.WatchKeepAlive.tenant_event_histories",

	// receiver.proto, job.proto and quarantine.proto, importing a
	// directory: names, ids, a pattern a person writes and fixed phrases;
	// and paths under an import root, which can hold a patient's name and
	// are listed in patientDataFields too.
	"nopacs.v1.ImportReceiver.root",
	"nopacs.v1.ImportReceiver.path_pattern",
	"nopacs.v1.ImportReceiver.map_file",
	"nopacs.v1.ImportReceiver.map_key_columns",
	"nopacs.v1.ImportReceiver.map_value_column",
	"nopacs.v1.ImportReceiver.map_key_parts",
	"nopacs.v1.ImportReceiver.map_gives",
	"nopacs.v1.ImportJob.map_key_columns",
	"nopacs.v1.ImportJob.map_value_column",
	"nopacs.v1.ImportJob.map_key_parts",
	"nopacs.v1.ImportJob.map_gives",
	"nopacs.v1.ImportJob.receiver",
	"nopacs.v1.ImportJob.path",
	"nopacs.v1.ImportJob.map_file",
	"nopacs.v1.ImportJob.map_csv",
	"nopacs.v1.ImportFile.path",
	"nopacs.v1.ImportFile.quarantined_file",
	"nopacs.v1.ImportFile.reception",
	"nopacs.v1.ImportFile.unit",
	"nopacs.v1.ImportFile.detail",
	"nopacs.v1.ImportUnit.id",
	"nopacs.v1.ImportUnit.folder",
	"nopacs.v1.ImportUnit.reason",
	"nopacs.v1.ListImportFilesRequest.tenant",
	"nopacs.v1.ListImportFilesRequest.id",
	"nopacs.v1.ListImportFilesRequest.unit",
	"nopacs.v1.ListImportFilesRequest.page_token",
	"nopacs.v1.ListImportFilesResponse.next_page_token",
	"nopacs.v1.ListImportUnitsRequest.tenant",
	"nopacs.v1.ListImportUnitsRequest.id",
	"nopacs.v1.ListImportUnitsRequest.page_token",
	"nopacs.v1.ListImportUnitsResponse.next_page_token",
	"nopacs.v1.QuarantinedFile.import_job",
	"nopacs.v1.QuarantinedFile.import_path",

	// storage.proto, replica.proto, job.proto and monitor.proto, replicas
	// and recalls: names of storages and receivers, ids, labels, fixed
	// phrases and lowercase hexadecimal checksums. A sampled object's key
	// and instance id hold a SOP Instance UID as received, as they do in
	// data.proto.
	"nopacs.v1.Storage.replica_of",
	"nopacs.v1.ReplicaStatus.replica",
	"nopacs.v1.ReplicaStatus.primary",
	"nopacs.v1.ReplicaStatus.backfill_job",
	"nopacs.v1.ListReplicasRequest.tenant",
	"nopacs.v1.ListReplicasRequest.primary",
	"nopacs.v1.ListReplicasRequest.page_token",
	"nopacs.v1.ListReplicasResponse.next_page_token",
	"nopacs.v1.AttachReplicaRequest.tenant",
	"nopacs.v1.AttachReplicaRequest.replica",
	"nopacs.v1.AttachReplicaRequest.primary",
	"nopacs.v1.DetachReplicaRequest.tenant",
	"nopacs.v1.DetachReplicaRequest.replica",
	"nopacs.v1.BackfillReplicaRequest.tenant",
	"nopacs.v1.BackfillReplicaRequest.replica",
	"nopacs.v1.RepairRequest.tenant",
	"nopacs.v1.RepairRequest.storage",
	"nopacs.v1.RepairRequest.from",
	"nopacs.v1.RepairRequest.findings",
	"nopacs.v1.RecallRequest.tenant",
	"nopacs.v1.RecallRequest.subject_label",
	"nopacs.v1.RecallRequest.session_label",
	"nopacs.v1.RecallRequest.scan_label",
	"nopacs.v1.RecallRequest.storage",
	"nopacs.v1.RecallResponse.storages",
	"nopacs.v1.SwitchStorageRequest.tenant",
	"nopacs.v1.SwitchStorageRequest.primary",
	"nopacs.v1.SwitchStorageRequest.replica",
	"nopacs.v1.SwitchStorageResponse.blockers",
	"nopacs.v1.SwitchStorageResponse.receivers",
	"nopacs.v1.VerifySampleRequest.tenant",
	"nopacs.v1.VerifySampleRequest.storage",
	"nopacs.v1.SampledObject.key",
	"nopacs.v1.SampledObject.sha256",
	"nopacs.v1.SampledObject.instance_id",
	"nopacs.v1.SampledObject.subject_label",
	"nopacs.v1.SampledObject.session_label",
	"nopacs.v1.SampledObject.scan_label",
	"nopacs.v1.RebuildJob.from_storage",
	"nopacs.v1.ReplicateJob.replica",
	"nopacs.v1.ReplicateJob.primary",
	"nopacs.v1.RepairJob.storage",
	"nopacs.v1.RepairJob.from",
	"nopacs.v1.RepairJob.findings",
	"nopacs.v1.StorageStatus.replica_of",
	"nopacs.v1.ReplicaBehind.replica",
	"nopacs.v1.ReplicaBehind.primary",
	"nopacs.v1.StorageSwitched.primary",
	"nopacs.v1.StorageSwitched.replica",

	// correction.proto, every copy and retention: storage, rule and hold
	// names and ids, and a person's reason for an override, which the
	// contract says never holds a value read from a file.
	"nopacs.v1.StorageDisposition.replica_of",
	"nopacs.v1.StoragePurge.replica_of",
	"nopacs.v1.DeletePreview.holds",
	"nopacs.v1.Correction.retention_rule",
	"nopacs.v1.Correction.override_reason",
	"nopacs.v1.DeleteRequest.override_reason",
	"nopacs.v1.ScrubFinding.replica_of",
	"nopacs.v1.ScrubFinding.repair_job",

	// retention.proto and monitor.proto, retention and holds: names of
	// rules, receivers, routes and storages, labels, ids, fixed phrases, a
	// confirm token (a digest) and a person's reasons, which the contract
	// says never hold a value read from a file.
	"nopacs.v1.RetentionSelector.receiver",
	"nopacs.v1.RetentionSelector.route",
	"nopacs.v1.RetentionSelector.storage",
	"nopacs.v1.RetentionRule.name",
	"nopacs.v1.DueSession.subject_label",
	"nopacs.v1.DueSession.session_label",
	"nopacs.v1.DueSession.rule",
	"nopacs.v1.RetentionStatus.pause_reason",
	"nopacs.v1.HoldTarget.subject_label",
	"nopacs.v1.HoldTarget.session_label",
	"nopacs.v1.HoldTarget.scan_label",
	"nopacs.v1.Hold.id",
	"nopacs.v1.Hold.reason",
	"nopacs.v1.Hold.placed_by_key_id",
	"nopacs.v1.Hold.release_reason",
	"nopacs.v1.Hold.released_by_key_id",
	"nopacs.v1.Hold.storages",
	"nopacs.v1.AddRetentionRuleRequest.tenant",
	"nopacs.v1.GetRetentionRuleRequest.tenant",
	"nopacs.v1.GetRetentionRuleRequest.name",
	"nopacs.v1.ListRetentionRulesRequest.tenant",
	"nopacs.v1.ListRetentionRulesRequest.page_token",
	"nopacs.v1.ListRetentionRulesResponse.next_page_token",
	"nopacs.v1.SetRetentionRuleRequest.tenant",
	"nopacs.v1.RemoveRetentionRuleRequest.tenant",
	"nopacs.v1.RemoveRetentionRuleRequest.name",
	"nopacs.v1.ListDueSessionsRequest.tenant",
	"nopacs.v1.ListDueSessionsRequest.rule",
	"nopacs.v1.ListDueSessionsRequest.page_token",
	"nopacs.v1.ListDueSessionsResponse.next_page_token",
	"nopacs.v1.DisposeRequest.tenant",
	"nopacs.v1.DisposeRequest.rule",
	"nopacs.v1.DisposeRequest.reason",
	"nopacs.v1.DisposeRequest.confirm",
	"nopacs.v1.DisposalPreview.rule",
	"nopacs.v1.DisposalPreview.confirm",
	"nopacs.v1.ConfirmClockRequest.reason",
	"nopacs.v1.ConfirmClockResponse.tenants",
	"nopacs.v1.Hold.storages_pending",
	"nopacs.v1.DisposeJob.rule",
	"nopacs.v1.DisposeJob.reason",
	"nopacs.v1.Correction.job",
	"nopacs.v1.Correction.copies_abandoned",
	"nopacs.v1.DetachReplicaRequest.abandon_reason",
	"nopacs.v1.RecallRequest.confirm",
	"nopacs.v1.RecallResponse.confirm",
	"nopacs.v1.SwitchStorageRequest.confirm",
	"nopacs.v1.SwitchStorageRequest.skip_reason",
	"nopacs.v1.SwitchStorageResponse.confirm",
	"nopacs.v1.StorageSwitched.refusal",
	"nopacs.v1.PlaceHoldRequest.tenant",
	"nopacs.v1.PlaceHoldRequest.reason",
	"nopacs.v1.ReleaseHoldRequest.tenant",
	"nopacs.v1.ReleaseHoldRequest.id",
	"nopacs.v1.ReleaseHoldRequest.reason",
	"nopacs.v1.GetHoldRequest.tenant",
	"nopacs.v1.GetHoldRequest.id",
	"nopacs.v1.ListHoldsRequest.tenant",
	"nopacs.v1.ListHoldsRequest.subject_label",
	"nopacs.v1.ListHoldsRequest.page_token",
	"nopacs.v1.ListHoldsResponse.next_page_token",
	"nopacs.v1.HoldChanged.hold",
	"nopacs.v1.HoldChanged.subject_label",
	"nopacs.v1.HoldChanged.session_label",
	"nopacs.v1.HoldChanged.scan_label",
}

// TestOnlyTheNamedFieldsCarrySecrets guards where secrets may cross the
// contract: exactly the named fields (secrets, and a trial run's values) are
// marked debug_redact, and every
// field that can carry text is on the reviewed list. A text field added
// without being listed, a secret field that loses its mark, or another field
// that gains one fails here rather than in a review.
func TestOnlyTheNamedFieldsCarrySecrets(t *testing.T) {
	fields := contractFields(t)

	// A walk that found nothing would pass the checks below for no reason.
	if len(fields) < 50 {
		t.Fatalf("found %d fields in %s, want at least 50: is the generated code registered?", len(fields), contractPackage)
	}

	var redacted, text []string
	for _, field := range fields {
		name := string(field.FullName())
		opts, _ := field.Options().(*descriptorpb.FieldOptions)
		if opts.GetDebugRedact() {
			redacted = append(redacted, name)
		}
		if carriesText(field) {
			text = append(text, name)
			if !slices.Contains(textFields, name) {
				t.Errorf("%s carries text and is not in textFields: list it once it is known to carry no secret", name)
			}
		}
	}

	slices.Sort(redacted)
	marked := slices.Concat(secretFields, patientDataFields)
	slices.Sort(marked)
	if !slices.Equal(redacted, marked) {
		t.Errorf("fields marked debug_redact = %q, want exactly %q", redacted, marked)
	}

	// An entry for a field that no longer exists or no longer carries text
	// would let the list drift from the contract it reviews.
	for _, name := range textFields {
		if !slices.Contains(text, name) {
			t.Errorf("textFields lists %s, which is not a text field of the contract", name)
		}
	}
}

// carriesText reports whether a field can hold free text: a string or bytes
// field, repeated or not, or a map whose keys or values are strings or bytes.
func carriesText(field protoreflect.FieldDescriptor) bool {
	if field.IsMap() {
		return isText(field.MapKey()) || isText(field.MapValue())
	}
	return isText(field)
}

// isText reports whether a field's values are strings or bytes.
func isText(field protoreflect.FieldDescriptor) bool {
	return field.Kind() == protoreflect.StringKind || field.Kind() == protoreflect.BytesKind
}

// contractFields answers every field of every message in the files of
// contractPackage, nested messages included. The entries of a map are
// reached through the map field itself, so their synthetic messages are
// skipped.
func contractFields(t *testing.T) []protoreflect.FieldDescriptor {
	t.Helper()
	var fields []protoreflect.FieldDescriptor
	var walk func(protoreflect.MessageDescriptors)
	walk = func(ms protoreflect.MessageDescriptors) {
		for i := range ms.Len() {
			m := ms.Get(i)
			if m.IsMapEntry() {
				continue
			}
			for j := range m.Fields().Len() {
				fields = append(fields, m.Fields().Get(j))
			}
			walk(m.Messages())
		}
	}
	files := 0
	protoregistry.GlobalFiles.RangeFilesByPackage(contractPackage, func(f protoreflect.FileDescriptor) bool {
		files++
		walk(f.Messages())
		return true
	})
	if files == 0 {
		t.Fatalf("no files of package %s are registered", contractPackage)
	}
	return fields
}
