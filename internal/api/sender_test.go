package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// noSecretInLog fails the test when the plaintext credential is in any
// entry of lab1's log or the installation's log.
func noSecretInLog(t *testing.T, f *senderFixture, secret string) {
	t.Helper()
	for name, log := range map[string][]master.AuditEntry{"lab1": f.tenantLog("lab1"), "installation": f.masterLog()} {
		if strings.Contains(fmt.Sprintf("%+v", log), secret) {
			t.Errorf("the credential is in the %s's audit log", name)
		}
	}
}

// testAddSender guards AddSender: admin on the tenant only; each rule of a
// sender's fields; the installation's destinations and plain http as
// preconditions; the defaults filled in and answered; a credential sealed
// for the sender's row and never answered or logged; a name taken; and the
// entry with the sender's type and destination.
func testAddSender(t *testing.T) {
	f := newSenderFixture(t)
	f.downTenant("down")
	add := func(s *v1.Sender) func(clients) outcome { return addSender("lab1", s) }
	withDimse := func(edit func(d *v1.DimseSender)) *v1.Sender {
		s := dimseTo("pacs", "pacs.lab.example", 104)
		edit(s.GetDimse())
		return s
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "reader", as: f.lab1Reader, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied},
		{name: "admin on the installation", as: f.instAdmin, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied},
		{name: "admin of another tenant", as: f.lab2Admin, call: add(dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: addSender("nolab", dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "no such tenant, may list", as: local, call: addSender("nolab", dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodeNotFound},
		{name: "denied before invalid", as: f.lab1Reader, call: add(nil), code: connect.CodePermissionDenied},
		{name: "no sender", as: f.lab1Admin, call: add(nil), code: connect.CodeInvalidArgument, msg: "sender: is required", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a bad name", as: f.lab1Admin, call: add(dimseTo("Pacs", "pacs.lab.example", 104)), code: connect.CodeInvalidArgument, msg: "sender.name"},
		{name: "a name of 58 characters", as: f.lab1Admin, call: add(dimseTo(strings.Repeat("a", 58), "pacs.lab.example", 104)), code: connect.CodeInvalidArgument, msg: "57"},
		{name: "no type", as: f.lab1Admin, call: add(&v1.Sender{Name: "pacs"}), code: connect.CodeInvalidArgument, msg: "has a type"},
		{name: "no port", as: f.lab1Admin, call: add(dimseTo("pacs", "pacs.lab.example", 0)), code: connect.CodeInvalidArgument, msg: "port"},
		{name: "a host with a bracket", as: f.lab1Admin, call: add(dimseTo("pacs", "[::1]", 104)), code: connect.CodeInvalidArgument, msg: "host"},
		{name: "an AE title too long", as: f.lab1Admin, call: add(withDimse(func(d *v1.DimseSender) { d.CalledAeTitle = strings.Repeat("A", 17) })), code: connect.CodeInvalidArgument, msg: "AE title"},
		{name: "TLS without a CA file", as: f.lab1Admin, call: add(withDimse(func(d *v1.DimseSender) { d.Tls = &v1.SenderTls{ServerName: "pacs.lab.example"} })), code: connect.CodeInvalidArgument, msg: "CA file"},
		{name: "a TLS file out of the tenant's directory", as: f.lab1Admin, call: add(withDimse(func(d *v1.DimseSender) { d.Tls = &v1.SenderTls{CaFile: "../lab2/ca.pem"} })), code: connect.CodeInvalidArgument, msg: "TLS"},
		{name: "too many associations", as: f.lab1Admin, call: add(&v1.Sender{Name: "pacs", MaxAssociations: 17, Type: dimseTo("x", "pacs.lab.example", 104).Type}), code: connect.CodeInvalidArgument, msg: "associations"},
		{name: "a byte rate too low", as: f.lab1Admin, call: add(&v1.Sender{Name: "pacs", MaxBytesPerSecond: 100, Type: dimseTo("x", "pacs.lab.example", 104).Type}), code: connect.CodeInvalidArgument, msg: "byte rate"},
		{name: "a connect timeout in fractions", as: f.lab1Admin, call: add(&v1.Sender{Name: "pacs", ConnectTimeout: func() *durationpb.Duration { d := seconds(5); d.Nanos = 5; return d }(), Type: dimseTo("x", "pacs.lab.example", 104).Type}), code: connect.CodeInvalidArgument, msg: "connect_timeout"},
		{name: "a connect timeout of zero", as: f.lab1Admin, call: add(&v1.Sender{Name: "pacs", ConnectTimeout: seconds(0), Type: dimseTo("x", "pacs.lab.example", 104).Type}), code: connect.CodeInvalidArgument, msg: "timeout"},
		{name: "a stow sender with no authentication stated", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_UNSPECIFIED, "", "")), code: connect.CodeInvalidArgument, msg: "authentication"},
		{name: "a stow sender with basic and no credential", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BASIC, "user", "")), code: connect.CodeInvalidArgument, msg: "stow.credential"},
		{name: "a stow sender with none and a credential", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "s3cret-token")), code: connect.CodeInvalidArgument, msg: "stow.credential"},
		{name: "a credential that starts with a space", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", " s3cret-token")), code: connect.CodeInvalidArgument, msg: "space"},
		{name: "a credential with a control character", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "s3cret\ntoken")), code: connect.CodeInvalidArgument, msg: "ASCII"},
		{name: "a URL with user information", as: f.lab1Admin, call: add(stowTo("orthanc", "https://user:pw@orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeInvalidArgument, msg: "user information"},
		{name: "a header name the server writes", as: f.lab1Admin, call: add(&v1.Sender{Name: "orthanc", Type: &v1.Sender_Stow{Stow: &v1.StowSender{
			Url: "https://orthanc.lab.example/dicom-web", Authentication: v1.StowAuthentication_STOW_AUTHENTICATION_HEADER, HeaderName: "Authorization", Credential: "x"}}}), code: connect.CodeInvalidArgument, msg: "header"},
		{name: "a host not listed", as: f.lab1Admin, call: add(dimseTo("pacs", "elsewhere.example", 104)), code: connect.CodeFailedPrecondition, msg: "sender.destinations",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "pacs", details: map[string]string{"type": "dimse"}}},
		{name: "a port not listed", as: f.lab1Admin, call: add(dimseTo("pacs", "pacs.lab.example", 105)), code: connect.CodeFailedPrecondition, msg: "sender.dimse.port"},
		{name: "a stow port not listed names the URL", as: f.lab1Admin, call: add(stowTo("orthanc", "https://pacs.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeFailedPrecondition, msg: "sender.stow.url: the port"},
		{name: "a stow URL port that does not fit", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example:99999999999999999999/x", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeInvalidArgument, msg: "sender.stow.url"},
		{name: "a stow URL port past 65535", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example:65536/x", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeInvalidArgument, msg: "1 to 65535"},
		{name: "a stow URL port of zero", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example:0/x", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeInvalidArgument, msg: "1 to 65535"},
		{name: "an address outside the listed range", as: f.lab1Admin, call: add(dimseTo("pacs", "10.20.1.5", 104)), code: connect.CodeFailedPrecondition, msg: "sender.destinations"},
		{name: "a plain http URL while it is not allowed", as: f.lab1Admin, call: add(stowTo("orthanc", "http://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")), code: connect.CodeFailedPrecondition, msg: "plain http"},
		{name: "a tenant not ready", as: local, call: addSender("down", dimseTo("pacs", "pacs.lab.example", 104)), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
		{name: "added, a DIMSE sender", as: f.lab1Admin, call: add(withDimse(func(d *v1.DimseSender) { d.CalledAeTitle = "  PACS " })),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs", tenant: "lab1", details: map[string]string{"type": "dimse", "destination": "pacs.lab.example:104"}},
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.AddSenderResponse).GetSender()
				if s.GetDimse().GetCalledAeTitle() != "PACS" || s.GetMaxAssociations() != 2 || s.GetConnectTimeout().GetSeconds() != 30 ||
					s.GetResponseTimeout().GetSeconds() != 300 || s.GetCreateTime() == nil || s.GetUpdateTime() == nil || !s.GetEnabled() {
					t.Errorf("the defaults and the stored values: %v", s)
				}
				if h := s.GetHealth(); h.GetState() != v1.SenderState_SENDER_STATE_UNKNOWN || h.GetSince() == nil {
					t.Errorf("the health of an enabled sender nothing connects: %v", h)
				}
			}},
		{name: "a name taken", as: f.lab1Admin, call: add(dimseTo("pacs", "pacs.lab.example", 11112)), code: connect.CodeAlreadyExists},
		{name: "added, disabled", as: f.lab1Admin, call: add(func() *v1.Sender { s := dimseTo("spare", "10.20.0.7", 104); s.Enabled = false; return s }()),
			check: func(t *testing.T, o outcome) {
				h := o.res.(*v1.AddSenderResponse).GetSender().GetHealth()
				if h.GetState() != v1.SenderState_SENDER_STATE_DOWN || h.GetDownReason() != v1.SenderDownReason_SENDER_DOWN_REASON_DISABLED || h.GetDetail() != "" {
					t.Errorf("the health of a disabled sender: %v", h)
				}
			}},
		{name: "added, a stow sender with a credential", as: f.lab1Admin, call: add(stowTo("orthanc", "https://orthanc.lab.example:8443/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "s3cret-token")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "orthanc", details: map[string]string{"type": "stow", "destination": "orthanc.lab.example:8443"}},
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.AddSenderResponse).GetSender().GetStow()
				if s.GetCredential() != "" || !s.GetCredentialSet() || s.GetMaxInstancesPerRequest() != 64 || s.GetMaxRequestBytes() != 1<<28 {
					t.Errorf("the answer: %v", s)
				}
				if strings.Contains(prototext.Format(o.res), "s3cret-token") {
					t.Error("the credential is in the answer")
				}
			}},
	})
	// The credential is sealed for the sender's row: the store holds neither
	// the plaintext nor something another sender's row would open, and
	// nothing logged holds it.
	sealed, err := f.sealedCredential("orthanc")
	if err != nil || strings.Contains(string(sealed), "s3cret-token") {
		t.Fatalf("the sealed credential: %q, %v", sealed, err)
	}
	if got, err := f.keys.Open(place.CredentialContext(f.tenantID("lab1"), "orthanc"), sealed); err != nil || string(got) != "s3cret-token" {
		t.Errorf("opened for its own row: %q, %v", got, err)
	}
	if _, err := f.keys.Open(place.CredentialContext(f.tenantID("lab1"), "other"), sealed); err == nil {
		t.Error("the credential opens for another sender's row")
	}
	noSecretInLog(t, f, "s3cret-token")
	// A plain http URL, once the installation allows it.
	f.setSetting(installation(), settings.SenderAllowPlainHTTP, "true")
	f.run(t, []tcase{
		{name: "plain http, allowed", as: f.lab1Admin, call: add(stowTo("plain", "http://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", ""))},
		{name: "plain http with TLS settings", as: f.lab1Admin, call: add(func() *v1.Sender {
			s := stowTo("plain2", "http://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", "")
			s.GetStow().Tls = &v1.SenderTls{ServerName: "x.example"}
			return s
		}()), code: connect.CodeInvalidArgument, msg: "TLS"},
	})
}

// testGetSender guards GetSender: reader on the tenant and up, a sender not
// there, a bad name, the health now, and a credential that is never
// answered, only whether one is held.
func testGetSender(t *testing.T) {
	f := newSenderFixture(t)
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104),
		stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "s3cret-token"))
	f.addedSenders(t, &v1.Sender{Name: "keyed", Enabled: true, Type: &v1.Sender_Stow{Stow: &v1.StowSender{
		Url: "https://orthanc.lab.example/x", Authentication: v1.StowAuthentication_STOW_AUTHENTICATION_HEADER, HeaderName: "X-Api-Key", Credential: "s3cret-token"}}})
	health := f.sending()
	health.setHealth("lab1", "pacs", api.SenderHealth{State: tenant.SenderDown, DownReason: tenant.SenderDownPeerFailing, Fault: tenant.SenderFaultRejected,
		Detail: "called-AE-title-not-recognized", Since: t0, FailedConnections: 3, NextTry: t0.Add(time.Minute), LastSuccess: t0.Add(-time.Hour), OpenAssociations: 0})
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: getSender("pacs"), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: getSender("pacs"), code: connect.CodePermissionDenied},
		{name: "a bad name", as: f.lab1Reader, call: getSender("Pacs"), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "no such sender", as: f.lab1Reader, call: getSender("none"), code: connect.CodeNotFound, msg: "no sender with this name"},
		{name: "reader", as: f.lab1Reader, call: getSender("pacs"), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs"},
			check: func(t *testing.T, o outcome) {
				h := o.res.(*v1.GetSenderResponse).GetSender().GetHealth()
				if h.GetState() != v1.SenderState_SENDER_STATE_DOWN || h.GetDownReason() != v1.SenderDownReason_SENDER_DOWN_REASON_PEER_FAILING ||
					h.GetFault() != v1.SenderFault_SENDER_FAULT_REJECTED || h.GetFailedConnections() != 3 || h.GetNextTryTime() == nil ||
					h.GetLastSuccessTime() == nil || h.GetDetail() != "called-AE-title-not-recognized" {
					t.Errorf("the health as the server's view gives it: %v", h)
				}
			}},
		{name: "a stow sender answers no credential", as: f.lab1Reader, call: getSender("keyed"), check: func(t *testing.T, o outcome) {
			s := o.res.(*v1.GetSenderResponse).GetSender().GetStow()
			if s.GetCredential() != "" || !s.GetCredentialSet() || s.GetHeaderName() != "X-Api-Key" || strings.Contains(prototext.Format(o.res), "s3cret-token") {
				t.Errorf("%v", s)
			}
		}},
		{name: "the same answer read twice", as: f.lab1Reader, call: getSender("orthanc"), check: func(t *testing.T, o outcome) {
			again := getSender("orthanc")(f.as(f.lab1Reader))
			if !proto.Equal(o.res, again.res) {
				t.Errorf("%v\n%v", o.res, again.res)
			}
		}},
	})
}

