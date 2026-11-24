package naming

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
)

// itemAttrs is attrs that also answers values inside sequences, by the
// path's tags written with %v, and a top-level value by its tag; a path in
// unreadable is there and cannot be read.
type itemAttrs struct {
	attrs
	items      map[string]string
	unreadable map[string]bool
}

func (a itemAttrs) Item(path []Tag) (string, bool, error) {
	if a.unreadable[fmt.Sprint(path)] {
		return "", true, errors.New("not in memory")
	}
	if len(path) == 1 {
		v, ok := a.attrs[path[0]]
		return v, ok, nil
	}
	v, ok := a.items[fmt.Sprint(path)]
	return v, ok, nil
}

// withIssuer returns studyAttrs with an Issuer of Patient ID, and the
// qualifiers given by their index in QualifierPaths.
func withIssuer(t *testing.T, issuer string, qualifiers map[int]string) itemAttrs {
	a := itemAttrs{attrs: studyAttrs(t), items: map[string]string{}}
	if issuer != "" {
		a.attrs[TagIssuerOfPatientID] = issuer
	}
	if len(qualifiers) > 0 {
		a.attrs[TagIssuerQualifiers] = ""
	}
	for i, v := range qualifiers {
		a.items[fmt.Sprint(QualifierPaths[i])] = v
	}
	return a
}

// The labels the default rule gives studyAttrs (Patient ID SUB-001) under
// testKey, each computed outside Go with Python's hmac, hashlib and base64
// modules over the bytes of TestIdentityEncodingIsPinned.
const (
	issuerSubject    = "sub-sucutqj5dvm7kmcp" // issuer HOSP_A
	namespaceSubject = "sub-s5sy4nqh3binjo5b" // no issuer, namespace site-b
)

// TestIdentityEncodingIsPinned guards the stored form of a subject
// identity, which every label, date shift and lookup code of an issuer's
// subject is keyed by and so may never change within a version: a Patient
// ID alone under the default namespace is its bytes alone; any other is
// the mark, the version, then each field length-prefixed. The bytes and
// the labels were worked out outside Go.
func TestIdentityEncodingIsPinned(t *testing.T) {
	for _, c := range []struct {
		id   SubjectIdentity
		want string
	}{
		{SubjectIdentity{PatientID: "SUB-001"}, hex.EncodeToString([]byte("SUB-001"))},
		{SubjectIdentity{PatientID: "SUB-001", Issuer: "HOSP_A"}, "0001075355422d30303106484f53505f41000000000000000000000000000000"},
		{SubjectIdentity{PatientID: "SUB-001", Issuer: "HOSP_A", Namespace: "site-b"}, "0001075355422d30303106484f53505f41000000000000000000000000000000"},
		{SubjectIdentity{PatientID: "SUB-001", Namespace: "site-b"}, "0001075355422d30303100000000000000000000000000000006736974652d62"},
	} {
		if got := hex.EncodeToString(c.id.Key()); got != c.want {
			t.Errorf("%+v: key %s, want %s", c.id, got, c.want)
		}
	}
	if k := (SubjectIdentity{PatientID: "x", Issuer: "y"}).Key(); k[0] != identityMark || k[1] != IdentityVersion {
		t.Errorf("an issuer's identity starts %x, want the mark and version %d", k[:2], IdentityVersion)
	}
}

// TestIdentityKeysNeverCollide guards that two different identities never
// share a key, so two patients are never one subject by an accident of
// their bytes: a value moved from one field to the next, a qualifier in
// place of the issuer, a namespace in place of an issuer, and a Patient ID
// that spells out another identity's encoding.
func TestIdentityKeysNeverCollide(t *testing.T) {
	var q0, q1 [len(QualifierPaths)]string
	q0[0], q1[1] = "A", "A"
	ids := []SubjectIdentity{
		{PatientID: "12"},
		{PatientID: "12", Issuer: "3"},
		{PatientID: "123"},
		{PatientID: "1", Issuer: "23"},
		{PatientID: "12", Issuer: "A"},
		{PatientID: "12", Qualifiers: q0},
		{PatientID: "12", Qualifiers: q1},
		{PatientID: "12", Namespace: "a"},
		{PatientID: "12", Namespace: "b"},
		{PatientID: string((SubjectIdentity{PatientID: "12", Issuer: "A"}).Key()[1:])},
	}
	seen := map[string]int{}
	for i, id := range ids {
		k := string(id.Key())
		if j, dup := seen[k]; dup {
			t.Errorf("identities %d (%+v) and %d (%+v) share a key", j, ids[j], i, id)
		}
		seen[k] = i
	}
}

