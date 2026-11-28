package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
)

// everyMethod returns one call of each method of the services, each with a
// request the local operator may make on the fixture.
func everyMethod(f *fixture) map[string]func(clients) outcome {
	m := map[string]func(clients) outcome{
		"CreateKey":        createKey(&v1.CreateKeyRequest{Holder: "a"}),
		"GetKey":           func(c clients) outcome { return call(c.access.GetKey, &v1.GetKeyRequest{Id: f.noGrantID}) },
		"ListKeys":         func(c clients) outcome { return call(c.access.ListKeys, &v1.ListKeysRequest{}) },
		"ListProfileBases": func(c clients) outcome { return call(c.profiles.ListProfileBases, &v1.ListProfileBasesRequest{}) },
		"RevokeKey":        func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: f.noGrantID}) },
		"SetGrant": func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: f.noGrantID, Scope: installation(), Role: v1.Role_ROLE_READER})
		},
		"ListGrants": func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_Scope{Scope: installation()}})
		},
		"RemoveGrant": func(c clients) outcome {
			return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: f.lab1ReaderID, Scope: tenantScope("lab1")})
		},
		"WhoAmI":      func(c clients) outcome { return call(c.access.WhoAmI, &v1.WhoAmIRequest{}) },
		"RotateKey":   func(c clients) outcome { return call(c.access.RotateKey, &v1.RotateKeyRequest{Id: f.noGrantID}) },
		"SetKeyRules": func(c clients) outcome { return call(c.access.SetKeyRules, &v1.SetKeyRulesRequest{Id: f.noGrantID}) },
		"ReinstateKeys": func(c clients) outcome {
			return call(c.access.ReinstateKeys, &v1.ReinstateKeysRequest{KeyIds: []string{f.noGrantID}})
		},
		"CreateTenant": createTenant("lab9", sqliteType, ""),
		"GetTenant":    func(c clients) outcome { return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: "lab1"}) },
		"ListTenants":  func(c clients) outcome { return call(c.tenants.ListTenants, &v1.ListTenantsRequest{}) },
		"RemoveTenant": func(c clients) outcome {
			return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "lab1", ConfirmName: "lab1"})
		},
		"GetSetting": getSetting(testRetention, tenantScope("lab1")),
		"SetSetting": setSetting(testRetention, tenantScope("lab1"), "1d"),
		"UnsetSetting": func(c clients) outcome {
			return call(c.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: testRetention, Scope: tenantScope("lab1")})
		},
		"ListSettings": func(c clients) outcome {
			return call(c.settings.ListSettings, &v1.ListSettingsRequest{Scope: installation()})
		},
		"ListAuditEntries": func(c clients) outcome {
			return call(c.audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: installation()})
		},
		"VerifyAudit": func(c clients) outcome {
			return call(c.auditSeal.VerifyAudit, &v1.VerifyAuditRequest{Scope: installation()})
		},
		"ListAuditCheckpoints": func(c clients) outcome {
			return call(c.auditSeal.ListAuditCheckpoints, &v1.ListAuditCheckpointsRequest{Scope: installation()})
		},
		"ExportAudit": func(c clients) outcome { return exportOnce(c, &v1.ExportAuditRequest{Scope: installation()}) },
		"GetAuditPublicKey": func(c clients) outcome {
			return call(c.auditSeal.GetAuditPublicKey, &v1.GetAuditPublicKeyRequest{})
		},
		"ExportTenant": func(c clients) outcome {
			x := export(c, "lab1")
			return outcome{err: x.err, requestID: x.requestID}
		},
		"CreateBackup": func(c clients) outcome { return createBackup(c).outcome() },
		"ReclaimTenant": func(c clients) outcome {
			return call(c.tenants.ReclaimTenant, &v1.ReclaimTenantRequest{Name: "lab1"})
		},
	}
	for name, call := range tenantMethods("lab1") {
		m[name] = call
	}
	return m
}

