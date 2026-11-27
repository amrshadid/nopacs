package profile_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestExpandedFormStoresWhatValidationDecided guards that each expanded
// rule holds the VRs validation decided for its selector, that every
// default is written out, and that the language edition is stated, so a
// stored version never depends on a later build's dictionary or defaults.
func TestExpandedFormStoresWhatValidationDecided(t *testing.T) {
	e := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("tag", kw("PatientName"), remove()),
		rl("two", kw("SmallestImagePixelValue"), remove()),
		rl("vr", vr("DA"), shift()),
		rl("group", grp("0010"), remove()),
		rl("private", priv("ACME", "0019", "xx"), remove()),
		rl("date", itemOf("DCM", "111536", "DATE"), shift()),
		rl("tcoord", itemOf("DCM", "111536", "TCOORD"), remove()),
		rl("item", item("DCM", "111536"), remove()),
		rl("lookup", kw("PatientID"), lookup("subjects", v1.LookupMissing_LOOKUP_MISSING_UNSPECIFIED)),
	}}, testCatalog(t))
	want := map[string][]string{
		"tag": {"PN"}, "two": {"US", "SS"}, "vr": {"DA"}, "group": nil, "private": nil,
		"date": {"DA"}, "tcoord": {"DS", "DT", "UL"}, "item": nil, "lookup": {"LO"},
	}
	for _, er := range e.GetRules() {
		if got := er.GetVrs(); !slices.Equal(got, want[er.GetRule().GetName()]) {
			t.Errorf("%s: VRs %v, want %v", er.GetRule().GetName(), got, want[er.GetRule().GetName()])
		}
	}
	if got := e.GetRules()[8].GetRule().GetAction().GetLookup().GetOnMissing(); got != v1.LookupMissing_LOOKUP_MISSING_QUARANTINE {
		t.Errorf("a lookup's default is %v in the expanded form", got)
	}
	// A document that uses nothing of a later edition is written as
	// edition 1, as it was before any later edition existed; this build
	// writes edition 3, which adds text lists.
	if e.GetLanguageEdition() != 1 || profile.LanguageEdition != 3 {
		t.Errorf("language edition %d", e.GetLanguageEdition())
	}
	p, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Decide(profile.Location{Tag: 0x00080020, VR: "DA"}, facts{}); !slices.Equal(d.VRs, []string{"DA"}) {
		t.Errorf("the decision's VRs: %v", d.VRs)
	}
}

// TestCompileReadsNoDictionary guards that a stored version is compiled by
// its structure and its stored VRs alone: a rule whose tag a later
// dictionary does not know, or gives another VR, still runs as validated;
// and a stored form whose VRs disagree with its selector, whose defaults
// are not written out, or whose language edition this build does not know
// does not compile.
func TestCompileReadsNoDictionary(t *testing.T) {
	cat := testCatalog(t)
	fresh := func() *v1.ExpandedProfile {
		return mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
			rl("name", kw("PatientName"), hash()),
			rl("by-vr", vr("PN"), hash()),
			rl("by-group", grp("0010"), remove()),
			rl("ids", kw("PatientID"), lookup("subjects", v1.LookupMissing_LOOKUP_MISSING_ASSIGN)),
		}}, cat)
	}
	// A tag the dictionary does not know (an unassigned element of group
	// 0010), and one it gives another VR, keep the VR stored with them.
	for _, tag := range []string{"00109990", "00080020"} {
		e := fresh()
		e.GetRules()[0].GetRule().Selector = tg(tag)
		p, err := profile.Compile(e)
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		if d := p.Decide(profile.Location{Tag: 0x00109990, VR: "PN"}, facts{}); tag == "00109990" && (d.Index != 0 || !slices.Equal(d.VRs, []string{"PN"})) {
			t.Errorf("%s: decided by %d with VRs %v", tag, d.Index, d.VRs)
		}
	}
	for _, c := range []struct {
		name   string
		change func(e *v1.ExpandedProfile)
	}{
		{"a vr selector with another VR", func(e *v1.ExpandedProfile) { e.GetRules()[1].Vrs = []string{"LO"} }},
		{"a group with a VR", func(e *v1.ExpandedProfile) { e.GetRules()[2].Vrs = []string{"PN"} }},
		{"a tag whose VR does not fit its action", func(e *v1.ExpandedProfile) { e.GetRules()[0].Vrs = []string{"DA"} }},
		{"a malformed VR", func(e *v1.ExpandedProfile) { e.GetRules()[0].Vrs = []string{"pn"} }},
		{"a keyword", func(e *v1.ExpandedProfile) { e.GetRules()[0].GetRule().Selector = kw("PatientName") }},
		{"a lookup default not written out", func(e *v1.ExpandedProfile) {
			e.GetRules()[3].GetRule().GetAction().GetLookup().OnMissing = v1.LookupMissing_LOOKUP_MISSING_UNSPECIFIED
		}},
		{"no edition", func(e *v1.ExpandedProfile) { e.LanguageEdition = 0 }},
		{"a later edition", func(e *v1.ExpandedProfile) { e.LanguageEdition = profile.LanguageEdition + 1 }},
	} {
		e := fresh()
		c.change(e)
		if _, err := profile.Compile(e); err == nil {
			t.Errorf("%s: compiled", c.name)
		}
	}
}