// testListSenders guards ListSenders: reader on the tenant, the order by
// name, paging with a token that is not another tenant's, and an entry in
// the tenant's log.
func testListSenders(t *testing.T) {
	f := newSenderFixture(t)
	f.addedSenders(t, dimseTo("c-pacs", "pacs.lab.example", 104), dimseTo("a-pacs", "pacs.lab.example", 11112), dimseTo("b-pacs", "10.20.0.9", 104))
	list := func(size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.senders.ListSenders, &v1.ListSendersRequest{Tenant: "lab1", PageSize: size, PageToken: token})
		}
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(0, ""), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(0, ""), code: connect.CodePermissionDenied},
		{name: "a negative page size", as: f.lab1Reader, call: list(-1, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "all, in order of name", as: f.lab1Reader, call: list(0, ""), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-"},
			check: func(t *testing.T, o outcome) {
				var names []string
				for _, s := range o.res.(*v1.ListSendersResponse).GetSenders() {
					names = append(names, s.GetName())
					if s.GetHealth() == nil {
						t.Errorf("%s has no health", s.GetName())
					}
				}
				if strings.Join(names, ",") != "a-pacs,b-pacs,c-pacs" {
					t.Errorf("names %v", names)
				}
			}},
		{name: "a first page", as: f.lab1Reader, call: list(2, ""), check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListSendersResponse)
			if len(r.GetSenders()) != 2 || r.GetNextPageToken() == "" {
				t.Fatalf("%v", r)
			}
			token = r.GetNextPageToken()
		}},
		{name: "the last page", as: f.lab1Reader, call: func(c clients) outcome { return list(2, token)(c) }, check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListSendersResponse)
			if len(r.GetSenders()) != 1 || r.GetSenders()[0].GetName() != "c-pacs" || r.GetNextPageToken() != "" {
				t.Errorf("%v", r)
			}
		}},
		{name: "a token of another list", as: f.lab1Reader, call: list(2, "x"), code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// testSetSender guards SetSender: admin on the tenant only; the mask's
// rules (empty, "*", a type's whole message, another type's field, a field
// the server answers, a value-less duration or count); the installation's
// destinations as preconditions; a credential kept unless the mask names
// it, replaced when it does, cleared when the authentication becomes none,
// never answered or logged; and the entry with the mask's paths.
func testSetSender(t *testing.T) {
	f := newSenderFixture(t)
	f.downTenant("down")
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104),
		stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "first-token"))
	var before []byte
	set := func(s *v1.Sender, paths ...string) func(clients) outcome { return setSender(s, paths...) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: set(&v1.Sender{Name: "pacs"}, "enabled"), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: set(&v1.Sender{Name: "pacs"}, "enabled"), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: set(&v1.Sender{Name: "pacs"}, "enabled"), code: connect.CodePermissionDenied},
		{name: "no sender", as: f.lab1Admin, call: set(nil, "enabled"), code: connect.CodeInvalidArgument, msg: "sender: is required"},
		{name: "no name", as: f.lab1Admin, call: set(&v1.Sender{}, "enabled"), code: connect.CodeInvalidArgument, msg: "sender.name"},
		{name: "no mask", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}), code: connect.CodeInvalidArgument, msg: "update_mask"},
		{name: "a star", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "*"), code: connect.CodeInvalidArgument, msg: "*"},
		{name: "a path twice", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "enabled", "enabled"), code: connect.CodeInvalidArgument, msg: "twice"},
		{name: "a type's whole message", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "dimse"), code: connect.CodeInvalidArgument, msg: "one by one"},
		{name: "a field no one has", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "shade"), code: connect.CodeInvalidArgument, msg: "names no field"},
		{name: "the name", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "name"), code: connect.CodeInvalidArgument, msg: "does not change"},
		{name: "the health", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "health"), code: connect.CodeInvalidArgument, msg: "answered by the server"},
		{name: "the other type's field", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "stow.url"), code: connect.CodeInvalidArgument, msg: "another type"},
		{name: "a count named with no value", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "max_associations"), code: connect.CodeInvalidArgument, msg: "no value"},
		{name: "a timeout named with no value", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "connect_timeout"), code: connect.CodeInvalidArgument, msg: "no value"},
		{name: "a stow size named with no value", as: f.lab1Admin, call: set(&v1.Sender{Name: "orthanc"}, "stow.max_request_bytes"), code: connect.CodeInvalidArgument, msg: "no value"},
		{name: "no such sender", as: f.lab1Admin, call: set(&v1.Sender{Name: "none"}, "enabled"), code: connect.CodeNotFound, msg: "no sender with this name"},
		{name: "a host not listed", as: f.lab1Admin, call: set(dimseTo("pacs", "elsewhere.example", 104), "dimse.host"), code: connect.CodeFailedPrecondition, msg: "sender.destinations"},
		{name: "a port not listed", as: f.lab1Admin, call: set(dimseTo("pacs", "pacs.lab.example", 9), "dimse.port"), code: connect.CodeFailedPrecondition, msg: "sender.dimse.port"},
		{name: "a stow URL port that does not fit", as: f.lab1Admin, call: set(stowTo("orthanc", "https://orthanc.lab.example:99999999999999999999/x", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "x"), "stow.url", "stow.credential"), code: connect.CodeInvalidArgument, msg: "1 to 65535"},
		{name: "authentication named and not stated", as: f.lab1Admin, call: set(&v1.Sender{Name: "orthanc", Type: &v1.Sender_Stow{Stow: &v1.StowSender{}}}, "stow.authentication"), code: connect.CodeInvalidArgument, msg: "stow.authentication"},
		{name: "a URL moved without its credential", as: f.lab1Admin, call: set(stowTo("orthanc", "https://orthanc.lab.example/other", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", ""), "stow.url"),
			code: connect.CodeInvalidArgument, msg: "credential"},
		{name: "a credential named and empty", as: f.lab1Admin, call: set(stowTo("orthanc", "", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", ""), "stow.credential"),
			code: connect.CodeInvalidArgument, msg: "credential"},
		{name: "a tenant not ready", as: local, call: func(c clients) outcome {
			return call(c.senders.SetSender, &v1.SetSenderRequest{Tenant: "down", Sender: &v1.Sender{Name: "pacs"}, UpdateMask: mask("enabled")})
		}, code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
		{name: "enabled turned off", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs"}, "enabled"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs", tenant: "lab1", details: map[string]string{"update_mask": "enabled"}},
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.SetSenderResponse).GetSender()
				if s.GetEnabled() || s.GetHealth().GetDownReason() != v1.SenderDownReason_SENDER_DOWN_REASON_DISABLED || s.GetDimse().GetHost() != "pacs.lab.example" {
					t.Errorf("only the named field changed: %v", s)
				}
			}},
		{name: "a field and a bound changed", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs", MaxAssociations: 4, MaxBytesPerSecond: 1 << 20, Type: dimseTo("x", "pacs.lab.example", 11112).Type}, "max_associations", "max_bytes_per_second", "dimse.port"),
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.SetSenderResponse).GetSender()
				if s.GetMaxAssociations() != 4 || s.GetMaxBytesPerSecond() != 1<<20 || s.GetDimse().GetPort() != 11112 || s.GetDimse().GetCalledAeTitle() != "PACS" {
					t.Errorf("%v", s)
				}
			}},
		{name: "a credential kept when another field changes", as: f.lab1Admin, call: set(&v1.Sender{Name: "orthanc", MaxAssociations: 3}, "max_associations"),
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.SetSenderResponse).GetSender(); s.GetStow().GetCredentialSet() != true || s.GetMaxAssociations() != 3 {
					t.Errorf("%v", s)
				}
				var err error
				if before, err = f.sealedCredential("orthanc"); err != nil {
					t.Fatal(err)
				}
				if got, err := f.keys.Open(place.CredentialContext(f.tenantID("lab1"), "orthanc"), before); err != nil || string(got) != "first-token" {
					t.Errorf("the credential kept: %q, %v", got, err)
				}
			}},
		{name: "a credential rotated, the authentication not stated", as: f.lab1Admin,
			call: set(&v1.Sender{Name: "orthanc", Type: &v1.Sender_Stow{Stow: &v1.StowSender{Credential: "rotated-token"}}}, "stow.credential"),
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.SetSenderResponse).GetSender().GetStow(); s.GetAuthentication() != v1.StowAuthentication_STOW_AUTHENTICATION_BEARER || !s.GetCredentialSet() {
					t.Errorf("%v", s)
				}
				sealed, err := f.sealedCredential("orthanc")
				if err != nil {
					t.Fatal(err)
				}
				if got, err := f.keys.Open(place.CredentialContext(f.tenantID("lab1"), "orthanc"), sealed); err != nil || string(got) != "rotated-token" {
					t.Errorf("the credential rotated: %q, %v", got, err)
				}
			}},
		{name: "a field named, an empty stow message", as: f.lab1Admin,
			call: set(&v1.Sender{Name: "orthanc", Enabled: false, Type: &v1.Sender_Stow{Stow: &v1.StowSender{}}}, "enabled"),
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.SetSenderResponse).GetSender(); s.GetEnabled() || s.GetStow().GetUrl() != "https://orthanc.lab.example/dicom-web" || s.GetStow().GetAuthentication() != v1.StowAuthentication_STOW_AUTHENTICATION_BEARER {
					t.Errorf("%v", s)
				}
			}},
		{name: "a URL and a new credential", as: f.lab1Admin, call: set(stowTo("orthanc", "https://orthanc.lab.example/v2/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BEARER, "", "second-token"), "stow.url", "stow.credential"),
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.SetSenderResponse).GetSender().GetStow()
				if s.GetUrl() != "https://orthanc.lab.example/v2/dicom-web" || s.GetCredential() != "" || strings.Contains(prototext.Format(o.res), "second-token") {
					t.Errorf("%v", s)
				}
				sealed, err := f.sealedCredential("orthanc")
				if err != nil {
					t.Fatal(err)
				}
				if got, err := f.keys.Open(place.CredentialContext(f.tenantID("lab1"), "orthanc"), sealed); err != nil || string(got) != "second-token" {
					t.Errorf("the credential replaced: %q, %v", got, err)
				}
			}},
		{name: "the authentication becomes none", as: f.lab1Admin, call: set(stowTo("orthanc", "", v1.StowAuthentication_STOW_AUTHENTICATION_NONE, "", ""), "stow.authentication"),
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.SetSenderResponse).GetSender().GetStow()
				if s.GetCredentialSet() || s.GetAuthentication() != v1.StowAuthentication_STOW_AUTHENTICATION_NONE {
					t.Errorf("%v", s)
				}
				if _, err := f.sealedCredential("orthanc"); err == nil {
					t.Error("the credential is still held")
				}
			}},
		{name: "TLS turned on", as: f.lab1Admin, call: set(&v1.Sender{Name: "pacs", Type: &v1.Sender_Dimse{Dimse: &v1.DimseSender{Tls: &v1.SenderTls{CaFile: "ca.pem", ServerName: "pacs.lab.example"}}}}, "dimse.tls"),
			check: func(t *testing.T, o outcome) {
				if tls := o.res.(*v1.SetSenderResponse).GetSender().GetDimse().GetTls(); tls.GetCaFile() != "ca.pem" || tls.GetServerName() != "pacs.lab.example" {
					t.Errorf("%v", tls)
				}
			}},
	})
	noSecretInLog(t, f, "first-token")
	noSecretInLog(t, f, "second-token")
	noSecretInLog(t, f, "rotated-token")
}

