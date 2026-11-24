package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
)

// codeWith is a forward lookup of a Patient ID into lab1 with an issuer
// and a namespace.
func codeWith(value string, issuer *v1.PatientIDIssuer, namespace string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.deid.Code, &v1.CodeRequest{Tenant: "lab1", Attribute: "PatientID", Value: value, Issuer: issuer, Namespace: namespace})
	}
}

// TestCodeTakesTheIssuer guards the forward lookup of an issuer's
// subject: the code is the tenant's keyed hash of the subject identity
// (the Patient ID with its issuer, or with a namespace), the same as the
// Patient ID's alone when there is neither; each receiver answers the
// label its default rule gives, so one issuer is one subject through a
// receiver of the default namespace and one of another, while a Patient
// ID with no issuer is two; an import with an issuer stores its code by
// that identity, found by a lookup with the issuer and not without; a
// receiver's namespace is stored, answered and changed by its mask path;
// an issuer or a namespace with another attribute, and a malformed
// namespace, are refused.
func TestCodeTakesTheIssuer(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	ns := dimseReceiver("scanner2", "main", 11120)
	ns.Namespace = "site-b"
	added := mustCall[*v1.AddReceiverResponse](t, "add scanner2", addReceiver("lab1", ns)(f.as(local)))
	if added.GetReceiver().GetNamespace() != "site-b" {
		t.Fatalf("scanner2 was added with namespace %q", added.GetReceiver().GetNamespace())
	}
	hasher, err := f.registry.NamingHasher(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	const pid = "PID-0001"
	plainCode := hasher.Hash([]byte(pid))
	issuerCode := hasher.Hash((naming.SubjectIdentity{PatientID: pid, Issuer: "HOSP_A"}).Key())
	nsCode := hasher.Hash((naming.SubjectIdentity{PatientID: pid, Namespace: "site-b"}).Key())
	labels := func(res *v1.CodeResponse) map[string]string {
		out := map[string]string{}
		for _, s := range res.GetSubjects() {
			out[s.GetReceiver()] = s.GetSubjectLabel()
		}
		return out
	}
	hospA := &v1.PatientIDIssuer{Issuer: "HOSP_A"}
	if o := importCodes("subjects", &v1.LookupEntry{Value: pid, Code: "A1", Issuer: hospA})(f.as(f.lab1Admin)); o.err != nil {
		t.Fatal(o.err)
	}
	f.run(t, []tcase{
		{name: "no issuer", as: f.lab1Admin, call: codeWith(pid, nil, ""), check: func(t *testing.T, o outcome) {
			res := o.res.(*v1.CodeResponse)
			l := labels(res)
			if res.GetCode() != plainCode || l["scanner1"] != "sub-"+plainCode || l["scanner2"] != "sub-"+nsCode {
				t.Errorf("no issuer: code %s, labels %v; want %s, scanner1 sub-%s, scanner2 sub-%s", res.GetCode(), l, plainCode, plainCode, nsCode)
			}
			if len(res.GetLookups()) != 0 {
				t.Errorf("the issuer's lookup code was found without the issuer: %v", res.GetLookups())
			}
		}},
		{name: "an issuer", as: f.lab1Admin, call: codeWith(pid, hospA, ""), check: func(t *testing.T, o outcome) {
			res := o.res.(*v1.CodeResponse)
			l := labels(res)
			if res.GetCode() != issuerCode || l["scanner1"] != "sub-"+issuerCode || l["scanner2"] != "sub-"+issuerCode {
				t.Errorf("issuer: code %s, labels %v; want %s through both receivers", res.GetCode(), l, issuerCode)
			}
			if lk := res.GetLookups(); len(lk) != 1 || lk[0].GetCode() != "A1" {
				t.Errorf("the issuer's lookup code: %v, want A1", lk)
			}
		}},
		{name: "a namespace", as: f.lab1Admin, call: codeWith(pid, nil, "site-b"), check: func(t *testing.T, o outcome) {
			if res := o.res.(*v1.CodeResponse); res.GetCode() != nsCode {
				t.Errorf("namespace: code %s, want %s", res.GetCode(), nsCode)
			}
		}},
		{name: "an issuer of another attribute", as: f.lab1Admin, code: connect.CodeInvalidArgument, msg: "issuer", call: func(c clients) outcome {
			return call(c.deid.Code, &v1.CodeRequest{Tenant: "lab1", Attribute: "AccessionNumber", Value: "A1", Issuer: hospA})
		}},
		{name: "a malformed namespace", as: f.lab1Admin, call: codeWith(pid, nil, "Site B"), code: connect.CodeInvalidArgument, msg: "namespace"},
		{name: "an import of a value holding a NUL", as: f.lab1Admin, code: connect.CodeInvalidArgument, msg: "NUL",
			call: importCodes("subjects", &v1.LookupEntry{Value: "\x00x", Code: "A2"})},
		{name: "a NUL in the Patient ID", as: f.lab1Admin, call: codeWith("\x00x", nil, ""), code: connect.CodeInvalidArgument, msg: "value"},
	})
	set := mustCall[*v1.SetReceiverResponse](t, "set scanner2's namespace", call(f.as(local).receivers.SetReceiver, &v1.SetReceiverRequest{
		Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner2"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"namespace"}}}))
	if set.GetReceiver().GetNamespace() != "" {
		t.Errorf("scanner2's namespace after returning to the default: %q", set.GetReceiver().GetNamespace())
	}
	if o := addReceiver("lab1", func() *v1.Receiver { r := dimseReceiver("scanner3", "main", 11121); r.Namespace = "Bad NS"; return r }())(f.as(local)); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a receiver with a malformed namespace: %v", o.err)
	}
}
