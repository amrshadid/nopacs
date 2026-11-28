package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// withRules is basicDocument with the rules given.
func withRules(rules ...*v1.ProfileRule) *v1.ProfileDocument {
	doc := basicDocument()
	doc.Rules = rules
	return doc
}

// baseFixture is a tenant with a storage, and profiles for the tests of the
// tenant's base: "floor" removes the KVP and the station name, which every
// profile a receiver, an archive rule or a destination points at must then
// remove too; "tight" removes more than it; "loose" keeps the KVP; "plain" is
// the basic profile alone.
func baseFixture(t *testing.T) *placeFixture {
	t.Helper()
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "main"))(f.as(local)))
	storeVersion(t, f.env, "floor", withRules(removeRule("kvp", "KVP"), removeRule("station", "StationName")))
	storeVersion(t, f.env, "tight", withRules(removeRule("kvp", "KVP"), removeRule("station", "StationName"), removeRule("protocol", "ProtocolName")))
	storeVersion(t, f.env, "loose", withRules(keepRule("KVP")))
	storeVersion(t, f.env, "plain", basicDocument())
	storeVersion(t, f.env, "kvponly", withRules(removeRule("kvp", "KVP")))
	return f
}

// setBase sets the tenant's base profile as a call of SetSetting.
func setBase(value string) func(clients) outcome {
	return set(settings.DeidBaseProfile, tenantScope("lab1"), value)
}

// profileRef is a version of a profile of lab1, as a request gives it.
func profileRef(name string) *v1.ProfileVersionRef {
	return &v1.ProfileVersionRef{Name: name, Version: 1}
}

// wantLoosens fails the test unless o was refused as a precondition failure
// whose ProfileProblems detail holds a problem of code, and whose message
// names the base.
func wantLoosens(t *testing.T, what string, o outcome, code string) {
	t.Helper()
	if o.err == nil {
		t.Errorf("%s: accepted, want refused", what)
		return
	}
	if o.code() != connect.CodeFailedPrecondition {
		t.Errorf("%s: code %v (%v), want a precondition failure", what, o.code(), o.err)
	}
	found := false
	for _, p := range problemsOf(o.err).GetProblems() {
		found = found || p.GetCode() == code
	}
	if !found {
		t.Errorf("%s: the refusal carries no %s problem: %v", what, code, o.err)
	}
}

// TestBaseProfileRefusesALooserReceiver guards the base as the lowest layer
// of a receiver's de-identification: a receiver added with a profile that
// keeps what the base removes is refused with the loosening as a
// ProfileProblems detail, as is a change of a receiver's profile to one;
// a profile that removes at least what the base does is taken, and the
// refused change leaves the receiver as it was.
func TestBaseProfileRefusesALooserReceiver(t *testing.T) {
	f := baseFixture(t)
	mustCall[*v1.SetSettingResponse](t, "set the base", setBase("floor@1")(f.as(f.lab1Admin)))
	add := func(name, prof string, port int32) outcome {
		r := dimseReceiver(name, "main", port)
		r.Deidentification = &v1.Receiver_Profile{Profile: profileRef(prof)}
		return addReceiver("lab1", r)(f.as(f.lab1Admin))
	}
	wantLoosens(t, "a receiver added with a looser profile", add("deid", "loose", 11120), "loosens_lower_layer")
	if o := add("deid", "tight", 11120); o.err != nil {
		t.Fatalf("a receiver added with a stricter profile: %v", o.err)
	}
	o := call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1",
		Receiver:   &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: profileRef("loose")}},
		UpdateMask: mask("deidentification")})
	wantLoosens(t, "a receiver changed to a looser profile", o, "loosens_lower_layer")
	if stored, err := f.pipelineOf("lab1").Receiver(ctx, "deid"); err != nil || stored.Profile.Name != "tight" {
		t.Errorf("after the refusal the receiver's profile is %v (%v), want tight", stored.Profile, err)
	}
	// Without a base the same profile is taken: the refusal is the base's.
	mustCall[*v1.UnsetSettingResponse](t, "unset the base", call(f.as(f.lab1Admin).settings.UnsetSetting,
		&v1.UnsetSettingRequest{Name: settings.DeidBaseProfile, Scope: tenantScope("lab1")}))
	if o := add("deid2", "loose", 11121); o.err != nil {
		t.Errorf("a looser profile with no base set: %v", o.err)
	}
}

