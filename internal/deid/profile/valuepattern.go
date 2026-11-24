package profile

import (
	"io"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
)

// NFC returns s in Unicode Normalization Form C (Unicode Standard Annex
// #15): each letter written with combining marks ("u" and U+0308) as its
// composed character ("ü") where Unicode has one. De-identification
// compares text in this one form, the form most systems write: a value
// pattern matches it, redact cuts it, clean removes the terms of a file
// from it, and the check after searches it, so a name received decomposed
// is found and removed as its composed form is. Text already in the form,
// as all ASCII is, is returned as it is. Bytes that are not UTF-8 are kept
// as they are.
func NFC(s string) string { return norm.NFC.String(s) }

// NFCReader returns a reader of r's text in NFC, as NFC gives it, for a
// text too long to hold whole.
func NFCReader(r io.Reader) io.Reader { return norm.NFC.Reader(r) }

// NFD returns s in Unicode Normalization Form D: each composed character
// written as its letter and combining marks. The check after searches the
// raw bytes of a file for a name in this form as well as in NFC, since
// bytes no reader decoded are not normalized.
func NFD(s string) string { return norm.NFD.String(s) }

// Bounds of a value pattern and of redact.
const (
	// MaxPatternValue is the longest value, in bytes, a value pattern
	// reads and redact cuts: a value pattern does not select a longer one,
	// and redact refuses the file. RE2 runs in time linear in the value,
	// so the bound keeps one element's work small whatever the pattern.
	MaxPatternValue = 64 << 10
	// MaxRedactMatches is the most matches redact cuts from one value; a
	// value with more refuses the file. It bounds the passes redact makes
	// over a value, each linear in its length.
	MaxRedactMatches = 256
	// MaxReplacementLength is the bound of redact's replacement.
	MaxReplacementLength = 64
	// valuePatternEdition is the language edition that has value patterns
	// and redact. A document that uses neither is expanded as edition 1,
	// so what it does and its checksum stay as they were before either
	// existed.
	valuePatternEdition = 2
)

// valueVRs are the VRs a value pattern reads and redact cuts: those whose
// text the file's character set governs (PS3.5 6.1.2.3), in which a name
// or an identifier is written as text. A coded string, a number, a date or
// a UID is not free text a person types an identifier into.
var valueVRs = []string{"SH", "LO", "UC", "ST", "LT", "UT", "PN"}

// singlePartVRs hold one value, in which a backslash is text and not a
// separator of values (PS3.5 6.2).
var singlePartVRs = []string{"ST", "LT", "UT"}

// valuePattern is a compiled ValuePatternSelector.
type valuePattern struct {
	re     *regexp.Regexp
	search bool
}

// parseValuePattern compiles a value pattern selector and returns it with
// its normalized form and the VRs it reads. The pattern is compiled whole
// or for a search, within the bounds and budget of a condition's pattern.
func parseValuePattern(a at, v *v1.ValuePatternSelector) (valuePattern, *v1.ValuePatternSelector, []string, bool) {
	before := len(a.c.list)
	var vrs []string
	switch vr := v.GetVr(); {
	case vr == "":
		vrs = slices.Clone(valueVRs)
	case slices.Contains(valueVRs, vr):
		vrs = []string{vr}
	default:
		a.in("vr").add(CodeBadVR, "a value pattern reads the text of VR %s; %q is not one", strings.Join(valueVRs, ", "), clipText(vr))
	}
	var re *regexp.Regexp
	pa := a.in("pattern")
	switch {
	case v.GetPattern() == "":
		pa.add(CodeRequired, "a value pattern needs its pattern")
	case !patternInNFC(pa, v.GetPattern()):
	case v.GetSearch():
		if re = compileSearch(pa, v.GetPattern()); re != nil && re.MatchString("") {
			pa.add(CodeBadPattern, "a search for a pattern that matches the empty string would match every value")
			re = nil
		}
	default:
		re = compilePattern(pa, v.GetPattern())
	}
	if len(a.c.list) != before {
		return valuePattern{}, nil, nil, false
	}
	norm := &v1.ValuePatternSelector{Pattern: v.GetPattern(), Search: v.GetSearch(), Vr: v.GetVr()}
	return valuePattern{re: re, search: v.GetSearch()}, norm, vrs, true
}

