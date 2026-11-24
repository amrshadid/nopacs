package api_test

import (
	"errors"
	"testing"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// basicDocument is a document on the Annex E basic profile alone.
func basicDocument() *v1.ProfileDocument {
	return &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}
}

// keepStudyID is basicDocument with one rule of its own: Study ID
// (0020,0010) kept, which the basic profile empties.
func keepStudyID() *v1.ProfileDocument {
	doc := basicDocument()
	doc.Rules = []*v1.ProfileRule{{Name: "keep-study-id", Reason: "the study's number at the site is needed",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}}}
	return doc
}

// createVersion is a CreateProfileVersion call into lab1.
func createVersion(name string, doc *v1.ProfileDocument, expect *int32) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.profiles.CreateProfileVersion, &v1.CreateProfileVersionRequest{Tenant: "lab1", Name: name, Document: doc, ExpectedLatestVersion: expect, Note: "first"})
	}
}

// problemsOf returns the ProfileProblems detail of an error, or nil.
func problemsOf(err error) *v1.ProfileProblems {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil
	}
	for _, d := range ce.Details() {
		m, err := connectproto.UnmarshalErrorDetail(d)
		if p, ok := m.(*v1.ProfileProblems); ok && err == nil {
			return p
		}
	}
	return nil
}

// storeVersion stores doc as the next version of the profile name in
// lab1 through the port, as a setup, and returns its checksum.
func storeVersion(t *testing.T, e *env, name string, doc *v1.ProfileDocument) string {
	t.Helper()
	v, err := profile.New(name, 1, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.pipelineOf("lab1").(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{
		Name: name, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum(), Note: "first"}, e.now(), e.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	return v.Checksum()
}

// testCreateProfileVersion guards CreateProfileVersion: admin on the tenant
// only; a document with a problem refused with every problem as a
// ProfileProblems detail; an expectation of the latest version that does
// not hold refused as a precondition; the version made and audited with
// its number, its creation and its checksum; the same document again
// answered with the latest and no version made.
func testCreateProfileVersion(t *testing.T) {
	f := newFixture(t)
	zero, one := int32(0), int32(1)
	bad := basicDocument()
	bad.Bases = append(bad.Bases, "annex-e/nothing:2026d")
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: createVersion("research", basicDocument(), nil), code: connect.CodeUnauthenticated},
		{name: "operator", as: f.lab1Operator, call: createVersion("research", basicDocument(), nil), code: connect.CodePermissionDenied},
		{name: "no grant", as: f.noGrant, call: createVersion("research", basicDocument(), nil), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "a bad name", as: f.lab1Admin, call: createVersion("Research", basicDocument(), nil), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "no document", as: f.lab1Admin, call: createVersion("research", nil, nil), code: connect.CodeInvalidArgument, msg: "document: is required",
			audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a document with a problem", as: f.lab1Admin, call: createVersion("research", bad, nil), code: connect.CodeInvalidArgument, msg: "bases[2]",
			check: func(t *testing.T, o outcome) {
				if p := problemsOf(o.err); len(p.GetProblems()) == 0 || p.GetProblems()[0].GetField() != "bases[2]" {
					t.Errorf("the problems detail: %v", p)
				}
			}},
		{name: "expects a version that is not there", as: f.lab1Admin, call: createVersion("research", basicDocument(), &one), code: connect.CodeFailedPrecondition},
		{name: "admin", as: f.lab1Admin, call: createVersion("research", basicDocument(), &zero),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research", details: map[string]string{"version": "1", "created": "true", "checksum": "*"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.CreateProfileVersionResponse)
				if !res.GetCreated() || res.GetVersion().GetVersion() != 1 || len(res.GetVersion().GetChecksum()) != 64 || res.GetVersion().GetExpanded() != nil ||
					!proto.Equal(res.GetVersion().GetDocument(), basicDocument()) {
					t.Errorf("created %v", res)
				}
			}},
		{name: "the same document again", as: f.lab1Admin, call: createVersion("research", basicDocument(), nil),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"version": "1", "created": "false"}},
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.CreateProfileVersionResponse); res.GetCreated() || res.GetVersion().GetVersion() != 1 || res.GetNoteDiffers() {
					t.Errorf("again %v", res)
				}
			}},
		{name: "a second version", as: f.lab1Admin, call: createVersion("research", keepStudyID(), &one),
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.CreateProfileVersionResponse); !res.GetCreated() || res.GetVersion().GetVersion() != 2 {
					t.Errorf("second %v", res)
				}
			}},
	})
}

