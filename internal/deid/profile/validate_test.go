package profile_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestValidDocuments guards that documents using every kind of selector,
// condition, action and pixel rule pass the checks, so the checks refuse
// only what is wrong.
func TestValidDocuments(t *testing.T) {
	cat := testCatalog(t)
	for _, c := range []struct {
		name string
		doc  *v1.ProfileDocument
	}{
		{"empty", &v1.ProfileDocument{}},
		{"rich", richDocument()},
		{"bases only", &v1.ProfileDocument{Bases: []string{baseA, baseB}}},
		{"tag in lower case", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", tg("0010001a"), remove())}}},
		{"path by tags", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", path("00081110[*]/00081155"), uid())}}},
		{"top-level path", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", path("PatientName"), remove())}}},
		{"deep path", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", path("**/ReferencedSeriesSequence[*]/**/ReferencedSOPInstanceUID"), uid())}}},
		{"odd group with private", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", grpPrivate("0019"), remove())}}},
		{"sequence emptied", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("OtherPatientIDsSequence"), empty())}}},
		{"empty constant", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("PatientName"), replace(""))}}},
		{"content item hashed", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", itemOf("DCM", "121008", "PNAME"), hash())}}},
		{"largest shift", &v1.ProfileDocument{DateShiftDays: profile.MaxDateShiftDays}},
		{"uid on private UIDs", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", vrPrivate("UI"), uid())}}},
		{"dummy by group", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", grp("0010"), dummy())}}},
		{"dummy on a private element", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", priv("ACME", "0019", "10"), dummy())}}},
		{"remove unless required, empty, by group", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", grp("0010"), unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY))}}},
		{"remove unless required, uid, on a sequence", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("SourceImageSequence"), unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID))}}},
		{"remove unless required, uid, on a UID", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("FrameOfReferenceUID"), unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID))}}},
		{"clean a sequence", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("ContentSequence"), clean())}}},
		{"clean overlay data", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", tg("60003000"), clean())}}},
		{"content item of a value type", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", itemOf("DCM", "121008", "DATE"), shift())}}},
		{"template into a long string", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("PatientID"), replaceTemplate("SUBJ-{#PatientID}-{#IssuerOfPatientID}"))}}},
		{"template into a code string", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("BodyPartExamined"), replaceTemplate("{#PatientID}"))}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, problems := profile.Expand(c.doc, cat); len(problems) > 0 {
				t.Errorf("refused: %v", problems)
			}
		})
	}
}

