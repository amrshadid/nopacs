package naming

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
)

// importGiven is what an import whose tree holds no subject folder gives
// with a map: three parts from the path and the subject from the map.
var importGiven = []string{"project", "session", "scan", "subject"}

// TestGivenPartsMakeLabels guards that a rule's templates may use parts
// given from a file's path, each placed as an attribute's value would be,
// and hashed when written after '#'.
func TestGivenPartsMakeLabels(t *testing.T) {
	r, err := ParseWithParts(Spec{Subject: "{project}_{subject}", Session: "{project}_{session}", Scan: "{scan}-{#scan}"}, importGiven)
	if err != nil {
		t.Fatal(err)
	}
	given := map[string]string{"project": "P1", "session": "MR1", "scan": "4", "subject": "S01"}
	h := testHasher(t)
	got, err := r.LabelsWithParts(studyAttrs(t), given, h)
	if err != nil {
		t.Fatal(err)
	}
	want := label.Path{Subject: "P1_S01", Session: "P1_MR1", Scan: "4-" + h.Hash([]byte("4"))}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if g := r.GivenParts(); !slices.Equal(g, []string{"project", "subject", "session", "scan"}) {
		t.Errorf("GivenParts = %v", g)
	}
	if p := r.Plain(); !slices.Equal(p, []string{"project of " + GivenSource, "subject of " + GivenSource, "session of " + GivenSource, "scan of " + GivenSource}) {
		t.Errorf("Plain = %v", p)
	}
}

// TestGivenPartNotGivenIsRefused guards that a template part neither the
// rule's pattern nor the given parts hold is refused when the rule is
// parsed, so a rule using a part its import layout never gives is refused
// when it is set, and the refusal names the parts that are given.
func TestGivenPartNotGivenIsRefused(t *testing.T) {
	_, err := ParseWithParts(Spec{Subject: "{subject}", Session: "{session}", Scan: "{visit}"}, []string{"subject", "session"})
	var se *SyntaxError
	if !errors.As(err, &se) || se.Field != "scan" || !strings.Contains(se.Msg, "gives subject, session") {
		t.Fatalf("got %v", err)
	}
	// Without given parts the plain Parse still refuses a part.
	if _, err := Parse(Spec{Subject: "{subject}", Session: "s", Scan: "c"}); err == nil {
		t.Fatal("Parse took a part with no pattern and nothing given")
	}
}

// TestGivenPartClashIsRefused guards that a part name both the rule's
// header pattern and the import's layout hold is refused: one placeholder
// with two sources would make labels depend on which one won.
func TestGivenPartClashIsRefused(t *testing.T) {
	s := Spec{Subject: "{subject}", Session: "{session}", Scan: "c", PatternAttribute: "PatientComments", Pattern: `(?P<subject>.+)`}
	_, err := ParseWithParts(s, []string{"session", "subject"})
	var se *SyntaxError
	if !errors.As(err, &se) || se.Field != "pattern" || !strings.Contains(se.Msg, "two sources") {
		t.Fatalf("got %v", err)
	}
	for _, bad := range [][]string{{"Subject"}, {"a", "a"}, {""}} {
		if _, err := ParseWithParts(Spec{Subject: "s", Session: "s", Scan: "s"}, bad); err == nil {
			t.Errorf("given %q was taken", bad)
		}
	}
}

// TestGivenPartsWithHeaderPattern guards that a rule may use a header
// pattern's parts and given parts together, each read from its own source.
func TestGivenPartsWithHeaderPattern(t *testing.T) {
	s := Spec{Subject: "{proj}_{subject}", Session: "{session}", Scan: "x", PatternAttribute: "PatientComments", Pattern: `(?P<proj>[^^]+)\^.*`}
	r, err := ParseWithParts(s, []string{"subject", "session"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.LabelsWithParts(studyAttrs(t), map[string]string{"subject": "S9", "session": "V1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (label.Path{Subject: "Proj_S9", Session: "V1", Scan: "x"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestGivenPartFaults guards that a given part missing from the map, or
// whose value breaks the label rule, is an *Error naming the part and its
// source and never the value, so the file is quarantined, not cleaned.
func TestGivenPartFaults(t *testing.T) {
	r, err := ParseWithParts(Spec{Subject: "{subject}", Session: "{session}", Scan: "c"}, []string{"subject", "session"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		given map[string]string
		part  string
		fault Fault
	}{
		{map[string]string{"session": "V1"}, "subject", Missing},
		{map[string]string{"subject": "Doe Jane", "session": "V1"}, "subject", BadCharacter},
		{map[string]string{"subject": "S1", "session": ""}, "session", Empty},
	} {
		_, err := r.LabelsWithParts(studyAttrs(t), tc.given, nil)
		var e *Error
		if !errors.As(err, &e) || e.Part != tc.part || e.Fault != tc.fault || e.Attribute != GivenSource {
			t.Errorf("%v: got %v", tc.given, err)
			continue
		}
		if strings.Contains(err.Error(), "Doe") {
			t.Errorf("error text holds the value: %v", err)
		}
	}
	// Labels, with no given parts, cannot make a label that needs one.
	if _, err := r.Labels(studyAttrs(t), nil); err == nil {
		t.Error("Labels made a label from a part it was not given")
	}
}