// TestDefaultRuleTellsIssuersApart guards the default subject label: two
// issuers of one Patient ID are two subjects; one issuer is one subject
// under any namespace (through any receiver); a file with no issuer is one
// subject per namespace; the qualifiers alone make an issuer, while a
// Code Meaning-like value outside QualifierPaths is never read; and a file
// with no issuer under the default namespace keeps the label its Patient
// ID alone gave (defaultWant).
func TestDefaultRuleTellsIssuersApart(t *testing.T) {
	h := testHasher(t)
	label := func(a Attributes, ns string) string {
		t.Helper()
		p, err := Default().WithNamespace(ns).Labels(a, h)
		if err != nil {
			t.Fatal(err)
		}
		if p.Session != defaultWant.Session || p.Scan != defaultWant.Scan {
			t.Errorf("the session or scan label moved with the issuer: %+v", p)
		}
		return p.Subject
	}
	plain := withIssuer(t, "", nil)
	a, b := withIssuer(t, "HOSP_A", nil), withIssuer(t, "HOSP_B", nil)
	if got := label(plain, ""); got != defaultWant.Subject {
		t.Errorf("no issuer, default namespace: %s, want today's %s", got, defaultWant.Subject)
	}
	if got := label(a, ""); got != issuerSubject {
		t.Errorf("issuer HOSP_A: %s, want %s", got, issuerSubject)
	}
	if got := label(plain, "site-b"); got != namespaceSubject {
		t.Errorf("no issuer, namespace site-b: %s, want %s", got, namespaceSubject)
	}
	if label(a, "") == label(b, "") {
		t.Error("two issuers of one Patient ID made one subject")
	}
	if label(a, "") != label(a, "site-b") {
		t.Error("one issuer through two namespaces made two subjects")
	}
	if label(plain, "site-a") == label(plain, "site-b") {
		t.Error("a Patient ID with no issuer under two namespaces made one subject")
	}
	byUID := withIssuer(t, "", map[int]string{0: "2.16.840.1.113883.3.1", 1: "ISO"})
	if got := label(byUID, ""); got == defaultWant.Subject || got != label(byUID, "site-b") {
		t.Errorf("qualifiers without an Issuer of Patient ID were not taken as an issuer: %s", got)
	}
}