// TestInvalidDocuments guards that each kind of mistake is refused, and
// that the problem names the rule by its position and name, the field by
// its path, and a fixed code.
func TestInvalidDocuments(t *testing.T) {
	cat := testCatalog(t)
	one := func(r *v1.ProfileRule) *v1.ProfileDocument { return &v1.ProfileDocument{Rules: []*v1.ProfileRule{r}} }
	pix := func(p *v1.PixelRule) *v1.ProfileDocument { return &v1.ProfileDocument{PixelRules: []*v1.PixelRule{p}} }
	redact := func(rs ...*v1.PixelRectangle) *v1.PixelRule_Redact {
		return &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: rs}}
	}
	many := &v1.ProfileDocument{}
	for range profile.MaxRules + 1 {
		many.Rules = append(many.Rules, rl("", kw("PatientName"), remove()))
	}
	longPath := strings.Repeat("ReferencedStudySequence[*]/", profile.MaxPathSteps) + "ReferencedSOPInstanceUID"

	for _, c := range []struct {
		name      string
		doc       *v1.ProfileDocument
		rule, pix int
		ruleName  string
		field     string
		code      profile.Code
	}{
		// Selectors.
		{"no selector", one(rl("r", nil, remove())), 1, 0, "r", "selector", profile.CodeRequired},
		{"unknown keyword", one(rl("r", kw("PatientNmae"), remove())), 1, 0, "r", "selector.keyword", profile.CodeUnknownKeyword},
		{"short tag", one(rl("", tg("0010001"), remove())), 1, 0, "", "selector.tag", profile.CodeBadTag},
		{"tag in parentheses", one(rl("", tg("(0010,0010)"), remove())), 1, 0, "", "selector.tag", profile.CodeBadTag},
		{"private tag", one(rl("", tg("00091010"), remove())), 1, 0, "", "selector.tag", profile.CodePrivateTag},
		{"file meta", one(rl("", tg("00020010"), remove())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"group length", one(rl("", tg("00100000"), remove())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"pixel data", one(rl("", kw("PixelData"), remove())), 1, 0, "", "selector.keyword", profile.CodeReservedElement},
		{"every group", one(rl("", grp("xxxx"), remove())), 1, 0, "", "selector.group", profile.CodeBadGroup},
		{"group reaching file meta", one(rl("", grp("00x2"), remove())), 1, 0, "", "selector.group", profile.CodeReservedElement},
		{"group not hex", one(rl("", grp("00g0"), remove())), 1, 0, "", "selector.group", profile.CodeBadGroup},
		{"unknown vr", one(rl("", vr("XX"), remove())), 1, 0, "", "selector.vr", profile.CodeBadVR},
		{"vr in lower case", one(rl("", vr("pn"), remove())), 1, 0, "", "selector.vr", profile.CodeBadVR},
		{"private even group", one(rl("", priv("ACME", "0010", "10"), remove())), 1, 0, "", "selector.privateElement.group", profile.CodeBadPrivate},
		{"private reserved group", one(rl("", priv("ACME", "0007", "10"), remove())), 1, 0, "", "selector.privateElement.group", profile.CodeBadPrivate},
		{"private creator padded", one(rl("", priv("ACME ", "0019", "10"), remove())), 1, 0, "", "selector.privateElement.creator", profile.CodeBadPrivate},
		{"private no creator", one(rl("", priv("", "0019", "10"), remove())), 1, 0, "", "selector.privateElement.creator", profile.CodeRequired},
		{"private element too long", one(rl("", priv("ACME", "0019", "100"), remove())), 1, 0, "", "selector.privateElement.element", profile.CodeBadPrivate},
		{"path without items", one(rl("", path("ReferencedStudySequence/ReferencedSOPInstanceUID"), uid())), 1, 0, "", "selector.path", profile.CodeBadPath},
		{"path ending in items", one(rl("", path("ReferencedStudySequence[*]"), remove())), 1, 0, "", "selector.path", profile.CodeBadPath},
		{"path through a non-sequence", one(rl("", path("PatientName[*]/PatientID"), remove())), 1, 0, "", "selector.path", profile.CodeBadPath},
		{"path ending in any depth", one(rl("", path("ReferencedStudySequence[*]/**"), remove())), 1, 0, "", "selector.path", profile.CodeBadPath},
		{"path any depth twice", one(rl("", path("**/**/PatientName"), remove())), 1, 0, "", "selector.path", profile.CodeBadPath},
		{"path that is a tag", one(rl("", path("**/PatientName"), remove())), 1, 0, "", "selector.path", profile.CodePathIsTag},
		{"path too long", one(rl("", path(longPath), uid())), 1, 0, "", "selector.path", profile.CodeTooMany},
		{"path through private", one(rl("", path("00091010[*]/PatientName"), remove())), 1, 0, "", "selector.path", profile.CodePrivateTag},
		{"empty path", one(rl("", path(""), remove())), 1, 0, "", "selector.path", profile.CodeRequired},
		{"content item without value", one(rl("", item("DCM", ""), remove())), 1, 0, "", "selector.contentItem.value", profile.CodeRequired},
		{"content item of an unknown value type", one(rl("", itemOf("DCM", "121008", "NAME"), remove())), 1, 0, "", "selector.contentItem.valueType", profile.CodeBadValueType},
		{"content item without value type hashed", one(rl("", item("DCM", "121008"), hash())), 1, 0, "", "action.hash", profile.CodeActionVR},
		{"content item of another VR", one(rl("", itemOf("DCM", "121008", "PNAME"), shift())), 1, 0, "", "action.shift", profile.CodeActionVR},
		{"private group without opting in", one(rl("", grp("0029"), keep())), 1, 0, "", "selector.group", profile.CodeIncludePrivate},
		{"include private on a tag", one(rl("", &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientName"}, IncludePrivate: true}, keep())), 1, 0, "", "selector.includePrivate", profile.CodeIncludePrivate},
		{"include private on a private element", one(rl("", &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{Creator: "ACME", Group: "0019", Element: "10"}}, IncludePrivate: true}, keep())), 1, 0, "", "selector.includePrivate", profile.CodeIncludePrivate},
		{"original attributes", one(rl("", kw("OriginalAttributesSequence"), keep())), 1, 0, "", "selector.keyword", profile.CodeReservedElement},
		{"encrypted attributes", one(rl("", tg("04000500"), keep())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"digital signatures", one(rl("", tg("FFFAFFFA"), keep())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"trailing padding", one(rl("", tg("FFFCFFFC"), keep())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"MAC parameters", one(rl("", tg("4FFE0001"), keep())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"extended offset table", one(rl("", tg("7FE00001"), remove())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"directory element", one(rl("", tg("00041511"), remove())), 1, 0, "", "selector.tag", profile.CodeReservedElement},
		{"pixel group", one(rl("", grp("7FE0"), remove())), 1, 0, "", "selector.group", profile.CodeReservedElement},
		{"signature group", one(rl("", grp("FFFA"), remove())), 1, 0, "", "selector.group", profile.CodeReservedElement},
		{"group reaching the directory", one(rl("", grp("x004"), remove())), 1, 0, "", "selector.group", profile.CodeReservedElement},

		// Actions.
		{"no action", one(rl("r", kw("PatientName"), nil)), 1, 0, "r", "action", profile.CodeRequired},
		{"uid on a name", one(rl("", kw("PatientName"), uid())), 1, 0, "", "action.uid", profile.CodeActionVR},
		{"shift on a name", one(rl("", kw("PatientName"), shift())), 1, 0, "", "action.shift", profile.CodeActionVR},
		{"hash on a date", one(rl("", kw("StudyDate"), hash())), 1, 0, "", "action.hash", profile.CodeActionVR},
		{"generalize a name", one(rl("", kw("PatientName"), generalize())), 1, 0, "", "action.generalize", profile.CodeActionVR},
		{"clean a name", one(rl("", kw("PatientName"), clean())), 1, 0, "", "action.clean", profile.CodeActionVR},
		{"hash on a group", one(rl("", grp("0010"), hash())), 1, 0, "", "action.hash", profile.CodeActionVR},
		{"hash on a private element", one(rl("", priv("ACME", "0019", "10"), hash())), 1, 0, "", "action.hash", profile.CodeActionVR},
		{"hash on either of two VRs", one(rl("", kw("SmallestImagePixelValue"), hash())), 1, 0, "", "action.hash", profile.CodeActionVR},
		{"set by vr", one(rl("", vr("PN"), set("X"))), 1, 0, "", "action.set", profile.CodeActionSelector},
		{"set by path", one(rl("", path("PatientName"), set("X"))), 1, 0, "", "action.set", profile.CodeActionSelector},
		{"set on a content item", one(rl("", item("DCM", "121008"), set("X"))), 1, 0, "", "action.set", profile.CodeActionSelector},
		{"replace without value", one(rl("", kw("PatientName"), &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{}}})), 1, 0, "", "action.replace", profile.CodeOneOf},
		{"bad date constant", one(rl("", kw("StudyDate"), replace("2020"))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"constant too long for its vr", one(rl("", kw("AccessionNumber"), replace(strings.Repeat("9", 17)))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"constant in a binary element", one(rl("", kw("Rows"), replace("1"))), 1, 0, "", "action.replace.constant", profile.CodeActionVR},
		{"template reading as received", one(rl("", kw("PatientComments"), replaceTemplate("{PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeBadTemplate},
		{"template lone brace", one(rl("", kw("PatientComments"), replaceTemplate("a}b"))), 1, 0, "", "action.replace.template", profile.CodeBadTemplate},
		{"template open brace", one(rl("", kw("PatientComments"), replaceTemplate("a{#PatientID"))), 1, 0, "", "action.replace.template", profile.CodeBadTemplate},
		{"template of a number", one(rl("", kw("PatientComments"), replaceTemplate("{#Rows}"))), 1, 0, "", "action.replace.template", profile.CodeBadTemplate},
		{"template unknown keyword", one(rl("", kw("PatientComments"), replaceTemplate("{#Nobody}"))), 1, 0, "", "action.replace.template", profile.CodeUnknownKeyword},
		{"template too many", one(rl("", kw("PatientComments"), replaceTemplate(strings.Repeat("{#PatientID}", profile.MaxPlaceholders+1)))), 1, 0, "", "action.replace.template", profile.CodeTooMany},
		{"template into a date", one(rl("", kw("StudyDate"), replaceTemplate("{#PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeActionVR},
		{"remove unless required without fallback", one(rl("", kw("PatientName"), unlessRequired(0))), 1, 0, "", "action.removeUnlessRequired.fallback", profile.CodeRequired},
		{"remove unless required unknown fallback", one(rl("", kw("PatientName"), unlessRequired(9))), 1, 0, "", "action.removeUnlessRequired.fallback", profile.CodeBadEnum},
		{"remove unless required, uid, on a name", one(rl("", kw("PatientName"), unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID))), 1, 0, "", "action.removeUnlessRequired.fallback", profile.CodeActionVR},
		{"remove unless required, uid, by group", one(rl("", grp("0008"), unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID))), 1, 0, "", "action.removeUnlessRequired.fallback", profile.CodeActionVR},
		{"clean a number", one(rl("", kw("Rows"), clean())), 1, 0, "", "action.clean", profile.CodeActionVR},
		{"constant with a backslash", one(rl("", kw("PatientName"), replace("A\\B"))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"constant with a control character", one(rl("", kw("StudyDescription"), replace("A\nB"))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"constant outside ASCII", one(rl("", kw("StudyDescription"), replace("Anonymis\u00e9"))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"constant in lower case for a code string", one(rl("", kw("BodyPartExamined"), replace("chest"))), 1, 0, "", "action.replace.constant", profile.CodeBadValue},
		{"template longer than its VR", one(rl("", kw("AccessionNumber"), replaceTemplate("{#PatientID}{#PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeBadValue},
		{"template with one placeholder past its VR", one(rl("", kw("StationName"), replaceTemplate("S{#PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeBadValue},
		{"template in lower case for a code string", one(rl("", kw("BodyPartExamined"), replaceTemplate("x{#PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeBadValue},
		{"template with a backslash", one(rl("", kw("PatientComments"), replaceTemplate("a\\{#PatientID}"))), 1, 0, "", "action.replace.template", profile.CodeBadValue},
		{"set constant outside ASCII", one(rl("", kw("DeidentificationMethod"), set("\u00e9"))), 1, 0, "", "action.set.constant", profile.CodeBadValue},
		{"lookup bad table", one(rl("", kw("PatientID"), lookup("Subjects", 0))), 1, 0, "", "action.lookup.table", profile.CodeBadName},
		{"lookup unknown policy", one(rl("", kw("PatientID"), lookup("subjects", 9))), 1, 0, "", "action.lookup.onMissing", profile.CodeBadEnum},

		// Conditions.
		{"condition without subject", one(rl("", kw("PatientName"), remove(), &v1.ProfileCondition{Test: &v1.ProfileCondition_Equals{Equals: "CT"}})), 1, 0, "", "when[1].subject", profile.CodeBadEnum},
		{"condition without test", one(rl("", kw("PatientName"), remove(), equals(modality, "CT"), &v1.ProfileCondition{Subject: modality})), 1, 0, "", "when[2]", profile.CodeOneOf},
		{"condition bad pattern", one(rl("", kw("PatientName"), remove(), pattern(modality, "("))), 1, 0, "", "when[1].pattern", profile.CodeBadPattern},
		{"condition escaping its anchors", one(rl("", kw("PatientName"), remove(), pattern(modality, "a)|(b"))), 1, 0, "", "when[1].pattern", profile.CodeBadPattern},
		{"condition empty prefix", one(rl("", kw("PatientName"), remove(), &v1.ProfileCondition{Subject: modality, Test: &v1.ProfileCondition_Prefix{}})), 1, 0, "", "when[1].prefix", profile.CodeRequired},

		// Rules.
		{"quarantine without name", one(rl("", kw("PatientName"), quarantine())), 1, 0, "", "name", profile.CodeRequired},
		{"bad rule name", one(rl("Upper", kw("PatientName"), remove())), 1, 0, "Upper", "name", profile.CodeBadName},
		{"duplicate rule name", &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("r", kw("PatientName"), remove()), rl("r", kw("PatientID"), remove())}}, 2, 0, "r", "name", profile.CodeDuplicateName},
		{"reason too long", one(&v1.ProfileRule{Selector: kw("PatientName"), Action: remove(), Reason: strings.Repeat("x", profile.MaxReasonLength+1)}), 1, 0, "", "reason", profile.CodeTooLong},

		// The document.
		{"too many rules", many, 0, 0, "", "rules", profile.CodeTooMany},
		{"shift too large", &v1.ProfileDocument{DateShiftDays: profile.MaxDateShiftDays + 1}, 0, 0, "", "dateShiftDays", profile.CodeOutOfRange},
		{"negative shift", &v1.ProfileDocument{DateShiftDays: -1}, 0, 0, "", "dateShiftDays", profile.CodeOutOfRange},
		{"unknown unnamed", &v1.ProfileDocument{Unnamed: 7}, 0, 0, "", "unnamed", profile.CodeBadEnum},
		{"description too long", &v1.ProfileDocument{Description: strings.Repeat("x", profile.MaxDescriptionLength+1)}, 0, 0, "", "description", profile.CodeTooLong},
		{"unknown base", &v1.ProfileDocument{Bases: []string{"test/nothing:1"}}, 0, 0, "", "bases[1]", profile.CodeUnknownBase},
		{"unavailable base", &v1.ProfileDocument{Bases: []string{baseGone}}, 0, 0, "", "bases[1]", profile.CodeBaseUnavailable},
		{"base on an unavailable base", &v1.ProfileDocument{Bases: []string{baseOnGone}}, 0, 0, "", "bases[1]", profile.CodeBaseUnavailable},
		{"base twice", &v1.ProfileDocument{Bases: []string{baseA, baseA}}, 0, 0, "", "bases[2]", profile.CodeDuplicateBase},

		// Pixel rules.
		{"pixel rule without name", pix(&v1.PixelRule{Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}}), 0, 1, "", "name", profile.CodeRequired},
		{"pixel rule without action", pix(&v1.PixelRule{Name: "p"}), 0, 1, "p", "", profile.CodeOneOf},
		{"accept without reason", pix(&v1.PixelRule{Name: "p", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}}), 0, 1, "p", "reason", profile.CodeRequired},
		{"redact without rectangles", pix(&v1.PixelRule{Name: "p", Action: redact()}), 0, 1, "p", "redact.rectangles", profile.CodeRequired},
		{"rectangle past the image", pix(&v1.PixelRule{Name: "p", Columns: 100, Action: redact(&v1.PixelRectangle{X: 50, Width: 51, Height: 1})}), 0, 1, "p", "redact.rectangles[1]", profile.CodeBadRectangle},
		{"empty rectangle", pix(&v1.PixelRule{Name: "p", Action: redact(&v1.PixelRectangle{Width: 0, Height: 1})}), 0, 1, "p", "redact.rectangles[1]", profile.CodeBadRectangle},
		{"negative rows", pix(&v1.PixelRule{Name: "p", Rows: -1, Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}}), 0, 1, "p", "rows", profile.CodeOutOfRange},
		{"pixel condition without test", pix(&v1.PixelRule{Name: "p", When: []*v1.ProfileCondition{{Subject: modality}}, Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}}), 0, 1, "p", "when[1]", profile.CodeOneOf},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, problems := profile.Expand(c.doc, cat)
			if e != nil {
				t.Error("an expanded form was answered with problems")
			}
			if len(problems) != 1 {
				t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
			}
			p := problems[0]
			if p.Rule != c.rule || p.PixelRule != c.pix || p.RuleName != c.ruleName || p.Field != c.field || p.Code != c.code {
				t.Errorf("got rule %d, pixel rule %d, name %q, field %q, code %s (%s); want rule %d, pixel rule %d, name %q, field %q, code %s",
					p.Rule, p.PixelRule, p.RuleName, p.Field, p.Code, p.Message, c.rule, c.pix, c.ruleName, c.field, c.code)
			}
			if p.Message == "" {
				t.Error("the problem has no message")
			}
		})
	}
}

// TestEveryProblemAtOnce guards that a document with several mistakes is
// answered with all of them, in the order of the document, so a person
// fixes a file in one pass.
func TestEveryProblemAtOnce(t *testing.T) {
	doc := &v1.ProfileDocument{
		Bases:         []string{"test/nothing:1"},
		DateShiftDays: -5,
		Rules: []*v1.ProfileRule{
			rl("ok", kw("PatientName"), remove()),
			rl("bad-kw", kw("Nobody"), remove()),
			rl("bad-action", kw("PatientName"), uid()),
		},
	}
	_, problems := profile.Expand(doc, testCatalog(t))
	var got []string
	for _, p := range problems {
		got = append(got, string(p.Code))
	}
	want := []string{"unknown_base", "out_of_range", "unknown_keyword", "action_vr"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("codes %v, want %v", got, want)
	}
	if msg := problems.Error(); !strings.HasPrefix(msg, "profile: document, bases[1]: unknown_base: ") || !strings.HasSuffix(msg, "(and 3 more)") {
		t.Errorf("the error does not name the first problem and the count: %v", msg)
	}
	if got := problems[2].Proto(); got.GetRule() != 2 || got.GetRuleName() != "bad-kw" || got.GetField() != "selector.keyword" || got.GetCode() != "unknown_keyword" {
		t.Errorf("the problem as the API answers it: %v", got)
	}
}

// TestNoDocument guards that a missing document is a problem, not a
// panic.
func TestNoDocument(t *testing.T) {
	if _, problems := profile.Expand(nil, testCatalog(t)); len(problems) != 1 || problems[0].Code != profile.CodeRequired {
		t.Errorf("problems %v", problems)
	}
}
