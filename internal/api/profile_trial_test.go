package api_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The values of the corpus file ct5n-2062.dcm a trial run is checked by:
// as pydicom reads them, padding removed.
const (
	ctName = "Doe^Peter"
	ctDate = "20010101"
	ctUID  = "1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.12"
	ctID   = "98890234"
)

// storedVersion names a stored version for a trial.
func storedVersion(name string, version int32) *v1.TrialProfile {
	return &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: name, Version: version}}}
}

// unsaved names a document for a trial.
func unsaved(doc *v1.ProfileDocument) *v1.TrialProfile {
	return &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: doc}}
}

// tryFiles is a TryProfile call into lab1 with files sent.
func tryFiles(p *v1.TrialProfile, files ...[]byte) func(clients) outcome {
	return func(c clients) outcome {
		req := &v1.TryProfileRequest{Tenant: "lab1", Profile: p}
		for i, b := range files {
			req.Files = append(req.Files, &v1.TrialFile{Name: fmt.Sprintf("file%d.dcm", i), Content: b})
		}
		return call(c.profiles.TryProfile, req)
	}
}

// tryShowing is tryFiles asking for the values (show_values).
func tryShowing(p *v1.TrialProfile, files ...[]byte) func(clients) outcome {
	return func(c clients) outcome {
		req := &v1.TryProfileRequest{Tenant: "lab1", Profile: p, ShowValues: true}
		for i, b := range files {
			req.Files = append(req.Files, &v1.TrialFile{Name: fmt.Sprintf("file%d.dcm", i), Content: b})
		}
		return call(c.profiles.TryProfile, req)
	}
}

// elementAt returns the element at path of a result, or nil.
func elementAt(r *v1.TrialResult, path string) *v1.TrialElement {
	for _, e := range r.GetElements() {
		if e.GetPath() == path {
			return e
		}
	}
	return nil
}

