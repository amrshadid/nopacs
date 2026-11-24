package api_test

import (
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// receiverWith returns the receiver of dimseReceiver changed by edit.
func receiverWith(edit func(r *v1.Receiver)) *v1.Receiver {
	r := dimseReceiver("scanner1", "main", 11112)
	edit(r)
	return r
}

// testAddReceiver guards AddReceiver: admin on the tenant only; the port
// among the tenant's allotted ports and the bind address among the
// installation's, as preconditions; each rule of a receiver's fields; the
// defaults filled in and answered; the health of a receiver nothing runs;
// a name taken and a port that clashes; and the entry with its type, port,
// AE title and storage.
func testAddReceiver(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	add := func(r *v1.Receiver) func(clients) outcome { return addReceiver("lab1", r) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: add(dimseReceiver("scanner1", "main", 11112)), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: add(dimseReceiver("scanner1", "main", 11112)), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: add(dimseReceiver("scanner1", "main", 11112)), code: connect.CodePermissionDenied},
		{name: "admin on the installation", as: f.instAdmin, call: add(dimseReceiver("scanner1", "main", 11112)), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: addReceiver("nolab", dimseReceiver("scanner1", "main", 11112)), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "no receiver", as: f.lab1Admin, call: add(nil), code: connect.CodeInvalidArgument, msg: "receiver: is required"},
		{name: "no type", as: f.lab1Admin, call: add(&v1.Receiver{Name: "scanner1", Storage: "main"}), code: connect.CodeInvalidArgument, msg: "has a type"},
		{name: "no storage named", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.Storage = "" })), code: connect.CodeInvalidArgument, msg: "receiver.storage"},
		{name: "any_caller false", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().Callers = &v1.DimseReceiver_AnyCaller{AnyCaller: false} })), code: connect.CodeInvalidArgument, msg: "any_caller"},
		{name: "a rule with neither", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) {
			r.GetDimse().Callers = &v1.DimseReceiver_AllowedCallers{AllowedCallers: &v1.AllowedCallers{Rules: []*v1.CallerRule{{}}}}
		})), code: connect.CodeInvalidArgument, msg: "caller rule"},
		{name: "a range with host bits", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) {
			r.GetDimse().Callers = &v1.DimseReceiver_AllowedCallers{AllowedCallers: &v1.AllowedCallers{Rules: []*v1.CallerRule{{AddressRange: "10.20.0.1/24"}}}}
		})), code: connect.CodeInvalidArgument, msg: "CIDR"},
		{name: "a TLS file out of the tenant's directory", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) {
			r.GetDimse().Tls = &v1.ReceiverTls{CertificateFile: "../lab2/cert.pem", PrivateKeyFile: "key.pem"}
		})), code: connect.CodeInvalidArgument, msg: "TLS"},
		{name: "a SOP class that is not a UID", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().SopClasses = []string{"CT"} })), code: connect.CodeInvalidArgument, msg: "UID"},
		{name: "an AE title too long", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().AeTitle = strings.Repeat("A", 17) })), code: connect.CodeInvalidArgument, msg: "AE title"},
		{name: "a naming rule with an unknown keyword", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) {
			r.Naming = &v1.NamingRule{Subject: "sub-{NoSuchKeyword}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
		})), code: connect.CodeInvalidArgument, msg: "naming rule"},
		{name: "a quiet period too short", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.QuietPeriod = seconds(4) })), code: connect.CodeInvalidArgument, msg: "quiet period"},
		{name: "a quiet period in fractions", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.QuietPeriod = seconds(10); r.QuietPeriod.Nanos = 5 })), code: connect.CodeInvalidArgument, msg: "quiet_period"},
		{name: "a file bound too small", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.MaxFileBytes = 1023 })), code: connect.CodeInvalidArgument, msg: "largest file"},
		{name: "a port not allotted", as: f.lab1Admin, call: add(dimseReceiver("scanner1", "main", 104)), code: connect.CodeFailedPrecondition, msg: "receiver.ports",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "scanner1", details: map[string]string{"port": "104", "ae_title": "NOPACS", "storage": "main", "type": "dimse"}}},
		{name: "a bind address while none is listed", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().BindAddress = "192.0.2.10" })), code: connect.CodeFailedPrecondition, msg: "lists no bind address"},
		{name: "a storage the tenant does not have", as: f.lab1Admin, call: add(dimseReceiver("scanner1", "none", 11112)), code: connect.CodeFailedPrecondition},
		{name: "added", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().AeTitle = "  NOPACS " })),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "scanner1", details: map[string]string{"type": "dimse", "port": "11112", "ae_title": "NOPACS", "storage": "main"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.AddReceiverResponse).GetReceiver()
				if r.GetQuietPeriod().GetSeconds() != 60 || r.GetMaxOpenTime().GetSeconds() != 3600 || r.GetMaxFileBytes() != 1<<32 {
					t.Errorf("the defaults: %v", r)
				}
				if r.GetDimse().GetAeTitle() != "NOPACS" || r.GetNaming() != nil || len(r.GetDimse().GetSopClasses()) != 0 {
					t.Errorf("%v", r)
				}
				h := r.GetHealth()
				if h.GetState() != v1.ReceiverState_RECEIVER_STATE_DOWN || h.GetDownReason() != v1.ReceiverDownReason_RECEIVER_DOWN_REASON_OTHER || h.GetDetail() != "not started" || h.GetSince() == nil {
					t.Errorf("the health of an enabled receiver nothing runs: %v", h)
				}
			}},
		{name: "not enabled", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.Name, r.Enabled, r.GetDimse().Port = "spare", false, 11120 })),
			check: func(t *testing.T, o outcome) {
				h := o.res.(*v1.AddReceiverResponse).GetReceiver().GetHealth()
				if h.GetState() != v1.ReceiverState_RECEIVER_STATE_DOWN || h.GetDownReason() != v1.ReceiverDownReason_RECEIVER_DOWN_REASON_DISABLED || h.GetDetail() != "" {
					t.Errorf("the health of a disabled receiver: %v", h)
				}
			}},
		{name: "a name taken", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.GetDimse().Port = 11121 })), code: connect.CodeAlreadyExists},
		{name: "another receiver on the same port and address", as: f.lab1Admin, call: add(dimseReceiver("scanner2", "main", 11112)), code: connect.CodeFailedPrecondition},
		{name: "any caller, in so many words", as: local, call: add(receiverWith(func(r *v1.Receiver) {
			r.Name, r.GetDimse().Port, r.GetDimse().Callers = "open", 11122, &v1.DimseReceiver_AnyCaller{AnyCaller: true}
		})), check: func(t *testing.T, o outcome) {
			if !o.res.(*v1.AddReceiverResponse).GetReceiver().GetDimse().GetAnyCaller() {
				t.Error(o.res)
			}
		}},
		// No port: the server gives the lowest allotted port that no
		// receiver holds and that it can bind (11112, 11120 and 11122 are
		// held above), and the entry records the port given.
		{name: "no port, the server chooses", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.Name, r.GetDimse().Port = "auto", 0 })),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "auto", details: map[string]string{"port": "*"}},
			check: func(t *testing.T, o outcome) {
				port := o.res.(*v1.AddReceiverResponse).GetReceiver().GetDimse().GetPort()
				if port < 11121 || port > 11129 || port == 11122 {
					t.Errorf("the port given: %d, want the lowest free of 11121 and 11123 to 11129", port)
				}
			}},
		// A host name is refused before any port is probed: the probe
		// binds only an address valid for a receiver, never a name the
		// server would resolve.
		{name: "no port, a bind address that is a name", as: f.lab1Admin,
			call: add(receiverWith(func(r *v1.Receiver) { r.Name, r.GetDimse().Port, r.GetDimse().BindAddress = "named", 0, "localhost" })),
			code: connect.CodeInvalidArgument, msg: "bind address",
			check: func(t *testing.T, _ outcome) {
				if slices.Contains(f.probed(), "localhost") {
					t.Error("a port was probed on a host name before the request was checked")
				}
			}},
	})
	// The installation's bind addresses, once listed, are the ones a
	// receiver may name.
	f.setSetting(installation(), settings.ReceiverBindAddresses, "192.0.2.10,2001:db8::1")
	f.run(t, []tcase{
		{name: "a bind address not listed", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.Name, r.GetDimse().Port, r.GetDimse().BindAddress = "b1", 11123, "192.0.2.11" })), code: connect.CodeFailedPrecondition, msg: "receiver.bind-addresses"},
		{name: "a bind address listed", as: f.lab1Admin, call: add(receiverWith(func(r *v1.Receiver) { r.Name, r.GetDimse().Port, r.GetDimse().BindAddress = "b2", 11123, "2001:db8::1" }))},
	})
}

