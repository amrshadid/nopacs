package profile

import (
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The codes of a problem of a layer. Like every code, part of the API's
// contract.
const (
	// CodeLoosensLowerLayer: a layer's rule lets through more of an
	// element than the layers below it decide for that element.
	CodeLoosensLowerLayer Code = "loosens_lower_layer"
	// CodeIncomparableActions: a layer's rule and a rule below it of the
	// same strictness differ (a hash and a shift, two constants), so
	// neither is known to let through less, and the layer is refused.
	CodeIncomparableActions Code = "incomparable_actions"
	// CodeLayerPixelRule: a layer above the first has pixel rules. The last
	// pixel rule that matches an image decides it, so a layer's rule would
	// replace, not tighten, what the layers below fill; pixel rules belong
	// to the first layer.
	CodeLayerPixelRule Code = "layer_pixel_rule"
	// CodeLayerOrder: the layers are not in the order tenant, receiver,
	// route, destination, each at most once.
	CodeLayerOrder Code = "layer_order"
)

// LayerKind is where a layer of de-identification is set.
type LayerKind int

// The layers, from the lowest. Each may only tighten what the layers below
// decide: a file is de-identified by the tenant's base, a receiver's
// profile over it, a route's over that, and a copy sent to a destination
// by the destination's over all of them.
const (
	// LayerTenant is the tenant's base profile.
	LayerTenant LayerKind = iota + 1
	// LayerReceiver is a receiver's profile.
	LayerReceiver
	// LayerRoute is a route's profile.
	LayerRoute
	// LayerDestination is the profile of a copy sent to a destination.
	LayerDestination
)

// String names the layer.
func (k LayerKind) String() string {
	switch k {
	case LayerTenant:
		return "tenant"
	case LayerReceiver:
		return "receiver"
	case LayerRoute:
		return "route"
	case LayerDestination:
		return "destination"
	}
	return "unknown"
}

// Layer is one layer's profile version.
type Layer struct {
	// Kind is where the layer is set.
	Kind LayerKind
	// Name is what the layer is known by in a problem and a description,
	// such as the version's reference "ct-research@3".
	Name string
	// Expanded is the version's expanded form.
	Expanded *v1.ExpandedProfile
}

// Effective is the result of layers stacked: one expanded profile that
// does what the layers do together, and for each of its rules the layer
// and the position in that layer it came from. It is never changed after
// it is made.
type Effective struct {
	expanded *v1.ExpandedProfile
	layers   []Layer
	// from is, per rule of expanded, its layer's index in layers and its
	// position in that layer's rules, from 0.
	from []ruleFrom
}

// ruleFrom is where a rule of an Effective came from.
type ruleFrom struct {
	layer, rule int
}

// Expanded returns a copy of the effective expanded profile, which
// Compile and the engine take as they take a version's.
func (e *Effective) Expanded() *v1.ExpandedProfile {
	c, _ := proto.Clone(e.expanded).(*v1.ExpandedProfile)
	return c
}

// Stack stacks layers, lowest first: the first as it is, each next one
// over the result by Add. The layers are in the order tenant, receiver,
// route, destination, each at most once, any of them absent. Problems
// lists every rule of a layer that would loosen or cannot be compared
// with what is below it, with the layer's name, and the result is nil
// when there is one.
func Stack(layers ...Layer) (*Effective, Problems) {
	if len(layers) == 0 {
		return nil, Problems{{Code: CodeRequired, Message: "a stack of layers needs at least one"}}
	}
	for i := 1; i < len(layers); i++ {
		if layers[i].Kind <= layers[i-1].Kind {
			return nil, Problems{{Code: CodeLayerOrder, Field: "layers", Message: "the layers are tenant, receiver, route and destination, in that order, each at most once; " +
				layers[i].Kind.String() + " comes after " + layers[i-1].Kind.String()}}
		}
	}
	first := layers[0]
	if first.Kind < LayerTenant || first.Kind > LayerDestination {
		return nil, Problems{{Code: CodeLayerOrder, Field: "layers", Message: "a layer is tenant, receiver, route or destination"}}
	}
	// The layers are judged, not run: their lists are built for this and
	// dropped, never kept in the cache stored versions share.
	if _, err := CompileOneOff(first.Expanded); err != nil {
		return nil, Problems{{Code: CodeBadEdition, Message: first.Kind.String() + " layer " + clipText(first.Name) + ": " + err.Error()}}
	}
	e := &Effective{expanded: first.Expanded, layers: []Layer{first}}
	for i := range first.Expanded.GetRules() {
		e.from = append(e.from, ruleFrom{0, i})
	}
	for _, l := range layers[1:] {
		var problems Problems
		if e, problems = e.Add(l); problems != nil {
			return nil, problems
		}
	}
	return e, nil
}

// Add returns the profile of layer stacked over e, when every rule of the
// layer is at least as strict as what e decides for the elements it
// selects (AtLeastAsStrict); otherwise Problems, one per rule refused
// (CodeLoosensLowerLayer, CodeIncomparableActions), each naming the
// layer, the elements and both actions, and never a value. e is not
// changed.
//
// What e decides for the elements a layer's rule selects is judged from
// the selectors, never from a file, and errs toward refusing: every rule
// below that may select one of those elements counts, whatever its
// conditions, but the rules a rule below of the very same selector, with
// no condition, outranks (a less specific tier, or an earlier rule of its
// tier); and, when no rule below has the very same selector, the default
// below (a private element removed, any other as e's unnamed says). A
// layer's rule is compared with each of them whatever its own conditions.
//
// In the result, the layer's rules come after e's, so of rules of one
// tier the layer's decides. A standard element no rule names is removed
// when e or the layer says so; the largest date shift is e's, so a
// subject's dates move by one offset whichever layers a file passes; an
// image whose compressed segments are not searched is held unless both
// keep it; the bases are e's, then the layer's not among them. A layer
// above the first has no pixel rules (CodeLayerPixelRule).
func (e *Effective) Add(layer Layer) (*Effective, Problems) {
	chk := newChecker(true)
	name := layer.Kind.String() + " layer " + clipText(layer.Name)
	last := e.layers[len(e.layers)-1]
	if layer.Kind <= last.Kind {
		return nil, Problems{{Code: CodeLayerOrder, Field: "layers", Message: "a " + layer.Kind.String() + " layer cannot stand over a " + last.Kind.String() + " layer"}}
	}
	lower, err := CompileOneOff(e.expanded)
	if err != nil {
		return nil, Problems{{Code: CodeBadEdition, Message: "the layers below: " + err.Error()}}
	}
	upper, err := CompileOneOff(layer.Expanded)
	if err != nil {
		return nil, Problems{{Code: CodeBadEdition, Message: name + ": " + err.Error()}}
	}
	for i := range layer.Expanded.GetPixelRules() {
		at{c: chk, pixelRule: i + 1}.add(CodeLayerPixelRule, "%s has pixel rules; the last pixel rule matching an image decides it, so a layer's would replace what the layers below fill: pixel rules belong to the first layer", name)
	}
	// A rule of the layer that a later rule of the same selector with no
	// condition replaces decides nothing in the layer (the later of two rules
	// of one selector wins, and one with no condition always holds), so only
	// the later is judged: a layer that takes a base's weaker action for an
	// element and then states a stricter one is as strict as the second says.
	replacedBy := make(map[string]int, len(upper.rules))
	for i, ur := range upper.rules {
		if len(ur.norm.GetWhen()) == 0 {
			replacedBy[selectorOnly(ur.norm)] = i
		}
	}
	for i, ur := range upper.rules {
		if j, ok := replacedBy[selectorOnly(ur.norm)]; ok && j > i {
			continue
		}
		e.judge(at{c: chk, rule: i + 1, name: ur.norm.GetName(), field: "action"}, name, ur, lower)
	}
	if len(chk.list) > 0 {
		return nil, chk.list
	}
	merged, _ := proto.Clone(e.expanded).(*v1.ExpandedProfile)
	for _, er := range layer.Expanded.GetRules() {
		c, _ := proto.Clone(er).(*v1.ExpandedRule)
		merged.Rules = append(merged.Rules, c)
	}
	// The layers are kept apart, so the profile decides each element by each
	// layer alone and the strictest decision wins, never a more specific
	// selector of one layer over a stricter rule of another (Program.Decide).
	if len(merged.Layers) == 0 {
		merged.Layers = []*v1.ExpandedLayer{{Rules: int32(len(e.expanded.GetRules())), PixelRules: int32(len(e.expanded.GetPixelRules())), //nolint:gosec // G115: bounded by MaxExpandedRules.
			Unnamed: e.expanded.GetUnnamed()}}
	}
	merged.Layers = append(merged.Layers, &v1.ExpandedLayer{Rules: int32(len(layer.Expanded.GetRules())), Unnamed: layer.Expanded.GetUnnamed()}) //nolint:gosec // G115: bounded by MaxExpandedRules.
	if layer.Expanded.GetUnnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE {
		merged.Unnamed = v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE
	}
	if layer.Expanded.GetUnsearchedSegments() != v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP {
		merged.UnsearchedSegments = v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_UNSPECIFIED
	}
	for _, b := range layer.Expanded.GetBases() {
		if !slices.Contains(merged.Bases, b) {
			merged.Bases = append(merged.Bases, b)
		}
	}
	// A rule names a list by its name, so a list of one name must be the
	// same list in every layer: one given another meaning by a layer above
	// would change what a rule below cuts.
	for _, l := range layer.Expanded.GetTextLists() {
		i := slices.IndexFunc(merged.TextLists, func(m *v1.TextList) bool { return m.GetName() == l.GetName() })
		switch {
		case i < 0:
			c, _ := proto.Clone(l).(*v1.TextList)
			merged.TextLists = append(merged.TextLists, c)
		case !proto.Equal(merged.TextLists[i], l):
			return nil, Problems{{Code: CodeDuplicateName, Field: "textLists", Message: name + " has a list " + l.GetName() +
				" that differs from the list of that name below; give it another name"}}
		}
	}
	merged.LanguageEdition = max(merged.GetLanguageEdition(), layer.Expanded.GetLanguageEdition())
	if len(merged.Rules) > MaxExpandedRules {
		return nil, Problems{{Code: CodeTooMany, Field: "rules", Message: "with " + name + " the layers have more rules than an expanded profile may hold"}}
	}
	if _, err := CompileOneOff(merged); err != nil {
		// The layers' lists together past their bounds, so the stack would
		// not run.
		return nil, Problems{{Code: CodeTooMany, Field: "textLists", Message: "with " + name + " the layers do not make one profile: " + err.Error()}}
	}
	out := &Effective{expanded: merged, layers: append(slices.Clone(e.layers), layer), from: slices.Clone(e.from)}
	for i := range layer.Expanded.GetRules() {
		out.from = append(out.from, ruleFrom{len(out.layers) - 1, i})
	}
	return out, nil
}

// judge records a problem at a for each decision below that the layer's
// rule ur would loosen or cannot be compared with.
func (e *Effective) judge(a at, name string, ur rule, lower *Program) {
	what, _ := describeSelector(ur.norm.GetSelector(), ur.sel.vrs)
	upperName, _ := oneofName(ur.action.ProtoReflect(), "kind")
	key := selectorOnly(ur.norm)
	cover := -1
	for j, lr := range lower.rules {
		if len(lr.norm.GetWhen()) == 0 && selectorOnly(lr.norm) == key {
			cover = j
		}
	}
	compare := func(lowerAction *v1.ProfileAction, below string) {
		lowerName, _ := oneofName(lowerAction.ProtoReflect(), "kind")
		switch CompareActions(ur.action, lowerAction) {
		case OrderLooser:
			a.add(CodeLoosensLowerLayer, "%s would %s %s, which %s makes %s: a layer may only tighten what is below it", name, upperName, what, below, lowerName)
		case OrderIncomparable:
			a.add(CodeIncomparableActions, "%s would %s %s, which %s makes %s: neither is known to let through less, so the layer is refused", name, upperName, what, below, lowerName)
		}
	}
	for j, lr := range lower.rules {
		if !mayOverlap(ur.sel, lr.sel) {
			continue
		}
		if cover >= 0 && j != cover {
			c := lower.rules[cover].sel.tier
			if lr.sel.tier < c || (lr.sel.tier == c && j < cover) {
				continue
			}
		}
		compare(lr.action, e.belowName(j, lower))
	}
	if cover >= 0 {
		return
	}
	private, standard := selectsPrivate(ur.sel), selectsStandard(ur.sel)
	if private {
		compare(removeAction, "the default below, for a private element no rule names,")
	}
	if standard {
		def := keepAction
		if e.expanded.GetUnnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE {
			def = removeAction
		}
		compare(def, "the default below, for an element no rule names,")
	}
}

// belowName names rule j of the layers below for a problem: its layer, and
// its name or position there.
func (e *Effective) belowName(j int, lower *Program) string {
	f := e.from[j]
	l := e.layers[f.layer]
	rn := lower.rules[j].norm.GetName()
	if rn == "" {
		rn = "rule " + strconv.Itoa(f.rule+1)
	} else {
		rn = "rule " + rn
	}
	return "the " + l.Kind.String() + " layer " + clipText(l.Name) + "'s " + rn
}

// selectsPrivate reports whether a selector may select a private element.
func selectsPrivate(s selector) bool {
	switch s.tier {
	case TierPrivateElement, TierPrivateBlock:
		return true
	case TierGroup, TierVR, TierValue:
		return s.includePrivate
	}
	return false
}

// selectsStandard reports whether a selector may select a standard
// element.
func selectsStandard(s selector) bool {
	switch s.tier {
	case TierPrivateElement, TierPrivateBlock:
		return false
	case TierGroup:
		// An odd group selects private elements only.
		return s.groupMask&1 == 0 || s.groupValue&1 == 0
	}
	return true
}

// mayOverlap reports whether two selectors may select one element: false
// only when they provably cannot (two tags that differ, a standard element
// and a private one, a tag outside a group, a VR the tag cannot have, two
// creators); true otherwise, which errs toward comparing.
func mayOverlap(a, b selector) bool {
	if (!selectsPrivate(a) && !selectsStandard(b)) || (!selectsStandard(a) && !selectsPrivate(b)) {
		return false
	}
	if a.tier < b.tier {
		a, b = b, a
	}
	// a is the more specific.
	ta, aTag := knownTag(a)
	tb, bTag := knownTag(b)
	switch {
	case aTag && bTag:
		return ta == tb
	case a.concept != nil && b.concept != nil:
		return *a.concept == *b.concept && (a.valueType == "" || b.valueType == "" || a.valueType == b.valueType)
	case aTag:
		switch b.tier {
		case TierGroup:
			return group(ta)&b.groupMask == b.groupValue
		case TierVR, TierValue:
			return vrsMeet(a.vrs, b.vrs)
		}
		return vrsMeet(a.vrs, b.vrs)
	case a.tier == TierPrivateElement || a.tier == TierPrivateBlock:
		switch b.tier {
		case TierPrivateElement, TierPrivateBlock:
			return a.creator == b.creator && a.privGroup == b.privGroup && (a.privElem < 0 || b.privElem < 0 || a.privElem == b.privElem)
		case TierGroup:
			return a.privGroup&b.groupMask == b.groupValue
		}
		return true
	case a.tier == TierGroup && b.tier == TierGroup:
		return (a.groupValue^b.groupValue)&a.groupMask&b.groupMask == 0
	case a.tier == TierVR || a.tier == TierValue:
		return vrsMeet(a.vrs, b.vrs)
	}
	return vrsMeet(a.vrs, b.vrs)
}

// knownTag returns the one tag a tag selector or a path selects.
func knownTag(s selector) (uint32, bool) {
	if s.tier == TierTag || (s.tier == TierPlace && s.concept == nil) {
		return s.tag, true
	}
	return 0, false
}

// vrsMeet reports whether two selectors' VRs may meet: either says
// nothing of the VR, or they share one.
func vrsMeet(a, b []string) bool {
	if a == nil || b == nil {
		return true
	}
	for _, v := range a {
		if slices.Contains(b, v) {
			return true
		}
	}
	return false
}

// EffectiveRule is what decides one class of elements once layers are
// stacked, and which layer decided it: input for a description of the
// effective profile.
type EffectiveRule struct {
	// What names the elements in plain words; Class is their class.
	What  string
	Class v1.ProfileElementClass
	// Action is the action's name ("remove", "hash").
	Action string
	// Conditional reports a rule with conditions, which decides only where
	// they hold.
	Conditional bool
	// Layer and LayerName are the layer that decided.
	Layer     LayerKind
	LayerName string
	// Rule is the rule's position in its layer's expanded rules, from 1;
	// RuleName its name, empty when it has none; Origin the base it came
	// from, empty for the layer's own rules.
	Rule     int
	RuleName string
	Origin   string
}

// Describe returns, for each selector and conditions of the stacked
// layers, the rule that decides and its layer, in the order each first
// appears: the last rule of that selector and those conditions, since a
// later layer's rule of the same selector is at least as strict and comes
// after (Add). It holds no value of a file.
func (e *Effective) Describe() []EffectiveRule {
	var out []EffectiveRule
	at := map[string]int{}
	for i, er := range e.expanded.GetRules() {
		r := er.GetRule()
		what, class := describeSelector(r.GetSelector(), er.GetVrs())
		action, _ := oneofName(r.GetAction().ProtoReflect(), "kind")
		f := e.from[i]
		l := e.layers[f.layer]
		x := EffectiveRule{What: what, Class: class, Action: action, Conditional: len(r.GetWhen()) > 0,
			Layer: l.Kind, LayerName: l.Name, Rule: f.rule + 1, RuleName: r.GetName(), Origin: er.GetOrigin()}
		key := selectorKey(r)
		if j, ok := at[key]; ok {
			out[j] = x
			continue
		}
		at[key] = len(out)
		out = append(out, x)
	}
	return out
}