// TestBaseTightenedPastARunningReceiverIsRefused guards setting a base
// against what runs now: a base that a receiver's profile, or an archive
// rule's or a destination's of the rule set the receiver runs, would loosen
// is refused with every problem as a detail and stored nowhere; a base all of
// them meet is taken.
func TestBaseTightenedPastARunningReceiverIsRefused(t *testing.T) {
	f := baseFixture(t)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: profileRef("loose")}
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", r)(f.as(f.lab1Admin)))
	o := setBase("floor@1")(f.as(f.lab1Admin))
	wantLoosens(t, "a base a running receiver's profile loosens", o, "loosens_lower_layer")
	if o.err != nil && o.message() == "" {
		t.Errorf("the refusal has no message")
	}
	got := call(f.as(f.lab1Reader).settings.GetSetting, &v1.GetSettingRequest{Name: settings.DeidBaseProfile, Scope: tenantScope("lab1")})
	if v := mustCall[*v1.GetSettingResponse](t, "get", got).GetSetting().GetValue(); v != "" {
		t.Errorf("the refused base is stored: %q", v)
	}
	// A base the profile meets is taken ("plain" removes nothing more than
	// the basic profile does, as "loose" keeps only the KVP, which the basic
	// profile keeps too).
	mustCall[*v1.SetSettingResponse](t, "a base the receiver meets", setBase("plain@1")(f.as(f.lab1Admin)))

	// The rule set a receiver runs is judged too: a base that the active
	// rule set's archive rule's profile, or a destination's, would loosen is
	// refused although the receiver's own profile meets it.
	mustCall[*v1.RemoveReceiverResponse](t, "remove", call(f.as(f.lab1Admin).receivers.RemoveReceiver, &v1.RemoveReceiverRequest{Tenant: "lab1", Name: "deid"}))
	if _, err := f.pipelineOf("lab1").AddSender(ctx, tenant.Sender{Name: "pacs", Type: tenant.SenderDIMSE, Enabled: true,
		DIMSE: tenant.DIMSESender{Host: "pacs.example", Port: 4242, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}},
		f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, f.env, "dkeep", withRules(keepRule("KVP")))
	rcv := dimseReceiver("deid2", "main", 11121)
	rcv.Deidentification = &v1.Receiver_Profile{Profile: profileRef("plain")}
	mustCall[*v1.AddReceiverResponse](t, "add deid2", addReceiver("lab1", rcv)(f.as(f.lab1Admin)))
	withDest := &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main"}},
		ForwardRules: []*v1.ForwardRule{{Name: "t", Senders: []string{"pacs"}, DestinationProfile: profileRef("dkeep")}}}
	mustCall[*v1.CreateRuleSetResponse](t, "run the rule set", createRuleSet("deid2", withDest, true)(f.as(f.lab1Admin)))
	wantLoosens(t, "a base the active destination profile loosens", setBase("kvponly@1")(f.as(f.lab1Admin)), "loosens_lower_layer")
	archive := &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main", Profile: profileRef("dkeep")}}}
	mustCall[*v1.CreateRuleSetResponse](t, "run an archive rule", call(f.as(f.lab1Admin).routes.CreateRuleSet, &v1.CreateRuleSetRequest{Tenant: "lab1",
		Receiver: "deid2", RuleSet: archive, Activate: true, Force: true, ForceReason: "a version this test does not try over receptions"}))
	wantLoosens(t, "a base the active archive rule's profile loosens", setBase("kvponly@1")(f.as(f.lab1Admin)), "loosens_lower_layer")

	// With the base set, a rule set whose profiles keep what it removes is
	// refused at creation with activation and at activation, and a stricter one
	// runs.
	mustCall[*v1.CreateRuleSetResponse](t, "no profiles", createRuleSet("deid2", &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main"}}}, true)(f.as(f.lab1Admin)))
	mustCall[*v1.SetSettingResponse](t, "a base the rule set meets", setBase("kvponly@1")(f.as(f.lab1Admin)))
	wantLoosens(t, "an archive rule's profile that loosens the base", createRuleSet("deid2", archive, true)(f.as(f.lab1Admin)), "loosens_lower_layer")
	stored := mustCall[*v1.CreateRuleSetResponse](t, "stored, not run", createRuleSet("deid2", withDest, false)(f.as(f.lab1Admin)))
	wantLoosens(t, "a destination's profile that loosens the base",
		call(f.as(f.lab1Admin).routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid2", Version: stored.GetRuleSet().GetVersion()}), "loosens_lower_layer")
	fine := &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main", Profile: profileRef("tight")}},
		ForwardRules: []*v1.ForwardRule{{Name: "t", Senders: []string{"pacs"}, DestinationProfile: profileRef("tight")}}}
	mustCall[*v1.CreateRuleSetResponse](t, "a rule set that meets the base", call(f.as(f.lab1Admin).routes.CreateRuleSet, &v1.CreateRuleSetRequest{
		Tenant: "lab1", Receiver: "deid2", RuleSet: fine, Activate: true, Force: true, ForceReason: "a version this test does not try over receptions"}))
}

// TestBaseProfileIsTheTenantAdminsAlone guards who sets the base: the
// tenant's admin does, a reader and an operator do not, a masked key never
// does (nor unsets it), a version the tenant does not have is a precondition
// failure, a value that is not name@version is invalid, and the tenant's value
// is read back as set; the installation has no base.
func TestBaseProfileIsTheTenantAdminsAlone(t *testing.T) {
	f := baseFixture(t)
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	f.run(t, []tcase{
		{name: "a reader", as: f.lab1Reader, call: setBase("floor@1"), code: connect.CodePermissionDenied},
		{name: "an operator", as: f.lab1Operator, call: setBase("floor@1"), code: connect.CodePermissionDenied},
		{name: "a masked admin key", as: masked, call: setBase("floor@1"), code: connect.CodePermissionDenied, msg: "masked key"},
		{name: "a masked admin key unsetting it", as: masked, call: unset(settings.DeidBaseProfile, tenantScope("lab1")), code: connect.CodePermissionDenied, msg: "masked key"},
		{name: "a version the tenant does not have", as: f.lab1Admin, call: setBase("floor@9"), code: connect.CodeFailedPrecondition, msg: "floor@9"},
		{name: "a profile the tenant does not have", as: f.lab1Admin, call: setBase("nothing@1"), code: connect.CodeFailedPrecondition},
		{name: "a value that is not name@version", as: f.lab1Admin, call: setBase("floor"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "on the installation", as: f.instAdmin, call: set(settings.DeidBaseProfile, installation(), "floor@1"), code: connect.CodeInvalidArgument, msg: "no installation value"},
		{name: "the tenant's admin", as: f.lab1Admin, call: setBase("floor@1"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: settings.DeidBaseProfile, tenant: "lab1", details: map[string]string{"scope": "tenant/lab1"}}},
		{name: "read back", as: f.lab1Reader, call: getSetting(settings.DeidBaseProfile, tenantScope("lab1")),
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.GetSettingResponse).GetSetting(); s.GetValue() != "floor@1" || s.GetSource() != v1.SettingSource_SETTING_SOURCE_TENANT {
					t.Errorf("%v", s)
				}
			}},
	})
}

// TestDescribeEffectiveNamesEachLayer guards describe --effective: for a
// receiver whose profile is stacked over the tenant's base, each selector is
// listed once with the action that decides it and the layer it came from:
// the receiver's for what it names (it may only tighten the base), the base's
// for what only the base names. A receiver that archives as received has no
// profile to describe, and a request that names both a profile and a receiver
// is invalid.
func TestDescribeEffectiveNamesEachLayer(t *testing.T) {
	f := baseFixture(t)
	mustCall[*v1.SetSettingResponse](t, "set the base", setBase("floor@1")(f.as(f.lab1Admin)))
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: profileRef("tight")}
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", r)(f.as(f.lab1Admin)))
	asReceived := dimseReceiver("raw", "main", 11121)
	mustCall[*v1.AddReceiverResponse](t, "add as received", addReceiver("lab1", asReceived)(f.as(f.lab1Admin)))
	describe := func(req *v1.DescribeProfileRequest) outcome {
		req.Tenant = "lab1"
		return call(f.as(f.lab1Operator).profiles.DescribeProfile, req)
	}
	got := mustCall[*v1.DescribeProfileResponse](t, "describe", describe(&v1.DescribeProfileRequest{EffectiveReceiver: "deid"}))
	layers := map[string]string{}
	for _, e := range got.GetEffective() {
		layers[e.GetWhat()] = e.GetLayer() + ":" + e.GetLayerProfile() + ":" + e.GetAction()
	}
	for what, want := range map[string]string{"ProtocolName": "receiver:tight@1:remove", "KVP": "receiver:tight@1:remove", "StationName": "receiver:tight@1:remove"} {
		if layers[what] != want {
			t.Errorf("%s: %q, want %q (all: %v)", what, layers[what], want, layers)
		}
	}
	if got.GetVersion().GetName() != "tight" || len(got.GetClasses()) != 0 {
		t.Errorf("the answer names %v and %d classes, want the receiver's profile and none", got.GetVersion(), len(got.GetClasses()))
	}
	// A base rule the receiver's profile does not repeat decides in the base's
	// layer: this receiver's profile names the station only.
	storeVersion(t, f.env, "stationonly", withRules(removeRule("station", "StationName")))
	r2 := dimseReceiver("deid2", "main", 11122)
	r2.Deidentification = &v1.Receiver_Profile{Profile: profileRef("stationonly")}
	mustCall[*v1.AddReceiverResponse](t, "add deid2", addReceiver("lab1", r2)(f.as(f.lab1Admin)))
	got = mustCall[*v1.DescribeProfileResponse](t, "describe deid2", describe(&v1.DescribeProfileRequest{EffectiveReceiver: "deid2"}))
	byWhat := map[string]*v1.EffectiveElement{}
	for _, e := range got.GetEffective() {
		byWhat[e.GetWhat()] = e
	}
	if e := byWhat["KVP"]; e == nil || e.GetLayer() != "tenant" || e.GetLayerProfile() != "floor@1" || e.GetAction() != "remove" || e.GetRule() == 0 {
		t.Errorf("KVP, which only the base names: %v, want the tenant layer's floor@1 remove", e)
	}
	if e := byWhat["StationName"]; e == nil || e.GetLayer() != "receiver" || e.GetLayerProfile() != "stationonly@1" || e.GetAction() != "remove" {
		t.Errorf("StationName, which both name: %v, want the receiver layer's stationonly@1 remove", e)
	}
	if e := byWhat["ProtocolName"]; e == nil || e.GetLayer() != "receiver" || e.GetOrigin() == "" {
		t.Errorf("ProtocolName, which a base both layers' profiles are built on names: %v, want the receiver layer's copy of the shared base's rule", e)
	}
	f.run(t, []tcase{
		{name: "a receiver that archives as received", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: "lab1", EffectiveReceiver: "raw"})
		}, code: connect.CodeFailedPrecondition, msg: "as received"},
		{name: "a receiver that does not exist", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: "lab1", EffectiveReceiver: "nobody"})
		}, code: connect.CodeNotFound},
		{name: "a profile and a receiver", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: "lab1", EffectiveReceiver: "deid",
				Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: "floor", Version: 1}}}})
		}, code: connect.CodeInvalidArgument},
		{name: "a reader", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: "lab1", EffectiveReceiver: "deid"})
		}, code: connect.CodePermissionDenied},
	})
}

