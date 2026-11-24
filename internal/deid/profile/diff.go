package profile

import (
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// MaxDiffItems bounds each list of a comparison, its rules and its
// effects, so an answer fits one message however large the versions:
// 1,000 rules of at most a few KiB each. What passes it is counted.
const MaxDiffItems = 1000

// Diff compares two expanded profiles and answers what changes from one to
// the other: bases, defaults, rules one by one, and the effect on the
// expanded form, selector by selector. Its From and To versions are left
// for the caller, who knows which stored versions they are.
//
// A rule is the same rule in both when it comes from the same origin and
// has the same name, or, unnamed, the same selector and conditions (the
// n-th of several such being the n-th of the other's); one present in both
// whose text differs is changed, and one whose place among the rules of
// its selector differs is moved, since between equals the later wins. The rules of a base present in only one
// of the two are not listed one by one: the base is, and its effect is.
//
// The effect is judged per selector with its conditions: of the rules
// with exactly that selector and those conditions, the later decides (as
// Decide does between equals), and its action before and after is
// compared; a selector whose rules of different conditions win in another
// order is listed as order_changed. Bases in another order are said to
// be, and a difference none of this shows is said to be (UnlistedChange).
// A difference between rules of different selectors (a tag rule
// that now takes an element from a group rule) is not an effect here; a
// trial run over files shows it. Equal checksums mean equal treatment of
// every file, and then no effect is listed.
func Diff(from, to *v1.ExpandedProfile) *v1.DiffProfileVersionsResponse {
	out := &v1.DiffProfileVersionsResponse{
		UnnamedFrom: from.GetUnnamed(), UnnamedTo: to.GetUnnamed(),
		DateShiftDaysFrom: from.GetDateShiftDays(), DateShiftDaysTo: to.GetDateShiftDays(),
		UnsearchedSegmentsFrom: from.GetUnsearchedSegments(), UnsearchedSegmentsTo: to.GetUnsearchedSegments(),
	}
	fromText, _ := Canonical(from)
	toText, _ := Canonical(to)
	out.SameEffect = fromText != nil && slices.Equal(fromText, toText)
	only := map[string]bool{}
	for _, b := range to.GetBases() {
		if !slices.Contains(from.GetBases(), b) {
			out.BasesAdded = append(out.BasesAdded, b)
			only[b] = true
		}
	}
	for _, b := range from.GetBases() {
		if !slices.Contains(to.GetBases(), b) {
			out.BasesRemoved = append(out.BasesRemoved, b)
			only[b] = true
		}
	}
	if len(only) == 0 && !slices.Equal(from.GetBases(), to.GetBases()) {
		out.BasesReordered = true
	}
	diffRules(out, from.GetRules(), to.GetRules(), only)
	if !out.SameEffect {
		diffEffects(out, from.GetRules(), to.GetRules())
		diffOrder(out, from.GetRules(), to.GetRules())
	}
	out.PixelRulesChanged = int32(pixelChanges(from.GetPixelRules(), to.GetPixelRules())) //nolint:gosec // G115: at most twice MaxPixelRules.
	listed := len(out.Rules) > 0 || out.RulesOmitted > 0 || len(out.Effects) > 0 || out.EffectsOmitted > 0 ||
		len(out.BasesAdded) > 0 || len(out.BasesRemoved) > 0 || out.BasesReordered || out.PixelRulesChanged > 0 ||
		out.UnnamedFrom != out.UnnamedTo || out.DateShiftDaysFrom != out.DateShiftDaysTo ||
		out.UnsearchedSegmentsFrom != out.UnsearchedSegmentsTo
	out.UnlistedChange = !out.SameEffect && !listed
	return out
}

// selectorOnly is a rule's selector as one text, without its conditions:
// the rules that share it compete for the same elements, the later
// winning among those whose conditions hold.
func selectorOnly(r *v1.ProfileRule) string {
	sel, _ := Canonical(r.GetSelector())
	return string(sel)
}

// conditionsKey is a rule's conditions as one text.
func conditionsKey(r *v1.ProfileRule) string {
	key := ""
	for _, c := range r.GetWhen() {
		ct, _ := Canonical(c)
		key += "\x00" + string(ct)
	}
	return key
}

// position is a rule's index as the answer writes it, from 1.
func position(i int) int32 {
	return int32(i + 1) //nolint:gosec // G115: an index under MaxExpandedRules.
}

// identities returns each rule's identity in a comparison: its origin and
// name, or, unnamed, its origin, selector and conditions, with the number
// of earlier rules of the same identity so that equal unnamed rules pair
// in order.
func identities(rules []*v1.ExpandedRule) []string {
	seen := map[string]int{}
	out := make([]string, len(rules))
	for i, er := range rules {
		r := er.GetRule()
		id := er.GetOrigin() + "\x00"
		if r.GetName() != "" {
			id += "name\x00" + r.GetName()
		} else {
			id += "selector\x00" + selectorKey(r)
		}
		n := seen[id]
		seen[id] = n + 1
		out[i] = id + "\x00" + strconv.Itoa(n)
	}
	return out
}

// selectorKey is a rule's selector and conditions as one text, which two
// rules share exactly when they select the same elements under the same
// conditions.
func selectorKey(r *v1.ProfileRule) string {
	sel, _ := Canonical(r.GetSelector())
	key := string(sel)
	for _, c := range r.GetWhen() {
		ct, _ := Canonical(c)
		key += "\x00" + string(ct)
	}
	return key
}

// diffRules lists the rules added, removed and changed, leaving out those
// whose origin is a base only one side has.
func diffRules(out *v1.DiffProfileVersionsResponse, from, to []*v1.ExpandedRule, only map[string]bool) {
	fromIDs, toIDs := identities(from), identities(to)
	fromAt := make(map[string]int, len(from))
	for i, id := range fromIDs {
		fromAt[id] = i
	}
	matched := make([]bool, len(from))
	add := func(c *v1.ProfileRuleChange) {
		if len(out.Rules) >= MaxDiffItems {
			out.RulesOmitted++
			return
		}
		out.Rules = append(out.Rules, c)
	}
	for j, er := range to {
		if only[er.GetOrigin()] {
			continue
		}
		i, ok := fromAt[toIDs[j]]
		if !ok {
			add(&v1.ProfileRuleChange{Change: v1.ProfileChange_PROFILE_CHANGE_ADDED, Origin: er.GetOrigin(),
				ToPosition: position(j), To: er.GetRule()})
			continue
		}
		matched[i] = true
		if !proto.Equal(from[i], er) {
			add(&v1.ProfileRuleChange{Change: v1.ProfileChange_PROFILE_CHANGE_CHANGED, Origin: er.GetOrigin(),
				FromPosition: position(i), ToPosition: position(j), From: from[i].GetRule(), To: er.GetRule()})
		}
	}
	moved(from, to, fromIDs, toIDs, fromAt, listedChanged(out), add)
	for i, er := range from {
		if matched[i] || only[er.GetOrigin()] {
			continue
		}
		add(&v1.ProfileRuleChange{Change: v1.ProfileChange_PROFILE_CHANGE_REMOVED, Origin: er.GetOrigin(),
			FromPosition: position(i), From: er.GetRule()})
	}
}

// winners returns, for each selector with its conditions, the position of
// the rule that decides between the rules that have exactly it (the
// later), and the selectors in the order their winners come.
func winners(rules []*v1.ExpandedRule) (map[string]int, []string) {
	at := map[string]int{}
	for i, er := range rules {
		at[selectorKey(er.GetRule())] = i
	}
	order := make([]string, 0, len(at))
	for i, er := range rules {
		k := selectorKey(er.GetRule())
		if at[k] == i {
			order = append(order, k)
		}
	}
	return at, order
}

// diffEffects lists each selector whose winning action differs between
// the two, or that only one has.
func diffEffects(out *v1.DiffProfileVersionsResponse, from, to []*v1.ExpandedRule) {
	fromAt, fromOrder := winners(from)
	toAt, toOrder := winners(to)
	add := func(c *v1.ProfileEffectChange) {
		if len(out.Effects) >= MaxDiffItems {
			out.EffectsOmitted++
			return
		}
		out.Effects = append(out.Effects, c)
	}
	for _, k := range toOrder {
		j := toAt[k]
		r := to[j].GetRule()
		c := &v1.ProfileEffectChange{Selector: r.GetSelector(), When: r.GetWhen(), To: r.GetAction(), ToPosition: position(j)}
		if i, ok := fromAt[k]; ok {
			if proto.Equal(from[i].GetRule().GetAction(), r.GetAction()) {
				continue
			}
			c.From, c.FromPosition = from[i].GetRule().GetAction(), position(i)
		}
		add(c)
	}
	for _, k := range fromOrder {
		if _, ok := toAt[k]; ok {
			continue
		}
		i := fromAt[k]
		r := from[i].GetRule()
		add(&v1.ProfileEffectChange{Selector: r.GetSelector(), When: r.GetWhen(), From: r.GetAction(), FromPosition: position(i)})
	}
}

// pixelChanges counts the pixel rules one side has that the other does
// not, each compared whole with its origin.
func pixelChanges(from, to []*v1.ExpandedPixelRule) int {
	count := map[string]int{}
	for _, r := range from {
		t, _ := Canonical(r)
		count[string(t)]++
	}
	changed := 0
	for _, r := range to {
		t, _ := Canonical(r)
		if count[string(t)] > 0 {
			count[string(t)]--
			continue
		}
		changed++
	}
	for _, n := range count {
		changed += n
	}
	return changed
}

// listedChanged returns the positions in to of the rules already listed
// as changed, which are not listed again as moved.
func listedChanged(out *v1.DiffProfileVersionsResponse) map[int32]bool {
	m := map[int32]bool{}
	for _, c := range out.Rules {
		if c.GetChange() == v1.ProfileChange_PROFILE_CHANGE_CHANGED {
			m[c.GetToPosition()] = true
		}
	}
	return m
}

// moved lists as moved each rule present in both whose place among the
// rules of its selector (present in both) differs: between equally
// specific rules the later wins, so another order can make another rule
// win.
func moved(from, to []*v1.ExpandedRule, fromIDs, toIDs []string, fromAt map[string]int, changed map[int32]bool, add func(*v1.ProfileRuleChange)) {
	toAt := make(map[string]int, len(to))
	for j, id := range toIDs {
		toAt[id] = j
	}
	order := func(rules []*v1.ExpandedRule, ids []string, other map[string]int) map[string][]string {
		groups := map[string][]string{}
		for i, er := range rules {
			if _, both := other[ids[i]]; both {
				k := selectorOnly(er.GetRule())
				groups[k] = append(groups[k], ids[i])
			}
		}
		return groups
	}
	fromGroups, toGroups := order(from, fromIDs, toAt), order(to, toIDs, fromAt)
	for j, er := range to {
		id := toIDs[j]
		i, both := fromAt[id]
		if !both || changed[position(j)] {
			continue
		}
		k := selectorOnly(er.GetRule())
		fg, tg := fromGroups[k], toGroups[k]
		if slices.Equal(fg, tg) || slices.Index(fg, id) == slices.Index(tg, id) {
			continue
		}
		add(&v1.ProfileRuleChange{Change: v1.ProfileChange_PROFILE_CHANGE_MOVED, Origin: er.GetOrigin(),
			FromPosition: position(i), ToPosition: position(j), From: from[i].GetRule(), To: er.GetRule()})
	}
}

// winnerOrder returns, for each selector, the conditions of its rules in
// the order they win: by the place of the last rule with those
// conditions, the later first.
func winnerOrder(rules []*v1.ExpandedRule) (map[string][]string, map[string]*v1.ProfileSelector, []string) {
	last := map[string]map[string]int{}
	sels := map[string]*v1.ProfileSelector{}
	var keys []string
	for i, er := range rules {
		k := selectorOnly(er.GetRule())
		if last[k] == nil {
			last[k] = map[string]int{}
			sels[k] = er.GetRule().GetSelector()
			keys = append(keys, k)
		}
		last[k][conditionsKey(er.GetRule())] = i
	}
	out := map[string][]string{}
	for k, m := range last {
		conds := make([]string, 0, len(m))
		for c := range m {
			conds = append(conds, c)
		}
		slices.SortFunc(conds, func(a, b string) int { return m[b] - m[a] })
		out[k] = conds
	}
	return out, sels, keys
}

// diffOrder lists each selector whose rules of different conditions win
// in another order, counting only the conditions both have: for a file
// that meets more than one rule's conditions, another rule then wins.
func diffOrder(out *v1.DiffProfileVersionsResponse, from, to []*v1.ExpandedRule) {
	fromOrder, _, _ := winnerOrder(from)
	toOrder, sels, keys := winnerOrder(to)
	for _, k := range keys {
		f, ok := fromOrder[k]
		if !ok {
			continue
		}
		common := func(a, b []string) []string {
			var c []string
			for _, x := range a {
				if slices.Contains(b, x) {
					c = append(c, x)
				}
			}
			return c
		}
		if fc, tc := common(f, toOrder[k]), common(toOrder[k], f); len(fc) > 1 && !slices.Equal(fc, tc) {
			if len(out.Effects) >= MaxDiffItems {
				out.EffectsOmitted++
				continue
			}
			out.Effects = append(out.Effects, &v1.ProfileEffectChange{Selector: sels[k], OrderChanged: true})
		}
	}
}