// testGetReceiver guards GetReceiver: reader on the tenant and up, a
// receiver not there, and the same answer read twice.
func testGetReceiver(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	get := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: get("scanner1"), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: get("scanner1"), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "scanner1"}},
		{name: "not there", as: f.lab1Reader, call: get("none"), code: connect.CodeNotFound, msg: "no receiver with this name"},
		{name: "a bad name", as: f.lab1Reader, call: get("Scanner"), code: connect.CodeInvalidArgument, msg: "name"},
	})
	a, b := get("scanner1")(f.as(f.lab1Reader)), get("scanner1")(f.as(f.lab1Reader))
	if !proto.Equal(a.res, b.res) {
		t.Error("two reads answer differently")
	}
}

// testListReceivers guards ListReceivers: reader and up, paging, and a
// token refused with another tenant.
func testListReceivers(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", receiverWith(func(r *v1.Receiver) { r.Name, r.GetDimse().Port = "scanner2", 11120 }))(f.as(local)))
	list := func(tn string, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receivers.ListReceivers, &v1.ListReceiversRequest{Tenant: tn, PageSize: size, PageToken: token})
		}
	}
	var token string
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: list("lab1", 0, ""), code: connect.CodePermissionDenied},
		{name: "a page of one", as: f.lab1Reader, call: list("lab1", 1, ""), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.ListReceiversResponse)
				token = res.GetNextPageToken()
				if len(res.GetReceivers()) != 1 || res.GetReceivers()[0].GetName() != "scanner1" || token == "" || res.GetReceivers()[0].GetHealth() == nil {
					t.Errorf("%v", res)
				}
			}},
		{name: "the next", as: f.lab1Reader, call: func(c clients) outcome { return list("lab1", 1, token)(c) }, check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.ListReceiversResponse).GetReceivers(); len(r) != 1 || r[0].GetName() != "scanner2" {
				t.Errorf("%v", r)
			}
		}},
		{name: "the token with another tenant", as: local, call: func(c clients) outcome { return list("lab2", 1, token)(c) }, code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// testSetReceiver guards SetReceiver: admin only; the update mask in every
// form the contract refuses; a duration named with no value; a port moved
// outside the allotted ones; a change of the quiet period alone, repeated
// with the same answer; and the entry with the mask's paths.
func testSetReceiver(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	set := func(r *v1.Receiver, m ...string) func(clients) outcome {
		return func(c clients) outcome {
			req := &v1.SetReceiverRequest{Tenant: "lab1", Receiver: r}
			if m != nil {
				req.UpdateMask = mask(m...)
			}
			return call(c.receivers.SetReceiver, req)
		}
	}
	quiet := &v1.Receiver{Name: "scanner1", QuietPeriod: seconds(300)}
	var first *v1.Receiver
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: set(quiet, "quiet_period"), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: set(quiet, "quiet_period"), code: connect.CodePermissionDenied},
		{name: "no mask", as: f.lab1Admin, call: set(quiet), code: connect.CodeInvalidArgument, msg: "update_mask"},
		{name: "*", as: f.lab1Admin, call: set(quiet, "*"), code: connect.CodeInvalidArgument, msg: `may not be "*"`},
		{name: "a whole type", as: f.lab1Admin, call: set(quiet, "dimse"), code: connect.CodeInvalidArgument, msg: "whole message"},
		{name: "the health", as: f.lab1Admin, call: set(quiet, "health"), code: connect.CodeInvalidArgument, msg: "answered by the server"},
		{name: "the name", as: f.lab1Admin, call: set(quiet, "name"), code: connect.CodeInvalidArgument, msg: "does not change"},
		{name: "an unknown path", as: f.lab1Admin, call: set(quiet, "dimse.color"), code: connect.CodeInvalidArgument, msg: "names no field"},
		{name: "a duration named with no value", as: f.lab1Admin, call: set(&v1.Receiver{Name: "scanner1"}, "quiet_period"), code: connect.CodeInvalidArgument, msg: "not reset"},
		{name: "a size named with no value", as: f.lab1Admin, call: set(&v1.Receiver{Name: "scanner1"}, "max_file_bytes"), code: connect.CodeInvalidArgument, msg: "not reset"},
		{name: "any_caller named, not given", as: f.lab1Admin, call: set(&v1.Receiver{Name: "scanner1"}, "dimse.any_caller"), code: connect.CodeInvalidArgument},
		{name: "not there", as: f.lab1Admin, call: set(&v1.Receiver{Name: "none", QuietPeriod: seconds(300)}, "quiet_period"), code: connect.CodeNotFound},
		{name: "a port not allotted", as: f.lab1Admin, call: set(receiverWith(func(r *v1.Receiver) { r.GetDimse().Port = 104 }), "dimse.port"), code: connect.CodeFailedPrecondition, msg: "receiver.ports"},
		{name: "the quiet period", as: f.lab1Admin, call: set(quiet, "quiet_period"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "scanner1", details: map[string]string{"update_mask": "quiet_period"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.SetReceiverResponse).GetReceiver()
				if first.GetQuietPeriod().GetSeconds() != 300 || first.GetDimse().GetPort() != 11112 || first.GetDimse().GetAllowedCallers() == nil {
					t.Errorf("%v", first)
				}
			}},
		{name: "the quiet period again", as: f.lab1Admin, call: set(quiet, "quiet_period"), check: func(t *testing.T, o outcome) {
			again := proto.Clone(o.res.(*v1.SetReceiverResponse).GetReceiver()).(*v1.Receiver)
			again.UpdateTime, again.Health = first.GetUpdateTime(), first.GetHealth()
			if !proto.Equal(again, first) {
				t.Errorf("a repeated change answers %v, the first %v", again, first)
			}
		}},
		{name: "every caller admitted", as: f.lab1Admin, call: set(receiverWith(func(r *v1.Receiver) { r.GetDimse().Callers = &v1.DimseReceiver_AnyCaller{AnyCaller: true} }), "dimse.any_caller"),
			check: func(t *testing.T, o outcome) {
				if !o.res.(*v1.SetReceiverResponse).GetReceiver().GetDimse().GetAnyCaller() {
					t.Error(o.res)
				}
			}},
		{name: "the callers emptied", as: f.lab1Admin, call: set(&v1.Receiver{Name: "scanner1"}, "dimse.allowed_callers"),
			check: func(t *testing.T, o outcome) {
				d := o.res.(*v1.SetReceiverResponse).GetReceiver().GetDimse()
				if d.GetAnyCaller() || len(d.GetAllowedCallers().GetRules()) != 0 {
					t.Errorf("%v", d)
				}
			}},
	})
}

// testRemoveReceiver guards RemoveReceiver: admin only, the entry with the
// receiver's type, port and AE title, the receiver answered as it was and
// stopped, and a repeat answered NOT_FOUND.
func testRemoveReceiver(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	remove := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receivers.RemoveReceiver, &v1.RemoveReceiverRequest{Tenant: "lab1", Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: remove("scanner1"), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: remove("scanner1"), code: connect.CodePermissionDenied},
		{name: "removed", as: f.lab1Admin, call: remove("scanner1"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "scanner1", details: map[string]string{"type": "dimse", "port": "11112", "ae_title": "NOPACS"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.RemoveReceiverResponse).GetReceiver()
				if r.GetName() != "scanner1" || !r.GetEnabled() || r.GetHealth().GetState() != v1.ReceiverState_RECEIVER_STATE_DOWN {
					t.Errorf("%v", r)
				}
			}},
		{name: "again", as: f.lab1Admin, call: remove("scanner1"), code: connect.CodeNotFound, msg: "no receiver with this name"},
	})
}