// TestBaseProfileLeavesPixelRulesToTheReceiver guards that a receiver's pixel
// rules are not judged as a layer over the base: a profile that meets the
// base's element rules and fills a rectangle of an image is taken by a
// receiver of a tenant with a base, and described with its elements.
func TestBaseProfileLeavesPixelRulesToTheReceiver(t *testing.T) {
	f := baseFixture(t)
	mustCall[*v1.SetSettingResponse](t, "set the base", setBase("floor@1")(f.as(f.lab1Admin)))
	doc := withRules(removeRule("kvp", "KVP"), removeRule("station", "StationName"))
	doc.PixelRules = []*v1.PixelRule{{Name: "us-banner", Rows: 480, Columns: 640, Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{
		Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 640, Height: 40}}}}}}
	storeVersion(t, f.env, "withpixels", doc)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: profileRef("withpixels")}
	mustCall[*v1.AddReceiverResponse](t, "a receiver whose profile has pixel rules", addReceiver("lab1", r)(f.as(f.lab1Admin)))
	got := mustCall[*v1.DescribeProfileResponse](t, "describe", call(f.as(f.lab1Operator).profiles.DescribeProfile,
		&v1.DescribeProfileRequest{Tenant: "lab1", EffectiveReceiver: "deid"}))
	if len(got.GetEffective()) == 0 {
		t.Errorf("the effective profile lists no element")
	}
}

