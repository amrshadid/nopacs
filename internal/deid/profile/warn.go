package profile

import (
	"fmt"
	"slices"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// MaxWarnings is the most warnings answered for one document; one more
// says how many were left out, so an answer stays small.
const MaxWarnings = 100

// maxNamed is the most elements one warning names; it gives the count of
// the rest.
const maxNamed = 5

// Warnings returns what a valid document does that its author may not
// mean, from its expanded form: a rule of the document's that a more
// specific rule of a base decides instead, with another action, for some
// of the elements it selects (CodeShadowedByBase), such as a group 0010
// remove under a base that keeps Patient's Sex by its tag; and a rule that
// keeps elements a base changes or removes (CodeKeepsBaseChange). Each
// names the document's rule by its position among the document's own
// rules and its name. Conditions are not weighed: a warning says that a
// base's rule may decide, when its conditions hold.
//
// An element is judged from what the selectors say, without a file: a tag
// against a group, a VR or a content item's value elements; a private
// element against its block, or a group or VR that includes private
// elements. Where neither selector names the elements (a group against a
// VR), no warning is given.
//
// The profile is compiled for this once: a document validated is not yet
// a stored version, and its lists never enter the cache stored versions
// share.
func Warnings(e *v1.ExpandedProfile) (Problems, error) {
	p, err := CompileOneOff(e)
	if err != nil {
		return nil, err
	}
	var base, own []int
	for i, r := range p.rules {
		if r.origin == "" {
			own = append(own, i)
		} else {
			base = append(base, i)
		}
	}
	var out Problems
	more := 0
	add := func(pr Problem) {
		if len(out) < MaxWarnings {
			out = append(out, pr)
		} else {
			more++
		}
	}
	for pos, i := range own {
		r := p.rules[i]
		var shadowed, overridden []string
		for _, j := range base {
			b := p.rules[j]
			if !overlap(b.sel, r.sel) {
				continue
			}
			bk, rk := actionKind(b.action), actionKind(r.action)
			switch {
			case b.sel.tier > r.sel.tier && bk != rk:
				shadowed = append(shadowed, fmt.Sprintf("%s (%s, %s)", describe(b), b.origin, bk))
			case b.sel.tier <= r.sel.tier && (rk == "keep" || rk == "redact") && bk != "keep" && bk != "quarantine":
				overridden = append(overridden, fmt.Sprintf("%s (%s, %s)", describe(b), b.origin, bk))
			}
		}
		where := Problem{Rule: pos + 1, RuleName: r.norm.GetName(), Field: "selector"}
		if len(shadowed) > 0 {
			w := where
			w.Code = CodeShadowedByBase
			w.Message = fmt.Sprintf("for some elements this rule selects, a more specific rule of a base decides, whatever the order: %s", listNamed(shadowed))
			add(w)
		}
		if len(overridden) > 0 {
			w := where
			w.Field = "action." + actionKind(r.action)
			w.Code = CodeKeepsBaseChange
			w.Message = fmt.Sprintf("this rule keeps elements a base changes or removes: %s", listNamed(overridden))
			if actionKind(r.action) == "redact" {
				w.Message = fmt.Sprintf("this rule keeps what it does not cut of elements a base changes or removes: %s", listNamed(overridden))
			}
			add(w)
		}
	}
	if more > 0 {
		out = append(out, Problem{Code: CodeMoreWarnings, Message: fmt.Sprintf("%d more warnings are not listed", more)})
	}
	return out, nil
}

// listNamed writes the first few entries and the count of the rest.
func listNamed(list []string) string {
	if len(list) <= maxNamed {
		return strings.Join(list, "; ")
	}
	return strings.Join(list[:maxNamed], "; ") + fmt.Sprintf("; and %d more", len(list)-maxNamed)
}

// describe names what a rule selects, for a warning.
func describe(r rule) string {
	s := r.sel
	switch {
	case s.concept != nil:
		return "the content item " + s.concept.Scheme + " " + s.concept.Value
	case s.tier == TierTag:
		return formatTag(s.tag)
	case s.tier == TierPlace:
		return r.norm.GetSelector().GetPath()
	case s.tier == TierPrivateElement, s.tier == TierPrivateBlock:
		pe := r.norm.GetSelector().GetPrivateElement()
		return fmt.Sprintf("%q %s,xx%s", pe.GetCreator(), pe.GetGroup(), pe.GetElement())
	case s.tier == TierGroup:
		return "group " + r.norm.GetSelector().GetGroup()
	case s.tier == TierValue:
		return fmt.Sprintf("the value pattern %q", clipText(r.norm.GetSelector().GetValuePattern().GetPattern()))
	case s.tier == TierVR:
		return "VR " + s.vr
	}
	return "rule"
}

// actionKind names an action's kind.
func actionKind(a *v1.ProfileAction) string {
	switch a.GetKind().(type) {
	case *v1.ProfileAction_Keep:
		return "keep"
	case *v1.ProfileAction_Remove:
		return "remove"
	case *v1.ProfileAction_Empty:
		return "empty"
	case *v1.ProfileAction_Replace:
		return "replace"
	case *v1.ProfileAction_Set:
		return "set"
	case *v1.ProfileAction_Hash:
		return "hash"
	case *v1.ProfileAction_Uid:
		return "uid"
	case *v1.ProfileAction_Shift:
		return "shift"
	case *v1.ProfileAction_Generalize:
		return "generalize"
	case *v1.ProfileAction_Lookup:
		return "lookup"
	case *v1.ProfileAction_Clean:
		return "clean"
	case *v1.ProfileAction_Quarantine:
		return "quarantine"
	case *v1.ProfileAction_Dummy:
		return "dummy"
	case *v1.ProfileAction_RemoveUnlessRequired:
		return "remove unless required"
	case *v1.ProfileAction_Redact:
		return "redact"
	}
	return "unknown"
}

// overlap reports whether some element could be selected by both
// selectors, as far as the selectors alone say.
func overlap(x, y selector) bool {
	if x.tier < y.tier {
		x, y = y, x
	}
	// x is the more specific, or as specific.
	switch {
	case x.concept != nil:
		return overlapContent(x, y)
	case x.tier == TierPlace || x.tier == TierTag:
		return overlapStandard(x.tag, x.vrs, y)
	case x.tier == TierPrivateElement || x.tier == TierPrivateBlock:
		switch y.tier {
		case TierPrivateElement, TierPrivateBlock:
			return x.creator == y.creator && x.privGroup == y.privGroup && (x.privElem < 0 || y.privElem < 0 || x.privElem == y.privElem)
		case TierGroup:
			return y.includePrivate && x.privGroup&y.groupMask == y.groupValue
		case TierValue:
			// The file gives a private element its VR, which may be one a
			// value pattern reads.
			return y.includePrivate
		}
	case x.tier == TierGroup && y.tier == TierGroup:
		common := x.groupMask & y.groupMask
		return (x.groupValue^y.groupValue)&common == 0
	case x.tier == TierGroup && y.tier == TierValue:
		// A group holds elements of many VRs; one of them may be text a
		// value pattern reads.
		return true
	case x.tier == TierValue:
		// Which values a pattern matches is not known from the selectors,
		// so two of them, or one and a VR it reads, may meet.
		if y.tier == TierVR {
			return slices.Contains(x.vrs, y.vr)
		}
		return slices.ContainsFunc(x.vrs, func(v string) bool { return slices.Contains(y.vrs, v) })
	case x.tier == TierVR && y.tier == TierVR:
		return x.vr == y.vr
	}
	return false
}

// overlapStandard reports whether y could select the standard element t,
// whose VRs are vrs.
func overlapStandard(t uint32, vrs []string, y selector) bool {
	if pipelineOwned(t) {
		return false
	}
	switch y.tier {
	case TierPlace, TierTag:
		if y.concept != nil {
			return overlapContent(y, selector{tier: TierTag, tag: t, vrs: vrs})
		}
		return y.tag == t
	case TierGroup:
		return group(t)&y.groupMask == y.groupValue
	case TierValue:
		return slices.ContainsFunc(vrs, func(v string) bool { return slices.Contains(y.vrs, v) })
	case TierVR:
		return slices.Contains(vrs, y.vr)
	}
	return false
}

// overlapContent reports whether y could select a value element of the
// content items x selects.
func overlapContent(x, y selector) bool {
	if y.concept != nil {
		return *x.concept == *y.concept && (x.valueType == "" || y.valueType == "" || x.valueType == y.valueType)
	}
	types := []string{x.valueType}
	if x.valueType == "" {
		types = ValueTypes()
	}
	for _, vt := range types {
		for _, v := range contentValues[vt] {
			if overlapStandard(v.tag, []string{v.vr}, y) {
				return true
			}
		}
	}
	return false
}
