package api_test

import (
	"context"
	"os"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
)

// methodTests is the table of tests of each method of the contract, by its
// full procedure name. TestEveryMethod walks the services' descriptors and
// fails for a method with no entry here, so a method added to the contract
// fails until it is tested.
var methodTests = map[string]func(t *testing.T){
	nopacsv1connect.CorrectionServiceDeleteProcedure:            testDelete,
	nopacsv1connect.CorrectionServiceRelabelProcedure:           testRelabel,
	nopacsv1connect.CorrectionServiceReplaceProcedure:           testReplace,
	nopacsv1connect.CorrectionServiceListCorrectionsProcedure:   testListCorrections,
	nopacsv1connect.CorrectionServiceGetCorrectionProcedure:     testGetCorrection,
	nopacsv1connect.CorrectionServiceUndoCorrectionProcedure:    testUndoCorrection,
	nopacsv1connect.CorrectionServicePurgeCorrectionProcedure:   testPurgeCorrection,
	nopacsv1connect.CorrectionServiceAllowArrivalsProcedure:     testAllowArrivals,
	nopacsv1connect.CorrectionServiceListScrubFindingsProcedure: testListScrubFindings,
	nopacsv1connect.CorrectionServiceResolveInstancesProcedure:  testResolveInstances,

	nopacsv1connect.AccessServiceCreateKeyProcedure:       testCreateKey,
	nopacsv1connect.AccessServiceGetKeyProcedure:          testGetKey,
	nopacsv1connect.AccessServiceListKeysProcedure:        testListKeys,
	nopacsv1connect.AccessServiceRevokeKeyProcedure:       testRevokeKey,
	nopacsv1connect.AccessServiceSetGrantProcedure:        testSetGrant,
	nopacsv1connect.AccessServiceListGrantsProcedure:      testListGrants,
	nopacsv1connect.AccessServiceRemoveGrantProcedure:     testRemoveGrant,
	nopacsv1connect.AccessServiceWhoAmIProcedure:          testWhoAmI,
	nopacsv1connect.AccessServiceRotateKeyProcedure:       testRotateKey,
	nopacsv1connect.AccessServiceSetKeyRulesProcedure:     testSetKeyRules,
	nopacsv1connect.AccessServiceReinstateKeysProcedure:   testReinstateKeys,
	nopacsv1connect.TenantServiceCreateTenantProcedure:    testCreateTenant,
	nopacsv1connect.TenantServiceGetTenantProcedure:       testGetTenant,
	nopacsv1connect.TenantServiceListTenantsProcedure:     testListTenants,
	nopacsv1connect.TenantServiceRemoveTenantProcedure:    testRemoveTenant,
	nopacsv1connect.TenantServiceReclaimTenantProcedure:   testReclaimTenant,
	nopacsv1connect.BackupServiceExportTenantProcedure:    testExportTenant,
	nopacsv1connect.BackupServiceCreateBackupProcedure:    testCreateBackup,
	nopacsv1connect.SettingsServiceGetSettingProcedure:    testGetSetting,
	nopacsv1connect.SettingsServiceSetSettingProcedure:    testSetSetting,
	nopacsv1connect.SettingsServiceUnsetSettingProcedure:  testUnsetSetting,
	nopacsv1connect.SettingsServiceListSettingsProcedure:  testListSettings,
	nopacsv1connect.AuditServiceListAuditEntriesProcedure: testListAuditEntries,

	nopacsv1connect.StorageServiceAddStorageProcedure:                testAddStorage,
	nopacsv1connect.StorageServiceGetStorageProcedure:                testGetStorage,
	nopacsv1connect.StorageServiceListStoragesProcedure:              testListStorages,
	nopacsv1connect.StorageServiceSetStorageProcedure:                testSetStorage,
	nopacsv1connect.StorageServiceRemoveStorageProcedure:             testRemoveStorage,
	nopacsv1connect.StorageServiceTestStorageProcedure:               testTestStorage,
	nopacsv1connect.ReceiverServiceAddReceiverProcedure:              testAddReceiver,
	nopacsv1connect.ReceiverServiceGetReceiverProcedure:              testGetReceiver,
	nopacsv1connect.ReceiverServiceListReceiversProcedure:            testListReceivers,
	nopacsv1connect.ReceiverServiceSetReceiverProcedure:              testSetReceiver,
	nopacsv1connect.ReceiverServiceRemoveReceiverProcedure:           testRemoveReceiver,
	nopacsv1connect.ReceptionServiceListReceptionsProcedure:          testListReceptions,
	nopacsv1connect.ReceptionServiceGetReceptionProcedure:            testGetReception,
	nopacsv1connect.ReceptionServiceRetryReceptionProcedure:          testRetryReception,
	nopacsv1connect.ReceptionServiceDiscardReceptionProcedure:        testDiscardReception,
	nopacsv1connect.QuarantineServiceListQuarantinedFilesProcedure:   testListQuarantinedFiles,
	nopacsv1connect.QuarantineServiceGetQuarantinedFileProcedure:     testGetQuarantinedFile,
	nopacsv1connect.QuarantineServiceReleaseQuarantinedFileProcedure: testReleaseQuarantinedFile,
	nopacsv1connect.QuarantineServiceRejectQuarantinedFileProcedure:  testRejectQuarantinedFile,
	nopacsv1connect.MonitorServiceGetStatusProcedure:                 testGetStatus,
	nopacsv1connect.MonitorServiceWatchProcedure:                     testWatch,
	nopacsv1connect.MetadataServiceGetScanMetadataProcedure:          testGetScanMetadata,
	nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure:      testGetInstanceMetadata,
	nopacsv1connect.CatalogServiceListSubjectsProcedure:              testListSubjects,
	nopacsv1connect.CatalogServiceGetSubjectProcedure:                testGetSubject,
	nopacsv1connect.CatalogServiceListSessionsProcedure:              testListSessions,
	nopacsv1connect.CatalogServiceGetSessionProcedure:                testGetSession,
	nopacsv1connect.CatalogServiceListScansProcedure:                 testListScans,
	nopacsv1connect.CatalogServiceGetScanProcedure:                   testGetScan,
	nopacsv1connect.DataServiceReadFilesProcedure:                    testReadFiles,
	nopacsv1connect.SenderServiceAddSenderProcedure:                  testAddSender,
	nopacsv1connect.SenderServiceGetSenderProcedure:                  testGetSender,
	nopacsv1connect.SenderServiceListSendersProcedure:                testListSenders,
	nopacsv1connect.SenderServiceSetSenderProcedure:                  testSetSender,
	nopacsv1connect.SenderServiceRemoveSenderProcedure:               testRemoveSender,
	nopacsv1connect.SenderServiceTestSenderProcedure:                 testTestSender,
	nopacsv1connect.JobServiceCreateJobProcedure:                     testCreateJob,
	nopacsv1connect.JobServiceGetJobProcedure:                        testGetJob,
	nopacsv1connect.JobServiceListJobsProcedure:                      testListJobs,
	nopacsv1connect.JobServiceCancelJobProcedure:                     testCancelJob,
	nopacsv1connect.JobServiceRetryJobProcedure:                      testRetryJob,
	nopacsv1connect.JobServiceListJobFailuresProcedure:               testListJobFailures,
	nopacsv1connect.JobServiceListImportFilesProcedure:               testListImportFiles,
	nopacsv1connect.JobServiceListImportUnitsProcedure:               testListImportUnits,
	nopacsv1connect.ProfileServiceCreateProfileVersionProcedure:      testCreateProfileVersion,
	nopacsv1connect.ProfileServiceGetProfileVersionProcedure:         testGetProfileVersion,
	nopacsv1connect.ProfileServiceListProfilesProcedure:              testListProfiles,
	nopacsv1connect.ProfileServiceListProfileVersionsProcedure:       testListProfileVersions,
	nopacsv1connect.ProfileServiceValidateProfileProcedure:           testValidateProfile,
	nopacsv1connect.ProfileServiceListProfileBasesProcedure:          testListProfileBases,
	nopacsv1connect.ProfileServiceTryProfileProcedure:                testTryProfile,
	nopacsv1connect.ProfileServiceExplainProfileProcedure:            testExplainProfile,
	nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure:       testDiffProfileVersions,
	nopacsv1connect.ProfileServiceDescribeProfileProcedure:           testDescribeProfile,
	nopacsv1connect.DeidServiceCodeProcedure:                         testCode,
	nopacsv1connect.DeidServiceImportLookupCodesProcedure:            testImportLookupCodes,
	nopacsv1connect.DeidServiceListLookupTablesProcedure:             testListLookupTables,
	nopacsv1connect.AuditSealServiceVerifyAuditProcedure:             testVerifyAudit,
	nopacsv1connect.AuditSealServiceListAuditCheckpointsProcedure:    testListAuditCheckpoints,
	nopacsv1connect.AuditSealServiceExportAuditProcedure:             testExportAudit,
	nopacsv1connect.AuditSealServiceGetAuditPublicKeyProcedure:       testGetAuditPublicKey,
}

