package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// createRuleSet is a call of CreateRuleSet for lab1's receiver.
func createRuleSet(receiver string, rs *v1.RuleSet, activate bool) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.routes.CreateRuleSet, &v1.CreateRuleSetRequest{Tenant: "lab1", Receiver: receiver, RuleSet: rs, Activate: activate})
	}
}

// keepRule is a profile rule keeping the attribute of keyword.
func keepRule(keyword string) *v1.ProfileRule {
	return &v1.ProfileRule{Name: "keep-" + strings.ToLower(keyword), Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: keyword}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}}
}

// TestRuleSetsOverTheAPI guards RouteService's versions: one is made and
// answered with its version, time and key, read with the reader role and
// made or activated only with admin, its change audited with the receiver
// and the version as its object; an activation is refused, storing
// nothing, for a storage, sender or profile version the tenant does not
// have, a profile that cannot de-identify a receiver's files, and a
// destination profile that would keep more than the receiver's profile does
// (loosens_lower_layer, carried as a ProfileProblems detail), while a
// stricter one is taken; a condition naming a patient's attribute is
// invalid; versions list newest first; an older version is activated again
// and 0 runs none, which the receiver answers.
func TestRuleSetsOverTheAPI(t *testing.T) {
	f := newPlaceFixture(t)
	for _, s := range []string{"main", "lab-a"} {
		mustCall[*v1.AddStorageResponse](t, "add "+s, addStorage("lab1", dirStorage(s, "fast", s))(f.as(local)))
	}
	p := f.pipelineOf("lab1")
	if _, err := p.AddSender(ctx, tenant.Sender{Name: "pacs", Type: tenant.SenderDIMSE, Enabled: true,
		DIMSE: tenant.DIMSESender{Host: "pacs.example", Port: 4242, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}},
		f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, f.env, "research", basicDocument())
	loose := basicDocument()
	loose.Rules = []*v1.ProfileRule{keepRule("InstitutionName")}
	storeVersion(t, f.env, "loose", loose)
	tight := basicDocument()
	tight.Rules = []*v1.ProfileRule{{Name: "kvp", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "KVP"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}}
	storeVersion(t, f.env, "tight", tight)
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", r)(f.as(local)))
	good := &v1.RuleSet{QuarantineUnmatched: true, Comment: "split by site",
		ArchiveRules: []*v1.ArchiveRule{{Name: "lab-a", Storage: "lab-a", When: `calling_ae == "LAB-A" and Modality in ("CT", "MR")`,
			Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}, {Name: "rest", Storage: "main"}},
		ForwardRules: []*v1.ForwardRule{{Name: "t1", When: `Modality = "MR"`, Series: `SeriesDescription contains "t1"`, Senders: []string{"pacs"},
			DestinationProfile: &v1.ProfileVersionRef{Name: "tight", Version: 1}}}}
	with := func(change func(*v1.RuleSet)) *v1.RuleSet {
		c := proto.CloneOf(good)
		change(c)
		return c
	}
	refused := func(name string, code connect.Code, want string, rs *v1.RuleSet) tcase {
		return tcase{name: name, as: f.lab1Admin, call: createRuleSet("deid", rs, true), code: code, msg: want}
	}
	f.run(t, []tcase{
		refused("an archive rule to a storage the tenant does not have", connect.CodeFailedPrecondition, "the tenant has no storage named nowhere",
			with(func(r *v1.RuleSet) { r.ArchiveRules[0].Storage = "nowhere" })),
		refused("a forward rule to a sender the tenant does not have", connect.CodeFailedPrecondition, "the tenant has no sender named nobody",
			with(func(r *v1.RuleSet) { r.ForwardRules[0].Senders = []string{"nobody"} })),
		refused("a profile version the tenant does not have", connect.CodeFailedPrecondition, "the tenant has no profile version research@2",
			with(func(r *v1.RuleSet) { r.ArchiveRules[0].Profile.Version = 2 })),
		{name: "a destination profile that loosens the receiver's", as: f.lab1Admin, code: connect.CodeFailedPrecondition, msg: "would loosen",
			call: createRuleSet("deid", with(func(r *v1.RuleSet) { r.ForwardRules[0].DestinationProfile.Name = "loose" }), true),
			check: func(t *testing.T, o outcome) {
				found := false
				for _, pr := range problemsOf(o.err).GetProblems() {
					found = found || pr.GetCode() == "loosens_lower_layer"
				}
				if !found {
					t.Errorf("the refusal carries no loosens_lower_layer problem: %v", o.err)
				}
			}},
		refused("a condition that reads the patient's name", connect.CodeInvalidArgument, "names who the patient is",
			with(func(r *v1.RuleSet) { r.ArchiveRules[0].When = `PatientName = "DOE^JOHN"` })),
		refused("a condition that does not read", connect.CodeInvalidArgument, "at byte",
			with(func(r *v1.RuleSet) { r.ArchiveRules[0].When = `Modality ==` })),
		{name: "a reader makes one", as: f.lab1Reader, call: createRuleSet("deid", good, true), code: connect.CodePermissionDenied},
		{name: "an admin makes one", as: f.lab1Admin, call: createRuleSet("deid", good, true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "deid@1", details: map[string]string{"activate": "true"}},
			check: func(t *testing.T, o outcome) {
				got := o.res.(*v1.CreateRuleSetResponse).GetRuleSet()
				if got.GetVersion() != 1 || !got.GetActive() || got.GetCreateTime() == nil || got.GetReceiver() != "deid" ||
					len(got.GetArchiveRules()) != 2 || got.GetForwardRules()[0].GetSeries() != good.GetForwardRules()[0].GetSeries() {
					t.Errorf("the version made: %v", got)
				}
			}},
		{name: "a second, not activated", as: f.lab1Admin, call: createRuleSet("deid", with(func(r *v1.RuleSet) { r.ForwardRules = nil }), false),
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.CreateRuleSetResponse).GetRuleSet(); got.GetVersion() != 2 || got.GetActive() {
					t.Errorf("the second version: %v", got)
				}
			}},
		{name: "a reader reads the one running", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.routes.GetRuleSet, &v1.GetRuleSetRequest{Tenant: "lab1", Receiver: "deid"})
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "deid"}, check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.GetRuleSetResponse).GetRuleSet(); got.GetVersion() != 1 || !got.GetActive() {
				t.Errorf("the version running: %v", got)
			}
		}},
		{name: "a reader lists", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.routes.ListRuleSets, &v1.ListRuleSetsRequest{Tenant: "lab1", Receiver: "deid"})
		}, check: func(t *testing.T, o outcome) {
			got := o.res.(*v1.ListRuleSetsResponse).GetRuleSets()
			if len(got) != 2 || got[0].GetVersion() != 2 || got[1].GetVersion() != 1 || !got[1].GetActive() {
				t.Errorf("the versions: %v", got)
			}
		}},
		{name: "a reader activates", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid", Version: 2})
		}, code: connect.CodePermissionDenied},
		{name: "an admin activates the second", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid", Version: 2})
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "deid@2"}},
		{name: "the receiver answers the version it runs", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "deid"})
		}, check: func(t *testing.T, o outcome) {
			if v := o.res.(*v1.GetReceiverResponse).GetReceiver().GetRuleSetVersion(); v != 2 {
				t.Errorf("the receiver runs version %d, want 2", v)
			}
		}},
		{name: "a receiver whose archive rule names a profile changed to as received", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid",
				Deidentification: &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}}}, UpdateMask: mask("deidentification")})
		}, code: connect.CodeFailedPrecondition, msg: "names a profile version"},
		{name: "an admin runs none", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid"})
		}, check: func(t *testing.T, o outcome) {
			if o.res.(*v1.ActivateRuleSetResponse).GetRuleSet() != nil {
				t.Errorf("running none answers a rule set")
			}
		}},
		{name: "the one running, when none runs", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.routes.GetRuleSet, &v1.GetRuleSetRequest{Tenant: "lab1", Receiver: "deid"})
		}, code: connect.CodeNotFound},
		{name: "routing named in a receiver's mask", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid"}, UpdateMask: mask("routing")})
		}, code: connect.CodeInvalidArgument},
	})
}

