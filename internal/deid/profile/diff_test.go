package profile_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestDiffRulesAndEffects guards the comparison of two versions: a rule
// added, removed and changed, each with its positions; an unnamed rule
// paired by its selector; the effect per selector (the later of equal
// selectors winning); a base added listed as a base, its rules not one by
// one; and equal versions with the same effect and nothing listed.
func TestDiffRulesAndEffects(t *testing.T) {
	from := expandFor(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("gone", kw("StudyID"), remove()),
		rl("changed", kw("PatientID"), remove()),
		rl("", kw("AccessionNumber"), empty()),
	}})
	to := expandFor(t, &v1.ProfileDocument{Bases: []string{baseA}, Rules: []*v1.ProfileRule{
		rl("changed", kw("PatientID"), hash()),
		rl("", kw("AccessionNumber"), empty()),
		rl("new", kw("StudyDescription"), keep()),
	}})
	d := profile.Diff(from, to)
	if d.GetSameEffect() || len(d.GetBasesAdded()) != 1 || d.GetBasesAdded()[0] != baseA {
		t.Errorf("bases %v, same %v", d.GetBasesAdded(), d.GetSameEffect())
	}
	changes := map[string]*v1.ProfileRuleChange{}
	for _, c := range d.GetRules() {
		r := c.GetTo()
		if r == nil {
			r = c.GetFrom()
		}
		if c.GetOrigin() != "" {
			t.Errorf("a rule of a base added is listed: %v", c)
		}
		changes[r.GetName()] = c
	}
	if len(changes) != 3 || changes["gone"].GetChange() != v1.ProfileChange_PROFILE_CHANGE_REMOVED || changes["gone"].GetFromPosition() != 1 ||
		changes["changed"].GetChange() != v1.ProfileChange_PROFILE_CHANGE_CHANGED || changes["new"].GetChange() != v1.ProfileChange_PROFILE_CHANGE_ADDED ||
		int(changes["new"].GetToPosition()) != len(to.GetRules()) {
		t.Errorf("rules %v", d.GetRules())
	}
	effects := map[string]*v1.ProfileEffectChange{}
	for _, e := range d.GetEffects() {
		effects[e.GetSelector().GetTag()] = e
	}
	if e := effects["00100020"]; e.GetFrom().GetRemove() == nil || e.GetTo().GetHash() == nil {
		t.Errorf("the patient id's effect %v", e)
	}
	if _, listed := effects["00080050"]; listed {
		t.Error("an unchanged unnamed rule changed effect")
	}
	if e := effects["00200010"]; e.GetFrom().GetRemove() == nil || e.GetTo() != nil {
		t.Errorf("the study id's effect %v", e)
	}
	if e := effects["00100010"]; e.GetFrom() != nil || e.GetTo().GetRemove() == nil {
		t.Errorf("the base's name rule's effect %v", e)
	}
	same := profile.Diff(to, to)
	if !same.GetSameEffect() || len(same.GetRules()) != 0 || len(same.GetEffects()) != 0 {
		t.Errorf("equal versions: %v", same)
	}
}

// TestDiffSeesWhichRuleWins guards a change of order: two rules on one
// selector, one with a condition, swapped, change which wins for a file
// that meets the condition (Decide differs), so the rules are listed as
// moved and the selector's effect as order_changed; bases in another order
// are said to be; and a difference no list shows is said to be.
func TestDiffSeesWhichRuleWins(t *testing.T) {
	a := rl("a", kw("PatientID"), keep(), equals(calling, "SCANNER1"))
	b := rl("b", kw("PatientID"), remove())
	from := expandFor(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{a, b}})
	to := expandFor(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{b, a}})
	loc := profile.Location{Tag: 0x00100020, VR: "LO"}
	f := facts{calling: "SCANNER1"}
	pf, _ := profile.Compile(from)
	pt, _ := profile.Compile(to)
	if pf.Decide(loc, f).Rule.GetRule().GetName() == pt.Decide(loc, f).Rule.GetRule().GetName() {
		t.Fatal("the swap does not change which rule wins; the test means nothing")
	}
	d := profile.Diff(from, to)
	moved := 0
	for _, c := range d.GetRules() {
		if c.GetChange() == v1.ProfileChange_PROFILE_CHANGE_MOVED {
			moved++
		}
	}
	if d.GetSameEffect() || moved != 2 {
		t.Errorf("rules %v", d.GetRules())
	}
	if len(d.GetEffects()) != 1 || !d.GetEffects()[0].GetOrderChanged() || d.GetEffects()[0].GetSelector().GetTag() != "00100020" {
		t.Errorf("effects %v", d.GetEffects())
	}

	shipped := func(bases ...string) *v1.ExpandedProfile {
		e, ps := profile.Expand(&v1.ProfileDocument{Bases: bases}, profile.Shipped())
		if len(ps) > 0 {
			t.Fatal(ps)
		}
		return e
	}
	ab := shipped(profile.AnnexE("basic"), profile.AnnexE("retain-uids"))
	ba := shipped(profile.AnnexE("retain-uids"), profile.AnnexE("basic"))
	if d := profile.Diff(ab, ba); !d.GetBasesReordered() || d.GetSameEffect() || len(d.GetRules()) == 0 {
		t.Errorf("bases reordered: %v", d)
	}

	// The same rules and defaults, a description that is not behavior
	// aside: an expanded form that differs only in what no list shows.
	x := expandFor(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{b}})
	y := proto.Clone(x).(*v1.ExpandedProfile)
	y.LanguageEdition++
	if d := profile.Diff(x, y); d.GetSameEffect() || !d.GetUnlistedChange() {
		t.Errorf("an unlisted change: %v", d)
	}
}