// testRemoveSender guards RemoveSender: admin on the tenant only; a sender
// answered as it was, without a credential; a repeat answered NOT_FOUND; a
// receiver that forwards to it, and a job of it in progress, as
// preconditions; cancel_jobs canceling those jobs with the removal and
// counting them; and the sealed credential gone with it.
func testRemoveSender(t *testing.T) {
	d := newJobData(t)
	f := d.sf
	f.addedSenders(t, stowTo("orthanc", "https://orthanc.lab.example/dicom-web", v1.StowAuthentication_STOW_AUTHENTICATION_BASIC, "user", "s3cret-token"),
		dimseTo("busy", "pacs.lab.example", 11112), dimseTo("fwd", "pacs.lab.example", 104))
	subject := d.session.GetSubjectLabel()
	job := d.makeJob(t, "busy", subject)
	mustCall[*v1.AddReceiverResponse](t, "a receiver that forwards", addReceiver("lab1", func() *v1.Receiver {
		r := dimseReceiver("scanner2", "main", 11120)
		r.ForwardTo = []string{"fwd"}
		return r
	}())(f.as(local)))
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: removeSender("orthanc", false), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: removeSender("orthanc", false), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: removeSender("orthanc", false), code: connect.CodePermissionDenied},
		{name: "a bad name", as: f.lab1Admin, call: removeSender("Orthanc", false), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "a receiver forwards to it", as: f.lab1Admin, call: removeSender("fwd", false), code: connect.CodeFailedPrecondition, msg: "scanner2",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "fwd"}},
		{name: "a job in progress", as: f.lab1Admin, call: removeSender("busy", false), code: connect.CodeFailedPrecondition,
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "busy"}},
		{name: "removed", as: f.lab1Admin, call: removeSender("orthanc", false),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "orthanc", tenant: "lab1", details: map[string]string{"type": "stow", "jobs_canceled": "0"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.RemoveSenderResponse)
				if r.GetSender().GetName() != "orthanc" || r.GetSender().GetStow().GetCredential() != "" || !r.GetSender().GetStow().GetCredentialSet() ||
					r.GetJobsCanceled() != 0 || strings.Contains(prototext.Format(o.res), "s3cret-token") {
					t.Errorf("%v", r)
				}
				if _, err := f.sealedCredential("orthanc"); err == nil {
					t.Error("the sealed credential outlived its sender")
				}
			}},
		{name: "again", as: f.lab1Admin, call: removeSender("orthanc", false), code: connect.CodeNotFound, msg: "no sender with this name"},
		{name: "its jobs canceled with it", as: f.lab1Admin, call: removeSender("busy", true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "busy", details: map[string]string{"cancel_jobs": "true", "jobs_canceled": "1"}},
			check: func(t *testing.T, o outcome) {
				if n := o.res.(*v1.RemoveSenderResponse).GetJobsCanceled(); n != 1 {
					t.Errorf("jobs canceled: %d", n)
				}
				j := mustCall[*v1.GetJobResponse](t, "the job", getJob(job.GetId())(f.as(f.lab1Reader))).GetJob()
				if j.GetState() != v1.JobState_JOB_STATE_CANCELED || j.GetSend().GetSender() != "busy" || j.GetReason() == "" {
					t.Errorf("the job of a removed sender: %v", j)
				}
			}},
		{name: "its name is held while a job names it", as: f.lab1Admin, call: addSender("lab1", dimseTo("busy", "pacs.lab.example", 104)), code: connect.CodeAlreadyExists},
	})
}

