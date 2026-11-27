package api_test

import (
	"context"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// markEvents returns the marks lab1's feed reports, in order, as
// "<sender>=<mark>".
func markEvents(t *testing.T, f *senderFixture) []string {
	t.Helper()
	ev, ok := f.pipelineOf("lab1").(tenant.Events)
	if !ok {
		t.Fatal("lab1's store keeps no events")
	}
	page, err := ev.ReadEvents(context.Background(), "", []tenant.EventType{tenant.EventSenderAllowIdentified}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range page.Events {
		p := e.Payload.(*tenant.SenderAllowIdentifiedChanged)
		mark := "false"
		if p.AllowIdentified {
			mark = "true"
		}
		out = append(out, p.Sender+"="+mark)
	}
	return out
}

// marked is a DIMSE sender as a request gives it with allow_identified set.
func marked(name string) *v1.Sender {
	s := dimseTo(name, "pacs.lab.example", 104)
	s.AllowIdentified = true
	return s
}

// TestSenderMark guards who may set or clear a sender's allow_identified:
// a tenant's admin alone is refused, setting and clearing alike, adding a
// sender marked too, recorded refused in the installation's log with the
// reason and nothing changed or reported; a masked key is refused even with
// admin on the installation and the tenant, recorded with the mark of a
// masked key; a key with admin on both, and the local operator, set and
// clear it, each change answered, audited in the tenant's log with the
// detail allow_identified and reported by an event; a change of another
// field by a tenant's admin keeps the mark and reports nothing.
func TestSenderMark(t *testing.T) {
	f := newSenderFixture(t)
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104))
	both, _ := f.key(onInstallation(master.RoleAdmin), f.onTenant("lab1", master.RoleAdmin))
	masked, _ := f.maskedKey(onInstallation(master.RoleAdmin), f.onTenant("lab1", master.RoleAdmin))
	notAdmin := &wantAudit{outcome: master.OutcomeRefused, tenant: "lab1", reason: "only an admin on the installation"}
	f.run(t, []tcase{
		{name: "a tenant's admin sets it", as: f.lab1Admin, call: setSender(marked("pacs"), "allow_identified"),
			code: connect.CodePermissionDenied, msg: "allow_identified", audit: notAdmin},
		{name: "a tenant's admin clears it", as: f.lab1Admin, call: setSender(dimseTo("pacs", "", 0), "allow_identified"),
			code: connect.CodePermissionDenied, msg: "allow_identified", audit: notAdmin},
		{name: "a tenant's admin adds a sender marked", as: f.lab1Admin, call: addSender("lab1", marked("other")),
			code: connect.CodePermissionDenied, msg: "allow_identified", audit: notAdmin},
		{name: "a masked key with admin on both sets it", as: masked, call: setSender(marked("pacs"), "allow_identified"),
			code: connect.CodePermissionDenied, audit: maskedRefusal()},
		{name: "a masked key with admin on both clears it", as: masked, call: setSender(dimseTo("pacs", "", 0), "allow_identified"),
			code: connect.CodePermissionDenied, audit: maskedRefusal()},
		{name: "a masked key with admin on both adds a sender marked", as: masked, call: addSender("lab1", marked("other")),
			code: connect.CodePermissionDenied, audit: maskedRefusal()},
	})
	if got := markEvents(t, f); len(got) != 0 {
		t.Fatalf("refused changes reported %v", got)
	}
	if got := mustCall[*v1.GetSenderResponse](t, "get", getSender("pacs")(f.as(local))); got.GetSender().GetAllowIdentified() {
		t.Fatal("a refused change set the mark")
	}
	answers := func(want bool) func(*testing.T, outcome) {
		return func(t *testing.T, o outcome) {
			var s *v1.Sender
			switch r := o.res.(type) {
			case *v1.SetSenderResponse:
				s = r.GetSender()
			case *v1.AddSenderResponse:
				s = r.GetSender()
			}
			if s.GetAllowIdentified() != want {
				t.Errorf("the answer's allow_identified is %v, want %v", s.GetAllowIdentified(), want)
			}
		}
	}
	done := func(mark string) *wantAudit {
		return &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs", details: map[string]string{tenant.DetailAllowIdentified: mark}}
	}
	f.run(t, []tcase{
		{name: "admin on both sets it", as: both, call: setSender(marked("pacs"), "allow_identified"), audit: done("true"), check: answers(true)},
		{name: "a tenant's admin changes another field", as: f.lab1Admin, call: setSender(&v1.Sender{Name: "pacs", MaxAssociations: 3,
			Type: dimseTo("x", "pacs.lab.example", 104).Type}, "max_associations"), check: answers(true)},
		{name: "a tenant's admin points the marked sender elsewhere", as: f.lab1Admin, call: setSender(dimseTo("pacs", "pacs.lab.example", 11112), "dimse.port"),
			code: connect.CodePermissionDenied, msg: "marked allow_identified", audit: &wantAudit{outcome: master.OutcomeRefused, tenant: "lab1", reason: "changes where it sends"}},
		{name: "the local operator clears it", as: local, call: setSender(dimseTo("pacs", "", 0), "allow_identified"), audit: done("false"), check: answers(false)},
		{name: "admin on both adds a sender marked", as: both, call: addSender("lab1", marked("other")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "other", details: map[string]string{tenant.DetailAllowIdentified: "true"}},
			check: answers(true)},
	})
	got := markEvents(t, f)
	if len(got) != 3 || got[0] != "pacs=true" || got[1] != "pacs=false" || got[2] != "other=true" {
		t.Errorf("the mark's events %v, want [pacs=true pacs=false other=true]", got)
	}
}