// tenantState is what a trial run must leave as it found it in a tenant's
// database: its profiles' versions, receptions and events, as counts.
func tenantState(t *testing.T, e *env) string {
	t.Helper()
	p := e.pipelineOf("lab1")
	profiles, _, err := p.(tenant.Profiles).ListProfiles(ctx, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	versions := 0
	for _, s := range profiles {
		versions += int(s.LatestVersion)
	}
	receptions, _, err := p.ListReceptions(ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	events, err := p.(tenant.Events).ReadEvents(ctx, "", nil, tenant.MaxEventPage)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("profiles=%d versions=%d receptions=%d events=%d", len(profiles), versions, len(receptions), len(events.Events))
}

// testTryProfile guards TryProfile: operator on the tenant; a corpus file
// answered element by element, with show_values, with its values before
// and after (a name
// emptied, a date emptied, a UID remapped), the rule or base and reason of
// each, and the verdict; the tenant's database as it was (no version, no
// reception, no event, no lookup code: a lookup answers UNASSIGNED each
// time); no value of the file in the server's log or the audit entry,
// which holds the counts; an unsaved document tried the same way; a rule
// that quarantines answered as held with its reason; a held file read by
// its record; and the bounds of a request.
func testTryProfile(t *testing.T) {
	f := newPlaceFixture(t)
	storeVersion(t, f.env, "research", basicDocument())
	ct := corpusFile(t, "ct5n-2062.dcm")
	before := tenantState(t, f.env)
	var requestID string
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: tryFiles(storedVersion("research", 1), ct), code: connect.CodePermissionDenied},
		{name: "no grant", as: f.noGrant, call: tryFiles(storedVersion("research", 1), ct), code: connect.CodePermissionDenied},
		{name: "no file", as: f.lab1Operator, call: tryFiles(storedVersion("research", 1)), code: connect.CodeInvalidArgument, msg: "files"},
		{name: "no profile", as: f.lab1Operator, call: tryFiles(nil, ct), code: connect.CodeInvalidArgument, msg: "profile"},
		{name: "no such version", as: f.lab1Operator, call: tryFiles(storedVersion("research", 4), ct), code: connect.CodeNotFound},
		{name: "an empty file", as: f.lab1Operator, call: tryFiles(storedVersion("research", 1), []byte{}), code: connect.CodeInvalidArgument, msg: "content"},
		{name: "operator, a corpus file", as: f.lab1Operator, call: tryShowing(storedVersion("research", 0), ct),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research@1",
				details: map[string]string{"files": "1", "passed": "1", "held": "0", "checksum": "*", "by_rule": "*", "by_reason": "", "show_values": "true"}},
			check: func(t *testing.T, o outcome) {
				requestID = o.requestID
				res := o.res.(*v1.TryProfileResponse)
				if res.GetVersion().GetVersion() != 1 || res.GetReceptionRefusal() != "" || len(res.GetResults()) != 1 {
					t.Fatalf("answer %v", res)
				}
				r := res.GetResults()[0]
				if r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_PASSED || r.GetName() != "file0.dcm" || r.GetValuesSearched() == 0 {
					t.Errorf("result %v", r)
				}
				name, date, uid := elementAt(r, "00100010"), elementAt(r, "00080020"), elementAt(r, "00080018")
				if name.GetBefore() != ctName || name.GetAfter() != "" || !name.GetChanged() || name.GetKeyword() != "PatientName" || name.GetReason() == "" {
					t.Errorf("the name %v", name)
				}
				if date.GetBefore() != ctDate || date.GetAfter() == ctDate || date.GetOrigin() != "annex-e/basic:2026d" || date.GetRule() == 0 {
					t.Errorf("the date %v", date)
				}
				if uid.GetBefore() != ctUID || !strings.HasPrefix(uid.GetAfter(), "2.25.") || uid.GetAction() != "uid" || !uid.GetPresentAfter() {
					t.Errorf("the UID %v", uid)
				}
				if s := res.GetSummary(); s.GetFiles() != 1 || s.GetPassed() != 1 || len(s.GetByRule()) == 0 {
					t.Errorf("summary %v", s)
				}
			}},
		{name: "an unsaved document", as: f.lab1Operator, call: tryFiles(unsaved(keepStudyID()), ct),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.TryProfileResponse)
				if res.GetVersion() != nil || len(res.GetChecksum()) != 64 || res.GetResults()[0].GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_PASSED {
					t.Errorf("answer %v", res)
				}
			}},
	})
	// No value of the file reaches the server's log or the audit entry.
	f.flush()
	entries := entriesOf(f.tenantLog("lab1"), requestID)
	for _, v := range []string{ctName, ctDate, ctUID, ctID, "Peter"} {
		if strings.Contains(f.log.String(), v) {
			t.Errorf("the server's log holds %q", v)
		}
		if strings.Contains(fmt.Sprintf("%+v", entries), v) {
			t.Errorf("the audit entry holds %q", v)
		}
	}
	if !strings.Contains(f.log.String(), "a profile was tried") {
		t.Error("no log line says a profile was tried")
	}

	// A lookup assigns nothing: the same value is UNASSIGNED each time.
	lookup := keepStudyID()
	lookup.Rules = append(lookup.Rules, &v1.ProfileRule{Name: "code-id",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "studies",
			OnMissing: v1.LookupMissing_LOOKUP_MISSING_ASSIGN}}}})
	for i := range 2 {
		res := mustCall[*v1.TryProfileResponse](t, "lookup", tryShowing(unsaved(lookup), ct)(f.as(f.lab1Operator)))
		r := res.GetResults()[0]
		if e := elementAt(r, "00200010"); e.GetAfter() != "UNASSIGNED" || r.GetLookupsUnassigned() != 1 {
			t.Errorf("try %d: the study id %v, %d unassigned", i, e, r.GetLookupsUnassigned())
		}
	}
	if after := tenantState(t, f.env); after != before {
		t.Errorf("the tenant's database changed: %s, then %s", before, after)
	}

	// A file the check after holds: residual_value, its kind and element,
	// and the values before still answered; nothing after. The MR's Study
	// ID, which the profile keeps, is its Patient ID "4MR1".
	mr := corpusFile(t, "MR_small.dcm")
	if r := mustCall[*v1.TryProfileResponse](t, "residual", tryShowing(unsaved(keepStudyID()), mr)(f.as(f.lab1Operator))).GetResults()[0]; r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_HELD ||
		r.GetReason() != "residual_value" || r.GetCause() != "identifier" || r.GetPath() != "00200010" ||
		elementAt(r, "00100010").GetBefore() != "CompressedSamples^MR1" || elementAt(r, "00100010").GetPresentAfter() {
		t.Errorf("a residual value: %v", r)
	}

	// A rule that quarantines: held, with its reason, and no value after.
	refuse := basicDocument()
	refuse.Rules = []*v1.ProfileRule{{Name: "no-ct",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientID"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Quarantine{Quarantine: &v1.QuarantineAction{}}}}}
	res := mustCall[*v1.TryProfileResponse](t, "refused", tryFiles(unsaved(refuse), ct)(f.as(f.lab1Operator)))
	if r := res.GetResults()[0]; r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_HELD || r.GetReason() != "profile_refused" || r.GetRule() != "no-ct" ||
		len(r.GetElements()) != 0 || res.GetSummary().GetByReason()[0].GetKey() != "profile_refused/rule" {
		t.Errorf("a refused file: %v", r)
	}

	// A held file, read by its record; tried with its reception's facts.
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	o := call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: []string{held}})
	if o.code() != connect.CodeUnavailable {
		t.Errorf("a held file that cannot be read: %v", o.err)
	}
	read := func(tenant.QuarantinedFile) ([]byte, error) { return ct, nil }
	f.parts.heldFile.Store(&read)
	got := mustCall[*v1.TryProfileResponse](t, "held", call(f.as(f.lab1Operator).profiles.TryProfile,
		&v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: []string{held}, ShowValues: true}))
	if r := got.GetResults()[0]; r.GetQuarantineRecord() != held || r.GetName() != "" || elementAt(r, "00100010").GetBefore() != ctName {
		t.Errorf("a held file: %v", r)
	}
	if o := call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: []string{"nope"}}); o.code() != connect.CodeNotFound {
		t.Errorf("a record the tenant does not have: %v", o.err)
	}
	many := make([][]byte, 33)
	for i := range many {
		many[i] = ct[:10]
	}
	if o := tryFiles(storedVersion("research", 1), many...)(f.as(f.lab1Operator)); o.code() != connect.CodeInvalidArgument {
		t.Errorf("33 files: %v", o.err)
	}
}

