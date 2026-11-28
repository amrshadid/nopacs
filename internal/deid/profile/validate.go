package profile

import (
	"regexp"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Bounds of a document. They keep a document within the API's message
// limit and a version's work per element bounded.
const (
	// MaxBases is the most bases a document names.
	MaxBases = 16
	// MaxRules is the most rules of a document.
	MaxRules = 1000
	// MaxPixelRules is the most pixel rules of a document.
	MaxPixelRules = 200
	// MaxExpandedRules is the most rules of an expanded profile, its bases'
	// included.
	MaxExpandedRules = 10000
	// MaxRectangles is the most rectangles of a pixel rule.
	MaxRectangles = 64
	// MaxReasonLength is the bound of a rule's reason.
	MaxReasonLength = 1024
	// MaxDescriptionLength is the bound of a document's description.
	MaxDescriptionLength = 4096
	// DefaultDateShiftDays is the largest date shift when no document of a
	// profile says.
	DefaultDateShiftDays = 365
	// MaxDateShiftDays is the largest date shift a document may give.
	MaxDateShiftDays = 3650
	// maxImageSide is the bound of a rectangle's side and of an image's
	// rows and columns, which are US values (PS3.3 C.7.6.3).
	maxImageSide = 65535
)

// ruleName is the rule of a rule's name.
var ruleName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// rule is a compiled rule of an expanded profile.
type rule struct {
	sel    selector
	when   []condition
	action *v1.ProfileAction
	// redact is a redact action's pattern, compiled for a search.
	redact *regexp.Regexp
	// lists are the bits of the text lists a clean or redact names.
	lists  uint32
	norm   *v1.ProfileRule
	origin string
	// cost is the compiled size of its patterns: its conditions', its
	// value pattern's and its redact's.
	cost int
}

// pixelRule is a compiled pixel rule.
type pixelRule struct {
	cost       int
	when       []condition
	rows, cols int32
	norm       *v1.PixelRule
	origin     string
}

// checkDocument checks every field of a document but its bases, and
// compiles its rules. The rules are nil when there is a problem.
func checkDocument(c *checker, doc *v1.ProfileDocument) ([]rule, []pixelRule) {
	before := len(c.list)
	top := at{c: c}
	if len(doc.GetDescription()) > MaxDescriptionLength {
		top.in("description").add(CodeTooLong, "a description is at most %d bytes", MaxDescriptionLength)
	}
	if d := doc.GetDateShiftDays(); d < 0 || d > MaxDateShiftDays {
		top.in("dateShiftDays").add(CodeOutOfRange, "the largest date shift is 1 to %d days, or 0 to take it from the bases", MaxDateShiftDays)
	}
	if _, known := v1.UnnamedElements_name[int32(doc.GetUnnamed())]; !known {
		top.in("unnamed").add(CodeBadEnum, "%d is not a value of UnnamedElements", doc.GetUnnamed())
	}
	if _, known := v1.UnsearchedSegments_name[int32(doc.GetUnsearchedSegments())]; !known {
		top.in("unsearchedSegments").add(CodeBadEnum, "%d is not a value of UnsearchedSegments", doc.GetUnsearchedSegments())
	}
	useLists(c, top, doc.GetTextLists())
	var rules []rule
	if len(doc.GetRules()) > MaxRules {
		top.in("rules").add(CodeTooMany, "a document has at most %d rules", MaxRules)
	} else {
		names := map[string]int{}
		for i, r := range doc.GetRules() {
			if c, ok := checkRule(at{c: c, rule: i + 1, name: r.GetName()}, r, names); ok {
				rules = append(rules, c)
			}
		}
	}
	var pixels []pixelRule
	if len(doc.GetPixelRules()) > MaxPixelRules {
		top.in("pixelRules").add(CodeTooMany, "a document has at most %d pixel rules", MaxPixelRules)
	} else {
		names := map[string]int{}
		for i, r := range doc.GetPixelRules() {
			if c, ok := checkPixelRule(at{c: c, pixelRule: i + 1, name: r.GetName()}, r, names); ok {
				pixels = append(pixels, c)
			}
		}
	}
	if len(c.list) != before {
		return nil, nil
	}
	return rules, pixels
}

// checkName checks a rule's name and that no earlier rule of the list has
// it; names maps each name to the position that has it.
func checkName(a at, name string, required bool, names map[string]int, pos int) {
	switch {
	case name == "" && required:
		a.in("name").add(CodeRequired, "this rule needs a name")
	case name == "":
	case !ruleName.MatchString(name):
		a.in("name").add(CodeBadName, "a rule's name is 1 to 64 of a-z, 0-9, \".\", \"_\" and \"-\", starting with a letter or a digit")
	case names[name] != 0:
		a.in("name").add(CodeDuplicateName, "rule %d already has this name", names[name])
	default:
		names[name] = pos
	}
}

// checkRule checks and compiles one rule.
func checkRule(a at, r *v1.ProfileRule, names map[string]int) (rule, bool) {
	before := len(a.c.list)
	costBefore := a.c.cost
	_, quarantines := r.GetAction().GetKind().(*v1.ProfileAction_Quarantine)
	checkName(a, r.GetName(), quarantines, names, a.rule)
	if len(r.GetReason()) > MaxReasonLength {
		a.in("reason").add(CodeTooLong, "a reason is at most %d bytes", MaxReasonLength)
	}
	sel, normSel, ok := parseSelector(a.in("selector"), r.GetSelector(), nil)
	when := parseConditions(a, "when", r.GetWhen())
	var action *v1.ProfileAction
	var redact *regexp.Regexp
	var lists uint32
	if ok {
		action, redact, lists = checkAction(a, r.GetAction(), sel)
	} else if r.GetAction().GetKind() == nil {
		a.in("action").add(CodeRequired, "a rule needs an action")
	}
	if len(a.c.list) != before {
		return rule{}, false
	}
	norm, _ := proto.Clone(r).(*v1.ProfileRule)
	norm.Selector = normSel
	norm.Action = action
	return rule{sel: sel, when: when, action: action, redact: redact, lists: lists, norm: norm, cost: a.c.cost - costBefore}, true
}

// checkPixelRule checks and compiles one pixel rule.
func checkPixelRule(a at, r *v1.PixelRule, names map[string]int) (pixelRule, bool) {
	before := len(a.c.list)
	costBefore := a.c.cost
	checkName(a, r.GetName(), true, names, a.pixelRule)
	if len(r.GetReason()) > MaxReasonLength {
		a.in("reason").add(CodeTooLong, "a reason is at most %d bytes", MaxReasonLength)
	}
	when := parseConditions(a, "when", r.GetWhen())
	if r.GetRows() < 0 || r.GetRows() > maxImageSide {
		a.in("rows").add(CodeOutOfRange, "rows is 0 for any, or 1 to %d", maxImageSide)
	}
	if r.GetColumns() < 0 || r.GetColumns() > maxImageSide {
		a.in("columns").add(CodeOutOfRange, "columns is 0 for any, or 1 to %d", maxImageSide)
	}
	switch k := r.GetAction().(type) {
	case *v1.PixelRule_Redact:
		checkRectangles(a.in("redact"), k.Redact.GetRectangles(), r.GetRows(), r.GetColumns())
	case *v1.PixelRule_Detect:
	case *v1.PixelRule_RedactByTemplate:
	case *v1.PixelRule_Accept:
		if r.GetReason() == "" {
			a.in("reason").add(CodeRequired, "accepting an image whose pixels may carry text needs a reason")
		}
	default:
		a.add(CodeOneOf, "a pixel rule needs one action: redact, redact_by_template, detect or accept")
	}
	if len(a.c.list) != before {
		return pixelRule{}, false
	}
	norm, _ := proto.Clone(r).(*v1.PixelRule)
	return pixelRule{when: when, rows: r.GetRows(), cols: r.GetColumns(), norm: norm, cost: a.c.cost - costBefore}, true
}

// checkRectangles checks the rectangles of a redaction against the image
// size the rule names, when it names one.
func checkRectangles(a at, rs []*v1.PixelRectangle, rows, cols int32) {
	switch {
	case len(rs) == 0:
		a.in("rectangles").add(CodeRequired, "a redaction needs at least one rectangle")
		return
	case len(rs) > MaxRectangles:
		a.in("rectangles").add(CodeTooMany, "a redaction has at most %d rectangles", MaxRectangles)
		return
	}
	for i, r := range rs {
		ra := a.index("rectangles", i)
		switch {
		case r.GetX() < 0 || r.GetY() < 0:
			ra.add(CodeBadRectangle, "x and y are at least 0")
		case r.GetWidth() < 1 || r.GetHeight() < 1 || r.GetWidth() > maxImageSide || r.GetHeight() > maxImageSide:
			ra.add(CodeBadRectangle, "width and height are 1 to %d", maxImageSide)
		case cols > 0 && int64(r.GetX())+int64(r.GetWidth()) > int64(cols):
			ra.add(CodeBadRectangle, "the rectangle reaches past the image's %d columns", cols)
		case rows > 0 && int64(r.GetY())+int64(r.GetHeight()) > int64(rows):
			ra.add(CodeBadRectangle, "the rectangle reaches past the image's %d rows", rows)
		}
	}
}