// TestMarkedSenderStaysWhereTheInstallationPutIt guards where a sender
// marked allow_identified sends: a tenant's admin changing its host, its
// port or a stow sender's URL is refused, recorded refused in the
// installation's log and nothing changed; a masked key with admin on both
// scopes is refused too; a key with admin on the installation and the
// tenant makes the change; a tenant's admin still moves a sender that is
// not marked, and still changes a marked sender's other fields.
func TestMarkedSenderStaysWhereTheInstallationPutIt(t *testing.T) {
	f := newSenderFixture(t)
	markedStow := stowTo("web", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")
	markedStow.AllowIdentified = true
	f.addedSenders(t, marked("open"), markedStow, dimseTo("plain", "pacs.lab.example", 104))
	both, _ := f.key(onInstallation(master.RoleAdmin), f.onTenant("lab1", master.RoleAdmin))
	masked, _ := f.maskedKey(onInstallation(master.RoleAdmin), f.onTenant("lab1", master.RoleAdmin))
	refused := &wantAudit{outcome: master.OutcomeRefused, tenant: "lab1", reason: "changes where it sends"}
	f.run(t, []tcase{
		{name: "a tenant's admin changes the host", as: f.lab1Admin, call: setSender(dimseTo("open", "10.20.0.7", 104), "dimse.host"),
			code: connect.CodePermissionDenied, msg: "marked allow_identified", audit: refused},
		{name: "a tenant's admin changes the port", as: f.lab1Admin, call: setSender(dimseTo("open", "pacs.lab.example", 11112), "dimse.port"),
			code: connect.CodePermissionDenied, msg: "marked allow_identified", audit: refused},
		{name: "a tenant's admin changes the called AE title", as: f.lab1Admin, call: setSender(dimseTo("open", "pacs.lab.example", 104), "dimse.called_ae_title"),
			code: connect.CodePermissionDenied, msg: "marked allow_identified", audit: refused},
		{name: "a tenant's admin changes the URL", as: f.lab1Admin, call: setSender(stowTo("web", "https://orthanc.lab.example:8443/dicom-web",
			v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", ""), "stow.url"),
			code: connect.CodePermissionDenied, msg: "marked allow_identified", audit: refused},
		{name: "a masked key with admin on both changes the host", as: masked, call: setSender(dimseTo("open", "10.20.0.7", 104), "dimse.host"),
			code: connect.CodePermissionDenied, audit: maskedRefusal()},
	})
	if got := mustCall[*v1.GetSenderResponse](t, "get", getSender("open")(f.as(local))).GetSender(); got.GetDimse().GetHost() != "pacs.lab.example" ||
		got.GetDimse().GetPort() != 104 || !got.GetAllowIdentified() {
		t.Fatalf("a refused change moved the marked sender: %v", got)
	}
	host := func(want string) func(*testing.T, outcome) {
		return func(t *testing.T, o outcome) {
			if got := o.res.(*v1.SetSenderResponse).GetSender().GetDimse().GetHost(); got != want {
				t.Errorf("the sender's host is %q, want %q", got, want)
			}
		}
	}
	f.run(t, []tcase{
		{name: "admin on both changes the marked sender's host", as: both, call: setSender(dimseTo("open", "10.20.0.7", 104), "dimse.host"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "open"}, check: host("10.20.0.7")},
		{name: "a tenant's admin changes an unmarked sender's host", as: f.lab1Admin, call: setSender(dimseTo("plain", "10.20.0.8", 104), "dimse.host"),
			check: host("10.20.0.8")},
		{name: "a tenant's admin changes the marked sender's rate", as: f.lab1Admin, call: setSender(&v1.Sender{Name: "open", MaxAssociations: 4,
			Type: dimseTo("x", "10.20.0.7", 104).Type}, "max_associations")},
	})
}

// TestMarkedSenderKeepsItsAccount guards the account a marked stow sender
// sends as, which on a DICOMweb peer with several accounts decides where
// the data lands: a tenant's admin changing its authentication, user name
// and credential is refused, recorded refused in the installation's log,
// and nothing moves; so is a tenant's admin changing the credential alone
// once it has one; a key with admin on the installation and the tenant
// makes the change.
func TestMarkedSenderKeepsItsAccount(t *testing.T) {
	f := newSenderFixture(t)
	web := stowTo("web", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")
	web.AllowIdentified = true
	f.addedSenders(t, web)
	both, _ := f.key(onInstallation(master.RoleAdmin), f.onTenant("lab1", master.RoleAdmin))
	account := stowTo("web", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BASIC,
		"other-project", "a-long-enough-secret-123")
	refused := &wantAudit{outcome: master.OutcomeRefused, tenant: "lab1", reason: "changes where it sends"}
	f.run(t, []tcase{
		{name: "a tenant's admin changes the account", as: f.lab1Admin, call: setSender(account, "stow.authentication", "stow.username", "stow.credential"),
			code: connect.CodePermissionDenied, msg: "account", audit: refused},
	})
	got := mustCall[*v1.GetSenderResponse](t, "get", getSender("web")(f.as(local))).GetSender()
	if got.GetStow().GetAuthentication() != v1.StowAuthentication_STOW_AUTHENTICATION_NONE || got.GetStow().GetUsername() != "" ||
		got.GetStow().GetCredentialSet() || !got.GetAllowIdentified() {
		t.Fatalf("a refused change moved the marked sender's account: %v", got)
	}
	if _, err := f.sealedCredential("web"); err == nil {
		t.Fatal("a refused change stored a credential")
	}
	f.run(t, []tcase{
		{name: "admin on both changes the account", as: both, call: setSender(account, "stow.authentication", "stow.username", "stow.credential"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "web"},
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.SetSenderResponse).GetSender()
				if s.GetStow().GetUsername() != "other-project" || !s.GetStow().GetCredentialSet() || !s.GetAllowIdentified() {
					t.Errorf("the changed sender: %v", s)
				}
			}},
		{name: "a tenant's admin changes the credential alone", as: f.lab1Admin, call: setSender(stowTo("web", "https://orthanc.lab.example/dicom-web",
			v1.StowAuthentication_STOW_AUTHENTICATION_BASIC, "other-project", "another-long-secret-456"), "stow.credential"),
			code: connect.CodePermissionDenied, msg: "account", audit: refused},
	})
	noSecretInLog(t, f, "a-long-enough-secret-123")
	noSecretInLog(t, f, "another-long-secret-456")
}