// testExplainProfile guards ExplainProfile: operator on the tenant; for
// each element of a file the deciding rule, its origin and how it selects
// the element, and the other rules that match with why they do not decide;
// no value of the file in the answer; one file or one record, exactly.
func testExplainProfile(t *testing.T) {
	f := newFixture(t)
	storeVersion(t, f.env, "research", keepStudyID())
	ct := corpusFile(t, "ct5n-2062.dcm")
	explain := func(file []byte, record string) func(clients) outcome {
		return func(c clients) outcome {
			req := &v1.ExplainProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecord: record}
			if file != nil {
				req.File = &v1.TrialFile{Content: file}
			}
			return call(c.profiles.ExplainProfile, req)
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: explain(ct, ""), code: connect.CodePermissionDenied},
		{name: "neither", as: f.lab1Operator, call: explain(nil, ""), code: connect.CodeInvalidArgument, msg: "exactly one"},
		{name: "both", as: f.lab1Operator, call: explain(ct, "id-1"), code: connect.CodeInvalidArgument, msg: "exactly one"},
		{name: "operator", as: f.lab1Operator, call: explain(ct, ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research@1"},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.ExplainProfileResponse)
				var desc, name *v1.ExplainedElement
				for _, e := range res.GetElements() {
					switch e.GetPath() {
					case "00200010":
						desc = e
					case "00100010":
						name = e
					}
				}
				if desc.GetRuleName() != "keep-study-id" || desc.GetOrigin() != "" || desc.GetSelectedBy() != "tag" || desc.GetAction() != "keep" {
					t.Errorf("the study id %v", desc)
				}
				lost := false
				for _, other := range desc.GetOthers() {
					lost = lost || (other.GetOrigin() == "annex-e/basic:2026d" && other.GetWhyNot() == "earlier")
				}
				if !lost {
					t.Errorf("the basic profile's rule for the study id is not among the others: %v", desc.GetOthers())
				}
				if name.GetRule() == 0 || name.GetOrigin() == "" || name.GetReason() == "" {
					t.Errorf("the name %v", name)
				}
				text := fmt.Sprintf("%v", res)
				for _, v := range []string{ctName, ctDate, ctUID} {
					if strings.Contains(text, v) {
						t.Errorf("an explanation holds the value %q", v)
					}
				}
			}},
	})
}

