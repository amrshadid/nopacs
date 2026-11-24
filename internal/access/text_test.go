package access

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAuditText guards the one function that cleans caller-controlled text
// for an audit entry: control characters, newline, carriage return, escape,
// DEL, C1 controls, the line separators and the bidirectional controls are
// written as visible escapes; the backslash is doubled so an escape is
// always ours; text that is not UTF-8 is replaced; and the result is cut at
// a whole character or escape, never in the middle of one.
func TestAuditText(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		max      int
	}{
		{"plain text", "plain text", 64},
		{"Zoë 山田", "Zoë 山田", 64},
		{"a\nb", `a\nb`, 64},
		{"a\r\nInjected: line", `a\r\nInjected: line`, 64},
		{"tab\there", `tab\there`, 64},
		{"esc\x1b[31mred", `esc\x1b[31mred`, 64},
		{"nul\x00", `nul\x00`, 64},
		{"del\x7f", `del\x7f`, 64},
		{"c1\u0085", `c1\x85`, 64},
		{"rlo\u202eevil", `rlo\u202eevil`, 64},
		{"isolate\u2066x\u2069", `isolate\u2066x\u2069`, 64},
		{"marks\u200e\u200f\u061c", `marks\u200e\u200f\u061c`, 64},
		{"sep\u2028\u2029", `sep\u2028\u2029`, 64},
		{`back\slash`, `back\\slash`, 64},
		{"bad\xffutf8", "bad\ufffdutf8", 64},
		{"abcdef", "abc", 3},
		{"ab\ncd", "ab", 3},     // the escape \n does not fit whole after "ab"
		{"ab\ncd", `ab\n`, 4},   // it does here
		{"aé", "a", 2},          // é is two bytes and does not fit
		{"\u202e", "", 5},       // its escape is six bytes
		{"\u202e", `\u202e`, 6}, // and fits in six
	} {
		got := auditText(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("auditText(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
		if len(got) > tc.max || !utf8.ValidString(got) {
			t.Errorf("auditText(%q, %d) = %q: over the bound or not UTF-8", tc.in, tc.max, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || escapedRune(r) || (r >= 0x80 && r < 0xa0) {
				t.Errorf("auditText(%q) left %U in %q", tc.in, r, got)
			}
		}
	}
	long := strings.Repeat("x", 300)
	if got := auditText(long, 256); len(got) != 256 {
		t.Errorf("a long plain text is cut to %d bytes, want 256", len(got))
	}
}
