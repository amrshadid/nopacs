package leaktest

import (
	"encoding/base64"
	"fmt"
	"time"
)

// apiCall is one request of the API as the Connect JSON protocol carries it.
type apiCall struct {
	// method is the API method's name, such as "GetScan".
	method string
	// variant says which of a method's requests this is; empty for the
	// first.
	variant string
	// body is the request as JSON.
	body string
	// last runs the call after the others of its key: a call that could
	// change what the others read.
	last bool
	// archived marks a call whose successful answer is the archived,
	// de-identified bytes themselves, which are not a surface: the long
	// base64 values of its answer are left out of the search.
	archived bool
}

// label names the call.
func (c apiCall) label() string {
	if c.variant == "" {
		return c.method
	}
	return c.method + "/" + c.variant
}

// jsonOf quotes a Go string as a JSON string.
func q(s string) string { return fmt.Sprintf("%q", s) }

// apiCalls returns the requests the keys make: at least one of every
// method of the API, with the real names of the world's tenants, scans and
// files, and, for a method that may be refused or fail in more than one way,
// one request for each. A request that would change what the archive holds
// names something that is not there, or asks to validate only, or is one a
// masked key is refused: the point is what each answers, and what the
// server writes about each.
func apiCalls(w *world) []apiCall {
	t, s, d, r := q(w.tenant), w.deid, w.deid, w.raw
	basic := `{"bases":["annex-e/basic:2026d"]}`
	own := base64.StdEncoding.EncodeToString(w.own)
	file := `{"name":"own.dcm","content":"` + own + `"}`
	byDoc := `{"document":` + basic + `}`
	byStored := `{"version":{"name":"research","version":1}}`
	label := func(x scanRef) string {
		return fmt.Sprintf(`"subjectLabel":%s,"sessionLabel":%s,"scanLabel":%s`, q(x.subject), q(x.session), q(x.scan))
	}
	inst := func(x scanRef) string { return label(x) + `,"instanceId":` + q(x.instanceID) }
	const ulid = `"01jd0000000000000000000000"`
	now := time.Now().UTC().Format(time.RFC3339)
	tn := `"tenant":` + t
	c := []apiCall{
		{method: "CreateKey", body: `{"holder":"made by an agent","masked":true,"neverExpire":true}`},
		{method: "CreateKey", variant: "not-masked", body: `{"holder":"made by an agent"}`},
		{method: "GetKey", body: `{"id":` + q(w.victim.id) + `}`},
		{method: "GetKey", variant: "not-masked", body: `{"id":` + q(w.plainVictim.id) + `}`},
		{method: "GetKey", variant: "no-such-key", body: `{"id":"nosuchkey000"}`},
		{method: "ListKeys", body: `{}`},
		{method: "ListProfileBases", body: `{}`},
		{method: "SetGrant", body: `{"keyId":` + q(w.victim.id) + `,"scope":{"installation":{}},"role":"ROLE_READER"}`},
		{method: "ListGrants", body: `{"scope":{"installation":{}}}`},
		{method: "ListGrants", variant: "by-key", body: `{"keyId":` + q(w.victim.id) + `}`},
		{method: "RemoveGrant", body: `{"keyId":` + q(w.victim.id) + `,"scope":{"tenant":"lab1"}}`},
		{method: "WhoAmI", body: `{}`},
		{method: "RotateKey", body: `{"id":` + q(w.victim.id) + `}`},
		{method: "RotateKey", variant: "not-masked", body: `{"id":` + q(w.plainVictim.id) + `}`},
		{method: "SetKeyRules", body: `{"id":` + q(w.victim.id) + `,"networks":["127.0.0.0/8"]}`},
		{method: "ReinstateKeys", body: `{"keyIds":[` + q(w.victim.id) + `]}`},
		{method: "CreateTenant", body: `{"name":"lab9","databaseType":"DATABASE_TYPE_SQLITE"}`},
		{method: "GetTenant", body: `{"name":"lab1"}`},
		{method: "GetTenant", variant: "other", body: `{"name":"lab2"}`},
		{method: "GetTenant", variant: "no-such-tenant", body: `{"name":"nosuch"}`},
		{method: "ListTenants", body: `{}`},
		{method: "GetSetting", body: `{"name":"retention.max-per-pass","scope":{"tenant":"lab1"}}`},
		{method: "GetSetting", variant: "installation", body: `{"name":"receiver.header-limit","scope":{"installation":{}}}`},
		{method: "GetSetting", variant: "no-such-setting", body: `{"name":"no.such.setting","scope":{"installation":{}}}`},
		{method: "SetSetting", body: `{"setting":{"name":"retention.max-per-pass","scope":{"tenant":"lab2"},"value":"500"}}`},
		{method: "UnsetSetting", body: `{"name":"retention.max-per-pass","scope":{"tenant":"lab2"}}`},
		{method: "ListSettings", body: `{"scope":{"installation":{}}}`},
		{method: "ListSettings", variant: "tenant", body: `{"scope":{"tenant":"lab1"}}`},
		{method: "ListAuditEntries", body: `{"scope":{"installation":{}}}`},
		{method: "ListAuditEntries", variant: "tenant", body: `{"scope":{"tenant":"lab1"},"pageSize":500}`},
		{method: "ListAuditEntries", variant: "refused", body: `{"scope":{"installation":{}},"outcome":"AUDIT_OUTCOME_REFUSED"}`},
		{method: "VerifyAudit", body: `{"scope":{"installation":{}}}`},
		{method: "ListAuditCheckpoints", body: `{"scope":{"installation":{}}}`},
		{method: "ExportAudit", body: `{"scope":{"installation":{}}}`},
		{method: "ExportAudit", variant: "tenant", body: `{"scope":{"tenant":"lab1"}}`},
		{method: "GetAuditPublicKey", body: `{}`},
		{method: "ExportTenant", body: `{"tenant":"lab1"}`},
		{method: "CreateBackup", body: `{}`},
		{method: "ReclaimTenant", body: `{"name":"lab1"}`},

		{method: "AddStorage", body: `{` + tn + `,"storage":{"name":"extra","directory":{"root":"fast","path":"extra"}}}`},
		{method: "GetStorage", body: `{` + tn + `,"name":"main"}`},
		{method: "GetStorage", variant: "raw", body: `{` + tn + `,"name":"raw"}`},
		{method: "ListStorages", body: `{` + tn + `}`},
		{method: "SetStorage", body: `{` + tn + `,"storage":{"name":"spare","capacityBytes":"1000000"},"updateMask":"capacityBytes"}`},
		{method: "TestStorage", body: `{` + tn + `,"name":"main"}`},
		{method: "AddReceiver", body: `{` + tn + `,"receiver":{"name":"extra","enabled":true,"storage":"main","dimse":{"port":11999,"aeTitle":"EXTRA"}}}`},
		{method: "GetReceiver", body: `{` + tn + `,"name":"scan-deid"}`},
		{method: "GetReceiver", variant: "raw", body: `{` + tn + `,"name":"scan-raw"}`},
		{method: "GetReceiver", variant: "stow", body: `{` + tn + `,"name":"web-deid"}`},
		{method: "ListReceivers", body: `{` + tn + `}`},
		{method: "SetReceiver", body: `{` + tn + `,"receiver":{"name":"scan-deid","enabled":true},"updateMask":"enabled"}`},
		{method: "RemoveReceiver", body: `{` + tn + `,"name":"no-such-receiver"}`},
		{method: "ListReceptions", body: `{` + tn + `}`},
		{method: "ListReceptions", variant: "by-state", body: `{` + tn + `,"steps":["RECEPTION_STEP_DONE","RECEPTION_STEP_FAILED"]}`},
		{method: "GetReception", variant: "no-such-reception", body: `{` + tn + `,"id":"r1"}`},
		{method: "GetScanMetadata", body: `{` + tn + `,` + label(d) + `}`},
		{method: "GetScanMetadata", variant: "raw", body: `{` + tn + `,` + label(r) + `}`},
		{method: "GetInstanceMetadata", body: `{` + tn + `,` + inst(d) + `}`},
		{method: "GetInstanceMetadata", variant: "raw", body: `{` + tn + `,` + inst(r) + `}`},
		{method: "ReadFiles", body: `{` + tn + `,` + label(d) + `}`, archived: true},
		{method: "ReadFiles", variant: "raw", body: `{` + tn + `,` + label(r) + `}`},
		{method: "ListSubjects", body: `{` + tn + `}`},
		{method: "GetSubject", body: `{` + tn + `,"subjectLabel":` + q(d.subject) + `}`},
		{method: "GetSubject", variant: "raw", body: `{` + tn + `,"subjectLabel":` + q(r.subject) + `}`},
		{method: "GetSubject", variant: "not-found", body: `{` + tn + `,"subjectLabel":"sub-nosuchsubject"}`},
		{method: "ListSessions", body: `{` + tn + `}`},
		{method: "GetSession", body: `{` + tn + `,"subjectLabel":` + q(d.subject) + `,"sessionLabel":` + q(d.session) + `}`},
		{method: "GetSession", variant: "raw", body: `{` + tn + `,"subjectLabel":` + q(r.subject) + `,"sessionLabel":` + q(r.session) + `}`},
		{method: "ListScans", body: `{` + tn + `}`},
		{method: "GetScan", body: `{` + tn + `,` + label(d) + `}`},
		{method: "GetScan", variant: "raw", body: `{` + tn + `,` + label(r) + `}`},
		{method: "ListQuarantinedFiles", body: `{` + tn + `}`},
		{method: "GetQuarantinedFile", variant: "no-such-file", body: `{` + tn + `,"id":"q1"}`},
		{method: "GetStatus", body: `{"scope":{"tenant":"lab1"}}`},
		{method: "GetStatus", variant: "installation", body: `{"scope":{"installation":{}}}`},
		{method: "AddSender", body: `{` + tn + `,"sender":{"name":"pacs2","enabled":true,"dimse":{"host":"127.0.0.1","port":1,"calledAeTitle":"PACS","callingAeTitle":"NOPACS"}}}`},
		{method: "GetSender", body: `{` + tn + `,"name":"pacs"}`},
		{method: "ListSenders", body: `{` + tn + `}`},
		{method: "SetSender", body: `{` + tn + `,"sender":{"name":"pacs","enabled":true},"updateMask":"enabled"}`},
		{method: "RemoveSender", body: `{` + tn + `,"name":"no-such-sender"}`},
		{method: "TestSender", body: `{` + tn + `,"name":"pacs"}`},
		{method: "CreateJob", variant: "send", body: `{` + tn + `,"send":{"sender":"pacs","subjectLabel":` + q(d.subject) + `}}`},
		{method: "CreateJob", variant: "deid-dry-run", body: `{` + tn + `,"deid":{"mode":"DEID_JOB_MODE_DRY_RUN","profile":"research","profileVersion":1}}`},
		{method: "CreateJob", variant: "scrub", body: `{` + tn + `,"scrub":{"storage":"main"}}`},
		{method: "GetJob", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "ListJobs", body: `{` + tn + `}`},
		{method: "CancelJob", variant: "no-such-job", body: `{` + tn + `,"id":"j1"}`},
		{method: "PauseJob", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "ResumeJob", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "RetryJob", variant: "no-such-job", body: `{` + tn + `,"id":"j1"}`},
		{method: "ListImportFiles", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "ListImportUnits", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "ListJobFailures", body: `{` + tn + `,"id":` + q(w.jobID) + `}`},
		{method: "Watch", body: `{"scope":{"tenant":"lab1"}}`},
		{method: "Watch", variant: "installation", body: `{"scope":{"installation":{}},"resume":true}`},
		{method: "CreateProfileVersion", body: `{` + tn + `,"name":"research","document":` + w.profile + `}`},
		{method: "CreateProfileVersion", variant: "no-lists", body: `{` + tn + `,"name":"agent-profile","document":` + basic + `}`},
		{method: "GetProfileVersion", body: `{` + tn + `,"name":"research"}`},
		{method: "GetProfileVersion", variant: "version-1", body: `{` + tn + `,"name":"research","version":1}`},
		{method: "ListProfiles", body: `{` + tn + `}`},
		{method: "ListProfileVersions", body: `{` + tn + `,"name":"research"}`},
		{method: "ValidateProfile", body: `{` + tn + `,"document":` + basic + `}`},
		{method: "ValidateProfile", variant: "lists", body: `{` + tn + `,"document":` + w.profile + `}`},
		{method: "TryProfile", body: `{` + tn + `,"profile":` + byDoc + `,"files":[` + file + `]}`},
		{method: "TryProfile", variant: "show-values", body: `{` + tn + `,"profile":` + byDoc + `,"files":[` + file + `],"showValues":true}`},
		{method: "TryProfile", variant: "stored", body: `{` + tn + `,"profile":` + byStored + `,"files":[` + file + `]}`},
		{method: "TryProfile", variant: "lists-document", body: `{` + tn + `,"profile":{"document":` + w.profile + `},"files":[` + file + `]}`},
		{method: "TryProfile", variant: "held-record", body: `{` + tn + `,"profile":` + byDoc + `,"quarantineRecords":[` + q(w.quarantined[0]) + `]}`},
		{method: "ExplainProfile", body: `{` + tn + `,"profile":` + byDoc + `,"file":` + file + `}`},
		{method: "ExplainProfile", variant: "stored", body: `{` + tn + `,"profile":` + byStored + `,"file":` + file + `}`},
		{method: "ExplainProfile", variant: "held-record", body: `{` + tn + `,"profile":` + byDoc + `,"quarantineRecord":` + q(w.quarantined[0]) + `}`},
		{method: "DiffProfileVersions", body: `{` + tn + `,"name":"research","fromVersion":1,"toVersion":1}`},
		{method: "DescribeProfile", body: `{` + tn + `,"profile":` + byDoc + `}`},
		{method: "DescribeProfile", variant: "stored", body: `{` + tn + `,"profile":` + byStored + `}`},
		{method: "Code", body: `{` + tn + `,"value":` + q(seedIDForCode) + `}`},
		{method: "ImportLookupCodes", body: `{` + tn + `,"table":"agent-table","entries":[{"value":"alpha","code":"A"}]}`},
		{method: "ListLookupTables", body: `{` + tn + `}`},
		{method: "Delete", body: `{` + tn + `,"target":{"subjectLabel":` + q(s.subject) + `},"reason":"x","validateOnly":true}`},
		{method: "DeleteBatch", body: `{` + tn + `,"targets":[{"subjectLabel":` + q(s.subject) + `}],"reason":"x","validateOnly":true}`},
		{method: "ListDeleteBatchTargets", body: `{` + tn + `,"jobId":` + q(w.jobID) + `}`},
		{method: "Relabel", body: `{` + tn + `,"target":{"subjectLabel":"sub-nosuchsubject"},"toLabel":"sub-other","reason":"x"}`},
		{method: "Replace", body: `{` + tn + `,"quarantinedFile":` + q(w.quarantined[0]) + `,"reason":"x"}`, last: true},
		{method: "Edit", body: `{` + tn + `,"target":{"subjectLabel":` + q(s.subject) + `},"changes":[{"attribute":"SeriesDescription","value":"x"}],"reason":"x","validateOnly":true}`},
		{method: "Move", body: `{` + tn + `,"subjectLabel":` + q(s.subject) + `,"sessionLabel":` + q(s.session) + `,"toSubjectLabel":"sub-nosuchsubject","reason":"x","validateOnly":true}`},
		{method: "Merge", body: `{` + tn + `,"subjectLabel":` + q(s.subject) + `,"intoSubjectLabel":"sub-nosuchsubject","reason":"x","validateOnly":true}`},
		{method: "ListCorrections", body: `{` + tn + `}`},
		{method: "GetCorrection", variant: "no-such-correction", body: `{` + tn + `,"id":` + ulid + `}`},
		{method: "UndoCorrection", body: `{` + tn + `,"id":` + ulid + `,"reason":"x"}`},
		{method: "PurgeCorrection", body: `{` + tn + `,"id":` + ulid + `}`},
		{method: "AllowArrivals", body: `{` + tn + `,"id":` + ulid + `,"reason":"x","value":{"kind":"HELD_VALUE_KIND_SUBJECT","value":"sub-nosuchsubject"}}`},
		{method: "ListScrubFindings", body: `{` + tn + `}`},
		{method: "ResolveInstances", body: `{` + tn + `,"members":[{"instanceId":` + q(d.instanceID) + `,"revision":1}]}`},
		{method: "ListReplicas", body: `{` + tn + `}`},
		{method: "AttachReplica", body: `{` + tn + `,"replica":"spare","primary":"main"}`},
		{method: "DetachReplica", body: `{` + tn + `,"replica":"spare"}`},
		{method: "Repair", body: `{` + tn + `,"storage":"main","all":true}`},
		{method: "SwitchStorage", body: `{` + tn + `,"primary":"main","replica":"spare","validateOnly":true}`},
		{method: "BackfillReplica", body: `{` + tn + `,"replica":"spare"}`},
		{method: "Recall", body: `{` + tn + `,"subjectLabel":` + q(d.subject) + `}`},
		{method: "VerifySample", body: `{` + tn + `,"storage":"main","count":1}`},
		{method: "AddRetentionRule", body: `{` + tn + `,"rule":{"name":"agent-rule","keepAtLeast":{"years":1}}}`},
		{method: "GetRetentionRule", body: `{` + tn + `,"name":"agent-rule"}`},
		{method: "ListRetentionRules", body: `{` + tn + `}`},
		{method: "SetRetentionRule", body: `{` + tn + `,"rule":{"name":"agent-rule"},"updateMask":"action"}`},
		{method: "RemoveRetentionRule", body: `{` + tn + `,"name":"agent-rule"}`},
		{method: "ListDueSessions", body: `{` + tn + `}`},
		{method: "Dispose", body: `{` + tn + `,"rule":"agent-rule","reason":"x","validateOnly":true}`},
		{method: "ConfirmClock", body: `{"scope":{"tenant":"lab1"},"observedTime":"` + now + `","reason":"x"}`},
		{method: "GetHold", variant: "no-such-hold", body: `{` + tn + `,"id":` + ulid + `}`},
		{method: "ListHolds", body: `{` + tn + `}`},
		{method: "PlaceHold", body: `{` + tn + `,"target":{"subjectLabel":` + q(s.subject) + `},"reason":"x"}`},
		{method: "ReleaseHold", body: `{` + tn + `,"id":` + ulid + `,"reason":"x"}`},
		{method: "CreateTransfer", body: `{` + tn + `,"subjectLabel":` + q(s.subject) + `,"targetTenant":"lab2","reason":"x","validateOnly":true}`},
		{method: "GetTransfer", body: `{` + tn + `,"id":` + ulid + `}`},
		{method: "ListTransfers", body: `{` + tn + `}`},
		{method: "Search", body: `{` + tn + `,"query":"Modality present","count":true}`},
		{method: "Search", variant: "sessions", body: `{` + tn + `,"level":"SEARCH_LEVEL_SESSION","query":"has scan (Modality present)","facet":"Modality"}`},
		{method: "ListSearchAttributes", body: `{` + tn + `}`},
		{method: "BackfillAttribute", body: `{` + tn + `,"attribute":"Modality"}`},
		{method: "CreateRuleSet", body: `{` + tn + `,"receiver":"no-such-receiver","ruleSet":{}}`},
		{method: "GetRuleSet", body: `{` + tn + `,"receiver":"scan-deid"}`},
		{method: "ListRuleSets", body: `{` + tn + `,"receiver":"scan-deid"}`},
		{method: "ActivateRuleSet", body: `{` + tn + `,"receiver":"no-such-receiver","version":1}`},
		{method: "ExplainRoute", body: `{` + tn + `,"receiver":"scan-deid"}`},
		{method: "DryRunRoutes", body: `{` + tn + `,"receiver":"scan-deid"}`},
		{method: "GetRouteHits", body: `{` + tn + `,"receiver":"scan-deid"}`},
	}
	for i, id := range w.receptions {
		v := fmt.Sprintf("reception-%d", i)
		c = append(c,
			apiCall{method: "GetReception", variant: v, body: `{` + tn + `,"id":` + q(id) + `}`},
			apiCall{method: "RetryReception", variant: v, body: `{` + tn + `,"id":` + q(id) + `}`, last: true},
			apiCall{method: "DiscardReception", variant: v, body: `{` + tn + `,"id":` + q(id) + `,"reason":"x"}`, last: true})
	}
	for i, id := range w.quarantined {
		v := fmt.Sprintf("file-%d", i)
		c = append(c,
			apiCall{method: "GetQuarantinedFile", variant: v, body: `{` + tn + `,"id":` + q(id) + `}`},
			apiCall{method: "GetQuarantinedFile", variant: v + "-header", body: `{` + tn + `,"id":` + q(id) + `,"includeHeader":true}`},
			apiCall{method: "GetQuarantinedFile", variant: v + "-values", body: `{` + tn + `,"id":` + q(id) + `,"includeHeader":true,"showValues":true}`})
	}
	c = append(c,
		apiCall{method: "ReleaseQuarantinedFile", body: `{` + tn + `,"id":` + q(w.quarantined[0]) + `,"mode":"RELEASE_MODE_AS_IS"}`, last: true},
		apiCall{method: "RejectQuarantinedFile", body: `{` + tn + `,"id":` + q(w.quarantined[1]) + `,"reason":"x"}`, last: true},
		apiCall{method: "CancelJob", body: `{` + tn + `,"id":` + q(w.jobID) + `}`, last: true},
		apiCall{method: "RetryJob", body: `{` + tn + `,"id":` + q(w.jobID) + `}`, last: true},
		// The revoking and the removal of what the test made for them come
		// last, so every other call of the key has seen them.
		apiCall{method: "RevokeKey", body: `{"id":` + q(w.victim.id) + `}`, last: true},
		apiCall{method: "RevokeKey", variant: "not-masked", body: `{"id":` + q(w.plainVictim.id) + `}`, last: true},
		apiCall{method: "RemoveTenant", variant: "wrong-name", body: `{"name":"lab1","confirmName":"lab"}`, last: true},
		apiCall{method: "RemoveTenant", body: `{"name":"lab2","confirmName":"lab2"}`, last: true},
		apiCall{method: "RemoveStorage", body: `{` + tn + `,"name":"spare"}`, last: true},
		apiCall{method: "GetHold", variant: "placed", body: `{` + tn + `,"id":` + q(w.holdID) + `}`})
	return c
}

// seedIDForCode is the Patient ID of the first seeded file, asked of the
// forward lookup, which a masked key is refused: the value goes into the
// request, and neither the answer nor any record of the request may hold it.
var seedIDForCode = func() string { _, id, _, _, _, _, _, _, _ := seedValues(1); return id }()