// TestExplanationIsBounded guards the bound of an explanation's other
// rules: an element twenty own rules and a base's rule match answers the
// first sixteen others and counts the rest.
func TestExplanationIsBounded(t *testing.T) {
	f := newFixture(t)
	doc := basicDocument()
	for i := range 20 {
		doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: fmt.Sprintf("keep-%d", i),
			Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
			Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}})
	}
	res := mustCall[*v1.ExplainProfileResponse](t, "explain", call(f.as(f.lab1Operator).profiles.ExplainProfile, &v1.ExplainProfileRequest{
		Tenant: "lab1", Profile: unsaved(doc), File: &v1.TrialFile{Content: corpusFile(t, "ct5n-2062.dcm")}}))
	for _, e := range res.GetElements() {
		if e.GetPath() == "00200010" {
			if e.GetRuleName() != "keep-19" || len(e.GetOthers()) != 16 || e.GetOthersOmitted() != 4 {
				t.Errorf("decided by %s, %d others, %d left out", e.GetRuleName(), len(e.GetOthers()), e.GetOthersOmitted())
			}
			return
		}
	}
	t.Error("no explanation of Study ID")
}

// testDiffProfileVersions guards DiffProfileVersions: operator on the
// tenant; a rule added answered with its position and text and its effect
// (the study id kept where the basic profile empties it); equal
// versions answered with the same effect and nothing listed; a base added
// listed as a base, not rule by rule.
func testDiffProfileVersions(t *testing.T) {
	f := newFixture(t)
	storeVersion(t, f.env, "research", basicDocument())
	storeVersion(t, f.env, "research", keepStudyID())
	storeVersion(t, f.env, "longitudinal", &v1.ProfileDocument{Bases: []string{"annex-e/basic:2026d", "annex-e/retain-longitudinal-full-dates:2026d"}})
	diff := func(from, to int32, toName string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.DiffProfileVersions, &v1.DiffProfileVersionsRequest{Tenant: "lab1", Name: "research", FromVersion: from, ToVersion: to, ToName: toName})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: diff(1, 2, ""), code: connect.CodePermissionDenied},
		{name: "a rule added", as: f.lab1Operator, call: diff(1, 2, ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research", details: map[string]string{"from_version": "1", "to_version": "2"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.DiffProfileVersionsResponse)
				if res.GetSameEffect() || len(res.GetRules()) != 1 || res.GetRules()[0].GetChange() != v1.ProfileChange_PROFILE_CHANGE_ADDED ||
					res.GetRules()[0].GetTo().GetName() != "keep-study-id" || res.GetFrom().GetVersion() != 1 || res.GetTo().GetVersion() != 2 {
					t.Errorf("diff %v", res)
				}
				if len(res.GetEffects()) != 1 || res.GetEffects()[0].GetTo().GetKeep() == nil || res.GetEffects()[0].GetFrom() == nil {
					t.Errorf("effects %v", res.GetEffects())
				}
			}},
		{name: "the same version", as: f.lab1Operator, call: diff(1, 1, ""), check: func(t *testing.T, o outcome) {
			if res := o.res.(*v1.DiffProfileVersionsResponse); !res.GetSameEffect() || len(res.GetRules()) != 0 || len(res.GetEffects()) != 0 {
				t.Errorf("diff %v", res)
			}
		}},
		{name: "another profile, a base added", as: f.lab1Operator, call: diff(1, 1, "longitudinal"), check: func(t *testing.T, o outcome) {
			res := o.res.(*v1.DiffProfileVersionsResponse)
			if len(res.GetBasesAdded()) != 1 || len(res.GetRules()) != 0 || len(res.GetEffects()) == 0 {
				t.Errorf("diff: bases %v, %d rules, %d effects", res.GetBasesAdded(), len(res.GetRules()), len(res.GetEffects()))
			}
		}},
		{name: "no such version", as: f.lab1Operator, call: diff(1, 9, ""), code: connect.CodeNotFound},
		{name: "a negative version", as: f.lab1Operator, call: diff(-1, 1, ""), code: connect.CodeInvalidArgument},
	})
}

