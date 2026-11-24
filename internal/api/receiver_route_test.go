package api_test

import (
	"strconv"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// byCaller is a route condition on the calling AE title.
func byCaller(title string) *v1.ProfileCondition {
	return &v1.ProfileCondition{Subject: v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE,
		Test: &v1.ProfileCondition_Equals{Equals: title}}
}

// TestReceiverRouting guards a receiver's routing over the API: it is
// stored and answered as given, read with the reader role and changed only
// with admin, its change audited in the tenant's log with the mask; every
// rule of a route is checked and refused with the field (a storage, a
// sender or a profile version the tenant does not have, a profile version
// that cannot de-identify a receiver's files, a profile on a receiver that
// archives as received, the receiver subject, a pattern not RE2, too many
// routes); a mask naming a part of routing is refused; and a change made
// on a receiver read before another change is a precondition failure.
func TestReceiverRouting(t *testing.T) {
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
	store := func(name string, doc *v1.ProfileDocument) {
		t.Helper()
		v, err := profile.New(name, 1, doc, profile.Shipped())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{
			Name: name, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum()}, f.now(), f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
	basic := []string{profile.AnnexE("basic")}
	store("research", &v1.ProfileDocument{Bases: basic})
	store("strict", &v1.ProfileDocument{Bases: basic})
	store("keeps-uids", &v1.ProfileDocument{Bases: append(basic, profile.AnnexE("retain-uids"))})
	good := &v1.ReceiverRouting{QuarantineUnmatched: true, Routes: []*v1.Route{
		{Name: "lab-a", Storage: "lab-a", Conditions: []*v1.ProfileCondition{byCaller("LAB-A"),
			{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Pattern{Pattern: "CT|MR"}, Negate: true}},
			Profile: &v1.ProfileVersionRef{Name: "strict", Version: 1}, ForwardTo: &v1.RouteForwardTo{Senders: []string{"pacs"}}},
		{Name: "rest", Storage: "main", ForwardTo: &v1.RouteForwardTo{}},
	}}
	deid := func(name string, port int32, routing *v1.ReceiverRouting) *v1.Receiver {
		r := dimseReceiver(name, "main", port)
		r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
		r.Routing = routing
		return r
	}
	with := func(change func(r *v1.Route)) *v1.ReceiverRouting {
		r := proto.CloneOf(good)
		change(r.Routes[0])
		return r
	}
	precondition := func(name, want string, routing *v1.ReceiverRouting) tcase {
		return tcase{name: name, as: f.lab1Admin, call: addReceiver("lab1", deid("refused", 11121, routing)),
			code: connect.CodeFailedPrecondition, msg: want}
	}
	invalidCase := func(name, want string, routing *v1.ReceiverRouting) tcase {
		return tcase{name: name, as: f.lab1Admin, call: addReceiver("lab1", deid("refused", 11121, routing)),
			code: connect.CodeInvalidArgument, msg: want}
	}
	many := &v1.ReceiverRouting{}
	for i := range tenant.MaxRoutes + 1 {
		many.Routes = append(many.Routes, &v1.Route{Name: "r" + strconv.Itoa(i), Storage: "main"})
	}
	asReceived := dimseReceiver("plain", "main", 11122)
	asReceived.Routing = good
	f.run(t, []tcase{
		precondition("a route to a storage the tenant does not have", "receiver.routing: the tenant has no storage named nowhere",
			with(func(r *v1.Route) { r.Storage = "nowhere" })),
		precondition("a route to a sender the tenant does not have", "the tenant has no sender named nobody",
			with(func(r *v1.Route) { r.ForwardTo.Senders = []string{"nobody"} })),
		precondition("a route's profile version the tenant does not have", "route lab-a: the tenant has no profile version strict@2",
			with(func(r *v1.Route) { r.Profile.Version = 2 })),
		precondition("a route's profile version that keeps the placing UIDs", "it does not remap the UIDs a file is placed by",
			with(func(r *v1.Route) { r.Profile.Name = "keeps-uids" })),
		{name: "a route's profile on a receiver that archives as received", as: f.lab1Admin, call: addReceiver("lab1", asReceived),
			code: connect.CodeFailedPrecondition, msg: "make the receiver de-identify first"},
		invalidCase("the receiver subject", "a route belongs to one receiver", with(func(r *v1.Route) {
			r.Conditions[0].Subject = v1.ConditionSubject_CONDITION_SUBJECT_RECEIVER
		})),
		invalidCase("a pattern not RE2", "not a regular expression of RE2", with(func(r *v1.Route) {
			r.Conditions[1].Test = &v1.ProfileCondition_Pattern{Pattern: `(?<=a)b`}
		})),
		invalidCase("a repetition past its bound", "a repetition counts at most 100", with(func(r *v1.Route) {
			r.Conditions[1].Test = &v1.ProfileCondition_Pattern{Pattern: `a{1000}`}
		})),
		invalidCase("too many routes", "holds at most 64 routes", many),
		invalidCase("a route with no name", "route 1", with(func(r *v1.Route) { r.Name = "" })),
		{name: "a reader adds", as: f.lab1Reader, call: addReceiver("lab1", deid("deid", 11120, good)), code: connect.CodePermissionDenied},
		{name: "an admin adds", as: f.lab1Admin, call: addReceiver("lab1", deid("deid", 11120, good)),
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.AddReceiverResponse).GetReceiver().GetRouting(); !proto.Equal(got, good) {
					t.Errorf("the routing answered:\n%v\nwant\n%v", got, good)
				}
			}},
		{name: "a reader reads", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "deid"})
		}, check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.GetReceiverResponse).GetReceiver().GetRouting(); !proto.Equal(got, good) {
				t.Errorf("the routing read:\n%v\nwant\n%v", got, good)
			}
		}},
		{name: "a reader changes", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid"}, UpdateMask: mask("routing")})
		}, code: connect.CodePermissionDenied},
		{name: "a mask naming a part of routing", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid"}, UpdateMask: mask("routing.routes")})
		}, code: connect.CodeInvalidArgument, msg: "replaced whole"},
		{name: "a receiver whose route names a profile changed to as received", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid",
				Deidentification: &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}}}, UpdateMask: mask("deidentification")})
		}, code: connect.CodeFailedPrecondition, msg: "names a profile version"},
	})
	read := mustCall[*v1.GetReceiverResponse](t, "get", call(f.as(f.lab1Admin).receivers.GetReceiver,
		&v1.GetReceiverRequest{Tenant: "lab1", Name: "deid"})).GetReceiver()
	stale := timestamppb.New(read.GetUpdateTime().AsTime().Add(-1))
	f.run(t, []tcase{
		{name: "a change on a receiver read before another change", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid"},
				UpdateMask: mask("routing"), ExpectedUpdateTime: stale})
		}, code: connect.CodeFailedPrecondition, msg: "changed after it was read"},
		{name: "a change on the receiver as read", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "deid"},
				UpdateMask: mask("routing"), ExpectedUpdateTime: read.GetUpdateTime()})
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "deid", details: map[string]string{"update_mask": "routing"}},
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.SetReceiverResponse).GetReceiver().GetRouting(); got != nil {
					t.Errorf("a routing set to none answers %v", got)
				}
			}},
	})
	if !strings.Contains(read.String(), "LAB-A") {
		t.Errorf("the receiver read does not hold its route's condition: %v", read)
	}
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