// TestReleaseNamesStorage guards a release that names a storage over the
// API: a name that is not a storage's is invalid, a storage the tenant does
// not have is a precondition failure, and a storage it has is kept with the
// reception the file joins, the release's audit entry naming it.
func TestReleaseNamesStorage(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	mustCall[*v1.AddStorageResponse](t, "add lab-a", addStorage("lab1", dirStorage("lab-a", "fast", "lab-a"))(f.as(local)))
	id := f.heldFile(t, "scanner1", false)
	release := func(storage string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: "lab1", Id: id,
				Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Storage: storage})
		}
	}
	var joined string
	f.run(t, []tcase{
		{name: "a storage's name that is not valid", as: f.lab1Operator, call: release("Not A Name"), code: connect.CodeInvalidArgument, msg: "storage"},
		{name: "a storage the tenant does not have", as: f.lab1Operator, call: release("nowhere"), code: connect.CodeFailedPrecondition,
			msg: "no storage named nowhere"},
		{name: "a storage the tenant has", as: f.lab1Operator, call: release("lab-a"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"storage": "lab-a"}},
			check: func(t *testing.T, o outcome) {
				joined = o.res.(*v1.ReleaseQuarantinedFileResponse).GetFile().GetReleaseReception()
			}},
	})
	r, _, err := f.pipelineOf("lab1").Reception(ctx, joined)
	if err != nil || r.Route.ReleasedStorage != "lab-a" {
		t.Errorf("the reception the release joined reads %+v (%v), want the storage it named", r.Route, err)
	}
}

// TestDeprecatedRoutingFieldIsRefused guards the receiver's routing field,
// kept in the contract and no longer read: a request that sets it, in an add
// or a change, or names it in an update mask, is refused as invalid with a
// message naming RouteService and nopacs route, and a receiver is always
// answered with it empty.
func TestDeprecatedRoutingFieldIsRefused(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	routing := &v1.ReceiverRouting{Routes: []*v1.Route{{Name: "a", Storage: "main"}}} //nolint:staticcheck // the deprecated field is what is tested.
	add := dimseReceiver("other", "main", 11120)
	add.Routing = routing //nolint:staticcheck // as above.
	f.run(t, []tcase{
		{name: "add with routing", as: f.lab1Admin, call: addReceiver("lab1", add), code: connect.CodeInvalidArgument, msg: "RouteService"},
		{name: "set with routing and a mask naming it", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner1", Routing: routing}, //nolint:staticcheck // as above.
				UpdateMask: mask("routing")})
		}, code: connect.CodeInvalidArgument, msg: "nopacs route"},
		{name: "set with routing set and another field named", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner1", Routing: routing, Enabled: true}, //nolint:staticcheck // as above.
				UpdateMask: mask("enabled")})
		}, code: connect.CodeInvalidArgument, msg: "RouteService"},
	})
	got := mustCall[*v1.GetReceiverResponse](t, "get", call(f.as(f.lab1Reader).receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "scanner1"}))
	if got.GetReceiver().GetRouting() != nil { //nolint:staticcheck // as above.
		t.Errorf("the receiver is answered with routing %v, want it empty", got.GetReceiver().GetRouting()) //nolint:staticcheck // as above.
	}
}
