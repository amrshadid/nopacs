package profile

// Match is a rule whose selector selects an element, as Matches answers
// it: where it stands in the precedence, and whether its conditions hold
// for the file.
type Match struct {
	// Index is the rule's position in the expanded profile, from 0.
	Index int
	// Tier is how specific its selector is.
	Tier Tier
	// Holds reports whether all its conditions hold for the file's facts.
	Holds bool
}

// Matches returns every rule whose selector selects the element at loc, in
// the order Decide weighs them: the most specific tier first, and within a
// tier the later rule first. The first that holds is the one Decide
// chooses; the rest are why the element is not treated otherwise, which an
// explanation of a file shows. It reads nothing but the compiled rules.
func (p *Program) Matches(loc Location, f Facts) []Match {
	var out []Match
	for _, candidates := range p.tiers(loc) {
		for j := len(candidates) - 1; j >= 0; j-- {
			i := candidates[j]
			r := p.rules[i]
			if r.sel.matches(loc) {
				out = append(out, Match{Index: i, Tier: r.sel.tier, Holds: allHold(r.when, f)})
			}
		}
	}
	return out
}

// String returns the tier as the word an explanation names it by: place,
// tag, private_element, private_block, group, value_pattern or vr.
func (t Tier) String() string {
	switch t {
	case TierPlace:
		return "place"
	case TierTag:
		return "tag"
	case TierPrivateElement:
		return "private_element"
	case TierPrivateBlock:
		return "private_block"
	case TierGroup:
		return "group"
	case TierValue:
		return "value_pattern"
	case TierVR:
		return "vr"
	}
	return "unknown"
}
