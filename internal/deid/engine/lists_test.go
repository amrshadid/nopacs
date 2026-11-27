package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// tImageComments is Image Comments (0020,4000), LT.
const tImageComments = 0x00204000

// seededTerm is a term no other text of the tests holds, searched for in
// every printed form the engine produces.
const seededTerm = "Zebulonquist"

// staffAndSites are the lists of the tests: a person, a site, a term of
// a script written without spaces.
func staffAndSites() []*v1.TextList {
	return []*v1.TextList{
		{Name: "staff", Version: 1, Terms: []string{"Ann Lee", seededTerm, "Müller", "田中"}},
		{Name: "sites", Version: 4, Terms: []string{"North Clinic"}},
	}
}

// aCleanLists is a clean action naming lists.
func aCleanLists(lists ...string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{Lists: lists}}}
}

// aRedactLists is a redact action of a pattern, a replacement and lists.
func aRedactLists(p, repl string, lists ...string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: p, Replacement: repl, Lists: lists}}}
}

// newListEngine makes an Engine over a document of lists and rules.
func newListEngine(t testing.TB, lists []*v1.TextList, rules ...*v1.ProfileRule) *Engine {
	t.Helper()
	return newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{TextLists: lists, Rules: rules})
}

// TestCleanRemovesListTerms guards clean with lists: the terms of the
// lists a rule names are cut from its text where they stand as words, in
// any case, width and Unicode form, the rest kept as written; a term
// inside a longer word stays ("Ann Leeds"); a term of a script written
// without spaces is cut wherever it stands; a clean rule naming no list,
// and a list the rule does not name, leave the terms; and every file's
// record carries one needle of the version's lists, with no value.
func TestCleanRemovesListTerms(t *testing.T) {
	en := newListEngine(t, staffAndSites(),
		rule("desc", kw("StudyDescription"), aCleanLists("staff", "sites")),
		rule("comments", kw("PatientComments"), aClean),
		rule("image", kw("ImageComments"), aCleanLists("sites")),
	)
	ds := build(t,
		el{tSpecificCharacterSet, "CS", "ISO_IR 192"},
		el{tStudyDesc, "LO", "Annual CT by ann  lee at NORTH CLINIC; Ann Leeds"},
		el{tPatientComments, "LT", "seen by Ann Lee at North Clinic"},
		el{tImageComments, "LT", "Müller and ＭÜＬＬＥＲ at North Clinic, 田中先生"},
	)
	rec := apply(t, en, ds)
	for tg, want := range map[uint32]string{
		tStudyDesc:       "Annual CT by  at ; Ann Leeds",
		tPatientComments: "seen by Ann Lee at North Clinic",
		tImageComments:   "Müller and ＭÜＬＬＥＲ at , 田中先生",
	} {
		if got := mustText(t, ds, tg); got != want {
			t.Errorf("%08X: %q, want %q", tg, got, want)
		}
	}
	n := 0
	for _, nd := range rec.Needles {
		if nd.Kind == NeedleList {
			n++
			if nd.Value != "" || nd.Lists == nil || nd.Lists != en.lists || nd.Lists.Len() != 2 {
				t.Errorf("the lists' needle: %q, %v", nd.Value, nd.Lists)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d needles of the lists", n)
	}

	// A file the lists' rules do not touch still carries the needle, so the
	// check after searches it for every term.
	other := build(t, el{tModality, "CS", "CT"})
	if rec := apply(t, en, other); len(rec.Needles) != 1 || rec.Needles[0].Kind != NeedleList {
		t.Errorf("needles of a file without text: %v", rec.Needles)
	}
}

// TestRedactCutsListTerms guards redact with lists: the terms of its lists
// are cut with the pattern's matches, each becoming the replacement, terms
// that overlap cut as one; a redact of lists alone cuts the terms; the
// record counts every cut, and no needle is made of a term (the lists'
// needle covers them).
func TestRedactCutsListTerms(t *testing.T) {
	en := newListEngine(t, staffAndSites(),
		rule("image", kw("ImageComments"), aRedactLists(`MRN\d{4}`, "[x]", "staff")),
		rule("desc", kw("StudyDescription"), aRedactLists("", "-", "sites")),
	)
	ds := build(t,
		el{tImageComments, "LT", "Ann Lee MRN1234 and müller; Ann Leeds"},
		el{tStudyDesc, "LO", "CT at North Clinic"},
	)
	rec := apply(t, en, ds)
	if got := mustText(t, ds, tImageComments); got != "[x] [x] and [x]; Ann Leeds" {
		t.Errorf("redacted %q", got)
	}
	if got := mustText(t, ds, tStudyDesc); got != "CT at -" {
		t.Errorf("redacted by lists alone %q", got)
	}
	var redactions int
	for _, e := range rec.Entries {
		if e.Tag == tImageComments {
			redactions = e.Redactions
		}
	}
	if redactions != 3 {
		t.Errorf("%d redactions", redactions)
	}
	for _, n := range rec.Needles {
		if n.Kind == NeedleText && strings.Contains(strings.ToLower(n.Value), "lee") {
			t.Errorf("a list's term became a needle: %v", n.Kind)
		}
	}
}

// TestListTermsAreNeverPrinted guards every printed form the engine makes
// of a file de-identified with lists, the term in values cleaned, redacted
// and kept: the record in every verb, its entries, needles and unnamed
// elements, an explanation, and the error of a refused file hold no term.
func TestListTermsAreNeverPrinted(t *testing.T) {
	rules := []*v1.ProfileRule{
		rule("desc", kw("StudyDescription"), aCleanLists("staff")),
		rule("image", kw("ImageComments"), aRedactLists(`MRN\d{4}`, "", "staff")),
		rule("block", privSel("ACME "+seededTerm, "0009", "xx"), aRemove),
	}
	en := newListEngine(t, staffAndSites(), rules...)
	file := func() []el {
		return []el{
			{tStudyDesc, "LO", "by " + seededTerm},
			{tImageComments, "LT", seededTerm + " MRN1234"},
			{tPatientComments, "LT", "kept " + seededTerm},
			{0x00090010, "LO", "ACME " + seededTerm},
			{0x00091001, "LO", "private " + seededTerm},
			{0x00110010, "LO", seededTerm + " Corp"},
			{0x00111001, "LO", "unnamed"},
		}
	}
	rec := apply(t, en, build(t, file()...))
	expl, err := en.Explain(context.Background(), build(t, file()...), Reception{})
	if err != nil {
		t.Fatal(err)
	}
	quarantining := newListEngine(t, staffAndSites(), append(rules[:2:2], rule("q", kw("PatientComments"), aQuarantine))...)
	_, refusal := quarantining.Apply(context.Background(), build(t, file()...), Reception{})
	if refusal == nil {
		t.Fatal("no refusal")
	}
	var surfaces []string
	for _, v := range []string{"%v", "%+v", "%#v", "%s"} {
		surfaces = append(surfaces, fmt.Sprintf(v, rec), fmt.Sprintf(v, *rec), fmt.Sprintf(v, rec.Entries), fmt.Sprintf(v, rec.Needles),
			fmt.Sprintf(v, expl), fmt.Sprintf(v, refusal), fmt.Sprintf(v, en.lists))
	}
	unreadable := 0
	for _, u := range rec.Unnamed {
		surfaces = append(surfaces, u.String(), u.Creator)
		if u.CreatorUnreadable {
			unreadable++
		}
	}
	if unreadable != 1 {
		t.Errorf("%d creators not given, want the one holding a term", unreadable)
	}
	surfaces = append(surfaces, refusal.Error())
	for _, s := range surfaces {
		if strings.Contains(strings.ToLower(s), strings.ToLower(seededTerm)) {
			t.Fatalf("a printed form holds the term: %.300s", s)
		}
	}
}

// TestListUsesAreRefusedWhole guards that a version is not made, and a
// stored form not compiled, with a use of lists it cannot apply: a list
// the profile lacks, a list named twice, a redact whose replacement holds
// a term, a version of an edition before lists that holds one. A profile
// is refused, never run without the list it names.
func TestListUsesAreRefusedWhole(t *testing.T) {
	for _, c := range []struct {
		name string
		rule *v1.ProfileRule
		code profile.Code
	}{
		{"unknown", rule("desc", kw("StudyDescription"), aCleanLists("nurses")), profile.CodeUnknownList},
		{"twice", rule("desc", kw("StudyDescription"), aCleanLists("staff", "staff")), profile.CodeDuplicateName},
		{"replacement", rule("desc", kw("StudyDescription"), aRedactLists("", "Ann Lee", "staff")), profile.CodeBadValue},
		{"no pattern nor list", rule("desc", kw("StudyDescription"), aRedactLists("", "")), profile.CodeRequired},
	} {
		_, problems := profile.Expand(&v1.ProfileDocument{TextLists: staffAndSites(), Rules: []*v1.ProfileRule{c.rule}}, profile.Shipped())
		if len(problems) == 0 || problems[0].Code != c.code {
			t.Errorf("%s: %v", c.name, problems)
		}
	}
	e, problems := profile.Expand(&v1.ProfileDocument{TextLists: staffAndSites(), Rules: []*v1.ProfileRule{
		rule("desc", kw("StudyDescription"), aCleanLists("staff")),
	}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	if e.GetLanguageEdition() != 3 {
		t.Errorf("a version with lists is of edition %d", e.GetLanguageEdition())
	}
	e.LanguageEdition = 2
	if _, err := New(e, testKeys, Options{}); err == nil || !strings.Contains(err.Error(), string(profile.CodeBadEdition)) {
		t.Errorf("an edition 2 form with lists: %v", err)
	}
}

// TestListCleanIsBounded guards the bound of the lists' work: a file whose
// texts make terms end at every character, far past the work a file may
// take, is refused as too_large and left as it was, never searched for
// minutes.
func TestListCleanIsBounded(t *testing.T) {
	terms := make([]string, 0, 200)
	for n := 2; n < 202; n++ {
		terms = append(terms, strings.Repeat("q", n))
	}
	en := newListEngine(t, []*v1.TextList{{Name: "q", Version: 1, Terms: terms}}, rule("text", kw("TextValue"), aCleanLists("q")))
	items := make([]*dataset.Dataset, 40)
	for i := range items {
		items[i] = build(t, el{0x0040A160, "UT", strings.Repeat("q", 60000) + " x"})
	}
	ds := build(t, el{tContentSequence, "SQ", items})
	before := writeFile(t, ds)
	_, err := en.Apply(context.Background(), ds, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseTooLarge {
		t.Fatalf("a file past the lists' work: %v", err)
	}
	if string(before) != string(writeFile(t, ds)) {
		t.Error("a refused file was changed")
	}
}

// TestRedactOfListsAlone guards the cut of a redact that names lists and
// no pattern: its terms are cut, each becoming the replacement, a person's
// name component by component, and a value with no term is left as it
// came.
func TestRedactOfListsAlone(t *testing.T) {
	lists, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Ann Lee", "Müller"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	mask, _ := lists.Mask([]string{"staff"})
	r := &run{en: &Engine{lists: lists}, ctx: context.Background()}
	cutter := r.listCutter(Action{Kind: ActRedact, Lists: mask, Replacement: "X"})
	for _, c := range []struct{ vr, in, want string }{
		{"LT", "by Ann Lee and Müller", "by X and X"},
		{"PN", "Müller^Ann Lee", "X^X"},
		{"LO", "nothing here", "nothing here"},
	} {
		out, _, count, cause := redactValue(nil, "X", c.vr, c.in, func(int) bool { return true }, cutter)
		if cause != "" {
			t.Fatalf("%q: %s", c.in, cause)
		}
		if count > 0 && out != c.want || count == 0 && c.in != c.want {
			t.Errorf("%q: %q (%d cut), want %q", c.in, out, count, c.want)
		}
	}
}