// tenantMethods returns one call of each method of the four services that
// act inside one tenant, into the tenant named name.
func tenantMethods(name string) map[string]func(clients) outcome {
	return map[string]func(clients) outcome{
		"AddStorage": addStorage(name, dirStorage("main", "fast", "mri")),
		"GetStorage": func(c clients) outcome {
			return call(c.storages.GetStorage, &v1.GetStorageRequest{Tenant: name, Name: "main"})
		},
		"ListStorages": func(c clients) outcome {
			return call(c.storages.ListStorages, &v1.ListStoragesRequest{Tenant: name})
		},
		"SetStorage": func(c clients) outcome {
			return call(c.storages.SetStorage, &v1.SetStorageRequest{Tenant: name, Storage: &v1.Storage{Name: "main"}, UpdateMask: mask("capacity_bytes")})
		},
		"RemoveStorage": func(c clients) outcome {
			return call(c.storages.RemoveStorage, &v1.RemoveStorageRequest{Tenant: name, Name: "main"})
		},
		"TestStorage": func(c clients) outcome {
			return call(c.storages.TestStorage, &v1.TestStorageRequest{Tenant: name, Name: "main"})
		},
		"AddReceiver": addReceiver(name, dimseReceiver("scanner1", "main", 11112)),
		"GetReceiver": func(c clients) outcome {
			return call(c.receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: name, Name: "scanner1"})
		},
		"ListReceivers": func(c clients) outcome {
			return call(c.receivers.ListReceivers, &v1.ListReceiversRequest{Tenant: name})
		},
		"SetReceiver": func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: name, Receiver: &v1.Receiver{Name: "scanner1", Enabled: true}, UpdateMask: mask("enabled")})
		},
		"RemoveReceiver": func(c clients) outcome {
			return call(c.receivers.RemoveReceiver, &v1.RemoveReceiverRequest{Tenant: name, Name: "scanner1"})
		},
		"ListReceptions": func(c clients) outcome {
			return call(c.receptions.ListReceptions, &v1.ListReceptionsRequest{Tenant: name})
		},
		"GetReception": func(c clients) outcome {
			return call(c.receptions.GetReception, &v1.GetReceptionRequest{Tenant: name, Id: "r1"})
		},
		"RetryReception": func(c clients) outcome {
			return call(c.receptions.RetryReception, &v1.RetryReceptionRequest{Tenant: name, Id: "r1"})
		},
		"DiscardReception": func(c clients) outcome {
			return call(c.receptions.DiscardReception, &v1.DiscardReceptionRequest{Tenant: name, Id: "r1", Reason: "x"})
		},
		"GetScanMetadata": func(c clients) outcome {
			return call(c.metadata.GetScanMetadata, &v1.GetScanMetadataRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "T1"})
		},
		"GetInstanceMetadata": func(c clients) outcome {
			return call(c.metadata.GetInstanceMetadata, &v1.GetInstanceMetadataRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "T1", InstanceId: "2.25.1"})
		},
		"ReadFiles": func(c clients) outcome {
			return readFilesOnce(c, &v1.ReadFilesRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "T1"})
		},
		"ListSubjects": func(c clients) outcome { return call(c.catalog.ListSubjects, &v1.ListSubjectsRequest{Tenant: name}) },
		"GetSubject": func(c clients) outcome {
			return call(c.catalog.GetSubject, &v1.GetSubjectRequest{Tenant: name, SubjectLabel: "S1"})
		},
		"ListSessions": func(c clients) outcome { return call(c.catalog.ListSessions, &v1.ListSessionsRequest{Tenant: name}) },
		"GetSession": func(c clients) outcome {
			return call(c.catalog.GetSession, &v1.GetSessionRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1"})
		},
		"ListScans": func(c clients) outcome { return call(c.catalog.ListScans, &v1.ListScansRequest{Tenant: name}) },
		"GetScan": func(c clients) outcome {
			return call(c.catalog.GetScan, &v1.GetScanRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "T1"})
		},
		"ListQuarantinedFiles": func(c clients) outcome {
			return call(c.quarantine.ListQuarantinedFiles, &v1.ListQuarantinedFilesRequest{Tenant: name})
		},
		"GetQuarantinedFile": func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: name, Id: "q1"})
		},
		"ReleaseQuarantinedFile": func(c clients) outcome {
			return call(c.quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: name, Id: "q1", Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS})
		},
		"RejectQuarantinedFile": func(c clients) outcome {
			return call(c.quarantine.RejectQuarantinedFile, &v1.RejectQuarantinedFileRequest{Tenant: name, Id: "q1", Reason: "x"})
		},
		"GetStatus": func(c clients) outcome {
			return call(c.monitor.GetStatus, &v1.GetStatusRequest{Scope: tenantScope(name)})
		},
		"AddSender": func(c clients) outcome {
			return call(c.senders.AddSender, &v1.AddSenderRequest{Tenant: name, Sender: dimseTo("pacs", "pacs.lab.example", 104)})
		},
		"GetSender": func(c clients) outcome {
			return call(c.senders.GetSender, &v1.GetSenderRequest{Tenant: name, Name: "pacs"})
		},
		"ListSenders": func(c clients) outcome { return call(c.senders.ListSenders, &v1.ListSendersRequest{Tenant: name}) },
		"SetSender": func(c clients) outcome {
			return call(c.senders.SetSender, &v1.SetSenderRequest{Tenant: name, Sender: &v1.Sender{Name: "pacs"}, UpdateMask: mask("enabled")})
		},
		"RemoveSender": func(c clients) outcome {
			return call(c.senders.RemoveSender, &v1.RemoveSenderRequest{Tenant: name, Name: "pacs"})
		},
		"TestSender": func(c clients) outcome {
			return call(c.senders.TestSender, &v1.TestSenderRequest{Tenant: name, Name: "pacs"})
		},
		"CreateJob": func(c clients) outcome {
			return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: name, Kind: &v1.CreateJobRequest_Send{Send: target("pacs", "S1")}})
		},
		"GetJob":    func(c clients) outcome { return call(c.jobs.GetJob, &v1.GetJobRequest{Tenant: name, Id: "j1"}) },
		"ListJobs":  func(c clients) outcome { return call(c.jobs.ListJobs, &v1.ListJobsRequest{Tenant: name}) },
		"CancelJob": func(c clients) outcome { return call(c.jobs.CancelJob, &v1.CancelJobRequest{Tenant: name, Id: "j1"}) },
		"PauseJob":  func(c clients) outcome { return call(c.jobs.PauseJob, &v1.PauseJobRequest{Tenant: name, Id: "j1"}) },
		"ResumeJob": func(c clients) outcome { return call(c.jobs.ResumeJob, &v1.ResumeJobRequest{Tenant: name, Id: "j1"}) },
		"RetryJob":  func(c clients) outcome { return call(c.jobs.RetryJob, &v1.RetryJobRequest{Tenant: name, Id: "j1"}) },
		"ListImportFiles": func(c clients) outcome {
			return call(c.jobs.ListImportFiles, &v1.ListImportFilesRequest{Tenant: name, Id: "j1"})
		},
		"ListImportUnits": func(c clients) outcome {
			return call(c.jobs.ListImportUnits, &v1.ListImportUnitsRequest{Tenant: name, Id: "j1"})
		},
		"ListJobFailures": func(c clients) outcome {
			return call(c.jobs.ListJobFailures, &v1.ListJobFailuresRequest{Tenant: name, Id: "j1"})
		},
		"Watch": func(c clients) outcome { return watchOnce(c, &v1.WatchRequest{Scope: tenantScope(name)}) },
		"CreateProfileVersion": func(c clients) outcome {
			return call(c.profiles.CreateProfileVersion, &v1.CreateProfileVersionRequest{Tenant: name, Name: "research", Document: &v1.ProfileDocument{}})
		},
		"GetProfileVersion": func(c clients) outcome {
			return call(c.profiles.GetProfileVersion, &v1.GetProfileVersionRequest{Tenant: name, Name: "research"})
		},
		"ListProfiles": func(c clients) outcome { return call(c.profiles.ListProfiles, &v1.ListProfilesRequest{Tenant: name}) },
		"ListProfileVersions": func(c clients) outcome {
			return call(c.profiles.ListProfileVersions, &v1.ListProfileVersionsRequest{Tenant: name, Name: "research"})
		},
		"ValidateProfile": func(c clients) outcome {
			return call(c.profiles.ValidateProfile, &v1.ValidateProfileRequest{Tenant: name, Document: &v1.ProfileDocument{}})
		},
		"TryProfile": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: name, Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: &v1.ProfileDocument{}}}})
		},
		"ExplainProfile": func(c clients) outcome {
			return call(c.profiles.ExplainProfile, &v1.ExplainProfileRequest{Tenant: name, Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: &v1.ProfileDocument{}}}})
		},
		"DiffProfileVersions": func(c clients) outcome {
			return call(c.profiles.DiffProfileVersions, &v1.DiffProfileVersionsRequest{Tenant: name, Name: "research"})
		},
		"DescribeProfile": func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: name, Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: &v1.ProfileDocument{}}}})
		},
		"Code": func(c clients) outcome {
			return call(c.deid.Code, &v1.CodeRequest{Tenant: name, Value: "ID-1"})
		},
		"ImportLookupCodes": func(c clients) outcome {
			return call(c.deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: name, Table: "t", Entries: []*v1.LookupEntry{{Value: "a", Code: "A"}}})
		},
		"ListLookupTables": func(c clients) outcome {
			return call(c.deid.ListLookupTables, &v1.ListLookupTablesRequest{Tenant: name})
		},
		"Delete": func(c clients) outcome {
			return call(c.corrections.Delete, &v1.DeleteRequest{Tenant: name, Target: &v1.CorrectionTarget{SubjectLabel: "S1"}, Reason: "x", ValidateOnly: true})
		},
		"DeleteBatch": func(c clients) outcome {
			return call(c.corrections.DeleteBatch, &v1.DeleteBatchRequest{Tenant: name, Targets: []*v1.CorrectionTarget{{SubjectLabel: "S1"}}, Reason: "x", ValidateOnly: true})
		},
		"CreateTransfer": func(c clients) outcome {
			return call(c.transfers.CreateTransfer, &v1.CreateTransferRequest{Tenant: name, SubjectLabel: "S1", TargetTenant: "lab2", Reason: "x", ValidateOnly: true})
		},
		"GetTransfer": func(c clients) outcome {
			return call(c.transfers.GetTransfer, &v1.GetTransferRequest{Tenant: name, Id: "j1"})
		},
		"ListTransfers": func(c clients) outcome {
			return call(c.transfers.ListTransfers, &v1.ListTransfersRequest{Tenant: name})
		},
		"ListDeleteBatchTargets": func(c clients) outcome {
			return call(c.corrections.ListDeleteBatchTargets, &v1.ListDeleteBatchTargetsRequest{Tenant: name, JobId: "j1"})
		},
		"Relabel": func(c clients) outcome {
			return call(c.corrections.Relabel, &v1.RelabelRequest{Tenant: name, Target: &v1.CorrectionTarget{SubjectLabel: "S1"}, ToLabel: "S2", Reason: "x"})
		},
		"Replace": func(c clients) outcome {
			return call(c.corrections.Replace, &v1.ReplaceRequest{Tenant: name, QuarantinedFile: "q1", Reason: "x"})
		},
		"Edit": func(c clients) outcome {
			return call(c.corrections.Edit, &v1.EditRequest{Tenant: name, Target: &v1.CorrectionTarget{SubjectLabel: "S1"},
				Changes: []*v1.EditChange{{Attribute: "SeriesDescription", Value: "x"}}, Reason: "x", ValidateOnly: true})
		},
		"Move": func(c clients) outcome {
			return call(c.corrections.Move, &v1.MoveRequest{Tenant: name, SubjectLabel: "S1", SessionLabel: "E1", ToSubjectLabel: "S2", Reason: "x",
				ValidateOnly: true})
		},
		"Merge": func(c clients) outcome {
			return call(c.corrections.Merge, &v1.MergeRequest{Tenant: name, SubjectLabel: "S1", IntoSubjectLabel: "S2", Reason: "x", ValidateOnly: true})
		},
		"ListCorrections": func(c clients) outcome {
			return call(c.corrections.ListCorrections, &v1.ListCorrectionsRequest{Tenant: name})
		},
		"GetCorrection": func(c clients) outcome {
			return call(c.corrections.GetCorrection, &v1.GetCorrectionRequest{Tenant: name, Id: correctionULID})
		},
		"UndoCorrection": func(c clients) outcome {
			return call(c.corrections.UndoCorrection, &v1.UndoCorrectionRequest{Tenant: name, Id: correctionULID, Reason: "x"})
		},
		"PurgeCorrection": func(c clients) outcome {
			return call(c.corrections.PurgeCorrection, &v1.PurgeCorrectionRequest{Tenant: name, Id: correctionULID})
		},
		"AllowArrivals": func(c clients) outcome {
			return call(c.corrections.AllowArrivals, &v1.AllowArrivalsRequest{Tenant: name, Id: correctionULID, Reason: "x",
				Value: &v1.HeldValue{Kind: v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT, Value: "S1"}})
		},
		"ListScrubFindings": func(c clients) outcome {
			return call(c.corrections.ListScrubFindings, &v1.ListScrubFindingsRequest{Tenant: name})
		},
		"ResolveInstances": func(c clients) outcome {
			return call(c.corrections.ResolveInstances, &v1.ResolveInstancesRequest{Tenant: name, Members: []*v1.InstanceRef{{InstanceId: "2.25.1", Revision: 1}}})
		},
		"ListReplicas": func(c clients) outcome {
			return call(c.replicas.ListReplicas, &v1.ListReplicasRequest{Tenant: name})
		},
		"AttachReplica": func(c clients) outcome {
			return call(c.replicas.AttachReplica, &v1.AttachReplicaRequest{Tenant: name, Replica: "copy", Primary: "main"})
		},
		"DetachReplica": func(c clients) outcome {
			return call(c.replicas.DetachReplica, &v1.DetachReplicaRequest{Tenant: name, Replica: "copy"})
		},
		"Repair": func(c clients) outcome {
			return call(c.replicas.Repair, &v1.RepairRequest{Tenant: name, Storage: "main", All: true})
		},
		"SwitchStorage": func(c clients) outcome {
			return call(c.replicas.SwitchStorage, &v1.SwitchStorageRequest{Tenant: name, Primary: "main", Replica: "copy", ValidateOnly: true})
		},
		"BackfillReplica": func(c clients) outcome {
			return call(c.replicas.BackfillReplica, &v1.BackfillReplicaRequest{Tenant: name, Replica: "copy"})
		},
		"Recall": func(c clients) outcome {
			return call(c.replicas.Recall, &v1.RecallRequest{Tenant: name, SubjectLabel: "SUBJ"})
		},
		"VerifySample": func(c clients) outcome {
			return call(c.replicas.VerifySample, &v1.VerifySampleRequest{Tenant: name, Storage: "main", Count: 1})
		},
		"AddRetentionRule": func(c clients) outcome {
			return call(c.retention.AddRetentionRule, &v1.AddRetentionRuleRequest{Tenant: name, Rule: &v1.RetentionRule{Name: "r", KeepAtLeast: &v1.RetentionPeriod{Years: 1}}})
		},
		"GetRetentionRule": func(c clients) outcome {
			return call(c.retention.GetRetentionRule, &v1.GetRetentionRuleRequest{Tenant: name, Name: "r"})
		},
		"ListRetentionRules": func(c clients) outcome {
			return call(c.retention.ListRetentionRules, &v1.ListRetentionRulesRequest{Tenant: name})
		},
		"SetRetentionRule": func(c clients) outcome {
			return call(c.retention.SetRetentionRule, &v1.SetRetentionRuleRequest{Tenant: name, Rule: &v1.RetentionRule{Name: "r"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"action"}}})
		},
		"RemoveRetentionRule": func(c clients) outcome {
			return call(c.retention.RemoveRetentionRule, &v1.RemoveRetentionRuleRequest{Tenant: name, Name: "r"})
		},
		"ListDueSessions": func(c clients) outcome {
			return call(c.retention.ListDueSessions, &v1.ListDueSessionsRequest{Tenant: name})
		},
		"Dispose": func(c clients) outcome {
			return call(c.retention.Dispose, &v1.DisposeRequest{Tenant: name, Rule: "r", Reason: "x", ValidateOnly: true})
		},
		"ConfirmClock": func(c clients) outcome {
			return call(c.retention.ConfirmClock, &v1.ConfirmClockRequest{Scope: &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: name}},
				ObservedTime: timestamppb.Now(), Reason: "x"})
		},
		"GetHold": func(c clients) outcome {
			return call(c.retention.GetHold, &v1.GetHoldRequest{Tenant: name, Id: correctionULID})
		},
		"ListHolds": func(c clients) outcome {
			return call(c.retention.ListHolds, &v1.ListHoldsRequest{Tenant: name})
		},
		"PlaceHold": func(c clients) outcome {
			return call(c.retention.PlaceHold, &v1.PlaceHoldRequest{Tenant: name, Target: &v1.HoldTarget{SubjectLabel: "S01"}, Reason: "x"})
		},
		"ReleaseHold": func(c clients) outcome {
			return call(c.retention.ReleaseHold, &v1.ReleaseHoldRequest{Tenant: name, Id: correctionULID, Reason: "x"})
		},
		"CreateRuleSet": func(c clients) outcome {
			return call(c.routes.CreateRuleSet, &v1.CreateRuleSetRequest{Tenant: name, Receiver: "r", RuleSet: &v1.RuleSet{}})
		},
		"GetRuleSet": func(c clients) outcome {
			return call(c.routes.GetRuleSet, &v1.GetRuleSetRequest{Tenant: name, Receiver: "r"})
		},
		"ListRuleSets": func(c clients) outcome {
			return call(c.routes.ListRuleSets, &v1.ListRuleSetsRequest{Tenant: name, Receiver: "r"})
		},
		"ActivateRuleSet": func(c clients) outcome {
			return call(c.routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: name, Receiver: "r", Version: 1})
		},
		"ExplainRoute": func(c clients) outcome {
			return call(c.routes.ExplainRoute, &v1.ExplainRouteRequest{Tenant: name, Receiver: "r"})
		},
		"DryRunRoutes": func(c clients) outcome {
			return call(c.routes.DryRunRoutes, &v1.DryRunRoutesRequest{Tenant: name, Receiver: "r"})
		},
		"GetRouteHits": func(c clients) outcome {
			return call(c.routes.GetRouteHits, &v1.GetRouteHitsRequest{Tenant: name, Receiver: "r"})
		},
	}
}

