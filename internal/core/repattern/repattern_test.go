package repattern

import (
	"strings"
	"testing"
)

// TestCompile guards the bounds and the whole-value match: a pattern that
// would escape its anchors, a long text, a large repetition and a large
// class are refused; a good one matches only a whole value.
func TestCompile(t *testing.T) {
	re, cost, err := Compile(`CT|MR`)
	if err != nil || cost == 0 {
		t.Fatalf("Compile: %v, cost %d", err, cost)
	}
	for v, want := range map[string]bool{"CT": true, "MR": true, "CTX": false, "XMR": false, "": false} {
		if re.MatchString(v) != want {
			t.Errorf("%q: match %v, want %v", v, !want, want)
		}
	}
	for _, bad := range []string{`a)|(b`, `(`, strings.Repeat("a", MaxLength+1), `x{101}`, `\pL{100}`} {
		if _, _, err := Compile(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestCompileSearch guards the unanchored form: it finds a match inside a
// value, the longest at its leftmost start, and refuses what Compile
// refuses, within the same bounds.
func TestCompileSearch(t *testing.T) {
	re, cost, err := CompileSearch(`MRN\d{6}`)
	if err != nil || cost == 0 {
		t.Fatalf("CompileSearch: %v, cost %d", err, cost)
	}
	if got := re.FindString("Head CT MRN123456 follow-up"); got != "MRN123456" {
		t.Errorf("found %q, want MRN123456", got)
	}
	for _, bad := range []string{`(`, strings.Repeat("a", MaxLength+1), `x{101}`, `\pL{100}`} {
		if _, _, err := CompileSearch(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	prefix, _, _ := CompileSearch(`MRN|MRN\d+`)
	if got := prefix.FindString("x MRN123456 y"); got != "MRN123456" {
		t.Errorf("a prefix alternation found %q, want the longest match MRN123456", got)
	}
	whole, wholeCost, _ := Compile(`MRN\d{6}`)
	if whole.MatchString("x MRN123456") || wholeCost != cost {
		t.Errorf("Compile and CompileSearch differ in cost (%d, %d) or Compile matched a part", wholeCost, cost)
	}
}
