package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// rankedAction is an action of the strictness table and the rank it is
// expected at; rank 0 marks an action whose outcome is per Type.
type rankedAction struct {
	name   string
	action *v1.ProfileAction
	rank   profile.Strictness
}

// strictnessActions are one or more actions of every kind the language
// has, two of a kind where they can differ.
func strictnessActions() []rankedAction {
	redact := &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: `MRN\d+`}}}
	return []rankedAction{
		{"keep", keep(), profile.StrictKeep},
		{"hash", hash(), profile.StrictTransform},
		{"uid", uid(), profile.StrictTransform},
		{"lookup", lookup("codes", v1.LookupMissing_LOOKUP_MISSING_QUARANTINE), profile.StrictTransform},
		{"shift", shift(), profile.StrictTransform},
		{"generalize", generalize(), profile.StrictTransform},
		{"clean", clean(), profile.StrictTransform},
		{"redact", redact, profile.StrictTransform},
		{"replace-a", replace("A"), profile.StrictReplace},
		{"replace-b", replace("B"), profile.StrictReplace},
		{"set", set("A"), profile.StrictReplace},
		{"dummy", dummy(), profile.StrictReplace},
		{"empty", empty(), profile.StrictEmpty},
		{"remove", remove(), profile.StrictRemove},
		{"quarantine", quarantine(), profile.StrictQuarantine},
		{"rur-empty", unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY), 0},
		{"rur-dummy", unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY), 0},
		{"rur-uid", unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID), 0},
	}
}

// rurOutcomes are the ranks of remove_unless_required's outcomes for an
// element of Type 3, 2 and 1, by fallback, and the action of its Type 1
// outcome.
var rurOutcomes = map[string][3]profile.Strictness{
	"rur-empty": {profile.StrictRemove, profile.StrictEmpty, profile.StrictEmpty},
	"rur-dummy": {profile.StrictRemove, profile.StrictEmpty, profile.StrictReplace},
	"rur-uid":   {profile.StrictRemove, profile.StrictEmpty, profile.StrictTransform},
}

// rurFallback is the action of remove_unless_required's Type 1 outcome.
var rurFallback = map[string]string{"rur-empty": "empty", "rur-dummy": "dummy", "rur-uid": "uid"}

// expectedOrder works out the order of upper over lower from the ranks the
// design gives, independently of CompareActions: outcome by outcome, a
// higher rank stricter, a lower looser, equal ranks the same only for the
// same action.
func expectedOrder(u, l rankedAction) profile.Order {
	if u.name == l.name {
		return profile.OrderSame
	}
	type outcome struct {
		rank profile.Strictness
		name string
	}
	outs := func(a rankedAction) [3]outcome {
		if a.rank != 0 {
			return [3]outcome{{a.rank, a.name}, {a.rank, a.name}, {a.rank, a.name}}
		}
		r := rurOutcomes[a.name]
		return [3]outcome{{r[0], "remove"}, {r[1], "empty"}, {r[2], rurFallback[a.name]}}
	}
	uo, lo := outs(u), outs(l)
	stricter, incomparable := false, false
	for i := range uo {
		switch {
		case uo[i].rank < lo[i].rank:
			return profile.OrderLooser
		case uo[i].rank > lo[i].rank:
			stricter = true
		case uo[i].name != lo[i].name:
			incomparable = true
		}
	}
	switch {
	case incomparable:
		return profile.OrderIncomparable
	case stricter:
		return profile.OrderStricter
	}
	return profile.OrderSame
}

// TestStrictnessTable guards the order of strictness over every pair of
// actions (keep < hash, uid, lookup, shift, generalize, clean, redact <
// replace, set, dummy < empty < remove < quarantine; remove_unless_required
// compared Type by Type): CompareActions agrees with the table worked out
// from the ranks for each of the 324 ordered pairs, two actions of one rank
// that differ are incomparable, and what is stricter one way is looser the
// other.
func TestStrictnessTable(t *testing.T) {
	acts := strictnessActions()
	for _, a := range acts {
		got, ok := profile.ActionStrictness(a.action)
		if a.rank == 0 {
			if ok {
				t.Errorf("%s has one rank, %d", a.name, got)
			}
			continue
		}
		if !ok || got != a.rank {
			t.Errorf("%s: rank %d, want %d", a.name, got, a.rank)
		}
	}
	// What is stricter one way is looser the other, and the same is the
	// same; but remove_unless_required can be looser both ways (empty over
	// it keeps a Type 3 element it removes, it over empty writes a dummy
	// where empty writes nothing), each way letting more through for some
	// elements, so looser does not imply stricter back.
	reverse := map[profile.Order]profile.Order{profile.OrderSame: profile.OrderSame, profile.OrderStricter: profile.OrderLooser}
	n := 0
	for _, u := range acts {
		for _, l := range acts {
			n++
			got := profile.CompareActions(u.action, l.action)
			if want := expectedOrder(u, l); got != want {
				t.Errorf("%s over %s: %s, want %s", u.name, l.name, got, want)
			}
			if want, ok := reverse[got]; ok && profile.CompareActions(l.action, u.action) != want {
				t.Errorf("%s over %s is %s, and the other way not %s", u.name, l.name, got, want)
			}
			if profile.AtLeastAsStrict(u.action, l.action) != (got == profile.OrderSame || got == profile.OrderStricter) {
				t.Errorf("AtLeastAsStrict(%s, %s) disagrees with %s", u.name, l.name, got)
			}
		}
	}
	if n != 324 {
		t.Errorf("%d pairs", n)
	}
	// Spot checks of the table, each a sentence of the design.
	for _, c := range []struct {
		u, l *v1.ProfileAction
		want profile.Order
	}{
		{remove(), keep(), profile.OrderStricter},
		{keep(), hash(), profile.OrderLooser},
		{hash(), shift(), profile.OrderIncomparable},
		{replace("A"), replace("B"), profile.OrderIncomparable},
		{replace("A"), replace("A"), profile.OrderSame},
		{unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY), empty(), profile.OrderStricter},
		{unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY), remove(), profile.OrderLooser},
		{unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY), dummy(), profile.OrderStricter},
		{unlessRequired(v1.RequiredFallback_REQUIRED_FALLBACK_UID), hash(), profile.OrderIncomparable},
		{&v1.ProfileAction{}, keep(), profile.OrderIncomparable},
		{quarantine(), &v1.ProfileAction{}, profile.OrderIncomparable},
	} {
		if got := profile.CompareActions(c.u, c.l); got != c.want {
			t.Errorf("%v over %v: %s, want %s", c.u, c.l, got, c.want)
		}
	}
}