// correctionULID is an operation id no correction of a test has.
const correctionULID = "01jd0000000000000000000000"

// TestNoDecisionIsInternal guards the rule that a handler never decides by
// itself: when the server did not put a Decision in the context, every
// method answers INTERNAL with the fixed message, logs it, and changes
// nothing, even for the local operator, who would be allowed everything.
func TestNoDecisionIsInternal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.decide.Store(false)
	methods := everyMethod(f)
	if len(methods) != len(contractMethods()) {
		t.Fatalf("everyMethod has %d methods, the contract %d", len(methods), len(contractMethods()))
	}
	for name, m := range methods {
		o := m(f.as(local))
		if o.code() != connect.CodeInternal || o.message() != "internal error" {
			t.Errorf("%s without a decision: %v, %q", name, o.code(), o.message())
		}
	}
	if n := strings.Count(f.log.String(), "without an access decision"); n != len(methods) {
		t.Errorf("%d log lines about a missing decision, want %d", n, len(methods))
	}
	if _, err := f.master.TenantByName(ctx, "lab9"); err == nil {
		t.Error("CreateTenant acted without a decision")
	}
	if _, ok := f.registry.State("lab1"); !ok {
		t.Error("RemoveTenant acted without a decision")
	}
	if k, err := f.master.Key(ctx, f.noGrantID); err != nil || !k.Revoked.IsZero() {
		t.Error("RevokeKey acted without a decision")
	}
}

