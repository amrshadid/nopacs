package profile

import (
	"slices"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// effectOf returns the effect of the action on the class, or nil.
func effectOf(d []*v1.ProfileClassEffects, class v1.ProfileElementClass, action string) *v1.ProfileClassEffect {
	for _, c := range d {
		if c.GetClass() != class {
			continue
		}
		for _, e := range c.GetEffects() {
			if e.GetAction() == action {
				return e
			}
		}
	}
	return nil
}

// TestDescribeGroupsTheBasicProfile guards a description of the basic
// profile with rules of its own: names, identifiers, dates and UIDs each
// in their class with the action that decides them (an own rule that keeps
// what the base removes is listed as kept, not also as removed); a rule
// with a condition is marked conditional; a private element and a pixel
// rule are in their classes; the classes come in their order.
func TestDescribeGroupsTheBasicProfile(t *testing.T) {
	e, ps := Expand(&v1.ProfileDocument{
		Bases: []string{AnnexE("basic")},
		Rules: []*v1.ProfileRule{
			{Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientAge"}}, Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}},
			{Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDate"}}, Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}},
			{Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "SeriesDescription"}}, Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}},
				When: []*v1.ProfileCondition{{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Equals{Equals: "CT"}}}},
			{Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{Creator: "ACME 1", Group: "0009", Element: "10"}}},
				Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}},
		},
		PixelRules: []*v1.PixelRule{{Name: "us-banner", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{Width: 10, Height: 10}}}}}},
	}, Shipped())
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	d := Describe(e)
	var classes []v1.ProfileElementClass
	for _, c := range d {
		classes = append(classes, c.GetClass())
	}
	if !slices.IsSorted(classes) || len(classes) != 7 {
		t.Errorf("the classes: %v", classes)
	}
	has := func(class v1.ProfileElementClass, action, what string) *v1.ProfileClassEffect {
		t.Helper()
		x := effectOf(d, class, action)
		if x == nil || !slices.Contains(x.GetElements(), what) {
			t.Errorf("%s is not among %v %s: %v", what, class, action, x.GetElements())
		}
		return x
	}
	names := effectOf(d, v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES, "remove")
	nameEmpty := effectOf(d, v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES, "empty")
	if !slices.Contains(names.GetElements(), "PatientName") && !slices.Contains(nameEmpty.GetElements(), "PatientName") &&
		!slices.Contains(effectOf(d, v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES, "remove_unless_required").GetElements(), "PatientName") {
		t.Errorf("PatientName is not removed or emptied among the names: %v", d[0])
	}
	has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_UIDS, "uid", "StudyInstanceUID")
	has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_DATES, "shift", "StudyDate")
	has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_DATES, "keep", "PatientAge")
	has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PRIVATE, "keep", "private 'ACME 1' 0009,10")
	if x := has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PIXELS, "redact", "us-banner"); x.GetConditional() {
		t.Error("a pixel rule of no condition is conditional")
	}
	if x := has(v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_OTHER, "keep", "SeriesDescription"); !x.GetConditional() {
		t.Error("a rule with a condition is not marked conditional")
	}
	for _, c := range d {
		for _, x := range c.GetEffects() {
			if x.GetAction() != "keep" && (slices.Contains(x.GetElements(), "PatientAge") || slices.Contains(x.GetElements(), "StudyDate") && x.GetAction() != "shift") {
				t.Errorf("an element an own rule decides is listed under %s too", x.GetAction())
			}
		}
	}
	found := false
	for _, c := range d {
		if c.GetClass() != v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_IDENTIFIERS {
			continue
		}
		for _, x := range c.GetEffects() {
			found = found || slices.Contains(x.GetElements(), "PatientID")
		}
	}
	if !found {
		t.Error("PatientID is not among the identifiers")
	}
	notes := strings.Join(DescribeNotes(e), " ")
	for _, want := range []string{"is kept", "never kept", "searched", "1 to 365 days", "held"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q: %s", want, notes)
		}
	}
}

// TestIdentifierKeyword guards which keywords count as identifiers: ID as
// a word, the identifiers' and addresses' words; never a word that only
// holds the letters (VideoImageFormatAcquired, IDontKnow).
func TestIdentifierKeyword(t *testing.T) {
	for kw, want := range map[string]bool{
		"PatientID": true, "OtherPatientIDsSequence": true, "StudyID": true, "IssuerOfPatientID": true,
		"AccessionNumber": true, "InstitutionAddress": true, "PatientTelephoneNumbers": true, "DeviceIdentifier": true,
		"Modality": false, "SeriesDescription": false, "VideoImageFormatAcquired": false, "IDontKnow": false,
	} {
		if got := identifierKeyword(kw); got != want {
			t.Errorf("%s: %v, want %v", kw, got, want)
		}
	}
}
