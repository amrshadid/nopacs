package profile_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// cleanLists is a clean action naming lists.
func cleanLists(lists ...string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{Lists: lists}}}
}

// redactLists is a redact action of a pattern, a replacement and lists.
func redactLists(pattern, repl string, lists ...string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: pattern, Replacement: repl, Lists: lists}}}
}

// docLists are the lists of the document tests.
func docLists() []*v1.TextList {
	return []*v1.TextList{
		{Name: "staff", Version: 1, Terms: []string{"Ann Lee", seededTerm}},
		{Name: "sites", Version: 2, Terms: []string{"North Clinic"}},
	}
}

// TestDocumentListsAreChecked guards the lists of a document and the rules
// naming them: a list's problem is at its place in the document
// ("textLists[1].terms[2]"), never with the term; a rule naming a list the
// document lacks (unknown_list), a list twice, more lists than a rule may
// name, a redact whose replacement holds a term of its lists, a redact of
// neither pattern nor list, and a rule naming a list of a document whose
// list has a problem (no second problem for that rule) are refused at the
// rule's field; a redact of lists alone is taken.
func TestDocumentListsAreChecked(t *testing.T) {
	cat := testCatalog(t)
	bad := []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{"Ann Lee", seededTerm + `\x`}}}
	_, problems := profile.Expand(&v1.ProfileDocument{TextLists: bad, Rules: []*v1.ProfileRule{rl("d", kw("StudyDescription"), cleanLists("staff"))}}, cat)
	if len(problems) != 1 || problems[0].Field != "textLists[1].terms[2]" || problems[0].Code != profile.CodeBadTerm ||
		strings.Contains(strings.ToLower(problems.Error()), strings.ToLower(seededTerm)) {
		t.Errorf("a bad term: %v", problems)
	}
	many := make([]string, profile.MaxRuleLists+1)
	for i := range many {
		many[i] = "staff"
	}
	for _, c := range []struct {
		name  string
		act   *v1.ProfileAction
		code  profile.Code
		field string
	}{
		{"unknown", cleanLists("nurses"), profile.CodeUnknownList, "action.clean.lists[1]"},
		{"twice", cleanLists("staff", "sites", "staff"), profile.CodeDuplicateName, "action.clean.lists[3]"},
		{"too many", cleanLists(many...), profile.CodeTooMany, "action.clean.lists"},
		{"replacement", redactLists(`MRN\d+`, "ann lee", "staff"), profile.CodeBadValue, "action.redact.replacement"},
		{"replacement of another list", redactLists("", "North Clinic", "staff"), profile.CodeBadValue, "action.redact.replacement"},
		{"constant", replace("by Ann Lee"), profile.CodeBadValue, "action.replace.constant"},
		{"template", replaceTemplate("North Clinic {#PatientID}"), profile.CodeBadValue, "action.replace.template"},
		{"nothing to cut", redactLists("", ""), profile.CodeRequired, "action.redact.pattern"},
		{"unknown in redact", redactLists("", "", "nurses"), profile.CodeUnknownList, "action.redact.lists[1]"},
	} {
		_, problems := profile.Expand(&v1.ProfileDocument{TextLists: docLists(), Rules: []*v1.ProfileRule{rl("d", kw("StudyDescription"), c.act)}}, cat)
		if len(problems) == 0 || problems[0].Code != c.code || problems[0].Field != c.field || problems[0].Rule != 1 {
			t.Errorf("%s: %v", c.name, problems)
		}
	}
	mustExpand(t, &v1.ProfileDocument{TextLists: docLists(), Rules: []*v1.ProfileRule{rl("d", kw("StudyDescription"), redactLists("", "", "sites"))}}, cat)
}

