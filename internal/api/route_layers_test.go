package api_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// removeRule is a profile rule removing the attribute of keyword.
func removeRule(name, keyword string) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: keyword}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}
}

// TestReceiverProfileChangeKeepsLayers guards that changing a receiver's
// profile cannot leave a destination profile of the rule set it runs looser
// than the new profile: the layer rule judged when a version is made or
// activated holds for this change too, with the loosening as a detail.
func TestReceiverProfileChangeKeepsLayers(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "main"))(f.as(local)))
	if _, err := f.pipelineOf("lab1").AddSender(ctx, tenant.Sender{Name: "pacs", Type: tenant.SenderDIMSE, Enabled: true,
		DIMSE: tenant.DIMSESender{Host: "pacs.example", Port: 4242, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}},
		f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, f.env, "research", basicDocument())
	tight := basicDocument()
	tight.Rules = []*v1.ProfileRule{removeRule("kvp", "KVP")}
	storeVersion(t, f.env, "tight", tight)
	stricter := basicDocument()
	stricter.Rules = []*v1.ProfileRule{removeRule("kvp", "KVP"), removeRule("inst", "InstitutionName")}
	storeVersion(t, f.env, "stricter", stricter)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", r)(f.as(local)))
	rs := &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main"}},
		ForwardRules: []*v1.ForwardRule{{Name: "t", Senders: []string{"pacs"},
			DestinationProfile: &v1.ProfileVersionRef{Name: "tight", Version: 1}}}}
	mustCall[*v1.CreateRuleSetResponse](t, "make", createRuleSet("deid", rs, true)(f.as(f.lab1Admin)))
	set := func(name string) outcome {
		return call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid",
			Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: name, Version: 1}}}, UpdateMask: mask("deidentification"),
			Force: true, ForceReason: "a version this test does not try over receptions"})
	}
	if o := set("stricter"); o.err == nil {
		t.Errorf("the receiver's profile was changed to one the active destination profile loosens")
	}
	stored, err := f.pipelineOf("lab1").Receiver(ctx, "deid")
	if err != nil || stored.Profile.Name != "research" {
		t.Errorf("after the refusal the receiver's profile is %v (%v), want research", stored.Profile, err)
	}
	// A profile the destination does not loosen is accepted.
	mustCall[*v1.SetReceiverResponse](t, "tighter receiver", set("tight"))
}

// TestArchiveRuleProfileCannotLoosenReceiver guards the layer order (tenant,
// receiver, route, destination: each only tightens what is below): an
// archive rule's profile that keeps what the receiver's profile removes is
// refused when the version is made and activated, and a receiver's profile
// change that the active rule's profile would loosen is refused.
func TestArchiveRuleProfileCannotLoosenReceiver(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "main"))(f.as(local)))
	stricter := basicDocument()
	stricter.Rules = []*v1.ProfileRule{removeRule("inst", "InstitutionName")}
	storeVersion(t, f.env, "stricter", stricter)
	loose := basicDocument()
	loose.Rules = []*v1.ProfileRule{keepRule("InstitutionName")}
	storeVersion(t, f.env, "loose", loose)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "stricter", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", r)(f.as(local)))
	rs := &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "any", Storage: "main", Profile: &v1.ProfileVersionRef{Name: "loose", Version: 1}}}}
	if o := createRuleSet("deid", rs, true)(f.as(f.lab1Admin)); o.err == nil {
		t.Errorf("an archive rule whose profile keeps InstitutionName was activated over a receiver whose profile removes it")
	}
	if o := createRuleSet("deid", rs, false)(f.as(f.lab1Admin)); o.err != nil {
		t.Fatalf("storing the version without running it: %v", o.err)
	}
	if o := call(f.as(f.lab1Admin).routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid", Version: 1}); o.err == nil {
		t.Errorf("a stored version whose archive rule loosens the receiver's profile was activated")
	}
	// Under a receiver profile that keeps it too, the rule's profile is fine, and
	// a receiver change back to the stricter profile is then refused.
	mustCall[*v1.SetReceiverResponse](t, "loose receiver", call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1",
		Receiver:   &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "loose", Version: 1}}},
		UpdateMask: mask("deidentification"), Force: true, ForceReason: "a version this test does not try over receptions"}))
	mustCall[*v1.ActivateRuleSetResponse](t, "activate", call(f.as(f.lab1Admin).routes.ActivateRuleSet,
		&v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid", Version: 1}))
	if o := call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1",
		Receiver:   &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "stricter", Version: 1}}},
		UpdateMask: mask("deidentification"), Force: true, ForceReason: "a version this test does not try over receptions"}); o.err == nil {
		t.Errorf("a receiver's profile was changed to one the active archive rule's profile loosens")
	}
}

// TestMaskedKeyDryRunRefusedOverAsReceived guards that a masked key is
// never answered from headers archived as received: a dry run reads each
// scan's first header, so it answers whether a condition held over the
// original, which explain refuses for the same instance.
func TestMaskedKeyDryRunRefusedOverAsReceived(t *testing.T) {
	f := newPlaceFixture(t)
	routedArchive(t, f)
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	mc := f.as(masked)
	mustCall[*v1.CreateRuleSetResponse](t, "probe version", createRuleSet("scanner1", &v1.RuleSet{
		ArchiveRules: []*v1.ArchiveRule{{Name: "probe", Storage: "main", When: `Modality == "CT"`}}}, false)(mc))
	dry := call(mc.routes.DryRunRoutes, &v1.DryRunRoutesRequest{Tenant: "lab1", Receiver: "scanner1", Version: 2, Sessions: 10})
	if dry.err == nil {
		t.Errorf("a dry run over an as-received session is answered to a masked key: %v", dry.res)
	}
	// An unmasked key is still answered.
	mustCall[*v1.DryRunRoutesResponse](t, "unmasked", call(f.as(f.lab1Operator).routes.DryRunRoutes,
		&v1.DryRunRoutesRequest{Tenant: "lab1", Receiver: "scanner1", Version: 2, Sessions: 10}))
}

// TestMaskedKeyDryRunAnsweredOverDeidentified guards that the refusal of a
// dry run to a masked key is for headers archived as received only: over a
// session a receiver de-identified, the headers hold no original value and
// the run is answered.
func TestMaskedKeyDryRunAnsweredOverDeidentified(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	storeVersion(t, f.env, "research", basicDocument())
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", r)(f.as(local)))
	receiveWithTenantKey(t, f, "deid", corpusFile(t, "ct5n-2062.dcm"))
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	mc := f.as(masked)
	mustCall[*v1.CreateRuleSetResponse](t, "probe version", createRuleSet("deid", &v1.RuleSet{
		ArchiveRules: []*v1.ArchiveRule{{Name: "probe", Storage: "main", When: `Modality == "CT"`}}}, false)(mc))
	got := mustCall[*v1.DryRunRoutesResponse](t, "masked dry run", call(mc.routes.DryRunRoutes,
		&v1.DryRunRoutesRequest{Tenant: "lab1", Receiver: "deid", Version: 1, Sessions: 10}))
	if len(got.GetSessions()) != 1 {
		t.Errorf("the masked dry run over a de-identified session: %v", got)
	}
}