// TestIdentityPlaceholderIsOnlyHashed guards that the identity never
// enters a label as it is: {SubjectIdentity} is refused when a rule is
// parsed, and {#SubjectIdentity} is not a plain placeholder.
func TestIdentityPlaceholderIsOnlyHashed(t *testing.T) {
	_, err := Parse(Spec{Subject: "sub-{SubjectIdentity}", Session: "s-{#StudyInstanceUID}", Scan: "c-{#SeriesInstanceUID}"})
	var se *SyntaxError
	if !errors.As(err, &se) || se.Field != "subject" {
		t.Errorf("{SubjectIdentity} unhashed: %v, want a syntax error of the subject template", err)
	}
	r, err := Parse(Spec{Subject: "p-{#SubjectIdentity}", Session: "s-{#StudyInstanceUID}", Scan: "c-{#SeriesInstanceUID}"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Plain()) != 0 || !r.Hashes() {
		t.Errorf("{#SubjectIdentity}: plain %v, hashes %v", r.Plain(), r.Hashes())
	}
}

// TestSiteRuleKeepsItsAttributes guards that a rule a site writes with
// {#PatientID} keeps hashing the Patient ID alone, whatever the issuer.
func TestSiteRuleKeepsItsAttributes(t *testing.T) {
	r, err := Parse(Spec{Subject: "sub-{#PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.WithNamespace("site-b").Labels(withIssuer(t, "HOSP_A", nil), testHasher(t))
	if err != nil || p.Subject != defaultWant.Subject {
		t.Errorf("a site's {#PatientID} with an issuer: %s, %v; want %s", p.Subject, err, defaultWant.Subject)
	}
}

// TestReadSubjectIdentityRefuses guards what has no identity: no Patient
// ID, an empty one, one holding a NUL (which could pose as an encoding),
// and a qualifiers sequence the caller cannot read into, which is refused
// rather than read as no issuer; each fault names the attribute only.
func TestReadSubjectIdentityRefuses(t *testing.T) {
	pid := TagPatientID
	for _, c := range []struct {
		what  string
		a     Attributes
		attr  string
		fault Fault
	}{
		{"no Patient ID", attrs{}, "PatientID", Missing},
		{"an empty Patient ID", attrs{pid: ""}, "PatientID", Empty},
		{"a NUL in the Patient ID", attrs{pid: "\x00\x01x"}, "PatientID", BadCharacter},
		{"qualifiers unread", attrs{pid: "x", TagIssuerQualifiers: ""}, "(0010,0024)", Missing},
		{"an issuer unread", attrs{pid: "x", TagIssuerOfPatientID: "H"}, "(0010,0021)", Missing},
		{"an issuer left in the file", itemAttrs{attrs: attrs{pid: "x", TagIssuerOfPatientID: ""},
			unreadable: map[string]bool{fmt.Sprint([]Tag{TagIssuerOfPatientID}): true}}, "IssuerOfPatientID", TooLong},
		{"a qualifier left in the file", itemAttrs{attrs: attrs{pid: "x"},
			unreadable: map[string]bool{fmt.Sprint(QualifierPaths[0]): true}}, "(0010,0024)", TooLong},
		{"a Patient ID left in the file", itemAttrs{attrs: attrs{pid: ""},
			unreadable: map[string]bool{fmt.Sprint([]Tag{pid}): true}}, "PatientID", TooLong},
	} {
		_, err := ReadSubjectIdentity(c.a, "")
		var ne *Error
		if !errors.As(err, &ne) || ne.Attribute != c.attr || ne.Fault != c.fault {
			t.Errorf("%s: %v, want %s %v", c.what, err, c.attr, c.fault)
			continue
		}
		if bytes.Contains([]byte(err.Error()), []byte("\x01x")) {
			t.Errorf("%s: the error holds the value", c.what)
		}
	}
	if _, err := Default().Labels(attrs{pid: "\x00\x01x"}, testHasher(t)); err == nil {
		t.Error("the default rule named a Patient ID holding a NUL")
	}
	site, err := Parse(Spec{Subject: "sub-{#PatientID}", Session: "s", Scan: "c"})
	if err != nil {
		t.Fatal(err)
	}
	var ne *Error
	if _, err := site.Labels(attrs{pid: "\x00\x01x"}, testHasher(t)); !errors.As(err, &ne) || ne.Fault != BadCharacter {
		t.Errorf("a site's {#PatientID} hashed a Patient ID holding a NUL: %v", err)
	}
	if _, _, err := site.HashedSubject(pid, "\x00\x01x", SubjectIdentity{}, testHasher(t)); !errors.As(err, &ne) || ne.Fault != BadCharacter {
		t.Errorf("the forward lookup of a site's {#PatientID} hashed a Patient ID holding a NUL: %v", err)
	}
}

// TestHashedSubjectTakesTheIssuer guards the forward lookup of the
// default rule: HashedSubject with an issuer gives the label Labels gives
// a file of that Patient ID from that issuer, and with none the label of
// a file with no issuer through a receiver of the rule's namespace.
func TestHashedSubjectTakesTheIssuer(t *testing.T) {
	h := testHasher(t)
	for _, c := range []struct {
		issuer string
		ns     string
	}{{"HOSP_A", ""}, {"HOSP_A", "site-b"}, {"", ""}, {"", "site-b"}} {
		r := Default().WithNamespace(c.ns)
		want, err := r.Labels(withIssuer(t, c.issuer, nil), h)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := r.HashedSubject(TagPatientID, "SUB-001", SubjectIdentity{Issuer: c.issuer}, h)
		if err != nil || !ok || got != want.Subject {
			t.Errorf("issuer %q namespace %q: %s, %v, %v; want %s", c.issuer, c.ns, got, ok, err, want.Subject)
		}
	}
	if _, ok, _ := Default().HashedSubject(kw(t, "AccessionNumber"), "A1", SubjectIdentity{}, h); ok {
		t.Error("the default rule answered a subject label for an attribute that is not the Patient ID")
	}
}

// TestSubjectWithPartsIsLabelsSubject guards that the subject label alone
// is the one Labels makes, for the default rule with and without an issuer
// and under a namespace, so a file checked against its session is checked
// against the label it would have been given.
func TestSubjectWithPartsIsLabelsSubject(t *testing.T) {
	h := testHasher(t)
	for _, a := range []itemAttrs{withIssuer(t, "", nil), withIssuer(t, "HOSP_A", nil)} {
		for _, ns := range []string{"", "site-b"} {
			r := Default().WithNamespace(ns)
			want, err := r.Labels(a, h)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.SubjectWithParts(a, nil, h)
			if err != nil || got != want.Subject {
				t.Errorf("namespace %q: SubjectWithParts %q, %v; Labels %q", ns, got, err, want.Subject)
			}
		}
	}
}
