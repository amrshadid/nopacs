package profile_test

import (
	"bytes"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// richChecksum is the checksum of richDocument expanded over testCatalog.
// It is written here, not computed, so a change in how a document expands
// or in its canonical text, from this package or from the protobuf
// library, fails this test instead of silently giving stored versions and
// new ones different checksums for the same rules.
const richChecksum = "105df9da5bc668b5c261900ec867a2e15955d615485f3560998eaf9d23711757"

// TestExpansionIsDeterministic guards that one document always expands to
// the same text and checksum: run after run, document copy after copy, and
// equal to the checksum written in this file.
func TestExpansionIsDeterministic(t *testing.T) {
	cat := testCatalog(t)
	first, sum, err := profile.Checksum(mustExpand(t, richDocument(), cat))
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		text, again, err := profile.Checksum(mustExpand(t, richDocument(), cat))
		if err != nil {
			t.Fatal(err)
		}
		if again != sum || !bytes.Equal(text, first) {
			t.Fatalf("checksum %s, then %s", sum, again)
		}
	}
	if sum != richChecksum {
		t.Errorf("checksum %s, want %s; the expanded text is:\n%s", sum, richChecksum, first)
	}
}

// TestKeywordsAreFrozenAsTags guards that a keyword is written as its tag
// in the expanded form (in a selector, a path and a template), so a rule
// written with a keyword and one written with the tag are the same version,
// and a later data dictionary cannot change which element a stored rule
// names.
func TestKeywordsAreFrozenAsTags(t *testing.T) {
	cat := testCatalog(t)
	byKeyword := &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("n", kw("PatientName"), remove()),
		rl("p", path("ReferencedStudySequence[*]/ReferencedSOPInstanceUID"), uid()),
		rl("c", kw("PatientComments"), replaceTemplate("S-{#PatientID}")),
		rl("g", grp("60XX"), remove()),
		rl("x", priv("ACME", "0019", "0c"), remove()),
	}}
	byTag := &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("n", tg("00100010"), remove()),
		rl("p", path("00081110[*]/00081155"), uid()),
		rl("c", tg("00104000"), replaceTemplate("S-{#00100020}")),
		rl("g", grp("60xx"), remove()),
		rl("x", priv("ACME", "0019", "0C"), remove()),
	}}
	_, a, _ := profile.Checksum(mustExpand(t, byKeyword, cat))
	_, b, _ := profile.Checksum(mustExpand(t, byTag, cat))
	if a != b {
		t.Errorf("by keyword %s, by tag %s", a, b)
	}
	e := mustExpand(t, byKeyword, cat)
	if got := e.GetRules()[0].GetRule().GetSelector().GetTag(); got != "00100010" {
		t.Errorf("the keyword is expanded as %q", got)
	}
	if got := e.GetRules()[2].GetRule().GetAction().GetReplace().GetTemplate(); got != "S-{#00100020}" {
		t.Errorf("the template is expanded as %q", got)
	}
}

// TestBasesExpandInOrder guards the order of an expansion: each base's own
// bases first, a base named again used once where it first comes, then the
// document's rules, each rule with the base it came from.
func TestBasesExpandInOrder(t *testing.T) {
	cat := testCatalog(t)
	// b is built on a, so naming a after b adds nothing and does not move a
	// after b, where it would override b.
	e := mustExpand(t, &v1.ProfileDocument{Bases: []string{baseB, baseA}, Rules: []*v1.ProfileRule{rl("own", kw("PatientName"), keep())}}, cat)
	if !slices.Equal(e.GetBases(), []string{baseA, baseB}) {
		t.Errorf("bases %v", e.GetBases())
	}
	var got []string
	for _, r := range e.GetRules() {
		got = append(got, r.GetOrigin()+"/"+r.GetRule().GetName())
	}
	want := []string{baseA + "/a-name", baseA + "/a-id", baseB + "/b-name", baseB + "/b-id", "/own"}
	if !slices.Equal(got, want) {
		t.Errorf("rules %v, want %v", got, want)
	}
	if len(e.GetPixelRules()) != 1 || e.GetPixelRules()[0].GetOrigin() != baseB {
		t.Errorf("pixel rules %v", e.GetPixelRules())
	}
}