// TestEveryMethodWithTheMasterDown guards the unavailable line of every
// method: with the master database closed, each answers UNAVAILABLE, never a
// refusal or an internal error. The local operator calls, so the check reads
// nothing for most methods and the handler meets the closed master itself;
// WhoAmI, ListProfileBases and GetAuditPublicKey are called with a key,
// since the local operator's call of any of them reads nothing at all (the
// audit public keys are derived from the key file).
func TestEveryMethodWithTheMasterDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := f.master.Close(); err != nil {
		t.Fatal(err)
	}
	for name, m := range everyMethod(f) {
		as := local
		if name == "WhoAmI" || name == "ListProfileBases" || name == "GetAuditPublicKey" {
			as = f.instAdmin
		}
		if o := m(f.as(as)); o.code() != connect.CodeUnavailable {
			t.Errorf("%s with the master down: %v (%v)", name, o.code(), o.err)
		}
	}
}

// TestLaterLookupDoesNotUndoTheAccessRule guards the rule that a lookup
// after the check that finds no tenant answers as the check would: a key
// whose grant names a tenant the registry does not hold (here a row written
// to the master behind the registry's back, with the registry's rounds,
// which would reconcile it, an hour apart) is denied with the message of no
// grant, and only a caller who may list every tenant is told not found.
func TestLaterLookupDoesNotUndoTheAccessRule(t *testing.T) {
	t.Parallel()
	f := fixtureOn(t, newEnvChecking(t, time.Hour))
	row := master.Tenant{ID: "ghost-id", Name: "ghost", Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: t0}
	if err := f.master.CreateTenant(ctx, row, f.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	admin, _ := f.key(master.Grant{Scope: master.TenantScope("ghost-id"), Role: master.RoleAdmin})
	calls := map[string]func(clients) outcome{
		"GetSetting": getSetting(testRetention, tenantScope("ghost")),
		"SetSetting": setSetting(testRetention, tenantScope("ghost"), "1d"),
		"UnsetSetting": func(c clients) outcome {
			return call(c.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: testRetention, Scope: tenantScope("ghost")})
		},
		"ListSettings": func(c clients) outcome {
			return call(c.settings.ListSettings, &v1.ListSettingsRequest{Scope: tenantScope("ghost")})
		},
		"ListAuditEntries": func(c clients) outcome {
			return call(c.audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: tenantScope("ghost")})
		},
	}
	for name, m := range tenantMethods("ghost") {
		calls[name] = m
	}
	for name, m := range calls {
		if o := m(f.as(admin)); o.code() != connect.CodePermissionDenied || o.message() != access.MessageDenied {
			t.Errorf("%s by a key that may not list every tenant: %v %q, want permission denied %q", name, o.code(), o.message(), access.MessageDenied)
		}
		if o := m(f.as(local)); o.code() != connect.CodeNotFound || o.message() != access.MessageNoTenant {
			t.Errorf("%s by the local operator: %v %q, want not found", name, o.code(), o.message())
		}
	}
}

