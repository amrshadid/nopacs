package engine

import (
	"context"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// The issuer's attributes (PS3.6): Issuer of Patient ID, its qualifiers
// sequence, and the Universal Entity ID and its Type in an item of it.
const (
	tIssuer          = 0x00100021
	tIssuerQualifier = 0x00100024
	tUniversalID     = 0x00400032
	tUniversalIDType = 0x00400033
)

// patient builds a data set of Patient ID PAT001 dated 20200610, with the
// issuer and the qualifiers' items given.
func patient(t *testing.T, issuer string, qualifiers ...*dataset.Dataset) *dataset.Dataset {
	els := []el{{tPatientID, "LO", "PAT001"}, {tStudyDate, "DA", "20200610"}}
	if issuer != "" {
		els = append(els, el{tIssuer, "LO", issuer})
	}
	if len(qualifiers) > 0 {
		els = append(els, el{tIssuerQualifier, "SQ", qualifiers})
	}
	return build(t, els...)
}

// shiftedBy applies en to ds under ns and returns the shifted Study Date.
func shiftedBy(t *testing.T, en *Engine, ds *dataset.Dataset, ns string) string {
	t.Helper()
	if _, err := en.Apply(context.Background(), ds, Reception{Namespace: ns}); err != nil {
		t.Fatal(err)
	}
	return mustText(t, ds, tStudyDate)
}

// TestDateShiftFollowsTheSubjectIdentity guards the date shift's key: two
// issuers of one Patient ID shift by two offsets; one issuer shifts alike
// under any namespace; a Patient ID with no issuer shifts apart under two
// namespaces; qualifiers alone are an issuer; and every shift is the one
// ShiftDays gives the identity's key (naming.SubjectIdentity.Key), the
// key the default subject label is the hash of.
func TestDateShiftFollowsTheSubjectIdentity(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	a := shiftedBy(t, en, patient(t, "HOSP_A"), "")
	b := shiftedBy(t, en, patient(t, "HOSP_B"), "")
	if a == b {
		t.Errorf("two issuers of one Patient ID shifted alike: %s", a)
	}
	if got := shiftedBy(t, en, patient(t, "HOSP_A"), "site-b"); got != a {
		t.Errorf("one issuer shifted to %s under a namespace and %s under none", got, a)
	}
	plainA, plainB := shiftedBy(t, en, patient(t, ""), "site-a"), shiftedBy(t, en, patient(t, ""), "site-b")
	if plainA == plainB {
		t.Errorf("no issuer under two namespaces shifted alike: %s", plainA)
	}
	q := build(t, el{tUniversalID, "UT", "2.16.840.1.113883.3.1"}, el{tUniversalIDType, "CS", "ISO"})
	byUID := shiftedBy(t, en, patient(t, "", q), "")
	var qual [len(naming.QualifierPaths)]string
	qual[0], qual[1] = "2.16.840.1.113883.3.1", "ISO"
	for _, c := range []struct {
		what string
		got  string
		id   naming.SubjectIdentity
	}{
		{"issuer HOSP_A", a, naming.SubjectIdentity{PatientID: "PAT001", Issuer: "HOSP_A"}},
		{"no issuer, site-b", plainB, naming.SubjectIdentity{PatientID: "PAT001", Namespace: "site-b"}},
		{"qualifiers only", byUID, naming.SubjectIdentity{PatientID: "PAT001", Qualifiers: qual}},
	} {
		want, err := shiftDA("20200610", ShiftDays(testKeys.Date, string(c.id.Key()), en.dec.DateShiftDays()))
		if err != nil {
			t.Fatal(err)
		}
		if c.got != want {
			t.Errorf("%s: shifted to %s, want %s", c.what, c.got, want)
		}
	}
}

// TestDateShiftWithoutIssuerIsUnchanged guards the archives made before
// issuers were told apart: a Patient ID with no issuer under the default
// namespace shifts by exactly the offset its Patient ID alone gave, 261
// days for PAT001 under testKeys' date key, worked out outside Go with
// Python's hmac and hashlib (1 + the first 64 bits of HMAC-SHA-256, big
// endian, mod 365).
func TestDateShiftWithoutIssuerIsUnchanged(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	if d := ShiftDays(testKeys.Date, "PAT001", 365); d != 261 {
		t.Fatalf("ShiftDays of PAT001 is %d, want 261", d)
	}
	want, err := shiftDA("20200610", 261)
	if err != nil {
		t.Fatal(err)
	}
	if got := shiftedBy(t, en, patient(t, ""), ""); got != want {
		t.Errorf("no issuer, default namespace: shifted to %s, want %s", got, want)
	}
}

// TestHashedPatientIDFollowsTheIssuer guards a profile's hash and lookup
// of the Patient ID: each is taken over the subject identity, so two
// issuers' patients of one Patient ID get two codes, a code equals the
// default subject label's, and a Patient ID with no issuer under the
// default namespace keeps the code of its value alone.
func TestHashedPatientIDFollowsTheIssuer(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("pid", kw("PatientID"), aHash))
	hashed := func(ds *dataset.Dataset, ns string) string {
		t.Helper()
		if _, err := en.Apply(context.Background(), ds, Reception{Namespace: ns}); err != nil {
			t.Fatal(err)
		}
		return mustText(t, ds, tPatientID)
	}
	a, b := hashed(patient(t, "HOSP_A"), ""), hashed(patient(t, "HOSP_B"), "")
	if a == b {
		t.Error("two issuers' Patient IDs hashed alike")
	}
	h, err := naming.NewHasher(testKeys.Hash)
	if err != nil {
		t.Fatal(err)
	}
	// The default rule's subject template, with constant session and
	// scan labels: the data set holds no UIDs.
	r, err := naming.Parse(naming.Spec{Subject: naming.DefaultSpec().Subject, Session: "ses", Scan: "scan"})
	if err != nil {
		t.Fatal(err)
	}
	label, err := r.Labels(datasetAttributes{patient(t, "HOSP_A")}, h)
	if err != nil {
		t.Fatal(err)
	}
	if "sub-"+a != label.Subject {
		t.Errorf("the hashed Patient ID %s is not the default subject label's code (%s)", a, label.Subject)
	}
	if got := hashed(patient(t, ""), ""); got != naming.HashText(testKeys.Hash, []byte("PAT001")) {
		t.Errorf("no issuer, default namespace: hashed to %s, want the hash of the Patient ID alone", got)
	}
	if hashed(patient(t, ""), "site-a") == hashed(patient(t, ""), "site-b") {
		t.Error("no issuer under two namespaces hashed alike")
	}

	tables := &mapLookups{codes: map[string]string{}}
	look := newTestEngine(t, testKeys, Options{Lookups: tables}, rule("pid", kw("PatientID"), aLookup("subjects", true)))
	for _, ds := range []*dataset.Dataset{patient(t, "HOSP_A"), patient(t, "HOSP_B"), patient(t, "HOSP_A")} {
		if _, err := look.Apply(context.Background(), ds, Reception{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(tables.codes) != 2 {
		t.Errorf("two issuers' Patient IDs took %d lookup codes, want 2", len(tables.codes))
	}
	if _, ok := tables.codes["subjects/"+a]; !ok {
		t.Errorf("issuer HOSP_A's code is not keyed by its subject identity's hash: %v", tables.codes)
	}
}