// contractMethods returns the full procedure name and descriptor of every
// method of the services this package serves, read from the generated
// descriptors, but those api/not-served.txt names (a method added to a
// served service, not served yet), which TestNotServedServices holds.
func contractMethods() map[string]protoreflect.MethodDescriptor {
	b, err := os.ReadFile(notServedFile)
	if err != nil {
		panic("the list of services not served: " + err.Error())
	}
	notServed, err := parseNotServed(string(b))
	if err != nil {
		panic(notServedFile + ": " + err.Error())
	}
	out := map[string]protoreflect.MethodDescriptor{}
	for _, f := range []protoreflect.FileDescriptor{
		v1.File_nopacs_v1_access_proto, v1.File_nopacs_v1_tenant_proto,
		v1.File_nopacs_v1_settings_proto, v1.File_nopacs_v1_audit_proto,
		v1.File_nopacs_v1_storage_proto, v1.File_nopacs_v1_receiver_proto,
		v1.File_nopacs_v1_reception_proto, v1.File_nopacs_v1_quarantine_proto,
		v1.File_nopacs_v1_monitor_proto, v1.File_nopacs_v1_metadata_proto,
		v1.File_nopacs_v1_catalog_proto, v1.File_nopacs_v1_data_proto,
		v1.File_nopacs_v1_sender_proto, v1.File_nopacs_v1_job_proto,
		v1.File_nopacs_v1_profile_proto,
		v1.File_nopacs_v1_deid_proto,
		v1.File_nopacs_v1_audit_seal_proto,
		v1.File_nopacs_v1_correction_proto,
		v1.File_nopacs_v1_backup_proto,
	} {
		for i := 0; i < f.Services().Len(); i++ {
			s := f.Services().Get(i)
			for j := 0; j < s.Methods().Len(); j++ {
				m := s.Methods().Get(j)
				proc := "/" + string(s.FullName()) + "/" + string(m.Name())
				if !inServices(notServed, proc) {
					out[proc] = m
				}
			}
		}
	}
	return out
}

