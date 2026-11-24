package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestBroadSelectorsLeavePrivateElements guards that a vr or a group rule
// does not select private elements unless it says include_private, so the
// default that removes a private element no rule names by its creator
// holds: a rule keeping every LO must not keep a vendor's header of VR LO.
func TestBroadSelectorsLeavePrivateElements(t *testing.T) {
	cat := testCatalog(t)
	csa := profile.Location{Tag: csaElement, Creator: "SIEMENS CSA HEADER", VR: "LO"}
	orphan := profile.Location{Tag: csaElement, VR: "UN"}
	for _, c := range []struct {
		name   string
		doc    *v1.ProfileDocument
		loc    profile.Location
		action string
	}{
		{"vr keep leaves a private element", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("v", vr("LO"), keep())}}, csa, "remove"},
		{"vr keep with private keeps it", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("v", vrPrivate("LO"), keep())}}, csa, "keep"},
		{"vr keep leaves a private element without creator", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("v", vr("UN"), keep())}}, orphan, "remove"},
		{"group keep leaves a private element", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("g", grp("002x"), keep())}}, csa, "remove"},
		{"group keep with private keeps it", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("g", grpPrivate("0029"), keep())}}, csa, "keep"},
		{"vr with private still selects a standard element", &v1.ProfileDocument{Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE, Rules: []*v1.ProfileRule{rl("v", vrPrivate("PN"), keep())}}, profile.Location{Tag: patientName, VR: "PN"}, "keep"},
		{"vr with private does not select a creator element", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("v", vrPrivate("LO"), keep())}}, profile.Location{Tag: 0x00290010, Creator: "SIEMENS CSA HEADER", VR: "LO"}, "remove"},
		{"private element beats a vr with private", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", priv("SIEMENS CSA HEADER", "0029", "10"), remove()), rl("v", vrPrivate("LO"), keep())}}, csa, "remove"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := mustCompile(t, c.doc, cat).Decide(c.loc, facts{})
			if got := actionName(d.Action); got != c.action {
				t.Errorf("action %s, want %s (rule %v)", got, c.action, d.Rule)
			}
		})
	}
}

// TestContentItemSelectsItsValueElement guards that a content item rule
// selects only the element its item's value type holds the value in
// (PS3.3 C.17.3.3), and that the item's other elements follow the other
// rules.
func TestContentItemSelectsItsValueElement(t *testing.T) {
	observer := &profile.Concept{Scheme: "DCM", Value: "121008"}
	in := func(tag uint32, vrName, valueType string) profile.Location {
		return profile.Location{Sequences: []uint32{contentSequence}, Tag: tag, VR: vrName, ContentItem: observer, ValueType: valueType}
	}
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("any", item("DCM", "121008"), remove()),
		rl("names", itemOf("DCM", "121008", "PNAME"), hash()),
	}}, testCatalog(t))
	for _, c := range []struct {
		name string
		loc  profile.Location
		rule string
	}{
		{"the person name of a PNAME item", in(0x0040A123, "PN", "PNAME"), "names"},
		{"the text of a TEXT item", in(0x0040A160, "UT", "TEXT"), "any"},
		{"the measured value of a NUM item", in(0x0040A300, "SQ", "NUM"), "any"},
		{"the reference of an IMAGE item", in(0x00081199, "SQ", "IMAGE"), "any"},
		{"the concept name is not the value", in(0x0040A043, "SQ", "PNAME"), ""},
		{"the observation time is not the value", in(0x0040A032, "DT", "PNAME"), ""},
		{"a text value in a PNAME item is not its value", in(0x0040A160, "UT", "PNAME"), ""},
		{"a container has no value element", in(0x0040A050, "CS", "CONTAINER"), ""},
		{"an unknown value type has no value element", in(0x0040A123, "PN", "NAME"), ""},
	} {
		d := p.Decide(c.loc, facts{})
		if got := d.Rule.GetRule().GetName(); got != c.rule {
			t.Errorf("%s: rule %q, want %q", c.name, got, c.rule)
		}
	}
	if got := profile.ValueTypes(); len(got) != 15 || got[0] != "CODE" {
		t.Errorf("value types %v", got)
	}
}

// TestPipelineElementsAreNeverSelected guards that pixel data, the
// directory elements and the elements the pipeline removes from every file
// (PS3.15 E.1.1) are selected by no group or vr rule, so only pixel rules
// change the image and the pipeline alone handles the rest.
func TestPipelineElementsAreNeverSelected(t *testing.T) {
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("ob", vr("OB"), remove()),
		rl("ow", vr("OW"), remove()),
		rl("sq", vr("SQ"), empty()),
		rl("fam", grp("7Fxx"), remove()),
		rl("sig", grp("FFxA"), remove()),
		rl("pad", grp("FFxC"), remove()),
		rl("four", grp("04xx"), remove()),
	}}, testCatalog(t))
	for _, loc := range []profile.Location{
		{Tag: 0x7FE00010, VR: "OB"},
		{Tag: 0x7FE00010, VR: "OW"},
		{Tag: 0x7FE00001, VR: "OV"},
		{Tag: 0x04000561, VR: "SQ"},
		{Tag: 0x04000500, VR: "SQ"},
		{Tag: 0xFFFAFFFA, VR: "SQ"},
		{Tag: 0xFFFCFFFC, VR: "OB"},
		{Tag: 0x4FFE0001, VR: "SQ"},
	} {
		if d := p.Decide(loc, facts{}); d.Rule != nil {
			t.Errorf("%08X %s selected by %q", loc.Tag, loc.VR, d.Rule.GetRule().GetName())
		}
	}
	if d := p.Decide(profile.Location{Tag: 0x04000550, VR: "SQ"}, facts{}); d.Rule.GetRule().GetName() != "four" {
		t.Errorf("another element of group 0400: %v", d.Rule)
	}
}

// TestVeryLongVRsAreSelectable guards that a rule may select by OV, SV and
// UV, the 64-bit VRs PS3.5 6.2 added in 2019, and selects an element of
// each: before the DICOM library knew them, such a rule was refused as an
// unknown VR.
func TestVeryLongVRsAreSelectable(t *testing.T) {
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("ov", vr("OV"), remove()),
		rl("sv", vr("SV"), empty()),
		rl("uv", vr("UV"), keep()),
	}}, testCatalog(t))
	for name, loc := range map[string]profile.Location{
		"ov": {Tag: 0x00660040, VR: "OV"},
		"sv": {Tag: 0x00720082, VR: "SV"},
		"uv": {Tag: 0x00720083, VR: "UV"},
	} {
		if d := p.Decide(loc, facts{}); d.Rule.GetRule().GetName() != name {
			t.Errorf("%08X %s: decided by %v", loc.Tag, loc.VR, d.Rule)
		}
	}
}
