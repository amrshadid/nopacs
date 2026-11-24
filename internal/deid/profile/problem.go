package profile

import (
	"fmt"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Code is what is wrong with a document, as a fixed word a program can test.
// The words are part of the API's contract (ProfileProblem.code): one is
// never renamed or given a new meaning, and new ones may be added.
type Code string

// The codes of a problem.
const (
	// CodeRequired: a field that must be given is missing.
	CodeRequired Code = "required"
	// CodeTooLong: a text is longer than its bound.
	CodeTooLong Code = "too_long"
	// CodeTooMany: a list has more entries than its bound.
	CodeTooMany Code = "too_many"
	// CodeOutOfRange: a number is outside its bounds.
	CodeOutOfRange Code = "out_of_range"
	// CodeBadName: a name does not follow its rule.
	CodeBadName Code = "bad_name"
	// CodeDuplicateName: two rules of one list share a name.
	CodeDuplicateName Code = "duplicate_name"
	// CodeUnknownBase: no base has this name.
	CodeUnknownBase Code = "unknown_base"
	// CodeBaseUnavailable: the base is known but this build cannot expand it.
	CodeBaseUnavailable Code = "base_unavailable"
	// CodeDuplicateBase: a document names one base twice.
	CodeDuplicateBase Code = "duplicate_base"
	// CodeBadEnum: an enumerated field holds a value this version does not
	// know, or the unspecified value where one is needed.
	CodeBadEnum Code = "bad_enum"
	// CodeBadTag: a tag is not eight hexadecimal digits.
	CodeBadTag Code = "bad_tag"
	// CodeUnknownKeyword: no element of the data dictionary has this keyword.
	CodeUnknownKeyword Code = "unknown_keyword"
	// CodeBadGroup: a group is not four hexadecimal digits or "x".
	CodeBadGroup Code = "bad_group"
	// CodeBadVR: a VR is not one of PS3.5 6.2.
	CodeBadVR Code = "bad_vr"
	// CodeBadPrivate: a private element's creator, group or element is
	// malformed.
	CodeBadPrivate Code = "bad_private"
	// CodeBadPath: a path does not follow its grammar.
	CodeBadPath Code = "bad_path"
	// CodePathIsTag: a path that only says what a tag selector says.
	CodePathIsTag Code = "path_is_tag"
	// CodeBadCode: a coded concept's scheme or value is malformed.
	CodeBadCode Code = "bad_code"
	// CodeReservedElement: the selector names what no rule may change: the
	// file meta information, an item or delimiter, a group length, or Pixel
	// Data.
	CodeReservedElement Code = "reserved_element"
	// CodePrivateTag: a tag or keyword selector names a private element.
	CodePrivateTag Code = "private_tag"
	// CodeBadPattern: a regular expression does not compile, or is too long.
	CodeBadPattern Code = "bad_pattern"
	// CodeOneOf: none, or more than one, of the fields of a choice is set.
	CodeOneOf Code = "one_of"
	// CodeActionVR: the action does not fit the VR the selector selects.
	CodeActionVR Code = "action_vr"
	// CodeActionSelector: the action does not fit the kind of selector.
	CodeActionSelector Code = "action_selector"
	// CodeBadValue: a constant is not a valid value of the element's VR.
	CodeBadValue Code = "bad_value"
	// CodeBadTemplate: a template does not follow its grammar.
	CodeBadTemplate Code = "bad_template"
	// CodeBadRectangle: a pixel rectangle is empty or outside the image.
	CodeBadRectangle Code = "bad_rectangle"
	// CodeIncludePrivate: include_private on a selector that is not a
	// group or a VR, or an odd group without it, which would select
	// nothing.
	CodeIncludePrivate Code = "include_private"
	// CodeBadValueType: a content item's value type is not one of PS3.3
	// C.17.3.
	CodeBadValueType Code = "bad_value_type"
	// CodeTooLarge: the document and its expanded form together are larger
	// than one answer may carry.
	CodeTooLarge Code = "too_large"
	// CodeBadEdition: a stored expanded form is of a language edition this
	// build does not know.
	CodeBadEdition Code = "bad_edition"
)

// The codes of a warning: what a valid document does that its author may
// not mean. Like the codes of a problem, part of the API's contract.
const (
	// CodeShadowedByBase: a more specific rule of a base decides some of
	// the elements a rule of the document selects, with another action.
	CodeShadowedByBase Code = "shadowed_by_base"
	// CodeKeepsBaseChange: a rule of the document keeps elements a base
	// changes or removes.
	CodeKeepsBaseChange Code = "keeps_base_change"
	// CodeMoreWarnings: the warnings stop at their bound; this one says how
	// many more there are.
	CodeMoreWarnings Code = "more_warnings"
)

// Problem is one thing wrong with a document: where it is, a fixed code and
// a sentence. Rule and PixelRule are positions from 1, zero when the
// problem is not in such a rule; Field is a path in the JSON names of the
// document or of the rule, positions in it from 1.
type Problem struct {
	// Rule is the rule's position among the document's rules, from 1.
	Rule int
	// PixelRule is the pixel rule's position, from 1.
	PixelRule int
	// RuleName is the rule's name, when it has one.
	RuleName string
	// Field is the field, such as "selector.keyword" or "bases[2]".
	Field string
	// Code is what is wrong, as a fixed word.
	Code Code
	// Message is what is wrong, for a person.
	Message string
}

// String writes the problem in one line: where, the code, the sentence.
func (p Problem) String() string {
	var b strings.Builder
	switch {
	case p.Rule > 0:
		b.WriteString("rule " + strconv.Itoa(p.Rule))
	case p.PixelRule > 0:
		b.WriteString("pixel rule " + strconv.Itoa(p.PixelRule))
	default:
		b.WriteString("document")
	}
	if p.RuleName != "" {
		b.WriteString(" (" + p.RuleName + ")")
	}
	if p.Field != "" {
		b.WriteString(", " + p.Field)
	}
	fmt.Fprintf(&b, ": %s: %s", p.Code, p.Message)
	return b.String()
}

// Proto returns the problem as the API answers it.
func (p Problem) Proto() *v1.ProfileProblem {
	return &v1.ProfileProblem{
		Rule:      int32(p.Rule),      //nolint:gosec // G115: a position under MaxRules.
		PixelRule: int32(p.PixelRule), //nolint:gosec // G115: a position under MaxPixelRules.
		RuleName:  p.RuleName,
		Field:     p.Field,
		Code:      string(p.Code),
		Message:   p.Message,
	}
}

// Problems is every problem of a document, in the order of the document. A
// non-empty Problems is the error a document with problems is refused
// with.
type Problems []Problem

// Error names the first problem and how many more there are.
func (ps Problems) Error() string {
	if len(ps) == 0 {
		return "profile: no problem"
	}
	s := "profile: " + ps[0].String()
	if len(ps) > 1 {
		s += fmt.Sprintf(" (and %d more)", len(ps)-1)
	}
	return s
}

// checker is one check of a document, or of a stored expanded form: the
// problems found, the regular expressions compiled so far (each distinct
// text once), and their cost against the budget of one document.
type checker struct {
	list Problems
	// stored is set when the check reads a stored expanded form: it checks
	// structure only and never reads the data dictionary, so a later
	// library cannot change what a stored version does or refuse it.
	stored   bool
	patterns map[string]compiledPattern
	cost     int
	// overBudget is set once the budget's problem is recorded, so it is
	// recorded once.
	overBudget bool
}

// newChecker returns a checker for a document, or for a stored expanded
// form with stored set.
func newChecker(stored bool) *checker {
	return &checker{stored: stored, patterns: map[string]compiledPattern{}}
}

// at is where a check is: which rule, and the field prefix within it. The
// checks append to its checker's list.
type at struct {
	c         *checker
	rule      int
	pixelRule int
	name      string
	field     string
}

// in returns a place one field deeper.
func (a at) in(field string) at {
	if a.field != "" {
		field = a.field + "." + field
	}
	a.field = field
	return a
}

// index returns a place at a position, from 1, of a list field.
func (a at) index(field string, i int) at {
	return a.in(field + "[" + strconv.Itoa(i+1) + "]")
}

// add records a problem here.
func (a at) add(code Code, format string, args ...any) {
	a.c.list = append(a.c.list, Problem{
		Rule:      a.rule,
		PixelRule: a.pixelRule,
		RuleName:  a.name,
		Field:     a.field,
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
	})
}