// hashStudyID is a document on the basic profile whose own rules hash
// Patient ID and give Study ID its table's code, assigning missing ones.
func hashStudyID() *v1.ProfileDocument {
	doc := basicDocument()
	doc.Rules = []*v1.ProfileRule{
		{Name: "hash-id", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientID"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}},
		{Name: "code-study", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "studies", OnMissing: v1.LookupMissing_LOOKUP_MISSING_ASSIGN}}}},
	}
	return doc
}

// TestTrialUsesNoTenantSecret guards that a trial runs under keys of its
// own and reads no lookup table: a Study ID whose code the tenant's table
// holds is answered UNASSIGNED, the remapped Study Instance UID differs
// from the tenant's remapping (what a receiver archives), and the hash of
// Patient ID differs from the tenant's keyed hash of it (the subject's
// label code); two trials of one file differ in every keyed value; and a
// lookup that holds a file whose value has no code makes the verdict
// undecided, not decided by reading the table.
func TestTrialUsesNoTenantSecret(t *testing.T) {
	f := newFixture(t)
	ct := corpusFile(t, "ct5n-2062.dcm")
	tenantKeys, err := f.registry.DeidKeys(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := f.registry.NamingHasher(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	// The tenant's table holds a code for this Study ID, made as a
	// receiver makes its key: the tenant's keyed hash of the value.
	if _, err := f.pipelineOf("lab1").(tenant.LookupTables).AssignLookupCodes(ctx,
		[]tenant.LookupKey{{Table: "studies", Key: hasher.Hash([]byte("2"))}}, f.now()); err != nil {
		t.Fatal(err)
	}
	try := func() *v1.TrialResult {
		t.Helper()
		res := mustCall[*v1.TryProfileResponse](t, "try", tryShowing(unsaved(hashStudyID()), ct)(f.as(f.lab1Operator)))
		return res.GetResults()[0]
	}
	r1, r2 := try(), try()
	if e := elementAt(r1, "00200010"); e.GetAfter() != "UNASSIGNED" {
		t.Errorf("a Study ID the table holds a code for: %v", e)
	}
	study := elementAt(r1, "0020000D").GetAfter()
	if study == "" || study == engine.RemapUID(tenantKeys.UID, ctStudyUID) {
		t.Errorf("the trial's Study Instance UID %q is the tenant's remapping", study)
	}
	id := elementAt(r1, "00100020").GetAfter()
	if id == "" || id == hasher.Hash([]byte(ctID)) || strings.EqualFold(id, hasher.Hash([]byte(ctID))) {
		t.Errorf("the trial's hash of Patient ID %q is the subject's label code", id)
	}
	if elementAt(r2, "0020000D").GetAfter() == study || elementAt(r2, "00100020").GetAfter() == id {
		t.Error("two trials of one file gave the same keyed values: the keys are not the request's own")
	}

	quarantineMissing := basicDocument()
	quarantineMissing.Rules = []*v1.ProfileRule{{Name: "code-study", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "studies", OnMissing: v1.LookupMissing_LOOKUP_MISSING_QUARANTINE}}}}}
	res := mustCall[*v1.TryProfileResponse](t, "undecided", tryFiles(unsaved(quarantineMissing), ct)(f.as(f.lab1Operator)))
	if r := res.GetResults()[0]; r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_UNDECIDED || r.GetCause() != "lookup_table" || res.GetSummary().GetUndecided() != 1 {
		t.Errorf("a lookup that would hold a file without a code: %v", r)
	}
}

