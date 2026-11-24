package api_test

import (
	"slices"
	"strconv"
	"sync"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestReceiverForwardsTo guards forward_to of a receiver: stored, answered
// and changed by a mask naming it, a sender the tenant does not have a
// precondition on add and on change, more than 16, a name twice and a bad
// name invalid, and a sender a receiver names that cannot be removed until
// the receiver stops naming it.
func TestReceiverForwardsTo(t *testing.T) {
	f := newSenderFixture(t)
	f.receiving(t)
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104), dimseTo("backup", "10.20.0.9", 104))
	with := func(name string, port int32, forward ...string) *v1.Receiver {
		r := dimseReceiver(name, "main", port)
		r.ForwardTo = forward
		return r
	}
	forwarding := func(name string, forward ...string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: name, ForwardTo: forward}, UpdateMask: mask("forward_to")})
		}
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = "pacs"
	}
	f.run(t, []tcase{
		{name: "a sender the tenant does not have", as: f.lab1Admin, call: addReceiver("lab1", with("r1", 11120, "none")), code: connect.CodeFailedPrecondition, msg: "none"},
		{name: "a bad sender name", as: f.lab1Admin, call: addReceiver("lab1", with("r1", 11120, "Pacs")), code: connect.CodeInvalidArgument},
		{name: "a sender named twice", as: f.lab1Admin, call: addReceiver("lab1", with("r1", 11120, "pacs", "pacs")), code: connect.CodeInvalidArgument, msg: "once"},
		{name: "17 senders", as: f.lab1Admin, call: addReceiver("lab1", with("r1", 11120, many...)), code: connect.CodeInvalidArgument, msg: "forward_to"},
		{name: "an operator may not set it", as: f.lab1Operator, call: addReceiver("lab1", with("r1", 11120, "pacs")), code: connect.CodePermissionDenied},
		{name: "added with senders", as: f.lab1Admin, call: addReceiver("lab1", with("r1", 11120, "pacs", "backup")), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.AddReceiverResponse).GetReceiver().GetForwardTo(); !slices.Equal(got, []string{"pacs", "backup"}) {
				t.Errorf("forward_to answered %v", got)
			}
		}},
		{name: "a sender named by a receiver is not removed", as: f.lab1Admin, call: removeSender("backup", false), code: connect.CodeFailedPrecondition, msg: "r1"},
		{name: "changed to another sender", as: f.lab1Admin, call: forwarding("r1", "pacs"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "r1", details: map[string]string{"update_mask": "forward_to"}},
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.SetReceiverResponse).GetReceiver().GetForwardTo(); !slices.Equal(got, []string{"pacs"}) {
					t.Errorf("forward_to answered %v", got)
				}
			}},
		{name: "changed to a sender the tenant does not have", as: f.lab1Admin, call: forwarding("r1", "none"), code: connect.CodeFailedPrecondition},
		{name: "another field changed leaves it", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "r1", Enabled: false}, UpdateMask: mask("enabled")})
		}, check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.SetReceiverResponse).GetReceiver().GetForwardTo(); !slices.Equal(got, []string{"pacs"}) {
				t.Errorf("forward_to answered %v", got)
			}
		}},
		{name: "a sender no receiver names is removed now", as: f.lab1Admin, call: removeSender("backup", false)},
		{name: "forward_to emptied", as: f.lab1Admin, call: forwarding("r1"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.SetReceiverResponse).GetReceiver().GetForwardTo(); len(got) != 0 {
				t.Errorf("forward_to answered %v", got)
			}
		}},
		{name: "and its sender is removed", as: f.lab1Admin, call: removeSender("pacs", false)},
	})
}

// TestStatusAnswersJobsAndSenders guards the tenant's status: its jobs in
// progress, one entry per state in the order of the states and none for
// an ended one; and each sender ordered by name with the health the part
// that sends reports and its open associations.
func TestStatusAnswersJobsAndSenders(t *testing.T) {
	d := newJobData(t)
	f := d.f
	sending := f.sending()
	subject := d.session.GetSubjectLabel()
	d.makeJob(t, "pacs", subject)
	d.work(t, "pacs", false, false)
	d.makeJob(t, "pacs", subject)
	d.makeJob(t, "pacs", subject)
	sending.setHealth("lab1", "pacs", api.SenderHealth{State: tenant.SenderUp, Since: t0, LastSuccess: t0, OpenAssociations: 2})
	status := mustCall[*v1.GetStatusResponse](t, "status", call(f.as(f.lab1Reader).monitor.GetStatus, &v1.GetStatusRequest{Scope: tenantScope("lab1")}))
	s := status.GetTenants()[0]
	var jobs []string
	for _, c := range s.GetCounts().GetJobsInProgress() {
		jobs = append(jobs, c.GetState().String()+"="+strconv.FormatInt(c.GetJobs(), 10))
	}
	if !slices.Equal(jobs, []string{"JOB_STATE_QUEUED=2"}) {
		t.Errorf("jobs in progress %v: two queued, the one that ended not listed", jobs)
	}
	if len(s.GetSenders()) != 2 || s.GetSenders()[0].GetSender() != "pacs" || s.GetSenders()[1].GetSender() != "spare" {
		t.Fatalf("senders %v", s.GetSenders())
	}
	if p := s.GetSenders()[0]; p.GetHealth().GetState() != v1.SenderState_SENDER_STATE_UP || p.GetOpenAssociations() != 2 {
		t.Errorf("pacs: %v", p)
	}
	if sp := s.GetSenders()[1]; sp.GetHealth().GetState() != v1.SenderState_SENDER_STATE_UNKNOWN || sp.GetOpenAssociations() != 0 {
		t.Errorf("a sender nothing reports on is unknown: %v", sp)
	}
}