// TestReadsDoNotFailForTheirEntry guards the rule that a read never fails
// because its audit entry could not be queued: with the access package's
// audit writer closed, so every read's entry is dropped, reads still answer.
func TestReadsDoNotFailForTheirEntry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.access.Close(cctx); err != nil {
		t.Fatal(err)
	}
	reads := map[string]func(clients) outcome{
		"GetTenant":   func(c clients) outcome { return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: "lab1"}) },
		"ListTenants": func(c clients) outcome { return call(c.tenants.ListTenants, &v1.ListTenantsRequest{}) },
		"GetSetting":  getSetting(testRetention, installation()),
		"ListSettings": func(c clients) outcome {
			return call(c.settings.ListSettings, &v1.ListSettingsRequest{Scope: installation()})
		},
		"ListAuditEntries": func(c clients) outcome {
			return call(c.audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: installation()})
		},
	}
	for name, m := range reads {
		if o := m(f.as(f.instAdmin)); o.err != nil {
			t.Errorf("%s with the audit writer closed: %v", name, o.err)
		}
	}
	if f.access.AuditStats().Dropped() == 0 {
		t.Error("no entry was dropped, so the test showed nothing")
	}
}

// TestTenantLogFallsBackToTheMaster guards the one choice of log for a
// read: a read on a tenant's scope is recorded in the tenant's log, and when
// that log does not take the entry, the entry is written to the
// installation's log with access.DetailRerouted and the tenant's name, and
// the read still answers.
func TestTenantLogFallsBackToTheMaster(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if o := createTenant("pg", pgType, pgAddress)(e.as(local)); o.err != nil {
		t.Fatal(o.err)
	}
	read := func() outcome {
		return call(e.as(local).settings.GetSetting, &v1.GetSettingRequest{Name: testRetention, Scope: tenantScope("pg")})
	}
	ok := read()
	if ok.err != nil || len(entriesOf(e.tenantLog("pg"), ok.requestID)) != 1 {
		t.Fatalf("a read with the tenant's log up: %v, not in the tenant's log", ok.err)
	}
	e.pg.setLogDown("pg", true)
	o := read()
	if o.err != nil {
		t.Fatalf("a read with the tenant's log down failed: %v", o.err)
	}
	got := entriesOf(e.masterLog(), o.requestID)
	if len(got) != 1 || got[0].Details[access.DetailRerouted] == "" || got[0].TenantName != "pg" || got[0].Outcome != master.OutcomeDone {
		t.Errorf("the master's log holds %+v for the read, want one entry marked rerouted", got)
	}
	e.pg.setLogDown("pg", false)
	if n := len(entriesOf(e.tenantLog("pg"), o.requestID)); n != 0 {
		t.Errorf("the tenant's log holds the rerouted entry too (%d)", n)
	}
	if e.access.AuditStats().Rerouted == 0 {
		t.Error("the audit writer counted no rerouted entry")
	}
}

