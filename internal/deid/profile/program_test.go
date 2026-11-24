package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Tags of the elements the precedence cases visit.
const (
	patientName     = 0x00100010
	patientID       = 0x00100020
	refStudySeq     = 0x00081110
	refSeriesSeq    = 0x00081115
	refSOPInstance  = 0x00081155
	contentSequence = 0x0040A730
	textValue       = 0x0040A160
	csaElement      = 0x00291010
	csaOther        = 0x00291020
	overlayData     = 0x60003000
)

// actionName returns the kind of an action, for comparing decisions.
func actionName(a *v1.ProfileAction) string {
	switch a.GetKind().(type) {
	case *v1.ProfileAction_Keep:
		return "keep"
	case *v1.ProfileAction_Remove:
		return "remove"
	case *v1.ProfileAction_Empty:
		return "empty"
	case *v1.ProfileAction_Replace:
		return "replace"
	case *v1.ProfileAction_Hash:
		return "hash"
	case *v1.ProfileAction_Uid:
		return "uid"
	case *v1.ProfileAction_Clean:
		return "clean"
	}
	return "other"
}

// TestPrecedence guards which rule decides when several apply: the most
// specific selector, then the later rule; the document's rules over its
// bases', a later base over an earlier; a condition that does not hold
// leaving the element to the next rule; and the defaults when no rule
// decides.
func TestPrecedence(t *testing.T) {
	cat := testCatalog(t)
	top := func(tag uint32, vrName string) profile.Location { return profile.Location{Tag: tag, VR: vrName} }
	for _, c := range []struct {
		name   string
		doc    *v1.ProfileDocument
		loc    profile.Location
		facts  facts
		action string
		rule   string // the deciding rule's name; "" for the default
	}{
		{"a tag beats a later vr",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("t", kw("PatientName"), keep()), rl("v", vr("PN"), remove())}},
			top(patientName, "PN"), nil, "keep", "t"},
		{"a vr decides what no tag names",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("t", kw("PatientName"), keep()), rl("v", vr("PN"), hash())}},
			top(0x00081050, "PN"), nil, "hash", "v"},
		{"a path beats a later tag",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("ReferencedStudySequence[*]/ReferencedSOPInstanceUID"), keep()), rl("t", kw("ReferencedSOPInstanceUID"), uid())}},
			profile.Location{Sequences: []uint32{refStudySeq}, Tag: refSOPInstance, VR: "UI"}, nil, "keep", "p"},
		{"a path does not reach another sequence",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("ReferencedStudySequence[*]/ReferencedSOPInstanceUID"), keep()), rl("t", kw("ReferencedSOPInstanceUID"), uid())}},
			profile.Location{Sequences: []uint32{refSeriesSeq}, Tag: refSOPInstance, VR: "UI"}, nil, "uid", "t"},
		{"a top-level path does not reach into sequences",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("PatientName"), keep()), rl("t", kw("PatientName"), remove())}},
			profile.Location{Sequences: []uint32{refStudySeq}, Tag: patientName, VR: "PN"}, nil, "remove", "t"},
		{"a top-level path decides at the top level",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("PatientName"), keep()), rl("t", kw("PatientName"), remove())}},
			top(patientName, "PN"), nil, "keep", "p"},
		{"any depth matches no level",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("**/ContentSequence[*]/TextValue"), clean())}},
			profile.Location{Sequences: []uint32{contentSequence}, Tag: textValue, VR: "UT"}, nil, "clean", "p"},
		{"any depth matches many levels",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("**/ContentSequence[*]/TextValue"), clean())}},
			profile.Location{Sequences: []uint32{contentSequence, contentSequence, contentSequence, contentSequence}, Tag: textValue, VR: "UT"}, nil, "clean", "p"},
		{"any depth still needs the last sequence",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", path("**/ContentSequence[*]/TextValue"), clean())}},
			profile.Location{Sequences: []uint32{contentSequence, refStudySeq}, Tag: textValue, VR: "UT"}, nil, "keep", ""},
		{"a content item by its concept",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("i", item("DCM", "121008"), remove()), rl("t", kw("PersonName"), keep())}},
			profile.Location{Sequences: []uint32{contentSequence}, Tag: 0x0040A123, VR: "PN", ContentItem: &profile.Concept{Scheme: "DCM", Value: "121008"}, ValueType: "PNAME"}, nil, "remove", "i"},
		{"a content item of another concept",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("i", item("DCM", "121008"), remove()), rl("t", kw("PersonName"), keep())}},
			profile.Location{Sequences: []uint32{contentSequence}, Tag: 0x0040A123, VR: "PN", ContentItem: &profile.Concept{Scheme: "DCM", Value: "121009"}, ValueType: "PNAME"}, nil, "keep", "t"},
		{"the later of two tag rules",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("first", kw("PatientName"), remove()), rl("second", tg("00100010"), hash())}},
			top(patientName, "PN"), nil, "hash", "second"},
		{"own rule over the bases'",
			&v1.ProfileDocument{Bases: []string{baseB}, Rules: []*v1.ProfileRule{rl("own", kw("PatientName"), keep())}},
			top(patientName, "PN"), nil, "keep", "own"},
		{"a later base over an earlier",
			&v1.ProfileDocument{Bases: []string{baseB}},
			top(patientName, "PN"), nil, "replace", "b-name"},
		{"a less specific own rule loses to a base's tag",
			&v1.ProfileDocument{Bases: []string{baseB}, Rules: []*v1.ProfileRule{rl("own", vr("PN"), keep())}},
			top(patientName, "PN"), nil, "replace", "b-name"},
		{"a private element over its block over its group",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("e", priv("SIEMENS CSA HEADER", "0029", "10"), empty()), rl("b", priv("SIEMENS CSA HEADER", "0029", "xx"), keep()), rl("g", grpPrivate("0029"), remove())}},
			profile.Location{Tag: csaElement, Creator: "SIEMENS CSA HEADER", VR: "OB"}, nil, "empty", "e"},
		{"a block decides its other elements",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("e", priv("SIEMENS CSA HEADER", "0029", "10"), empty()), rl("b", priv("SIEMENS CSA HEADER", "0029", "xx"), keep()), rl("g", grpPrivate("0029"), remove())}},
			profile.Location{Tag: csaOther, Creator: "SIEMENS CSA HEADER", VR: "OB"}, nil, "keep", "b"},
		{"another creator in the same group",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("b", priv("SIEMENS CSA HEADER", "0029", "xx"), keep()), rl("g", grpPrivate("0029"), empty())}},
			profile.Location{Tag: csaElement, Creator: "ACME", VR: "OB"}, nil, "empty", "g"},
		{"a block is matched by creator, not by block number",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("b", priv("SIEMENS CSA HEADER", "0029", "10"), keep())}},
			profile.Location{Tag: 0x00291110, Creator: "SIEMENS CSA HEADER", VR: "OB"}, nil, "keep", "b"},
		{"a private creator element is not a data element of its block",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("b", priv("SIEMENS CSA HEADER", "0029", "xx"), keep())}},
			profile.Location{Tag: 0x00290010, Creator: "SIEMENS CSA HEADER", VR: "LO"}, nil, "remove", ""},
		{"a group beats a later vr",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("g", grp("0010"), keep()), rl("v", vr("PN"), remove())}},
			top(patientName, "PN"), nil, "keep", "g"},
		{"a block beats a later group",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("b", priv("ACME", "0029", "xx"), keep()), rl("g", grpPrivate("0029"), remove())}},
			profile.Location{Tag: csaElement, Creator: "ACME", VR: "UN"}, nil, "keep", "b"},
		{"a group by wildcard",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("g", grp("60xx"), remove())}},
			top(overlayData+0x00020000, "OW"), facts{}, "remove", "g"},
		{"a condition that holds",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("ct", kw("PatientName"), keep(), equals(modality, "CT")), rl("v", vr("PN"), hash())}},
			top(patientName, "PN"), facts{modality: "CT"}, "keep", "ct"},
		{"a condition that fails leaves the element to the next rule",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("ct", kw("PatientName"), keep(), equals(modality, "CT")), rl("v", vr("PN"), hash())}},
			top(patientName, "PN"), facts{modality: "MR"}, "hash", "v"},
		{"an absent subject fails equals",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("ct", kw("PatientName"), keep(), equals(modality, ""))}},
			top(patientName, "PN"), facts{}, "keep", ""},
		{"a negated condition",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("not-us", kw("PatientName"), remove(), &v1.ProfileCondition{Subject: modality, Test: &v1.ProfileCondition_Equals{Equals: "US"}, Negate: true})}},
			top(patientName, "PN"), facts{modality: "CT"}, "remove", "not-us"},
		{"a pattern matches the whole value",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("ct", kw("PatientName"), remove(), pattern(modality, "C"))}},
			top(patientName, "PN"), facts{modality: "CT"}, "keep", ""},
		{"present false holds for an empty value",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("no-aet", kw("PatientName"), remove(), &v1.ProfileCondition{Subject: calling, Test: &v1.ProfileCondition_Present{Present: false}})}},
			top(patientName, "PN"), facts{calling: ""}, "remove", "no-aet"},
		{"every condition must hold",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("both", kw("PatientName"), remove(), equals(modality, "CT"), equals(calling, "SCANNER"))}},
			top(patientName, "PN"), facts{modality: "CT", calling: "OTHER"}, "keep", ""},
		{"a private element no rule names is removed",
			&v1.ProfileDocument{Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP},
			profile.Location{Tag: csaElement, Creator: "SIEMENS CSA HEADER", VR: "OB"}, nil, "remove", ""},
		{"a private element without creator no rule names is removed",
			&v1.ProfileDocument{},
			top(csaElement, "UN"), nil, "remove", ""},
		{"unnamed keep",
			&v1.ProfileDocument{},
			top(patientID, "LO"), nil, "keep", ""},
		{"unnamed remove from a base",
			&v1.ProfileDocument{Bases: []string{baseA}},
			top(0x00080060, "CS"), nil, "remove", ""},
		{"a tag rule does not select a private element of the same number",
			&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("t", kw("PatientName"), keep())}},
			profile.Location{Tag: patientName, Creator: "ACME", VR: "PN"}, nil, "keep", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := mustCompile(t, c.doc, cat)
			f := c.facts
			if f == nil {
				f = facts{}
			}
			d := p.Decide(c.loc, f)
			if got := actionName(d.Action); got != c.action {
				t.Errorf("action %s, want %s (rule %v)", got, c.action, d.Rule)
			}
			if got := d.Rule.GetRule().GetName(); got != c.rule {
				t.Errorf("rule %q, want %q", got, c.rule)
			}
			if (d.Rule == nil) != (d.Index == -1) {
				t.Errorf("index %d with rule %v", d.Index, d.Rule)
			}
		})
	}
}