// bigCatalog is a catalog of two bases of 750 rules each, the second
// built on the first: 1,500 rules, as many as the bases of PS3.15 Table
// E.1-1 and its options together are expected to hold.
func bigCatalog(t *testing.T) *profile.Catalog {
	t.Helper()
	table := func() *v1.ProfileDocument {
		doc := &v1.ProfileDocument{}
		for i := range 750 {
			r := rl("", kw("PatientName"), remove())
			r.Reason = strings.Repeat("r", 60)
			if i%3 == 0 {
				r.Action = unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY)
			}
			doc.Rules = append(doc.Rules, r)
		}
		return doc
	}
	options := table()
	options.Bases = []string{"test/table:1"}
	c, err := profile.NewCatalog(
		profile.Base{Name: "test/table:1", Description: "a table", Document: table()},
		profile.Base{Name: "test/options:1", Description: "its options", Document: options},
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// largeDocument is a document of the most rules, each with a reason of
// reasonBytes and conditions of conditionBytes.
func largeDocument(reasonBytes, conditionBytes int) *v1.ProfileDocument {
	doc := &v1.ProfileDocument{Bases: []string{"test/options:1"}}
	for range profile.MaxRules {
		r := rl("", kw("PatientComments"), replaceTemplate("SUBJ-{#PatientID}"))
		r.Reason = strings.Repeat("x", reasonBytes)
		if conditionBytes > 0 {
			r.When = []*v1.ProfileCondition{equals(modality, strings.Repeat("y", conditionBytes))}
		}
		doc.Rules = append(doc.Rules, r)
	}
	return doc
}

// TestEveryVersionFitsAnAnswer guards that a version the API accepts can
// be answered whole: with bases of 1,500 rules (Table E.1-1 adds about
// 650), a document of the most rules with the longest reasons is valid,
// and its full answer, in protobuf and in JSON, fits the 4 MiB message
// limit with a note and the most warnings; a document that fits a request
// but whose expanded form would not fit an answer with it is refused.
func TestEveryVersionFitsAnAnswer(t *testing.T) {
	const messageLimit = 4 << 20
	cat := bigCatalog(t)
	doc := largeDocument(profile.MaxReasonLength, 0)
	e := mustExpand(t, doc, cat)
	_, sum, err := profile.Checksum(e)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []*v1.ProfileProblem
	for range profile.MaxWarnings + 1 {
		warnings = append(warnings, profile.Problem{Rule: 1000, RuleName: strings.Repeat("n", 64), Field: "selector", Code: profile.CodeShadowedByBase, Message: strings.Repeat("m", 600)}.Proto())
	}
	answer := &v1.CreateProfileVersionResponse{
		Version: &v1.ProfileVersion{
			Name: strings.Repeat("p", 63), Version: profile.MaxVersions, Checksum: sum, CreateTime: timestamppb.Now(),
			CreatedByKeyId: strings.Repeat("k", 64), Note: strings.Repeat("z", 1024), Document: doc, Expanded: e,
		},
		Created: true, Warnings: warnings,
	}
	j, err := protojson.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if n := proto.Size(answer); n > messageLimit {
		t.Errorf("the protobuf answer is %d bytes", n)
	}
	if len(j) > messageLimit {
		t.Errorf("the JSON answer is %d bytes", len(j))
	}
	t.Logf("a full answer: %d bytes of protobuf, %d of JSON", proto.Size(answer), len(j))

	big := largeDocument(profile.MaxReasonLength, 1024)
	if n := proto.Size(&v1.CreateProfileVersionRequest{Tenant: "t", Name: "p", Document: big}); n > messageLimit {
		t.Fatalf("the large document does not fit a request: %d bytes", n)
	}
	_, problems := profile.Expand(big, cat)
	if len(problems) != 1 || problems[0].Code != profile.CodeTooLarge || problems[0].Rule != 0 {
		t.Errorf("problems %v", problems)
	}
}
