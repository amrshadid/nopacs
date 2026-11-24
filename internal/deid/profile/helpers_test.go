package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Selectors.
func kw(k string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
}
func tg(t string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: t}}
}
func grp(g string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Group{Group: g}}
}
func vr(v string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: v}}
}
func path(p string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Path{Path: p}}
}
func priv(creator, group, elem string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{Creator: creator, Group: group, Element: elem}}}
}
func grpPrivate(g string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Group{Group: g}, IncludePrivate: true}
}
func vrPrivate(v string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: v}, IncludePrivate: true}
}
func itemOf(scheme, value, valueType string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ContentItem{ContentItem: &v1.ContentItemSelector{Scheme: scheme, Value: value, ValueType: valueType}}}
}
func item(scheme, value string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ContentItem{ContentItem: &v1.ContentItemSelector{Scheme: scheme, Value: value}}}
}

// Actions.
func keep() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
}
func remove() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
}
func empty() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}
}
func hash() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}
}
func uid() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Uid{Uid: &v1.UidAction{}}}
}
func shift() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}
}
func generalize() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Generalize{Generalize: &v1.GeneralizeAction{}}}
}
func clean() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}
}
func dummy() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Dummy{Dummy: &v1.DummyAction{}}}
}
func unlessRequired(f v1.RequiredFallback) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_RemoveUnlessRequired{RemoveUnlessRequired: &v1.RemoveUnlessRequiredAction{Fallback: f}}}
}
func quarantine() *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Quarantine{Quarantine: &v1.QuarantineAction{}}}
}
func replace(c string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{Value: &v1.ReplaceAction_Constant{Constant: c}}}}
}
func replaceTemplate(t string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{Value: &v1.ReplaceAction_Template{Template: t}}}}
}
func set(c string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Set{Set: &v1.SetAction{Value: &v1.SetAction_Constant{Constant: c}}}}
}
func lookup(table string, missing v1.LookupMissing) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: table, OnMissing: missing}}}
}

// rl makes a rule.
func rl(name string, sel *v1.ProfileSelector, act *v1.ProfileAction, when ...*v1.ProfileCondition) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: sel, Action: act, When: when}
}

// Conditions.
func equals(s v1.ConditionSubject, v string) *v1.ProfileCondition {
	return &v1.ProfileCondition{Subject: s, Test: &v1.ProfileCondition_Equals{Equals: v}}
}
func pattern(s v1.ConditionSubject, p string) *v1.ProfileCondition {
	return &v1.ProfileCondition{Subject: s, Test: &v1.ProfileCondition_Pattern{Pattern: p}}
}

const (
	modality = v1.ConditionSubject_CONDITION_SUBJECT_MODALITY
	calling  = v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE
)

// facts are the values conditions read in a test.
type facts map[v1.ConditionSubject]string

// Value answers a subject's value; a subject not in the map is absent.
func (f facts) Value(s v1.ConditionSubject) (string, bool) {
	v, ok := f[s]
	return v, ok
}

// Base names of the test catalog.
const (
	baseA      = "test/base-a:1"
	baseB      = "test/base-b:1"
	baseGone   = "test/gone:1"
	baseOnGone = "test/on-gone:1"
)

// testCatalog is a catalog of small bases: a, which removes the patient's
// name, says unnamed elements are removed and shifts up to 30 days; b,
// built on a, which replaces the name and the patient's id; one that is not
// available; and one built on it.
func testCatalog(t *testing.T) *profile.Catalog {
	t.Helper()
	c, err := profile.NewCatalog(
		profile.Base{Name: baseA, Description: "a", Document: &v1.ProfileDocument{
			Unnamed:       v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE,
			DateShiftDays: 30,
			Rules:         []*v1.ProfileRule{rl("a-name", kw("PatientName"), remove()), rl("a-id", kw("PatientID"), remove())},
		}},
		profile.Base{Name: baseB, Description: "b", Document: &v1.ProfileDocument{
			Bases:      []string{baseA},
			Rules:      []*v1.ProfileRule{rl("b-name", kw("PatientName"), replace("ANON")), rl("b-id", kw("PatientID"), hash())},
			PixelRules: []*v1.PixelRule{{Name: "b-pix", Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}}},
		}},
		profile.Base{Name: baseGone, Description: "gone", Unavailable: "its table is not in this build"},
		profile.Base{Name: baseOnGone, Description: "on gone", Document: &v1.ProfileDocument{Bases: []string{baseGone}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustExpand expands a document that must be valid.
func mustExpand(t *testing.T, doc *v1.ProfileDocument, cat *profile.Catalog) *v1.ExpandedProfile {
	t.Helper()
	e, problems := profile.Expand(doc, cat)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	return e
}

// mustCompile expands and compiles a document that must be valid.
func mustCompile(t *testing.T, doc *v1.ProfileDocument, cat *profile.Catalog) *profile.Program {
	t.Helper()
	p, err := profile.Compile(mustExpand(t, doc, cat))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// richDocument uses every kind of selector, condition and action, and
// every kind of pixel rule.
func richDocument() *v1.ProfileDocument {
	return &v1.ProfileDocument{
		Bases:         []string{baseB},
		Unnamed:       v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP,
		DateShiftDays: 90,
		Description:   "every kind of rule",
		Rules: []*v1.ProfileRule{
			rl("names", vr("PN"), hash()),
			rl("overlays", grp("60xx"), remove()),
			rl("csa", priv("SIEMENS CSA HEADER", "0029", "xx"), keep()),
			rl("csa-one", priv("SIEMENS CSA HEADER", "0029", "10"), remove()),
			rl("ref-study", path("ReferencedStudySequence[*]/ReferencedSOPInstanceUID"), uid()),
			rl("any-text", path("**/ContentSequence[*]/TextValue"), clean()),
			rl("observer", item("DCM", "121008"), remove()),
			rl("study-uid", kw("StudyInstanceUID"), uid()),
			rl("study-date", tg("00080020"), shift(), pattern(modality, "CT|MR")),
			rl("age", kw("PatientAge"), generalize()),
			rl("description", kw("StudyDescription"), clean(), &v1.ProfileCondition{Subject: calling, Test: &v1.ProfileCondition_Present{Present: true}, Negate: true}),
			rl("subject", kw("PatientID"), lookup("subjects", v1.LookupMissing_LOOKUP_MISSING_ASSIGN)),
			rl("comments", kw("PatientComments"), replaceTemplate("SUBJ-{#PatientID}-{{x}}")),
			rl("method", kw("DeidentificationMethod"), set("NoPACs")),
			rl("institution", kw("InstitutionName"), empty(), &v1.ProfileCondition{Subject: modality, Test: &v1.ProfileCondition_Prefix{Prefix: "U"}}),
			rl("no-sc", kw("BurnedInAnnotation"), quarantine(), equals(modality, "SC")),
		},
		PixelRules: []*v1.PixelRule{
			{Name: "us-banner", Rows: 480, Columns: 640, When: []*v1.ProfileCondition{equals(modality, "US")}, Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 640, Height: 40}, {X: 10, Y: 10, Width: 100, Height: 20, FromRight: true, FromBottom: true}}}}},
			{Name: "detect-rest", Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}},
			{Name: "reviewed", When: []*v1.ProfileCondition{equals(calling, "REVIEW")}, Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "a person reviews these images before release"},
		},
	}
}