// testTestSender guards TestSender: admin on the tenant only; the steps
// answered as the part of the server that sends reports them, a failed step
// a result and not an error, a skipped step without a duration; a clear
// "unavailable" answer while nothing can connect; and the entry with whether
// every step passed.
func testTestSender(t *testing.T) {
	f := newSenderFixture(t)
	f.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104))
	test := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.senders.TestSender, &v1.TestSenderRequest{Tenant: "lab1", Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: test("pacs"), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: test("pacs"), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: test("pacs"), code: connect.CodePermissionDenied},
		{name: "a bad name", as: f.lab1Admin, call: test("Pacs"), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "no such sender", as: f.lab1Admin, call: test("none"), code: connect.CodeNotFound, msg: "no sender with this name"},
		{name: "nothing can connect yet", as: f.lab1Admin, call: test("pacs"), code: connect.CodeUnavailable, msg: "cannot test a sender",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "pacs"}},
	})
	sending := f.sending()
	sending.results = []api.SenderTestResult{
		{Step: api.StepResolve, Outcome: api.TestPassed, Duration: time.Millisecond},
		{Step: api.StepConnect, Outcome: api.TestFailed, Duration: 3 * time.Second, Fault: tenant.SenderFaultUnreachable, Detail: "no answer"},
		{Step: api.StepTLS, Outcome: api.TestSkipped},
		{Step: api.StepAssociate, Outcome: api.TestSkipped},
		{Step: api.StepRequest, Outcome: api.TestSkipped},
		{Step: api.StepRelease, Outcome: api.TestSkipped},
	}
	f.run(t, []tcase{
		{name: "a step failed", as: f.lab1Admin, call: test("pacs"), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs", details: map[string]string{"passed": "false"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.TestSenderResponse)
				if r.GetPassed() || len(r.GetResults()) != 6 {
					t.Fatalf("%v", r)
				}
				res := r.GetResults()
				if res[0].GetStep() != v1.SenderTestStep_SENDER_TEST_STEP_RESOLVE || res[0].GetOutcome() != v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED || res[0].GetDuration() == nil || res[0].GetFault() != v1.SenderFault_SENDER_FAULT_UNSPECIFIED {
					t.Errorf("a passed step: %v", res[0])
				}
				if res[1].GetOutcome() != v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED || res[1].GetFault() != v1.SenderFault_SENDER_FAULT_UNREACHABLE || res[1].GetDetail() != "no answer" {
					t.Errorf("a failed step: %v", res[1])
				}
				if res[2].GetOutcome() != v1.SenderTestOutcome_SENDER_TEST_OUTCOME_SKIPPED || res[2].GetDuration() != nil {
					t.Errorf("a skipped step: %v", res[2])
				}
			}},
	})
	sending.mu.Lock()
	sending.results = []api.SenderTestResult{{Step: api.StepResolve, Outcome: api.TestPassed}, {Step: api.StepConnect, Outcome: api.TestPassed}}
	sending.mu.Unlock()
	f.run(t, []tcase{
		{name: "every step passed", as: f.lab1Admin, call: test("pacs"), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "pacs", details: map[string]string{"passed": "true"}},
			check: func(t *testing.T, o outcome) {
				if !o.res.(*v1.TestSenderResponse).GetPassed() {
					t.Errorf("%v", o.res)
				}
			}},
	})
	if len(sending.asked) != 2 || sending.asked[0] != "lab1/pacs" {
		t.Errorf("the part that sends was asked %v", sending.asked)
	}
}