// TestInternalCauseIsLoggedNotReturned guards the rule for an internal
// fault: the answer says "internal error" and nothing more, and the log has
// the cause under the request's id. The fault is an id source that makes a
// tenant id the master would refuse, which must not come back as the
// caller's invalid argument.
func TestInternalCauseIsLoggedNotReturned(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.badTenantID.Store(true)
	o := createTenant("lab2", sqliteType, "")(f.as(f.instAdmin))
	if o.code() != connect.CodeInternal || o.message() != "internal error" {
		t.Fatalf("got %v %q", o.code(), o.message())
	}
	log := f.log.String()
	if !strings.Contains(log, "request_id="+o.requestID) || !strings.Contains(log, "id source made a tenant id") {
		t.Errorf("the log does not hold the cause under the request id:\n%s", log)
	}
	en := entriesOf(f.masterLog(), o.requestID)
	if len(en) != 1 || en[0].Outcome != master.OutcomeFailed || strings.Contains(en[0].Reason, "id source") {
		t.Errorf("the entry is %+v", en)
	}
}

// TestStateIsTheRegistrys guards the rule that GetTenant and ListTenants
// answer the state the registry sees now, not the master's row, which the
// registry updates at most once a minute: a tenant whose database stops
// answering is shown unreachable at once, while its row still says ready.
func TestStateIsTheRegistrys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if o := createTenant("pg", pgType, pgAddress)(e.as(local)); o.err != nil {
		t.Fatal(o.err)
	}
	e.pg.setDown("pg", true)
	deadline := time.Now().Add(10 * time.Second)
	for st, _ := e.registry.State("pg"); st.State != master.StateUnreachable; st, _ = e.registry.State("pg") {
		if time.Now().After(deadline) {
			t.Fatal("the registry did not see the tenant go down")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if row, err := e.master.TenantByName(ctx, "pg"); err != nil || row.State != master.StateReady {
		t.Fatalf("the master's row is %v (%v); the test needs it still ready", row.State, err)
	}
	got := call(e.as(local).tenants.GetTenant, &v1.GetTenantRequest{Name: "pg"})
	tn := got.res.(*v1.GetTenantResponse).GetTenant()
	if tn.GetState() != v1.TenantState_TENANT_STATE_UNREACHABLE || tn.GetStateReason() == "" {
		t.Errorf("GetTenant: %v", tn)
	}
	// The state time is when the registry's view began, not the creation.
	st, _ := e.registry.State("pg")
	if tn.GetStateTime() == nil || !tn.GetStateTime().AsTime().Equal(st.Since) || !tn.GetStateTime().AsTime().After(tn.GetCreateTime().AsTime()) {
		t.Errorf("GetTenant's state time is %v; the registry's view began %v, the tenant was created %v", tn.GetStateTime(), st.Since, tn.GetCreateTime())
	}
	if tn.GetStateTime().AsTime().Location() != time.UTC {
		t.Error("the state time is not UTC")
	}
	list := call(e.as(local).tenants.ListTenants, &v1.ListTenantsRequest{})
	if ts := list.res.(*v1.ListTenantsResponse).GetTenants(); len(ts) != 1 || ts[0].GetState() != v1.TenantState_TENANT_STATE_UNREACHABLE || !ts[0].GetStateTime().AsTime().Equal(st.Since) {
		t.Errorf("ListTenants: %v", ts)
	}
}

// TestTenantLifecycle guards a tenant's life end to end: a tenant created
// over the API is ready and has its database file; a tenant setting is set,
// read, unset and found in that tenant's audit log; after removal the
// registration is gone and the file is still there.
func TestTenantLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.as(local)
	o := call(admin.tenants.CreateTenant, &v1.CreateTenantRequest{Name: "study", DatabaseType: sqliteType})
	if o.err != nil || o.res.(*v1.CreateTenantResponse).GetTenant().GetState() != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("create: %v %v", o.err, o.res)
	}
	file := filepath.Join(e.data, "tenants", "study.db")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the tenant's file: %v", err)
	}
	set := call(admin.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: tenantScope("study"), Value: "30d"}})
	get := call(admin.settings.GetSetting, &v1.GetSettingRequest{Name: testRetention, Scope: tenantScope("study")})
	unset := call(admin.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: testRetention, Scope: tenantScope("study")})
	for name, o := range map[string]outcome{"set": set, "get": get, "unset": unset} {
		if o.err != nil {
			t.Fatalf("%s: %v", name, o.err)
		}
	}
	if v := get.res.(*v1.GetSettingResponse).GetSetting(); v.GetValue() != "30d" || v.GetScope().GetTenant() != "study" {
		t.Errorf("get: %v", v)
	}
	if v := unset.res.(*v1.UnsetSettingResponse).GetSetting(); v.GetValue() != "365d" || v.GetScope().GetInstallation() == nil {
		t.Errorf("unset: %v", v)
	}
	log := e.tenantLog("study")
	for name, o := range map[string]outcome{"set": set, "get": get, "unset": unset} {
		if got := entriesOf(log, o.requestID); len(got) != 1 || got[0].Outcome != master.OutcomeDone || got[0].Object != testRetention {
			t.Errorf("%s: the tenant's log holds %+v", name, got)
		}
	}
	rm := call(admin.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "study", ConfirmName: "study"})
	if rm.err != nil {
		t.Fatal(rm.err)
	}
	if _, err := e.master.TenantByName(ctx, "study"); err == nil {
		t.Error("the master still holds the tenant")
	}
	if _, ok := e.registry.State("study"); ok {
		t.Error("the registry still holds the tenant")
	}
	if o := call(admin.tenants.GetTenant, &v1.GetTenantRequest{Name: "study"}); o.code() != connect.CodeNotFound {
		t.Errorf("get after remove: %v", o.code())
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the file is gone after remove: %v", err)
	}
}

