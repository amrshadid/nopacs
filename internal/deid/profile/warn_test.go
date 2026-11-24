package profile_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// warnCatalog has a base that keeps Patient's Sex, removes Patient's
// Birth Date and hashes Patient ID by their tags, and removes a vendor's
// private block.
func warnCatalog(t *testing.T) *profile.Catalog {
	t.Helper()
	c, err := profile.NewCatalog(profile.Base{Name: "test/base:1", Description: "base", Document: &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("sex", kw("PatientSex"), keep()),
		rl("birth", kw("PatientBirthDate"), remove()),
		rl("id", kw("PatientID"), hash()),
		rl("acme", priv("ACME", "0019", "xx"), remove()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// warningsOf expands a document over warnCatalog and returns its warnings.
func warningsOf(t *testing.T, rules ...*v1.ProfileRule) profile.Problems {
	t.Helper()
	w, err := profile.Warnings(mustExpand(t, &v1.ProfileDocument{Bases: []string{"test/base:1"}, Rules: rules}, warnCatalog(t)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestWarnings guards the warnings a valid document gets: a broad rule a
// more specific base rule overrides for some elements, and a rule that
// keeps what a base changes or removes; and none where the document means
// what it does.
func TestWarnings(t *testing.T) {
	for _, c := range []struct {
		name  string
		rules []*v1.ProfileRule
		codes string
		says  string
	}{
		{"a group shadowed by a base's tag", []*v1.ProfileRule{rl("patient", grp("0010"), remove())}, "shadowed_by_base", "00100040 (test/base:1, keep)"},
		{"a vr shadowed by a base's tag", []*v1.ProfileRule{rl("dates", vr("DA"), shift())}, "shadowed_by_base", "00100030 (test/base:1, remove)"},
		{"a keep of what a base removes", []*v1.ProfileRule{rl("birth", kw("PatientBirthDate"), keep())}, "keeps_base_change", "00100030 (test/base:1, remove)"},
		{"a keep of what a base hashes", []*v1.ProfileRule{rl("id", tg("00100020"), keep())}, "keeps_base_change", "00100020 (test/base:1, hash)"},
		{"a keep of a block a base removes", []*v1.ProfileRule{rl("acme", priv("ACME", "0019", "10"), keep())}, "keeps_base_change", `"ACME" 0019,xxxx`},
		{"a group keep both shadowed and keeping", []*v1.ProfileRule{rl("all", grp("0010"), keep())}, "shadowed_by_base", "00100030"},
		{"the same action as the base", []*v1.ProfileRule{rl("sex", kw("PatientSex"), keep())}, "", ""},
		{"a rule the bases do not touch", []*v1.ProfileRule{rl("desc", kw("StudyDescription"), clean())}, "", ""},
		{"a remove where the base keeps, equally specific", []*v1.ProfileRule{rl("sex", kw("PatientSex"), remove())}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := warningsOf(t, c.rules...)
			var codes []string
			for _, p := range w {
				codes = append(codes, string(p.Code))
			}
			if got := strings.Join(codes, ","); !strings.HasPrefix(got, c.codes) || (c.codes == "" && got != "") {
				t.Fatalf("codes %q, want %q: %v", got, c.codes, w)
			}
			if c.codes == "" {
				return
			}
			if w[0].Rule != 1 || w[0].RuleName != c.rules[0].GetName() || !strings.Contains(w[0].Message, c.says) {
				t.Errorf("warning %v, want rule 1 (%s) saying %q", w[0], c.rules[0].GetName(), c.says)
			}
		})
	}
}

// TestWarningsAreBounded guards that the warnings stop at their bound,
// with one more saying how many were left out, so an answer stays small.
func TestWarningsAreBounded(t *testing.T) {
	var rules []*v1.ProfileRule
	for range profile.MaxWarnings + 7 {
		rules = append(rules, rl("", kw("PatientBirthDate"), keep()))
	}
	w := warningsOf(t, rules...)
	if len(w) != profile.MaxWarnings+1 || w[len(w)-1].Code != profile.CodeMoreWarnings || !strings.HasPrefix(w[len(w)-1].Message, "7 more") {
		t.Errorf("%d warnings, the last %v", len(w), w[len(w)-1])
	}
}