// TestDiffNamesListChanges guards the comparison of two versions whose
// only difference is in their text lists: a list whose terms changed, one
// removed, one added and one of a new version are each listed by name in
// byte order with both sides' summaries (version, count, checksum); the
// comparison is not of the same effect and needs no "unlisted change"; a
// list whose terms are only written apart (case, white space, order) is
// not listed; and no term is in the answer.
func TestDiffNamesListChanges(t *testing.T) {
	cat := testCatalog(t)
	rules := []*v1.ProfileRule{rl("d", kw("StudyDescription"), clean())}
	from := mustExpand(t, &v1.ProfileDocument{Rules: rules, TextLists: []*v1.TextList{
		{Name: "staff", Version: 1, Terms: []string{"Ann Lee", seededTerm}},
		{Name: "sites", Version: 1, Terms: []string{"North Clinic"}},
		{Name: "units", Version: 1, Terms: []string{"Ward Nine"}},
		{Name: "wards", Version: 1, Terms: []string{"East Wing", "West Wing"}},
	}}, cat)
	to := mustExpand(t, &v1.ProfileDocument{Rules: rules, TextLists: []*v1.TextList{
		{Name: "staff", Version: 1, Terms: []string{"Ann Lee", "Bea Moss", "Cy Nolan"}},
		{Name: "nurses", Version: 1, Terms: []string{"Dee Ott"}},
		{Name: "units", Version: 2, Terms: []string{"Ward Nine"}},
		{Name: "wards", Version: 1, Terms: []string{"west  WING", "East Wing"}},
	}}, cat)
	d := profile.Diff(from, to)
	if d.GetSameEffect() || d.GetUnlistedChange() {
		t.Errorf("same effect %v, unlisted %v", d.GetSameEffect(), d.GetUnlistedChange())
	}
	want := []struct {
		name     string
		change   v1.ProfileChange
		from, to int32
	}{
		{"nurses", v1.ProfileChange_PROFILE_CHANGE_ADDED, 0, 1},
		{"sites", v1.ProfileChange_PROFILE_CHANGE_REMOVED, 1, 0},
		{"staff", v1.ProfileChange_PROFILE_CHANGE_CHANGED, 2, 3},
		{"units", v1.ProfileChange_PROFILE_CHANGE_CHANGED, 1, 1},
	}
	got := d.GetTextLists()
	if len(got) != len(want) {
		t.Fatalf("list changes %v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.GetName() != w.name || g.GetChange() != w.change || g.GetFrom().GetTerms() != w.from || g.GetTo().GetTerms() != w.to {
			t.Errorf("change %d: %v, want %+v", i, g, w)
		}
		if g.GetFrom() != nil && len(g.GetFrom().GetSha256()) != 64 || g.GetTo() != nil && len(g.GetTo().GetSha256()) != 64 {
			t.Errorf("change %d without checksums: %v", i, g)
		}
	}
	if got[2].GetFrom().GetSha256() == got[2].GetTo().GetSha256() || got[3].GetFrom().GetVersion() != 1 || got[3].GetTo().GetVersion() != 2 {
		t.Errorf("staff and units: %v %v", got[2], got[3])
	}
	if strings.Contains(strings.ToLower(d.String()), strings.ToLower(seededTerm)) || strings.Contains(d.String(), "Bea Moss") {
		t.Error("the comparison holds a term")
	}
	if same := profile.Diff(from, from); len(same.GetTextLists()) != 0 || !same.GetSameEffect() {
		t.Errorf("a version against itself: %v", same.GetTextLists())
	}
}

// TestListsAreInTheVersion guards the lists as part of a version: the
// expanded form holds them as written, of language edition 3, and a
// version with another term has another checksum; a document with no list
// keeps the expanded text and checksum it had before lists existed; a
// compiled version decides a clean naming lists with their bits; and a
// description gives each list's name, version, count and checksum, with
// no term in its text.
func TestListsAreInTheVersion(t *testing.T) {
	cat := testCatalog(t)
	doc := &v1.ProfileDocument{TextLists: docLists(), Rules: []*v1.ProfileRule{rl("d", kw("StudyDescription"), cleanLists("sites", "staff"))}}
	v, err := profile.New("lists", 1, doc, cat)
	if err != nil {
		t.Fatal(err)
	}
	e := v.Expanded()
	if e.GetLanguageEdition() != 3 || len(e.GetTextLists()) != 2 || e.GetTextLists()[0].GetTerms()[1] != seededTerm {
		t.Errorf("the expanded form: edition %d, lists %d", e.GetLanguageEdition(), len(e.GetTextLists()))
	}
	other := &v1.ProfileDocument{TextLists: docLists(), Rules: doc.GetRules()}
	other.TextLists[0].Terms = []string{"Ann Lee", "Someone Else"}
	v2, err := profile.New("lists", 2, other, cat)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Checksum() == v.Checksum() {
		t.Error("a version with another term has the same checksum")
	}
	plain := &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("d", kw("StudyDescription"), clean())}}
	pe := mustExpand(t, plain, cat)
	text, _, err := profile.Checksum(pe)
	if err != nil {
		t.Fatal(err)
	}
	if pe.GetLanguageEdition() != 1 || strings.Contains(string(text), "textLists") {
		t.Errorf("a document without lists: edition %d, %s", pe.GetLanguageEdition(), text)
	}
	prog, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	if d := prog.Decide(profile.Location{Tag: 0x00081030, VR: "LO"}, facts{}); d.Lists != 3 {
		t.Errorf("decided with lists %b", d.Lists)
	}
	if prog.TextLists().Len() != 2 {
		t.Errorf("%d lists compiled", prog.TextLists().Len())
	}
	sums := profile.DescribeTextLists(e)
	if len(sums) != 2 || sums[0].GetName() != "staff" || sums[0].GetTerms() != 2 || sums[1].GetVersion() != 2 || len(sums[0].GetSha256()) != 64 {
		t.Errorf("summaries %v", sums)
	}
	for _, s := range sums {
		if strings.Contains(strings.ToLower(s.String()), strings.ToLower(seededTerm)) {
			t.Errorf("a summary holds a term: %v", s)
		}
	}
	if profile.DescribeTextLists(pe) != nil {
		t.Error("summaries of a profile with no list")
	}
}
