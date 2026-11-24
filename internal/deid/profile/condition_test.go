package profile_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// manyPatterns returns a document of the most rules, each with the most
// conditions, every condition the pattern of(rule, condition).
func manyPatterns(of func(r, c int) string) *v1.ProfileDocument {
	doc := &v1.ProfileDocument{}
	for r := range profile.MaxRules {
		rule := rl("", kw("PatientName"), remove())
		for c := range profile.MaxConditions {
			rule.When = append(rule.When, pattern(modality, of(r, c)))
		}
		doc.Rules = append(doc.Rules, rule)
	}
	return doc
}

// TestPatternSizeIsBounded guards that a regular expression is bounded by
// what it compiles to, not by its text: a short pattern with a large
// repetition or a large character class repeated is refused before the
// regexp package compiles it, so one check cannot cost minutes and
// gigabytes.
func TestPatternSizeIsBounded(t *testing.T) {
	cat := testCatalog(t)
	for _, c := range []struct {
		pattern string
		ok      bool
	}{
		{`CT|MR`, true},
		{`[A-Z]{1,100}`, true},
		{`\pL+`, true},
		{`\pL{999}`, false},
		{`x{101}`, false},
		{`(?:a{10}){10}`, true},
		{`\pL{20}`, false},
		{`[\pL\pN]{14}`, false},
	} {
		doc := &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("PatientName"), remove(), pattern(modality, c.pattern))}}
		_, problems := profile.Expand(doc, cat)
		if c.ok != (len(problems) == 0) {
			t.Errorf("%s: problems %v, want ok %v", c.pattern, problems, c.ok)
		}
		if !c.ok && (len(problems) != 1 || problems[0].Code != profile.CodeBadPattern || problems[0].Field != "when[1].pattern") {
			t.Errorf("%s: problems %v", c.pattern, problems)
		}
	}
}

// TestPatternBudgetBoundsADocument guards that the patterns of a whole
// document are bounded together, counted for each condition, so the most
// rules with the most conditions, each pattern within its own bound, are
// refused in well under a second with one problem, never compiled whole.
func TestPatternBudgetBoundsADocument(t *testing.T) {
	cat := testCatalog(t)
	for _, c := range []struct {
		name string
		of   func(r, c int) string
	}{
		{"one large pattern repeated", func(int, int) string { return `\pL{7}` }},
		{"distinct patterns", func(r, c int) string { return fmt.Sprintf(`[A-Z]{50}%d`, r*profile.MaxConditions+c) }},
		{"patterns past their own bound", func(int, int) string { return `\pL{999}` }},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			_, problems := profile.Expand(manyPatterns(c.of), cat)
			if took := time.Since(start); took > 3*time.Second {
				t.Errorf("the check took %v", took)
			}
			tooLarge := 0
			for _, p := range problems {
				if p.Code == profile.CodeTooLarge {
					tooLarge++
				}
			}
			if len(problems) == 0 {
				t.Fatal("the document was accepted")
			}
			if problems[0].Code == profile.CodeTooLarge && tooLarge != 1 {
				t.Errorf("the budget's problem is given %d times", tooLarge)
			}
			// The budget stops the check at the condition that passes it,
			// not after every pattern was compiled.
			if p := problems[0]; p.Rule == 0 || !strings.HasSuffix(p.Field, ".pattern") {
				t.Errorf("the first problem is not at a condition: %v", p)
			}
		})
	}
	// Within the budget, a document of many small patterns is valid.
	small := manyPatterns(func(int, int) string { return `CT|MR` })
	if _, problems := profile.Expand(small, cat); len(problems) > 0 {
		t.Errorf("small patterns: %v", problems[0])
	}
}

// TestPatternMatchesTheWholeValue guards that a valid pattern compiled
// once still matches only the whole value.
func TestPatternMatchesTheWholeValue(t *testing.T) {
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("a", kw("PatientName"), remove(), pattern(modality, `C|CT`)),
		rl("b", kw("PatientID"), remove(), pattern(modality, `C|CT`)),
	}}, testCatalog(t))
	for v, want := range map[string]string{"CT": "remove", "C": "remove", "CTX": "keep", "XCT": "keep", strings.Repeat("C", 2): "keep"} {
		if got := actionName(p.Decide(profile.Location{Tag: patientName, VR: "PN"}, facts{modality: v}).Action); got != want {
			t.Errorf("%q: %s, want %s", v, got, want)
		}
	}
}