// TestTrialAsAReceiverRunsOverTheBase guards that a trial as a receiver runs
// the profile as the receiver would, over the tenant's base: a profile that
// loosens the base is refused with its problems, as it would be set on a
// receiver, while one that meets it is tried; a trial with no receiver named
// is of the profile alone.
func TestTrialAsAReceiverRunsOverTheBase(t *testing.T) {
	f := baseFixture(t)
	mustCall[*v1.SetSettingResponse](t, "set the base", setBase("floor@1")(f.as(f.lab1Admin)))
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: profileRef("tight")}
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", r)(f.as(f.lab1Admin)))
	ct := corpusFile(t, "ct5n-2062.dcm")
	try := func(profile, receiver string) outcome {
		req := &v1.TryProfileRequest{Tenant: "lab1", Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: profile, Version: 1}}},
			Files: []*v1.TrialFile{{Content: ct}}}
		if receiver != "" {
			req.Reception = &v1.TrialReception{Receiver: receiver}
		}
		return call(f.as(f.lab1Operator).profiles.TryProfile, req)
	}
	wantLoosens(t, "a trial of a profile that loosens the base as a receiver", try("loose", "deid"), "loosens_lower_layer")
	if o := try("tight", "deid"); o.err != nil {
		t.Errorf("a trial of a profile that meets the base: %v", o.err)
	}
	if o := try("loose", ""); o.err != nil {
		t.Errorf("a trial of the profile alone: %v", o.err)
	}
}
