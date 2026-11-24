package engine

import (
	"fmt"
	"regexp"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/repattern"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// ActionKind is what the engine does to an element.
type ActionKind int

// The actions. Each is one function of the engine (actions.go).
const (
	// ActKeep keeps the element; a sequence's items are walked.
	ActKeep ActionKind = iota + 1
	// ActRemove removes the element.
	ActRemove
	// ActEmpty leaves it with no value; a sequence with no items.
	ActEmpty
	// ActReplace replaces a value present with Constant or Template.
	ActReplace
	// ActSet is ActReplace, and makes the element at the top level when
	// absent.
	ActSet
	// ActHash replaces a value with its keyed hash.
	ActHash
	// ActUID remaps every UID of a value (RemapUID).
	ActUID
	// ActShift moves dates back by the subject's shift.
	ActShift
	// ActGeneralize makes an age over 89 years 090Y and a date the first
	// of January of its year.
	ActGeneralize
	// ActLookup replaces a value with the code of Table.
	ActLookup
	// ActClean removes the file's identifying values from free text; a
	// sequence is kept and its items walked, and OB or OW is removed.
	ActClean
	// ActQuarantine refuses the whole file.
	ActQuarantine
	// ActDummy replaces a value with Dummy of its VR; a sequence with no
	// items.
	ActDummy
	// ActRemoveUnlessRequired removes the element unless the IOD of the
	// file's SOP class requires it: Type 2 empties it, Type 1 applies
	// Fallback (PS3.15 E.1.1's combined actions).
	ActRemoveUnlessRequired
	// ActRedact cuts the matches of Pattern out of a text value and keeps
	// the rest (redact.go).
	ActRedact
)

// Fallback is what ActRemoveUnlessRequired does to an element the IOD
// requires with a value (Type 1 or 1C).
type Fallback int

// The fallbacks.
const (
	// FallbackEmpty leaves it empty.
	FallbackEmpty Fallback = iota + 1
	// FallbackDummy gives it Dummy of its VR.
	FallbackDummy
	// FallbackUID remaps its UIDs; a sequence is kept and walked, its
	// UIDs remapped by their own rules.
	FallbackUID
)

// Action is an action with its arguments.
type Action struct {
	// Kind is the action.
	Kind ActionKind
	// Constant is ActReplace's or ActSet's constant.
	Constant string
	// Template is ActReplace's or ActSet's template in the expanded form,
	// placeholders written "{#GGGGEEEE}"; empty for a constant.
	Template string
	// Table is ActLookup's table.
	Table string
	// AssignMissing is ActLookup's policy for a value the table has no
	// code for: assign the next code, or (false) quarantine.
	AssignMissing bool
	// Fallback is ActRemoveUnlessRequired's.
	Fallback Fallback
	// Pattern is ActRedact's pattern, compiled for a search; Replacement
	// what each match becomes; PatternCost its compiled size
	// (repattern.Measure), which each pass over a value spends times the
	// value's length from the file's budget (maxPatternWork).
	Pattern     *regexp.Regexp
	Replacement string
	PatternCost int64
}

// Decision is what a profile does to one element.
type Decision struct {
	// Rule is the deciding rule's position in the expanded profile, from
	// 0; -1 for a default.
	Rule int
	// Name, Origin and Reason are the deciding rule's, or for a default
	// Reason says which default.
	Name, Origin, Reason string
	// Action is what is done.
	Action Action
	// VRs are the VRs the rule's selector selects, as the profile's checks
	// decided them; nil when it says nothing of the VR. An element whose
	// VR in the file is not one of them is refused when the action reads
	// or writes its value.
	VRs []string
	// PatternMatches are what the deciding rule's value pattern matched in
	// the value, each text once; nil when the deciding rule has none.
	PatternMatches []string
	// PatternOver reports more distinct matches than
	// profile.MaxRedactMatches; the file is refused.
	PatternOver bool
	// PatternUnread reports that a value pattern would have read a value
	// the engine could not give it (profile.Location.ValueUnread); the
	// file is refused.
	PatternUnread bool
}

// SetRule is a set rule: it makes the element Tag, of VR VR, at the top
// level when the file lacks it and the rule would decide it.
type SetRule struct {
	// Rule is its position in the expanded profile.
	Rule int
	// Tag and VR are the element it makes.
	Tag uint32
	VR  string
}

// Decider is all the engine needs of a compiled profile: the decision for
// an element, the set rules, the templates and the largest date shift. It
// is the one boundary between the engine and the profile language, so a
// change of the language's internals changes only its implementation
// (programDecider).
type Decider interface {
	// Decide returns the decision for the element at loc given the
	// file's facts.
	Decide(loc profile.Location, f profile.Facts) Decision
	// Sets returns the set rules, in the profile's order.
	Sets() []SetRule
	// Templates returns every template of the profile's rules.
	Templates() []string
	// DateShiftDays is the largest date shift, in days.
	DateShiftDays() int32
}

// programDecider is Decider over the language's compiled Program.
type programDecider struct {
	prog  *profile.Program
	rules []*v1.ExpandedRule
	sets  []SetRule
	tmpls []string
	// valueCost is the compiled size of every value pattern together,
	// what reading one byte of a text value may cost; redactCost each
	// redact rule's, by position.
	valueCost  int64
	redactCost map[int]int64
}

// newProgramDecider compiles an expanded profile into a Decider. A set
// rule must carry exactly one VR, the one its element is made with, as
// validation stores it.
func newProgramDecider(e *v1.ExpandedProfile) (*programDecider, error) {
	prog, err := profile.Compile(e)
	if err != nil {
		return nil, err
	}
	d := &programDecider{prog: prog, rules: e.GetRules(), redactCost: map[int]int64{}}
	for i, er := range d.rules {
		// The patterns compiled with the program, so they are within the
		// bounds and Measure cannot fail here.
		if vp := er.GetRule().GetSelector().GetValuePattern(); vp != nil {
			c, _ := repattern.Measure(vp.GetPattern())
			d.valueCost += int64(c)
		}
		if rd := er.GetRule().GetAction().GetRedact(); rd != nil {
			c, _ := repattern.Measure(rd.GetPattern())
			d.redactCost[i] = int64(c)
		}
		a := toAction(er.GetRule().GetAction())
		if a.Template != "" {
			d.tmpls = append(d.tmpls, a.Template)
		}
		if a.Kind == ActSet {
			t, err := strconv.ParseUint(er.GetRule().GetSelector().GetTag(), 16, 32)
			if err != nil {
				return nil, fmt.Errorf("engine: rule %d: a set rule's selector is not a tag", i+1)
			}
			if len(er.GetVrs()) != 1 {
				return nil, fmt.Errorf("engine: rule %d: a set rule states %d VRs, and makes its element with exactly one", i+1, len(er.GetVrs()))
			}
			d.sets = append(d.sets, SetRule{Rule: i, Tag: uint32(t), VR: er.GetVrs()[0]})
		}
	}
	return d, nil
}

// Decide implements Decider. The VRs are those validation stored with the
// rule, never the data dictionary's of this build, so a stored version
// acts on the same elements whatever library it runs with.
func (d *programDecider) Decide(loc profile.Location, f profile.Facts) Decision {
	pd := d.prog.Decide(loc, f)
	out := Decision{Rule: pd.Index, Action: toAction(pd.Action), VRs: pd.VRs, PatternMatches: pd.PatternMatches, PatternOver: pd.PatternOver, PatternUnread: pd.PatternUnread}
	if out.Action.Kind == ActRedact {
		if pd.Redact == nil {
			// A redact whose pattern did not compile cannot cut; the file
			// is refused rather than kept as it is.
			out.Action = Action{Kind: ActQuarantine}
		}
		out.Action.Pattern = pd.Redact
		out.Action.PatternCost = d.redactCost[pd.Index]
	}
	if pd.Rule == nil {
		switch {
		case loc.Tag>>16%2 == 1:
			out.Reason = "no rule names it, and a private element no rule names is removed (PS3.5 7.8)"
		case d.prog.Unnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE:
			out.Reason = "no rule names it, and the profile removes elements no rule names"
		default:
			out.Reason = "no rule names it, and the profile keeps elements no rule names"
		}
		return out
	}
	r := pd.Rule.GetRule()
	out.Name, out.Origin, out.Reason = r.GetName(), pd.Rule.GetOrigin(), r.GetReason()
	return out
}

// ruleLabel names a decision's rule as a failure does: its name, or its
// position ("rule 3") when it has none; empty for a default.
func ruleLabel(d Decision) string {
	switch {
	case d.Rule < 0:
		return ""
	case d.Name != "":
		return d.Name
	}
	return "rule " + strconv.Itoa(d.Rule+1)
}

// Sets implements Decider.
func (d *programDecider) Sets() []SetRule { return d.sets }

// Templates implements Decider.
func (d *programDecider) Templates() []string { return d.tmpls }

// DateShiftDays implements Decider.
func (d *programDecider) DateShiftDays() int32 { return d.prog.DateShiftDays() }

// ValueCost implements valueReader.
func (d *programDecider) ValueCost() int64 { return d.valueCost }

// valueReader is a Decider whose rules may read an element's value: its
// value patterns' compiled size together, zero when it has none. A
// Decider that is not one is given no value.
type valueReader interface {
	ValueCost() int64
}

// toAction converts the language's action.
func toAction(a *v1.ProfileAction) Action {
	switch k := a.GetKind().(type) {
	case *v1.ProfileAction_Keep:
		return Action{Kind: ActKeep}
	case *v1.ProfileAction_Remove:
		return Action{Kind: ActRemove}
	case *v1.ProfileAction_Empty:
		return Action{Kind: ActEmpty}
	case *v1.ProfileAction_Replace:
		return Action{Kind: ActReplace, Constant: k.Replace.GetConstant(), Template: k.Replace.GetTemplate()}
	case *v1.ProfileAction_Set:
		return Action{Kind: ActSet, Constant: k.Set.GetConstant(), Template: k.Set.GetTemplate()}
	case *v1.ProfileAction_Hash:
		return Action{Kind: ActHash}
	case *v1.ProfileAction_Uid:
		return Action{Kind: ActUID}
	case *v1.ProfileAction_Shift:
		return Action{Kind: ActShift}
	case *v1.ProfileAction_Generalize:
		return Action{Kind: ActGeneralize}
	case *v1.ProfileAction_Lookup:
		return Action{Kind: ActLookup, Table: k.Lookup.GetTable(), AssignMissing: k.Lookup.GetOnMissing() == v1.LookupMissing_LOOKUP_MISSING_ASSIGN}
	case *v1.ProfileAction_Clean:
		return Action{Kind: ActClean}
	case *v1.ProfileAction_Quarantine:
		return Action{Kind: ActQuarantine}
	case *v1.ProfileAction_Dummy:
		return Action{Kind: ActDummy}
	case *v1.ProfileAction_Redact:
		// The pattern is the compiled program's (programDecider.Decide).
		return Action{Kind: ActRedact, Replacement: k.Redact.GetReplacement()}
	case *v1.ProfileAction_RemoveUnlessRequired:
		switch k.RemoveUnlessRequired.GetFallback() {
		case v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY:
			return Action{Kind: ActRemoveUnlessRequired, Fallback: FallbackEmpty}
		case v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY:
			return Action{Kind: ActRemoveUnlessRequired, Fallback: FallbackDummy}
		case v1.RequiredFallback_REQUIRED_FALLBACK_UID:
			return Action{Kind: ActRemoveUnlessRequired, Fallback: FallbackUID}
		}
	}
	// An action this build does not know refuses the file rather than
	// guessing what it meant.
	return Action{Kind: ActQuarantine}
}

// String is the action's name in a record entry.
func (k ActionKind) String() string {
	names := [...]string{"", "keep", "remove", "empty", "replace", "set", "hash", "uid", "shift", "generalize", "lookup", "clean", "quarantine", "dummy", "remove_unless_required", "redact"}
	if int(k) > 0 && int(k) < len(names) {
		return names[k]
	}
	return "unknown"
}