// ctStudyUID is ct5n-2062.dcm's Study Instance UID, as pydicom reads it.
const ctStudyUID = "1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.1"

// TestTrialRunsTheReceiversChecks guards that a trial naming a receiver
// reaches the verdict that receiver would: a SOP class it does not take,
// a placing UID under the standard's root and a naming rule the file
// cannot meet are held as at reception, where the same files tried with
// no receiver pass with the answer saying the verdict is
// de-identification's alone; and a receiver the tenant does not have is
// not found.
func TestTrialRunsTheReceiversChecks(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	storeVersion(t, f.env, "research", basicDocument())
	mrOnly := dimseReceiver("mr-only", "main", 11120)
	mrOnly.GetDimse().SopClasses = []string{"1.2.840.10008.5.1.4.1.1.4"}
	mustCall[*v1.AddReceiverResponse](t, "add mr-only", addReceiver("lab1", mrOnly)(f.as(local)))
	named := dimseReceiver("named", "main", 11121)
	named.Naming = &v1.NamingRule{Subject: "sub-{#PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}",
		Pattern: &v1.NamingPattern{Attribute: "PatientComments", Expression: "(?P<site>[A-Z]+)-.*"}}
	named.Naming.Subject = "sub-{site}{#PatientID}"
	mustCall[*v1.AddReceiverResponse](t, "add named", addReceiver("lab1", named)(f.as(local)))

	ct := corpusFile(t, "ct5n-2062.dcm")
	root := "1.2.840.10008.99."
	root += strings.Repeat("1", len(ctStudyUID)-len(root))
	underRoot := bytes.ReplaceAll(ct, []byte(ctStudyUID), []byte(root))
	try := func(receiver string, file []byte) *v1.TrialResult {
		t.Helper()
		res := mustCall[*v1.TryProfileResponse](t, "try at "+receiver, call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{
			Tenant: "lab1", Profile: storedVersion("research", 1), Files: []*v1.TrialFile{{Content: file}},
			Reception: &v1.TrialReception{Receiver: receiver}}))
		return res.GetResults()[0]
	}
	for _, c := range []struct {
		receiver string
		file     []byte
		reason   string
		// alone is the cause with no receiver; "" for passed.
		alone string
	}{
		{"mr-only", ct, "sop_class_not_accepted", ""},
		// The engine keeps a UID the standard registers, so alone the file
		// is held later, for a UID not remapped.
		{"scanner1", underRoot, "bad_uid", "uid_not_remapped"},
		{"named", ct, "bad_label", ""},
	} {
		r := try(c.receiver, c.file)
		if r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_HELD || r.GetReason() != c.reason || r.GetReceiver() != c.receiver || r.GetDeidentificationOnly() {
			t.Errorf("at %s: %v %s/%s receiver %q", c.receiver, r.GetVerdict(), r.GetReason(), r.GetCause(), r.GetReceiver())
		}
		alone := try("", c.file)
		if alone.GetCause() != c.alone || (c.alone == "") != (alone.GetVerdict() == v1.TrialVerdict_TRIAL_VERDICT_PASSED) || !alone.GetDeidentificationOnly() {
			t.Errorf("with no receiver: %v %s, de-identification only %v", alone.GetVerdict(), alone.GetReason(), alone.GetDeidentificationOnly())
		}
	}
	if r := try("scanner1", ct); r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_PASSED || r.GetReceiver() != "scanner1" {
		t.Errorf("a file scanner1 takes: %v %s", r.GetVerdict(), r.GetReason())
	}
	o := call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1),
		Files: []*v1.TrialFile{{Content: ct}}, Reception: &v1.TrialReception{Receiver: "nope"}})
	if o.code() != connect.CodeNotFound {
		t.Errorf("a receiver the tenant does not have: %v", o.err)
	}
}