// TestPixelRuleOrder guards which pixel rule decides for an image: the
// last that matches its size and conditions, the document's over its
// bases'; none for an image no rule covers, which is then quarantined.
func TestPixelRuleOrder(t *testing.T) {
	p := mustCompile(t, richDocument(), testCatalog(t))
	for _, c := range []struct {
		name       string
		rows, cols int
		facts      facts
		want       string
	}{
		{"the last that matches", 480, 640, facts{modality: "US"}, "detect-rest"},
		{"a condition", 480, 640, facts{calling: "REVIEW"}, "reviewed"},
	} {
		r, ok := p.Pixels(c.rows, c.cols, c.facts)
		if !ok || r.GetRule().GetName() != c.want {
			t.Errorf("%s: %v, want %s", c.name, r, c.want)
		}
	}
	only := mustCompile(t, &v1.ProfileDocument{PixelRules: []*v1.PixelRule{
		{Name: "us", Rows: 480, Columns: 640, When: []*v1.ProfileCondition{equals(modality, "US")}, Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}},
	}}, testCatalog(t))
	if r, ok := only.Pixels(480, 640, facts{modality: "US"}); !ok || r.GetRule().GetName() != "us" {
		t.Errorf("the matching rule: %v", r)
	}
	if r, ok := only.Pixels(600, 800, facts{modality: "US"}); ok {
		t.Errorf("an image of another size matched %v", r)
	}
	if r, ok := only.Pixels(480, 640, facts{modality: "XA"}); ok {
		t.Errorf("an image of another modality matched %v", r)
	}
}

// TestCompileRefusesWhatExpandDidNotMake guards that a stored expanded form
// changed after it was made (a keyword where a tag was, a default left
// unspecified) does not compile, so a damaged version never runs.
func TestCompileRefusesWhatExpandDidNotMake(t *testing.T) {
	e := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("n", kw("PatientName"), remove())}}, testCatalog(t))
	if _, err := profile.Compile(e); err != nil {
		t.Fatal(err)
	}
	broken := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("n", kw("PatientName"), remove())}}, testCatalog(t))
	broken.GetRules()[0].GetRule().Action = uid()
	if _, err := profile.Compile(broken); err == nil {
		t.Error("a rule remapping a UID in a name compiled")
	}
	unstated := mustExpand(t, &v1.ProfileDocument{}, testCatalog(t))
	unstated.Unnamed = v1.UnnamedElements_UNNAMED_ELEMENTS_UNSPECIFIED
	if _, err := profile.Compile(unstated); err == nil {
		t.Error("an expanded form without its defaults compiled")
	}
	if _, err := profile.Compile(nil); err == nil {
		t.Error("nothing compiled")
	}
}
