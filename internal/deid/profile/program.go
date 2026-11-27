package profile

import (
	"fmt"
	"regexp"
	"slices"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Location is where an element is in a data set, as a rule's selector reads
// it. The engine makes one for each element it visits.
type Location struct {
	// Sequences are the tags of the sequences the element is inside,
	// outermost first; empty at the top level.
	Sequences []uint32
	// Tag is the element's tag.
	Tag uint32
	// Creator is the private creator of the block a private data element
	// is in (PS3.5 7.8.1), without padding; empty for any other element.
	Creator string
	// VR is the element's value representation.
	VR string
	// ContentItem is the concept name of the structured report content
	// item the element is directly in (PS3.3 C.17.3); nil for an element
	// of any other data set. Only the item's value element is selected by
	// a content item rule (IsContentValue).
	ContentItem *Concept
	// ValueType is that content item's Value Type (0040,A040), without
	// padding; empty for an element of any other data set.
	ValueType string
	// Value is the element's value as text, decoded from the file's
	// character set and its padding removed, which a value pattern reads;
	// the engine gives it for the VRs a value pattern reads, and leaves it
	// empty for any other element and for a value it could not read.
	Value string
	// ValueUnread reports a text value the engine could not give a value
	// pattern: longer than MaxPatternValue, not in memory, or holding an
	// escape no decoder turned into UTF-8. No value pattern selects it,
	// and the decision says one would have read it (PatternUnread), so
	// the engine refuses the file rather than pass it unread.
	ValueUnread bool
}

// Decision is what a profile does to one element: the rule that decides and
// its action, or the default when no rule does.
type Decision struct {
	// Index is the position of the rule in the expanded profile's rules,
	// from 0; -1 when no rule decides.
	Index int
	// Rule is the rule that decides; nil when no rule does.
	Rule *v1.ExpandedRule
	// Action is what happens to the element: the rule's action, or keep or
	// remove by the default.
	Action *v1.ProfileAction
	// VRs are the VRs validation decided for the rule's selector, as the
	// expanded form stores them; nil when the selector says nothing of
	// the VR, and when no rule decides.
	VRs []string
	// PatternMatches are what the deciding rule's value pattern matched in
	// the value (each whole part, or each text a search found), each text
	// once, at most MaxRedactMatches; nil when the deciding rule has no
	// value pattern.
	PatternMatches []string
	// PatternOver reports that the deciding rule's value pattern matched
	// more than MaxRedactMatches distinct texts.
	PatternOver bool
	// PatternUnread reports that a value pattern would have read the
	// element's value, and could not (Location.ValueUnread).
	PatternUnread bool
	// Redact is a redact action's pattern, compiled for a search; nil for
	// any other action.
	Redact *regexp.Regexp
	// Lists are the bits (TextLists.Mask) of the text lists the deciding
	// clean or redact rule removes the terms of; zero for none.
	Lists uint32
}

// Program is an expanded profile compiled for deciding, element by element,
// which rule applies. It is never changed after it is made, so one Program
// serves every file at once.
type Program struct {
	expanded *v1.ExpandedProfile
	rules    []rule
	pixels   []pixelRule
	// The rules of each tier, by position, so a decision reads only the
	// rules that could select an element.
	places []int
	byTag  map[uint32][]int
	privEl map[privateKey][]int
	block  map[privateKey][]int
	groups []int
	values []int
	byVR   map[string][]int
	// lists are the profile's text lists, nil when it has none; listMask
	// the lists each clean or redact rule names, by position.
	lists    *TextLists
	listMask map[int]uint32
}

// privateKey is a private block: its creator and group.
type privateKey struct {
	creator string
	group   uint16
}

// The default actions.
var (
	keepAction   = &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
	removeAction = &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
)

// Compile compiles an expanded profile, as Expand made it or as a stored
// version holds it. It checks structure only and never reads the data
// dictionary: the VRs each rule acts on are those validation stored with
// it, so a later library cannot change what a stored version does. An
// expanded profile that does not compile was not made by Expand, was
// changed after, or is of a language edition this build does not know.
//
// Its text lists are taken from, or kept in, the cache every user of a
// stored version shares, so one copy of their automata exists; a
// document's, compiled for a trial or a description, take CompileOneOff.
func Compile(e *v1.ExpandedProfile) (*Program, error) { return compile(e, true) }

// CompileOneOff is Compile for a profile used once (a document tried or
// described): its text lists are built, used and dropped, never kept in
// the cache stored versions share.
func CompileOneOff(e *v1.ExpandedProfile) (*Program, error) { return compile(e, false) }

// compile is Compile, its lists shared or not.
func compile(e *v1.ExpandedProfile, shared bool) (*Program, error) {
	if e == nil {
		return nil, fmt.Errorf("profile: no expanded profile")
	}
	ed := e.GetLanguageEdition()
	if ed < 1 || ed > LanguageEdition {
		return nil, fmt.Errorf("profile: %s: the expanded profile is of language edition %d, and this build runs editions 1 to %d", CodeBadEdition, ed, LanguageEdition)
	}
	if e.GetUnnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_UNSPECIFIED || e.GetDateShiftDays() < 1 || e.GetDateShiftDays() > MaxDateShiftDays {
		return nil, fmt.Errorf("profile: an expanded profile states its defaults")
	}
	if _, known := v1.UnnamedElements_name[int32(e.GetUnnamed())]; !known {
		return nil, fmt.Errorf("profile: an expanded profile's unnamed is not a value of UnnamedElements")
	}
	if _, known := v1.UnsearchedSegments_name[int32(e.GetUnsearchedSegments())]; !known {
		return nil, fmt.Errorf("profile: an expanded profile's unsearched segments is not a value of UnsearchedSegments")
	}
	if len(e.GetRules()) > MaxExpandedRules {
		return nil, fmt.Errorf("profile: an expanded profile has at most %d rules", MaxExpandedRules)
	}
	p := &Program{expanded: e, byTag: map[uint32][]int{}, privEl: map[privateKey][]int{}, block: map[privateKey][]int{}, byVR: map[string][]int{},
		listMask: map[int]uint32{}}
	chk := newChecker(true)
	chk.shared = shared
	if ed < listEdition && len(e.GetTextLists()) > 0 {
		return nil, fmt.Errorf("profile: %s: the expanded profile has text lists, which language edition %d does not have", CodeBadEdition, ed)
	}
	useLists(chk, at{c: chk}, e.GetTextLists())
	if len(chk.list) > 0 {
		return nil, fmt.Errorf("profile: the expanded profile: %w", chk.list)
	}
	p.lists = chk.lists
	for i, er := range e.GetRules() {
		r := er.GetRule()
		a := at{c: chk, rule: i + 1, name: r.GetName()}
		if ed < valuePatternEdition && usesValuePatterns(r) {
			return nil, fmt.Errorf("profile: %s: rule %d has a value pattern or redact, which language edition %d does not have", CodeBadEdition, i+1, ed)
		}
		if ed < listEdition && usesLists(r) {
			return nil, fmt.Errorf("profile: %s: rule %d names a text list, which language edition %d does not have", CodeBadEdition, i+1, ed)
		}
		sel, _, ok := parseSelector(a.in("selector"), r.GetSelector(), er.GetVrs())
		when := parseConditions(a, "when", r.GetWhen())
		var redact *regexp.Regexp
		var lists uint32
		if ok {
			_, redact, lists = checkAction(a, r.GetAction(), sel)
		}
		if len(chk.list) > 0 {
			return nil, fmt.Errorf("profile: the expanded profile: %w", chk.list)
		}
		if lists != 0 {
			p.listMask[i] = lists
		}
		p.rules = append(p.rules, rule{sel: sel, when: when, action: r.GetAction(), redact: redact, lists: lists, norm: r, origin: er.GetOrigin()})
		switch sel.tier {
		case TierPlace:
			p.places = append(p.places, i)
		case TierTag:
			p.byTag[sel.tag] = append(p.byTag[sel.tag], i)
		case TierPrivateElement:
			k := privateKey{sel.creator, sel.privGroup}
			p.privEl[k] = append(p.privEl[k], i)
		case TierPrivateBlock:
			k := privateKey{sel.creator, sel.privGroup}
			p.block[k] = append(p.block[k], i)
		case TierGroup:
			p.groups = append(p.groups, i)
		case TierValue:
			p.values = append(p.values, i)
		case TierVR:
			p.byVR[sel.vr] = append(p.byVR[sel.vr], i)
		}
	}
	for i, ep := range e.GetPixelRules() {
		r := ep.GetRule()
		c, ok := checkPixelRule(at{c: chk, pixelRule: i + 1, name: r.GetName()}, r, map[string]int{})
		if !ok {
			return nil, fmt.Errorf("profile: the expanded profile: %w", chk.list)
		}
		c.origin = ep.GetOrigin()
		p.pixels = append(p.pixels, c)
	}
	return p, nil
}

// Unnamed returns what happens to an element no rule decides, other than a
// private one.
func (p *Program) Unnamed() v1.UnnamedElements { return p.expanded.GetUnnamed() }

// DateShiftDays returns the largest date shift, in days.
func (p *Program) DateShiftDays() int32 { return p.expanded.GetDateShiftDays() }

// HoldsUnsearchedSegments reports whether an image whose compressed pixel
// data has segments that are not searched is held: unless the version says
// keep. A version stored before it said either holds, the side that never
// archives text unseen.
func (p *Program) HoldsUnsearchedSegments() bool {
	return p.expanded.GetUnsearchedSegments() != v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP
}

// Decide returns what the profile does to the element at loc, given the
// file's and the reception's facts. Of the rules whose selector matches and
// whose conditions all hold, the one of the highest tier decides, and
// between rules of one tier the later. With none, a private element (an
// odd group) is removed and any other follows Unnamed.
//
// A value pattern reads loc.Value, in one pass for each value pattern
// rule; one that would have read a value the engine could not read
// (loc.ValueUnread) does not select it, and the decision says so
// (PatternUnread).
func (p *Program) Decide(loc Location, f Facts) Decision {
	unread := false
	for _, candidates := range p.tiers(loc) {
		// The rules of a tier are in order, so the last that applies is
		// the later rule.
		for j := len(candidates) - 1; j >= 0; j-- {
			i := candidates[j]
			r := p.rules[i]
			var matches []string
			var over bool
			if r.sel.tier == TierValue {
				if !r.sel.readsValueOf(loc) {
					continue
				}
				if loc.ValueUnread {
					unread = true
					continue
				}
				if !allHold(r.when, f) {
					continue
				}
				if matches, over = r.sel.value.find(loc.VR, loc.Value); len(matches) == 0 {
					continue
				}
			} else if !r.sel.matches(loc) || !allHold(r.when, f) {
				continue
			}
			return Decision{Index: i, Rule: p.expanded.GetRules()[i], Action: r.action, VRs: r.sel.vrs,
				PatternMatches: matches, PatternOver: over, PatternUnread: unread, Redact: r.redact, Lists: p.listMask[i]}
		}
	}
	if group(loc.Tag)%2 == 1 || p.Unnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE {
		return Decision{Index: -1, Action: removeAction, PatternUnread: unread}
	}
	return Decision{Index: -1, Action: keepAction, PatternUnread: unread}
}

// tiers returns the rules that could select the element at loc, one list
// per tier, from the most specific.
func (p *Program) tiers(loc Location) [][]int {
	pk := privateKey{loc.Creator, group(loc.Tag)}
	return [][]int{
		p.places,
		p.byTag[loc.Tag],
		p.privEl[pk],
		p.block[pk],
		p.groups,
		p.values,
		p.byVR[loc.VR],
	}
}

// ReadsValues reports whether any rule has a value pattern, so the engine
// needs to give each element's value in its Location.
func (p *Program) ReadsValues() bool { return len(p.values) > 0 }

// Pixels returns the pixel rule that decides for an image of rows and
// columns, given the file's and the reception's facts: the last that
// matches. ok is false when none does, and an image at risk is then
// quarantined.
func (p *Program) Pixels(rows, columns int, f Facts) (r *v1.ExpandedPixelRule, ok bool) {
	for i := len(p.pixels) - 1; i >= 0; i-- {
		c := p.pixels[i]
		if (c.rows == 0 || int(c.rows) == rows) && (c.cols == 0 || int(c.cols) == columns) && allHold(c.when, f) {
			return p.expanded.GetPixelRules()[i], true
		}
	}
	return nil, false
}

// matches reports whether the selector selects the element at loc. No
// selector selects an element the pipeline handles, or a private creator
// element, which follows its block. A tag, a path or a content item never
// selects a private element, which only its creator identifies; a group or
// a VR selects one only with include_private, so the default that removes
// private elements holds unless a rule opts in; a private selector selects
// a data element of a block, never the creator element that reserves it.
func (s selector) matches(loc Location) bool {
	if pipelineOwned(loc.Tag) || isCreatorElement(loc.Tag) {
		return false
	}
	// A creator on an even group is no element a file can hold; it is
	// neither standard nor private, and nothing selects it.
	private := group(loc.Tag)%2 == 1
	standard := !private && loc.Creator == ""
	switch s.tier {
	case TierPlace:
		if s.concept != nil {
			return standard && loc.ContentItem != nil && *loc.ContentItem == *s.concept &&
				(s.valueType == "" || s.valueType == loc.ValueType) && IsContentValue(loc.ValueType, loc.Tag)
		}
		return standard && loc.Tag == s.tag && stepsMatch(s.steps, loc.Sequences)
	case TierTag:
		return standard && loc.Tag == s.tag
	case TierPrivateElement, TierPrivateBlock:
		if !private || loc.Creator != s.creator || group(loc.Tag) != s.privGroup || loc.Tag&0xFF00 < 0x1000 {
			return false
		}
		return s.privElem < 0 || int(loc.Tag&0xFF) == s.privElem
	case TierGroup:
		return (standard || private && s.includePrivate) && group(loc.Tag)&s.groupMask == s.groupValue
	case TierVR:
		return (standard || private && s.includePrivate) && loc.VR == s.vr
	case TierValue:
		if !s.readsValueOf(loc) || loc.ValueUnread {
			return false
		}
		m, _ := s.value.find(loc.VR, loc.Value)
		return len(m) > 0
	}
	return false
}

// readsValueOf reports whether a value pattern reads the value of the
// element at loc: a standard element, or a private one with
// include_private, of one of its VRs. The caller has already refused the
// elements no selector selects.
func (s selector) readsValueOf(loc Location) bool {
	private := group(loc.Tag)%2 == 1
	standard := !private && loc.Creator == ""
	return (standard || private && s.includePrivate) && slices.Contains(s.vrs, loc.VR) && !pipelineOwned(loc.Tag) && !isCreatorElement(loc.Tag)
}

// stepsMatch reports whether a path's steps above its element match the
// sequences an element is inside: a sequence step one level of that tag,
// "**" any number of levels. It runs in time proportional to the product of
// the two lengths, whatever the number of "**".
func stepsMatch(steps []step, seqs []uint32) bool {
	// ok[j]: the steps so far match the first j sequences.
	ok := make([]bool, len(seqs)+1)
	ok[0] = true
	for _, s := range steps {
		next := make([]bool, len(seqs)+1)
		for j := range ok {
			switch {
			case s.anyDepth:
				// Zero levels, or one more after a match.
				next[j] = ok[j] || (j > 0 && next[j-1])
			case j > 0:
				next[j] = ok[j-1] && seqs[j-1] == s.tag
			}
		}
		ok = next
	}
	return ok[len(seqs)]
}