// ValueParts splits a text value of VR vr into the parts redact cuts one
// by one, as written: the values of a multi-valued one, separated by a
// backslash (PS3.5 6.4); an ST, LT or UT value is one part. A value
// pattern matches each part with its padding removed (matchParts).
func ValueParts(vr, value string) []string {
	if slices.Contains(singlePartVRs, vr) {
		return []string{value}
	}
	return strings.Split(value, `\`)
}

// matchParts returns the parts of a value a pattern matches: each value
// of a multi-valued one with its padding removed as PS3.5 6.2 allows for
// its VR (leading spaces too for SH and LO), so "HEAD\ 123456" is the
// values "HEAD" and "123456".
func matchParts(vr, value string) []string {
	parts := ValueParts(vr, value)
	for i, p := range parts {
		parts[i] = naming.TrimValue(vr, p)
	}
	return parts
}

// find returns what the pattern matches in a value of VR vr, each text
// once: each whole part it matches, or for a search each text it finds
// (leftmost-longest, empty matches left out). over is true when there are
// more than MaxRedactMatches distinct texts; the caller then refuses the
// file rather than keep only some as needles. Each part is searched in
// one pass, linear in its length.
//
// The value is put in NFC first: a pattern matches the composed form of
// what it reads, so "Müller" written with a combining mark is matched by
// a pattern that writes it composed, and the texts it answers are in NFC.
func (p valuePattern) find(vr, value string) (matches []string, over bool) {
	value = NFC(value)
	seen := map[string]bool{}
	add := func(m string) bool {
		if m == "" || seen[m] {
			return true
		}
		if len(matches) == MaxRedactMatches {
			return false
		}
		seen[m] = true
		matches = append(matches, m)
		return true
	}
	for _, part := range matchParts(vr, value) {
		if part == "" {
			continue
		}
		if !p.search {
			if p.re.MatchString(part) && !add(part) {
				return matches, true
			}
			continue
		}
		for _, m := range p.re.FindAllStringIndex(part, -1) {
			if !add(part[m[0]:m[1]]) {
				return matches, true
			}
		}
	}
	return matches, false
}

// patternInNFC reports whether a value pattern or redact pattern is written
// in NFC, and refuses it (bad_pattern) when it is not. Both run over the
// NFC form of a value (find, the engine's redact), so a pattern written
// decomposed ("u" then U+0308) would never match what it reads: the rule
// would select nothing and redact would cut nothing, silently, and no
// needle would exist for the check after to find a copy by. An escape such
// as \x{308} is ASCII and stays allowed; it is the writer's own choice of
// code points. A replacement needs no such check: it is printable ASCII
// (checkLiteral), which is always in NFC.
func patternInNFC(a at, p string) bool {
	if norm.NFC.IsNormalString(p) {
		return true
	}
	a.add(CodeBadPattern, "the pattern is matched against values in Unicode NFC (composed) form, so it must be written in NFC too: write each accented letter as its composed character (\"ü\", not \"u\" followed by a combining mark)")
	return false
}

// checkRedact checks a redact action against its selector and compiles
// its pattern for a search.
func checkRedact(a at, r *v1.RedactAction, sel selector) *regexp.Regexp {
	a = a.in("redact")
	fits(a, sel, valueVRs, "redact")
	pa := a.in("pattern")
	var re *regexp.Regexp
	if r.GetPattern() == "" {
		pa.add(CodeRequired, "redact needs the pattern of what it cuts")
	} else if !patternInNFC(pa, r.GetPattern()) {
		re = nil
	} else if re = compileSearch(pa, r.GetPattern()); re != nil && re.MatchString("") {
		pa.add(CodeBadPattern, "the pattern matches the empty string, so it would match everywhere and cut nothing")
		re = nil
	}
	ra := a.in("replacement")
	repl := r.GetReplacement()
	switch {
	case len(repl) > MaxReplacementLength:
		ra.add(CodeTooLong, "a replacement is at most %d bytes", MaxReplacementLength)
		return nil
	case !checkLiteral(ra, repl, "a replacement"):
		return nil
	case repl != "" && slices.Contains(sel.vrs, "PN") && strings.ContainsAny(repl, "^="):
		ra.add(CodeBadValue, "a replacement in a person's name holds no ^ or =, which would change the name's components (PS3.5 6.2.1)")
		return nil
	case repl != "" && re != nil && re.MatchString(repl):
		ra.add(CodeBadValue, "the pattern matches the replacement, so what redact writes would be cut again")
		return nil
	}
	if repl != "" && !a.c.stored {
		for _, vr := range sel.vrs {
			if !checkWritten(ra, vr, repl, "the replacement") {
				return nil
			}
		}
	}
	return re
}

// usesValuePatterns reports whether a rule has a value pattern or redact,
// which only language edition 2 has.
func usesValuePatterns(r *v1.ProfileRule) bool {
	return r.GetSelector().GetValuePattern() != nil || r.GetAction().GetRedact() != nil
}