// TestDefaultsTakePrecedence guards where an expanded profile's defaults
// come from: the document's own word, else the last base that says, else
// keep and 365 days; and that the expanded form always states them.
func TestDefaultsTakePrecedence(t *testing.T) {
	cat := testCatalog(t)
	for _, c := range []struct {
		name    string
		doc     *v1.ProfileDocument
		unnamed v1.UnnamedElements
		days    int32
	}{
		{"nothing says", &v1.ProfileDocument{}, v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP, profile.DefaultDateShiftDays},
		{"a base says", &v1.ProfileDocument{Bases: []string{baseB}}, v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE, 30},
		{"the document says", &v1.ProfileDocument{Bases: []string{baseB}, Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP, DateShiftDays: 7}, v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := mustExpand(t, c.doc, cat)
			if e.GetUnnamed() != c.unnamed || e.GetDateShiftDays() != c.days {
				t.Errorf("unnamed %v, days %d; want %v, %d", e.GetUnnamed(), e.GetDateShiftDays(), c.unnamed, c.days)
			}
		})
	}
}

// TestDescriptionIsNotBehavior guards that a document's description is
// not part of its expanded form, so two documents that differ only in
// their words have one checksum: equal checksums mean equal behavior.
func TestDescriptionIsNotBehavior(t *testing.T) {
	cat := testCatalog(t)
	a, b := richDocument(), richDocument()
	b.Description = "other words"
	_, sa, _ := profile.Checksum(mustExpand(t, a, cat))
	_, sb, _ := profile.Checksum(mustExpand(t, b, cat))
	if sa != sb {
		t.Errorf("checksums %s and %s", sa, sb)
	}
	b.Rules[0].Action = remove()
	if _, sc, _ := profile.Checksum(mustExpand(t, b, cat)); sc == sa {
		t.Error("a changed rule kept the checksum")
	}
}

// TestExpansionDoesNotShareTheCatalog guards that an expanded form is a
// copy: changing it changes neither the catalog's bases nor the next
// expansion.
func TestExpansionDoesNotShareTheCatalog(t *testing.T) {
	cat := testCatalog(t)
	e := mustExpand(t, &v1.ProfileDocument{Bases: []string{baseA}}, cat)
	e.GetRules()[0].GetRule().Name = "changed"
	e.GetRules()[0].GetRule().Selector = kw("PatientID")
	again := mustExpand(t, &v1.ProfileDocument{Bases: []string{baseA}}, cat)
	if again.GetRules()[0].GetRule().GetName() != "a-name" || again.GetRules()[0].GetRule().GetSelector().GetTag() != "00100010" {
		t.Errorf("the catalog was changed through an expanded form: %v", again.GetRules()[0])
	}
	b, _, _ := cat.Lookup(baseA)
	if b.Document.GetRules()[0].GetName() != "a-name" {
		t.Error("the base's document was changed")
	}
}

// TestDocumentIsNotChanged guards that expanding a document leaves it as
// given: normalization works on a copy.
func TestDocumentIsNotChanged(t *testing.T) {
	doc := richDocument()
	before := proto.Clone(doc)
	mustExpand(t, doc, testCatalog(t))
	if !proto.Equal(doc, before) {
		t.Error("Expand changed its document")
	}
}

// TestTooManyRulesWithBases guards the bound on an expanded profile's
// rules, which counts the bases' rules too.
func TestTooManyRulesWithBases(t *testing.T) {
	var rules []*v1.ProfileRule
	for range profile.MaxRules {
		rules = append(rules, rl("", kw("PatientName"), remove()))
	}
	var bases []profile.Base
	var names []string
	for i := range 10 {
		n := "test/big-" + string(rune('a'+i)) + ":1"
		bases = append(bases, profile.Base{Name: n, Document: &v1.ProfileDocument{Rules: rules}})
		names = append(names, n)
	}
	cat, err := profile.NewCatalog(bases...)
	if err != nil {
		t.Fatal(err)
	}
	_, problems := profile.Expand(&v1.ProfileDocument{Bases: names, Rules: rules[:1]}, cat)
	if len(problems) != 1 || problems[0].Code != profile.CodeTooMany || problems[0].Field != "bases" {
		t.Errorf("problems %v", problems)
	}
}