// holdSized holds a record in tenantName with its bytes kept, of size
// bytes, and returns its id.
func holdSized(t *testing.T, f *placeFixture, tenantName string, size int64) string {
	t.Helper()
	id := f.newID()
	q := tenant.QuarantinedFile{
		ID: id, Reason: tenant.QuarantineBadLabel, Receiver: "scanner1",
		Details: map[string]string{"label": "subject", "attribute": "PatientID", "fault": "missing"},
		Size:    size, SHA256: sha256.Sum256([]byte(id)), SOPClassUID: "1.2.840.10008.5.1.4.1.1.2",
		Incoming: "q/" + id, ContentKept: true, Quarantined: f.now(),
	}
	if _, err := f.pipelineOf(tenantName).HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestTrialBounds guards what a trial may hold in memory: held files
// together past 128 MiB, judged from their records before any is read, are
// a precondition failure with nothing read; another tenant's record is not
// found; and with as many trials running as the server runs at once, a
// trial waits briefly and is refused as exhausted, then runs once one
// ends.
func TestTrialBounds(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	storeVersion(t, f.env, "research", basicDocument())
	var reads atomic.Int64
	read := func(tenant.QuarantinedFile) ([]byte, error) { reads.Add(1); return corpusFile(t, "ct5n-2062.dcm"), nil }
	f.parts.heldFile.Store(&read)
	big := []string{holdSized(t, f, "lab1", 50<<20), holdSized(t, f, "lab1", 50<<20), holdSized(t, f, "lab1", 50<<20)}
	o := call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: big})
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "128 MiB") || reads.Load() != 0 {
		t.Errorf("held files past 128 MiB: %v, %d read", o.err, reads.Load())
	}
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab2", dirStorage("main", "fast", "mri2"))(f.as(local)))
	other := holdSized(t, f, "lab2", 1000)
	o = call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: []string{other}})
	if o.code() != connect.CodeNotFound || reads.Load() != 0 {
		t.Errorf("another tenant's record: %v", o.err)
	}

	release := api.HoldTrialPlaces(f.handlers, api.MaxTrialsAtOnce)
	o = tryFiles(storedVersion("research", 1), corpusFile(t, "ct5n-2062.dcm"))(f.as(f.lab1Operator))
	if o.code() != connect.CodeResourceExhausted {
		t.Errorf("with every place taken: %v", o.err)
	}
	release()
	if o := tryFiles(storedVersion("research", 1), corpusFile(t, "ct5n-2062.dcm"))(f.as(f.lab1Operator)); o.err != nil {
		t.Errorf("once the places are free: %v", o.err)
	}
}

// TestATrialsErrorCarriesNoCause guards what a trial's failure is logged
// and answered with: its kind and its fixed message, never the error it
// wraps, which a parser may have written a file's value into.
func TestATrialsErrorCarriesNoCause(t *testing.T) {
	wrapped := fault.Wrap(fault.Internal, errors.New("element 00100010 holds "+ctName), "a de-identified file could not be written")
	got := api.SafeTrial(wrapped)
	if strings.Contains(got.Error(), ctName) || fault.KindOf(got) != fault.Internal || fault.Message(got) != "a de-identified file could not be written" {
		t.Errorf("%q, kind %v", got.Error(), fault.KindOf(got))
	}
}