// TestDestinationSettingsAreHeldWhileSendersUseThem guards the settings a
// sender depends on: a value of sender.destinations that would take away a
// destination a sender uses is refused, and so is unsetting it, with the
// sender named and the tenant; a value that keeps it is taken;
// sender.allow-plain-http cannot be turned off while a sender sends over
// plain http; and none of the sender settings accepts a value its rule
// refuses.
func TestDestinationSettingsAreHeldWhileSendersUseThem(t *testing.T) {
	f := newSenderFixture(t)
	f.setSetting(installation(), settings.SenderAllowPlainHTTP, "true")
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104),
		stowTo("plain", "http://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", ""))
	set := func(name, value string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: installation(), Value: value}})
		}
	}
	unset := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: name, Scope: installation()})
		}
	}
	f.run(t, []tcase{
		{name: "a host taken away", as: f.instAdmin, call: set(settings.SenderDestinations, "orthanc.lab.example"), code: connect.CodeFailedPrecondition, msg: "pacs"},
		{name: "a port taken away", as: f.instAdmin, call: set(settings.SenderDestinations, "pacs.lab.example ports=11112\northanc.lab.example"), code: connect.CodeFailedPrecondition, msg: "lab1"},
		{name: "unset", as: f.instAdmin, call: unset(settings.SenderDestinations), code: connect.CodeFailedPrecondition, msg: "no longer allows"},
		{name: "a value that is not one", as: f.instAdmin, call: set(settings.SenderDestinations, "10.0.0.1/8"), code: connect.CodeInvalidArgument},
		{name: "a value that keeps them", as: f.instAdmin, call: set(settings.SenderDestinations, "pacs.lab.example\northanc.lab.example ports=80\n10.0.0.0/8")},
		{name: "plain http off while one sender uses it", as: f.instAdmin, call: set(settings.SenderAllowPlainHTTP, "false"), code: connect.CodeFailedPrecondition, msg: "plain"},
		{name: "plain http unset while one sender uses it", as: f.instAdmin, call: unset(settings.SenderAllowPlainHTTP), code: connect.CodeFailedPrecondition},
		{name: "plain http on", as: f.instAdmin, call: set(settings.SenderAllowPlainHTTP, "true")},
		{name: "associations out of range", as: f.instAdmin, call: set(settings.SenderMaxAssociations, "1025"), code: connect.CodeInvalidArgument},
		{name: "associations", as: f.instAdmin, call: set(settings.SenderMaxAssociations, "64")},
		{name: "a tenant's share set by its own admin", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.SenderTenantMaxAssociations, Scope: tenantScope("lab1"), Value: "4"}})
		}, code: connect.CodePermissionDenied},
		{name: "a tenant's share set by the installation's admin", as: f.instAdmin, call: func(c clients) outcome {
			return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.SenderTenantMaxAssociations, Scope: tenantScope("lab1"), Value: "4"}})
		}},
		{name: "failures kept, a tenant's own", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.JobFailuresKept, Scope: tenantScope("lab1"), Value: "500"}})
		}},
		{name: "failures kept out of range", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.JobFailuresKept, Scope: tenantScope("lab1"), Value: "99"}})
		}, code: connect.CodeInvalidArgument},
	})
}

// TestConcurrentSetSenderKeepsTheDestinationsRule guards that two changes of
// one sender made at once cannot store a destination the installation does
// not allow: with "a.example" allowed on ports 104 and 11112 and
// "b.example" on 104 only, a change of the host to b.example and a change of
// the port to 11112 are each allowed on the sender as it was, and together
// they are not. Each change is read, checked and stored in one step, so the
// second sees the first.
func TestConcurrentSetSenderKeepsTheDestinationsRule(t *testing.T) {
	f := newSenderFixture(t)
	f.setSetting(installation(), settings.SenderDestinations, "a.example ports=104,11112\nb.example ports=104")
	f.addedSenders(t, dimseTo("pacs", "a.example", 104))
	cl := f.as(f.lab1Admin)
	for i := range 40 {
		mustCall[*v1.SetSenderResponse](t, "reset", setSender(dimseTo("pacs", "a.example", 104), "dimse.host", "dimse.port")(cl))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, change := range []func(clients) outcome{
			setSender(dimseTo("pacs", "b.example", 104), "dimse.host"),
			setSender(dimseTo("pacs", "a.example", 11112), "dimse.port"),
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				change(cl)
			}()
		}
		close(start)
		wg.Wait()
		got := mustCall[*v1.GetSenderResponse](t, "get", getSender("pacs")(cl)).GetSender().GetDimse()
		if got.GetHost() == "b.example" && got.GetPort() == 11112 {
			t.Fatalf("run %d: stored b.example:11112, which sender.destinations does not allow", i)
		}
	}
}
