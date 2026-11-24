package profile_test

import (
	"slices"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestValuePatternsMatchNFC guards that a value pattern matches the NFC
// form of what it reads: a pattern that writes Müller composed selects a
// value that writes it with a combining mark (u and U+0308), whole or in a
// search, and answers the composed text; an ASCII value is matched as it
// was.
func TestValuePatternsMatchNFC(t *testing.T) {
	cat := testCatalog(t)
	whole := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("w", valuePat("Müller", false, ""), remove())}}, cat)
	search := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("s", valuePat(`(?i)m\x{fc}ller|MRN\d+`, true, ""), remove())}}, cat)
	at := func(value string) profile.Location { return profile.Location{Tag: 0x00081030, VR: "LO", Value: value} }
	for _, c := range []struct {
		name    string
		p       *profile.Program
		value   string
		matches []string
	}{
		{"whole, decomposed", whole, "Müller", []string{"Müller"}},
		{"whole, composed", whole, "Müller", []string{"Müller"}},
		{"search, decomposed", search, "seen by MÜLLER today", []string{"MÜLLER"}},
		{"search, ASCII", search, "Head CT MRN123456", []string{"MRN123456"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.p.Decide(at(c.value), facts{})
			if d.Index != 0 || !slices.Equal(d.PatternMatches, c.matches) {
				t.Errorf("rule %d, matches %q; want %q", d.Index, d.PatternMatches, c.matches)
			}
		})
	}
	if got := profile.NFC("Müller"); got != "Müller" {
		t.Errorf("NFC gave %q", got)
	}
	if got := profile.NFD("Müller"); got != "Müller" {
		t.Errorf("NFD gave %q", got)
	}
}

// TestDecomposedPatternRefused guards that a value pattern or a redact
// pattern written decomposed ("u" then U+0308) is refused as bad_pattern,
// whole or in a search: values are matched in NFC, so such a pattern would
// silently match nothing, cut nothing and leave no needle. The same
// pattern written composed, and one that names the mark by an ASCII
// escape, are accepted.
func TestDecomposedPatternRefused(t *testing.T) {
	cat := testCatalog(t)
	one := func(r *v1.ProfileRule) *v1.ProfileDocument { return &v1.ProfileDocument{Rules: []*v1.ProfileRule{r}} }
	const decomposed, composed = "Müller", "Müller"
	for _, c := range []struct {
		name, field string
		doc         *v1.ProfileDocument
	}{
		{"whole value pattern", "selector.valuePattern.pattern", one(rl("", valuePat(decomposed, false, ""), remove()))},
		{"search value pattern", "selector.valuePattern.pattern", one(rl("", valuePat("(?i)"+decomposed, true, ""), remove()))},
		{"redact", "action.redact.pattern", one(rl("", kw("StudyDescription"), redactAct(decomposed, "")))},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, problems := profile.Expand(c.doc, cat)
			if len(problems) != 1 || problems[0].Code != profile.CodeBadPattern || problems[0].Field != c.field || !strings.Contains(problems[0].Message, "NFC") {
				t.Errorf("want one bad_pattern at %s naming NFC, got %v", c.field, problems)
			}
		})
	}
	for _, doc := range []*v1.ProfileDocument{
		one(rl("", valuePat(composed, true, ""), remove())),
		one(rl("", kw("StudyDescription"), redactAct(composed, ""))),
		one(rl("", kw("StudyDescription"), redactAct(`u\x{308}`, ""))),
	} {
		if _, problems := profile.Expand(doc, cat); len(problems) > 0 {
			t.Errorf("refused: %v", problems)
		}
	}
}
