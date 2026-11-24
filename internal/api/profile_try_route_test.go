package api_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestTrialAppliesTheReceiversRoutes guards a trial naming a receiver that
// routes: a file its route would de-identify with another profile version
// is tried with that version, the answer naming the route and the
// version; a file its routing would not route is held as unrouted; a file
// tried with no receiver keeps the profile asked for.
func TestTrialAppliesTheReceiversRoutes(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "main"))(f.as(local)))
	storeVersion(t, f.env, "research", basicDocument())
	strict := basicDocument()
	strict.Rules = []*v1.ProfileRule{{Name: "kvp", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "KVP"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}}
	storeVersion(t, f.env, "strict", strict)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	r.Routing = &v1.ReceiverRouting{QuarantineUnmatched: true, Routes: []*v1.Route{{Name: "lab-a", Storage: "main",
		Conditions: []*v1.ProfileCondition{byCaller("LAB-A")}, Profile: &v1.ProfileVersionRef{Name: "strict", Version: 1}}}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", r)(f.as(local)))
	ct := corpusFile(t, "ct5n-2062.dcm")
	try := func(reception *v1.TrialReception) *v1.TrialResult {
		t.Helper()
		res := mustCall[*v1.TryProfileResponse](t, "try", call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{
			Tenant: "lab1", Profile: storedVersion("research", 1), Files: []*v1.TrialFile{{Content: ct}}, Reception: reception}))
		return res.GetResults()[0]
	}
	routed := try(&v1.TrialReception{Receiver: "deid", CallingAeTitle: "LAB-A"})
	if routed.GetRoute() != "lab-a" || routed.GetRoutedProfile().GetName() != "strict" || routed.GetRoutedProfile().GetVersion() != 1 {
		t.Errorf("a file the route chooses: route %q, version %v", routed.GetRoute(), routed.GetRoutedProfile())
	}
	if kvp := elementAt(routed, "00180060"); kvp == nil || kvp.GetAfter() != "" || kvp.GetRuleName() != "kvp" {
		t.Errorf("KVP under the route's version: %v", kvp)
	}
	unrouted := try(&v1.TrialReception{Receiver: "deid", CallingAeTitle: "OTHER"})
	if unrouted.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_HELD || unrouted.GetReason() != "unrouted" || unrouted.GetCause() != "no_match" {
		t.Errorf("a file no route chooses: %v %s %s", unrouted.GetVerdict(), unrouted.GetReason(), unrouted.GetCause())
	}
	alone := try(nil)
	if alone.GetRoute() != "" || alone.GetRoutedProfile() != nil || elementAt(alone, "00180060").GetRuleName() == "kvp" {
		t.Errorf("a file tried with no receiver: route %q, version %v", alone.GetRoute(), alone.GetRoutedProfile())
	}
}