// TestEveryMethod guards the link between the contract and this package:
// every method of the services it serves is known to api.Methods, answers a
// requirement that is not the zero one, has the right request type when it
// reads the request, and has a test table here, which it runs; and nothing
// here names a method the contract does not have.
func TestEveryMethod(t *testing.T) {
	methods := contractMethods()
	if len(methods) != 94 {
		t.Errorf("the contract has %d methods; this test was written for 94, so look at what changed", len(methods))
	}
	var table api.Methods
	for proc, md := range methods {
		msg, known := table.RequestFor(proc)
		if !known {
			t.Errorf("%s: api.Methods does not know it", proc)
			continue
		}
		if msg != nil && msg.ProtoReflect().Descriptor().FullName() != md.Input().FullName() {
			t.Errorf("%s: RequestFor gives a %s, the method takes a %s", proc, msg.ProtoReflect().Descriptor().FullName(), md.Input().FullName())
		}
		if need, _ := table.Requirement(proc, msg); need == (access.Requirement{}) {
			t.Errorf("%s: the requirement of an empty request is the zero one", proc)
		}
		if _, ok := methodTests[proc]; !ok {
			t.Errorf("%s: no test table", proc)
		}
	}
	for proc := range methodTests {
		if _, ok := methods[proc]; !ok {
			t.Errorf("%s has a test table but is not a method of the contract", proc)
		}
	}
	if _, known := table.RequestFor("/nopacs.v1.TenantService/Nothing"); known {
		t.Error("an unknown procedure is known")
	}
	if need, _ := table.Requirement("/nopacs.v1.TenantService/Nothing", nil); need != (access.Requirement{}) {
		t.Error("an unknown procedure has a requirement other than the zero one, which nobody meets")
	}
	for proc, md := range methods {
		test := methodTests[proc]
		if test == nil {
			continue
		}
		t.Run(string(md.Name()), func(t *testing.T) {
			t.Parallel()
			test(t)
		})
	}
}