// testGetProfileVersion guards GetProfileVersion: operator on the tenant;
// the document answered as given (the profile's export), the expanded form
// only in full, with each rule's origin; zero for the latest; a version
// the tenant does not have not found.
func testGetProfileVersion(t *testing.T) {
	f := newFixture(t)
	checksum := storeVersion(t, f.env, "research", basicDocument())
	storeVersion(t, f.env, "research", keepStudyID())
	get := func(name string, version int32, view v1.ProfileView) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.GetProfileVersion, &v1.GetProfileVersionRequest{Tenant: "lab1", Name: name, Version: version, View: view})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: get("research", 1, 0), code: connect.CodePermissionDenied},
		{name: "operator exports", as: f.lab1Operator, call: get("research", 1, 0),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research", details: map[string]string{"version": "1"}},
			check: func(t *testing.T, o outcome) {
				v := o.res.(*v1.GetProfileVersionResponse).GetVersion()
				if !proto.Equal(v.GetDocument(), basicDocument()) || v.GetChecksum() != checksum || v.GetExpanded() != nil || v.GetNote() != "first" {
					t.Errorf("export %v", v)
				}
			}},
		{name: "the latest, in full", as: f.lab1Operator, call: get("research", 0, v1.ProfileView_PROFILE_VIEW_FULL),
			check: func(t *testing.T, o outcome) {
				v := o.res.(*v1.GetProfileVersionResponse).GetVersion()
				rules := v.GetExpanded().GetRules()
				if v.GetVersion() != 2 || len(rules) < 2 || rules[len(rules)-1].GetOrigin() != "" || rules[0].GetOrigin() != profile.AnnexE("basic") {
					t.Errorf("latest %d, %d rules", v.GetVersion(), len(rules))
				}
			}},
		{name: "no such version", as: f.lab1Operator, call: get("research", 9, 0), code: connect.CodeNotFound, msg: "no such profile version"},
		{name: "a negative version", as: f.lab1Operator, call: get("research", -1, 0), code: connect.CodeInvalidArgument},
		{name: "an unknown view", as: f.lab1Operator, call: get("research", 1, 7), code: connect.CodeInvalidArgument, msg: "view"},
	})
}

// testListProfiles guards ListProfiles: operator on the tenant; each
// profile with its latest version.
func testListProfiles(t *testing.T) {
	f := newFixture(t)
	storeVersion(t, f.env, "research", basicDocument())
	storeVersion(t, f.env, "research", keepStudyID())
	list := func(c clients) outcome { return call(c.profiles.ListProfiles, &v1.ListProfilesRequest{Tenant: "lab1"}) }
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: list, code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: list, audit: inTenant("lab1", master.OutcomeDone), check: func(t *testing.T, o outcome) {
			ps := o.res.(*v1.ListProfilesResponse).GetProfiles()
			if len(ps) != 1 || ps[0].GetName() != "research" || ps[0].GetLatestVersion() != 2 {
				t.Errorf("profiles %v", ps)
			}
		}},
		{name: "a token of another list", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.profiles.ListProfiles, &v1.ListProfilesRequest{Tenant: "lab1", PageToken: "nope"})
		}, code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// testListProfileVersions guards ListProfileVersions: operator on the
// tenant; newest first, without texts, paged; a profile the tenant does not
// have not found.
func testListProfileVersions(t *testing.T) {
	f := newFixture(t)
	storeVersion(t, f.env, "research", basicDocument())
	storeVersion(t, f.env, "research", keepStudyID())
	versions := func(name string, size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.ListProfileVersions, &v1.ListProfileVersionsRequest{Tenant: "lab1", Name: name, PageSize: size})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: versions("research", 1), code: connect.CodePermissionDenied},
		{name: "a page of one", as: f.lab1Operator, call: versions("research", 1),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research"},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.ListProfileVersionsResponse)
				if vs := res.GetVersions(); len(vs) != 1 || vs[0].GetVersion() != 2 || vs[0].GetDocument() != nil || res.GetNextPageToken() == "" {
					t.Errorf("versions %v", res)
				}
			}},
		{name: "no such profile", as: f.lab1Operator, call: versions("none", 0), code: connect.CodeNotFound},
	})
}

// testValidateProfile guards ValidateProfile: operator on the tenant; a
// document with a problem answered with it, not refused, and audited with
// the count; a valid one with its checksum, the expanded form only in full.
func testValidateProfile(t *testing.T) {
	f := newFixture(t)
	bad := basicDocument()
	bad.Bases = append(bad.Bases, "annex-e/nothing:2026d")
	validate := func(doc *v1.ProfileDocument, view v1.ProfileView) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.ValidateProfile, &v1.ValidateProfileRequest{Tenant: "lab1", Document: doc, View: view})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: validate(bad, 0), code: connect.CodePermissionDenied},
		{name: "a problem", as: f.lab1Operator, call: validate(bad, 0),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"problems": "1"}},
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.ValidateProfileResponse); len(res.GetProblems()) != 1 || res.GetChecksum() != "" || res.GetExpanded() != nil {
					t.Errorf("validate %v", res)
				}
			}},
		{name: "valid, in full", as: f.lab1Operator, call: validate(basicDocument(), v1.ProfileView_PROFILE_VIEW_FULL),
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.ValidateProfileResponse); len(res.GetChecksum()) != 64 || len(res.GetExpanded().GetRules()) == 0 {
					t.Errorf("validate %v", res)
				}
			}},
		{name: "no document", as: f.lab1Operator, call: validate(nil, 0), code: connect.CodeInvalidArgument, msg: "document"},
	})
}

// testListProfileBases guards ListProfileBases: any authenticated caller,
// recorded in the installation's log; the bases the server ships.
func testListProfileBases(t *testing.T) {
	f := newFixture(t)
	bases := func(c clients) outcome { return call(c.profiles.ListProfileBases, &v1.ListProfileBasesRequest{}) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: bases, code: connect.CodeUnauthenticated},
		{name: "a key with no grant", as: f.noGrant, call: bases, audit: done(), check: func(t *testing.T, o outcome) {
			found := false
			for _, b := range o.res.(*v1.ListProfileBasesResponse).GetBases() {
				found = found || (b.GetName() == profile.AnnexE("basic") && b.GetAvailable())
			}
			if !found {
				t.Error("the basic profile is not among the bases")
			}
		}},
	})
}