// TestDatabaseAddressIsFoundNowhere guards that a secret stays sealed: a
// database address given to CreateTenant, with a password, user and host that
// are each searched for, is in no response, no audit entry of either log, no
// log line, no error, and in the master's files only sealed. The test's
// driver repeats the address in its own error when the database is down, so
// the path where a driver is careless is covered too.
func TestDatabaseAddressIsFoundNowhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const (
		password = "Hunter2-Secret-9f3k"
		user     = "aliceuser7q"
		host     = "db-secret-host.internal"
	)
	address := "postgres://" + user + ":" + password + "@" + host + ":5432/lab?sslmode=verify-full"
	// Each secret is reported by its label only, so a failing run prints no
	// secret in the test's output.
	secrets := map[string]string{"the address": address, "the password": password, "the user": user, "the host": host}

	var seen []string
	record := func(o outcome) {
		if o.res != nil {
			b, err := protojson.Marshal(o.res)
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, string(b))
		}
		if o.err != nil {
			seen = append(seen, o.err.Error(), o.message())
		}
	}
	admin := f.as(f.instAdmin)
	create := func(name, addr string, typ v1.DatabaseType) outcome {
		return call(admin.tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: typ, DatabaseAddress: addr})
	}
	o := create("pg1", address, pgType)
	if o.err != nil || o.res.(*v1.CreateTenantResponse).GetTenant().GetState() != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("create: %v %v", o.err, o.res)
	}
	record(o)
	record(create("pg1", address, pgType))        // conflict
	record(create("pg-bad", address, sqliteType)) // invalid
	// A database that does not answer at creation: its driver's error holds
	// the address.
	f.pg.setDown("pg2", true)
	record(create("pg2", address, pgType))
	record(call(admin.tenants.GetTenant, &v1.GetTenantRequest{Name: "pg2"}))
	// A database that stops answering after creation: the registry's
	// reason is made from a driver's error that holds the address.
	record(create("pg3", address, pgType))
	f.pg.setDown("pg3", true)
	f.advance(2 * registry.StateChangeInterval)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if row, err := f.master.TenantByName(ctx, "pg3"); err == nil && row.State == master.StateUnreachable {
			break
		}
	}
	record(call(admin.tenants.GetTenant, &v1.GetTenantRequest{Name: "pg3"}))
	record(call(admin.tenants.ListTenants, &v1.ListTenantsRequest{}))
	record(call(f.as(local).settings.GetSetting, &v1.GetSettingRequest{Name: testText, Scope: tenantScope("pg3")}))
	record(call(admin.tenants.GetTenant, &v1.GetTenantRequest{Name: "pg1"}))
	record(call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testText, Scope: installation(), Value: "1"}}))
	record(call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testText, Scope: tenantScope("pg1"), Value: "1"}}))
	record(call(f.as(local).audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: installation(), PageSize: 1000}))
	record(call(f.as(local).audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: tenantScope("pg1"), PageSize: 1000}))
	rm := call(admin.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "pg1", ConfirmName: "pg1"})
	if rm.err != nil || rm.res.(*v1.RemoveTenantResponse).GetDataLocation() != "pg.location.test:5432/pg1" {
		t.Errorf("remove: %v %v", rm.err, rm.res)
	}
	record(rm)

	for _, en := range f.masterLog() {
		seen = append(seen, fmt.Sprintf("%+v", en))
	}
	seen = append(seen, f.log.String())
	for _, name := range []string{"master.db", "master.db-wal"} {
		b, err := os.ReadFile(filepath.Join(f.data, name)) //nolint:gosec // G304: a file of the test's own directory.
		if err == nil {
			seen = append(seen, string(b))
		}
	}
	if len(seen) < 15 {
		t.Fatalf("only %d texts were gathered", len(seen))
	}
	for label, s := range secrets {
		for i, text := range seen {
			if strings.Contains(text, s) {
				t.Errorf("text %d of %d gathered holds %s", i, len(seen), label)
			}
		}
	}
	if !strings.Contains(strings.Join(seen, "\n"), "pg.location.test") {
		t.Error("the location is in nothing gathered, so the search may have looked at the wrong texts")
	}
}