// TestRequirementTable guards the method-to-role table against the Role
// line of each method, request by request: a fixed requirement where the
// line names no scope of the request, the tenant or scope the request names
// where it does, and the role on the installation for a malformed or
// missing scope and for a message of the wrong type, never a weaker one.
func TestRequirementTable(t *testing.T) {
	var m api.Methods
	admin := access.OnInstallation(master.RoleAdmin)
	need := func(proc string, msg proto.Message) access.Requirement {
		t.Helper()
		if pm, _ := m.RequestFor(proc); (pm == nil) != (msg == nil) {
			t.Errorf("%s: RequestFor gives %v for a request %v", proc, pm, msg)
		}
		r, _ := m.Requirement(proc, msg)
		return r
	}
	cases := []struct {
		name string
		proc string
		msg  proto.Message
		want access.Requirement
	}{
		{"CreateKey", nopacsv1connect.AccessServiceCreateKeyProcedure, nil, admin},
		{"GetKey", nopacsv1connect.AccessServiceGetKeyProcedure, nil, admin},
		{"ListKeys", nopacsv1connect.AccessServiceListKeysProcedure, nil, admin},
		{"RevokeKey", nopacsv1connect.AccessServiceRevokeKeyProcedure, nil, admin},
		{"SetGrant", nopacsv1connect.AccessServiceSetGrantProcedure, nil, admin},
		{"ListGrants", nopacsv1connect.AccessServiceListGrantsProcedure, nil, admin},
		{"RemoveGrant", nopacsv1connect.AccessServiceRemoveGrantProcedure, nil, admin},
		{"WhoAmI", nopacsv1connect.AccessServiceWhoAmIProcedure, nil, access.Authenticated()},
		{"RotateKey", nopacsv1connect.AccessServiceRotateKeyProcedure, nil, access.Authenticated()},
		{"SetKeyRules", nopacsv1connect.AccessServiceSetKeyRulesProcedure, nil, admin},
		{"CreateTenant", nopacsv1connect.TenantServiceCreateTenantProcedure, nil, admin},
		{"ListTenants", nopacsv1connect.TenantServiceListTenantsProcedure, nil, access.Authenticated()},
		{"RemoveTenant", nopacsv1connect.TenantServiceRemoveTenantProcedure, nil, admin},
		{"GetTenant", nopacsv1connect.TenantServiceGetTenantProcedure, &v1.GetTenantRequest{Name: "lab1"}, access.TenantRegistration("lab1")},
		{"GetTenant malformed", nopacsv1connect.TenantServiceGetTenantProcedure, &v1.GetTenantRequest{Name: "Lab 1"}, access.OnInstallation(master.RoleReader)},
		{"GetTenant empty", nopacsv1connect.TenantServiceGetTenantProcedure, &v1.GetTenantRequest{}, access.OnInstallation(master.RoleReader)},
		{"GetSetting tenant", nopacsv1connect.SettingsServiceGetSettingProcedure, &v1.GetSettingRequest{Scope: tenantScope("lab1")}, access.OnTenant("lab1", master.RoleReader)},
		{"GetSetting installation", nopacsv1connect.SettingsServiceGetSettingProcedure, &v1.GetSettingRequest{Scope: installation()}, access.OnInstallation(master.RoleReader)},
		{"GetSetting no scope", nopacsv1connect.SettingsServiceGetSettingProcedure, &v1.GetSettingRequest{}, access.OnInstallation(master.RoleReader)},
		{"GetSetting malformed", nopacsv1connect.SettingsServiceGetSettingProcedure, &v1.GetSettingRequest{Scope: tenantScope("../x")}, access.OnInstallation(master.RoleReader)},
		{"SetSetting tenant", nopacsv1connect.SettingsServiceSetSettingProcedure, &v1.SetSettingRequest{Setting: &v1.Setting{Scope: tenantScope("lab1")}}, access.OnTenant("lab1", master.RoleAdmin)},
		{"SetSetting installation", nopacsv1connect.SettingsServiceSetSettingProcedure, &v1.SetSettingRequest{Setting: &v1.Setting{Scope: installation()}}, admin},
		{"SetSetting no setting", nopacsv1connect.SettingsServiceSetSettingProcedure, &v1.SetSettingRequest{}, admin},
		{"UnsetSetting tenant", nopacsv1connect.SettingsServiceUnsetSettingProcedure, &v1.UnsetSettingRequest{Scope: tenantScope("lab1")}, access.OnTenant("lab1", master.RoleAdmin)},
		{"UnsetSetting installation", nopacsv1connect.SettingsServiceUnsetSettingProcedure, &v1.UnsetSettingRequest{Scope: installation()}, admin},
		{"ListSettings tenant", nopacsv1connect.SettingsServiceListSettingsProcedure, &v1.ListSettingsRequest{Scope: tenantScope("lab1")}, access.OnTenant("lab1", master.RoleReader)},
		{"ListSettings installation", nopacsv1connect.SettingsServiceListSettingsProcedure, &v1.ListSettingsRequest{Scope: installation()}, access.OnInstallation(master.RoleReader)},
		{"ListAuditEntries tenant", nopacsv1connect.AuditServiceListAuditEntriesProcedure, &v1.ListAuditEntriesRequest{Scope: tenantScope("lab1")}, access.OnTenant("lab1", master.RoleAdmin)},
		{"ListAuditEntries installation", nopacsv1connect.AuditServiceListAuditEntriesProcedure, &v1.ListAuditEntriesRequest{Scope: installation()}, admin},
		{"ListAuditEntries no scope", nopacsv1connect.AuditServiceListAuditEntriesProcedure, &v1.ListAuditEntriesRequest{}, admin},
	}
	for _, c := range cases {
		if got := need(c.proc, c.msg); got != c.want {
			t.Errorf("%s: the requirement is %+v, want %+v", c.name, got, c.want)
		}
	}
	// A fixed requirement reads no request: RequestFor answers nil.
	for _, proc := range []string{nopacsv1connect.AccessServiceCreateKeyProcedure, nopacsv1connect.TenantServiceCreateTenantProcedure, nopacsv1connect.TenantServiceListTenantsProcedure} {
		if msg, _ := m.RequestFor(proc); msg != nil {
			t.Errorf("%s: a fixed requirement asks for the request", proc)
		}
	}
	// A message of the wrong type is read as an empty one, so it is held to
	// the role on the installation and nothing panics.
	if got, _ := m.Requirement(nopacsv1connect.SettingsServiceGetSettingProcedure, &v1.GetTenantRequest{Name: "lab1"}); got != access.OnInstallation(master.RoleReader) {
		t.Errorf("a message of the wrong type gives %+v", got)
	}
	// The facts of a setting name the setting and the scope, never the value.
	_, facts := m.Requirement(nopacsv1connect.SettingsServiceSetSettingProcedure,
		&v1.SetSettingRequest{Setting: &v1.Setting{Name: "audit.retention", Scope: tenantScope("lab1"), Value: "a-secret-value"}})
	if facts.Object != "audit.retention" || facts.Details["scope"] != "tenant/lab1" {
		t.Errorf("SetSetting's facts are %+v", facts)
	}
	for k, v := range facts.Details {
		if strings.Contains(v, "a-secret-value") || strings.Contains(k, "value") {
			t.Errorf("SetSetting's facts hold the value: %q=%q", k, v)
		}
	}
}

