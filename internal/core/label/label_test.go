package label

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCheck guards each fault of the rule, at its edges, and that the fault
// reported is the one named.
func TestCheck(t *testing.T) {
	long := strings.Repeat("a", MaxLength)
	for _, c := range []struct {
		in   string
		want Fault // 0: accepted
	}{
		{"a", 0},
		{"Sub-01_x.y", 0},
		{"20240101-1.2.840.10008.1", 0},
		{"-leading-dash", 0},
		{"_", 0},
		{"a.b.c", 0},
		{long, 0},
		{"", Empty},
		{long + "a", TooLong},
		{".hidden", LeadingDot},
		{".", LeadingDot},
		{"..", LeadingDot},
		{"a.", TrailingDot},
		{"a..", TrailingDot},
		{"a b", BadCharacter},
		{"a/b", BadCharacter},
		{"a\\b", BadCharacter},
		{"a:b", BadCharacter},
		{"a\x00", BadCharacter},
		{"Müller", BadCharacter},
		{"a^b", BadCharacter},
		{"a*", BadCharacter},
		{"CON", DeviceName},
		{"con", DeviceName},
		{"cOn.txt", DeviceName},
		{"NUL.tar.gz", DeviceName},
		{"aux", DeviceName},
		{"PRN", DeviceName},
		{"COM0", DeviceName},
		{"com9", DeviceName},
		{"LPT1.x", DeviceName},
		{"lpt0", DeviceName},
		{"COM", 0},
		{"COM10", 0},
		{"CONSOLE", 0},
		{"NULL", 0},
		{"xCON", 0},
		{"LPT", 0},
		{"CON_", 0},
		{"COM¹", BadCharacter},
	} {
		err := Check(c.in)
		if c.want == 0 {
			if err != nil {
				t.Errorf("%q: refused: %v", c.in, err)
			}
			continue
		}
		var le *Error
		if !errors.As(err, &le) || le.Fault != c.want || le.Value != c.in {
			t.Errorf("%q: got %v, want fault %v", c.in, err, c.want)
		}
	}
}

// TestErrorHidesValue guards the rule that an error's text never carries the
// refused value, which may be a patient's identifier.
func TestErrorHidesValue(t *testing.T) {
	for _, v := range []string{"Jane Doe", ".JaneDoe", "JaneDoe.", "CON.JaneDoe", strings.Repeat("JaneDoe", 30)} {
		err := Check(v)
		if err == nil {
			t.Fatalf("%q accepted", v)
		}
		if strings.Contains(err.Error(), "Jane") {
			t.Errorf("%q: text %q carries the value", v, err.Error())
		}
	}
}

// TestIsDeviceNameSuperscript guards that the superscript digits Windows
// treats as digits in COM and LPT names are recognized as reserved.
func TestIsDeviceNameSuperscript(t *testing.T) {
	for _, s := range []string{"COM¹", "com²", "LPT³", "lpt¹.txt"} {
		if !IsDeviceName(s) {
			t.Errorf("%q not reserved", s)
		}
	}
	for _, s := range []string{"COM⁴", "COM¹¹", "LPT₁"} {
		if IsDeviceName(s) {
			t.Errorf("%q reserved", s)
		}
	}
}

// TestFold guards that labels differing only in case fold to one form and
// labels differing otherwise do not.
func TestFold(t *testing.T) {
	if Fold("Sub01-A.b") != Fold("sUB01-a.B") {
		t.Error("case variants fold apart")
	}
	if Fold("sub01") == Fold("sub02") {
		t.Error("different labels fold together")
	}
}

// safeSegment is an oracle written from the platforms' rules, not from
// Check: whether s is safe as one path segment on Linux (no "/" or NUL, not
// "." or ".."), on macOS (no ":", which Finder shows as "/"), and on Windows
// (Microsoft's "Naming Files, Paths, and Namespaces": none of <>:"/\|?*, no
// byte below 32, no trailing space or period, no reserved device name with
// or without an extension), and no longer than the 255 bytes the common file
// systems allow.
func safeSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	if strings.ContainsAny(s, "<>:\"/\\|?*") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] == 127 {
			return false
		}
	}
	if strings.HasSuffix(s, " ") || strings.HasSuffix(s, ".") {
		return false
	}
	base := strings.ToUpper(s)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	reserved := []string{"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"}
	for _, p := range []string{"COM", "LPT"} {
		for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			reserved = append(reserved, p+d)
		}
	}
	for _, r := range reserved {
		if base == r {
			return false
		}
	}
	return true
}

// randomCandidate draws a string that is often near a boundary of the rule:
// built from the label alphabet, a few hostile bytes, and reserved names.
func randomCandidate(r *rand.Rand) string {
	parts := []string{"a", "Z", "0", "9", ".", "_", "-", "..", "CON", "nul", "Com1", "lpt9", "LPT¹",
		" ", "/", "\\", ":", "\x00", "\x1f", "é", "*", "?", "$"}
	var b strings.Builder
	for n := r.IntN(8); n >= 0; n-- {
		b.WriteString(parts[r.IntN(len(parts))])
	}
	if r.IntN(50) == 0 {
		b.WriteString(strings.Repeat("x", r.IntN(200)))
	}
	return b.String()
}

// TestAcceptedIsSafeSegment guards the property the rule exists for: every
// label Check accepts is a safe single path segment on Linux, macOS and
// Windows, by the oracle above, over many drawn candidates.
func TestAcceptedIsSafeSegment(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a fixed seed, so a failure is reproduced; nothing secret is drawn.
	accepted := 0
	for range 200000 {
		s := randomCandidate(r)
		if Valid(s) {
			accepted++
			if !safeSegment(s) {
				t.Fatalf("%q accepted but not a safe segment", s)
			}
		}
	}
	if accepted < 1000 {
		t.Fatalf("only %d candidates accepted; the generator does not reach the rule", accepted)
	}
}

// FuzzCheck guards the same property on inputs the fuzzer finds, and that
// Check never panics.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{"a", "CON", "con.txt", ".a", "a.", "COM¹", "x\x00", strings.Repeat("a", 129)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if Valid(s) && !safeSegment(s) {
			t.Fatalf("%q accepted but not a safe segment", s)
		}
	})
}