// tcase is one call of a method's table.
type tcase struct {
	name string
	as   caller
	call func(clients) outcome
	// code is the answer's code, 0 for success.
	code connect.Code
	// msg, when set, is a part of the answer's message.
	msg string
	// audit, when set, is the entry the call must have written.
	audit *wantAudit
	// check, when set, looks at the answer.
	check func(t *testing.T, o outcome)
}

// wantAudit is the audit entry a call must write.
type wantAudit struct {
	// log is the tenant whose log holds it, "" for the installation's.
	log     string
	outcome master.Outcome
	// object, tenant: compared when set; "-" means empty.
	object string
	tenant string
	// details must hold these values; "*" means any value.
	details map[string]string
	// reason, when set, is a part of the entry's reason.
	reason string
}

// run runs the cases in order, each after the ones before it.
func (e *env) run(t *testing.T, cases []tcase) {
	t.Helper()
	for _, c := range cases {
		o := c.call(e.as(c.as))
		if o.code() != c.code {
			t.Errorf("%s: code %v (%v), want %v", c.name, o.code(), o.err, c.code)
			continue
		}
		if c.msg != "" && !strings.Contains(o.message(), c.msg) {
			t.Errorf("%s: message %q does not hold %q", c.name, o.message(), c.msg)
		}
		if o.requestID == "" {
			t.Errorf("%s: no request id", c.name)
		}
		if c.check != nil {
			c.check(t, o)
		}
		if c.audit != nil {
			e.checkAudit(t, c.name, o.requestID, *c.audit)
		}
	}
}

// checkAudit fails unless exactly one entry with the request id is in the
// log w names, and it is as w says.
func (e *env) checkAudit(t *testing.T, name, requestID string, w wantAudit) {
	t.Helper()
	var log []master.AuditEntry
	if w.log == "" {
		log = e.masterLog()
	} else {
		e.flush()
		log = e.tenantLog(w.log)
	}
	// A system entry that names the request (the registry's record of a
	// tenant's row being written) is not the request's own.
	var got []master.AuditEntry
	for _, en := range entriesOf(log, requestID) {
		if en.Method != registry.StartMethod {
			got = append(got, en)
		}
	}
	if len(got) != 1 {
		t.Errorf("%s: %d entries with request id %s in log %q, want 1", name, len(got), requestID, w.log)
		return
	}
	en := got[0]
	if en.Outcome != w.outcome {
		t.Errorf("%s: outcome %q (reason %q), want %q", name, en.Outcome, en.Reason, w.outcome)
	}
	match := func(field, got, want string) {
		if want == "" {
			return
		}
		if want == "-" {
			want = ""
		}
		if got != want {
			t.Errorf("%s: %s %q, want %q", name, field, got, want)
		}
	}
	match("object", en.Object, w.object)
	match("tenant", en.TenantName, w.tenant)
	for k, v := range w.details {
		gv, ok := en.Details[k]
		if !ok || (v != "*" && gv != v) {
			t.Errorf("%s: detail %s is %q (present %v), want %q; details %v", name, k, gv, ok, v, en.Details)
		}
	}
	if w.reason != "" && !strings.Contains(en.Reason, w.reason) {
		t.Errorf("%s: reason %q does not hold %q", name, en.Reason, w.reason)
	}
}

// fixture is an installation with one ready SQLite tenant, lab1, and a key
// for each role around the lines the contract draws.
type fixture struct {
	*env
	instAdmin, instOperator, instReader  caller
	lab1Admin, lab1Operator, lab1Reader  caller
	noGrant                              caller
	instAdminID, lab1ReaderID, noGrantID string
}

// newFixture returns the fixture.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return fixtureOn(t, newEnv(t))
}

// fixtureOn makes the fixture on the installation e.
func fixtureOn(t *testing.T, e *env) *fixture {
	t.Helper()
	e.tenant("lab1")
	f := &fixture{env: e}
	f.instAdmin, f.instAdminID = e.key(onInstallation(master.RoleAdmin))
	f.instOperator, _ = e.key(onInstallation(master.RoleOperator))
	f.instReader, _ = e.key(onInstallation(master.RoleReader))
	f.lab1Admin, _ = e.key(e.onTenant("lab1", master.RoleAdmin))
	f.lab1Operator, _ = e.key(e.onTenant("lab1", master.RoleOperator))
	f.lab1Reader, f.lab1ReaderID = e.key(e.onTenant("lab1", master.RoleReader))
	f.noGrant, f.noGrantID = e.key()
	return f
}

// done is the audit entry of a success in the installation's log.
func done() *wantAudit { return &wantAudit{outcome: master.OutcomeDone} }

// failed is the audit entry of a failure in the installation's log, whose
// reason holds reason.
func failed(reason string) *wantAudit {
	return &wantAudit{outcome: master.OutcomeFailed, reason: reason}
}

// refused is the audit entry of a refusal in the installation's log.
func refused() *wantAudit { return &wantAudit{outcome: master.OutcomeRefused} }

// inTenant is the audit entry of an allowed request on a tenant's scope, in
// that tenant's log, whatever its outcome.
func inTenant(name string, outcome master.Outcome) *wantAudit {
	return &wantAudit{log: name, outcome: outcome, tenant: name}
}

// inMasterRerouted is the audit entry of an allowed request on a tenant
// whose database is not ready: in the installation's log, with the tenant's
// name and the detail that says it was meant for the tenant's log.
func inMasterRerouted(name string) *wantAudit {
	return &wantAudit{outcome: master.OutcomeFailed, tenant: name, details: map[string]string{access.DetailRerouted: "*"}}
}

// ctx is a context for direct calls in tests.
var ctx = context.Background()

// serverMethods and serverWithDecision are the types package internal/server
// takes from this package, restated here so a change that breaks the join
// fails to compile in this package's own tests: the server must not be
// imported from here.
type serverMethods interface {
	RequestFor(procedure string) (msg proto.Message, known bool)
	Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request)
}

// serverWithDecision is the server's type of the context accessor.
type serverWithDecision func(ctx context.Context, d access.Decision) context.Context

var (
	_ serverMethods      = api.Methods{}
	_ serverWithDecision = api.WithDecision
)
