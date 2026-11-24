package engine

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/dataelem"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// patternVRs are the VRs a value pattern reads and redact cuts: the text
// the file's character set governs (PS3.5 6.1.2.3), as the profile
// language lists them.
var patternVRs = map[string]bool{"SH": true, "LO": true, "UC": true, "ST": true, "LT": true, "UT": true, "PN": true}

// maxPatternWork bounds what one file's value patterns and redacts may
// run: the bytes each reads times its compiled size (repattern.Measure),
// summed over the file. RE2 is linear in the value, but its constant is
// the pattern's size: a pattern near the bound of 10,000 takes seconds
// over one 64 KiB value, and a profile may hold many. Past the bound the
// file is refused (too_large) before the work is done, as clean's work is
// bounded; about a second of matching at the slowest measured rate.
const maxPatternWork = 1 << 27

// patternWorkFailure is the failure of a file whose patterns would pass
// maxPatternWork.
func patternWorkFailure(path string) error { return &Failure{Cause: CauseTooLarge, Path: path} }

// spend counts n bytes read at cost against the file's maxPatternWork,
// and reports whether the work stays within it.
func (r *run) spend(n int, cost int64) bool {
	r.patternWork += int64(n) * cost
	return r.patternWork <= maxPatternWork
}

// withValue gives loc the element's value for a value pattern, when the
// profile has one and the element is of a VR it reads: as text, its
// padding removed. The library has already decoded the value into UTF-8
// from the file's character set, or the value is of the default
// repertoire (checkText refuses any other). A value it cannot give is
// marked unread, with the cause the file is refused for should a value
// pattern have read it (unreadFailure): one longer than
// profile.MaxPatternValue (too_large), one not in memory, or one holding
// the escape of an ISO 2022 code extension no decoder turned into UTF-8,
// whose text a pattern would not see (bad_value).
//
// Reading spends the value's length times every value pattern's size from
// the file's budget, before any pattern runs, since Decide runs each value
// pattern once over the value; twice when the file is explained, whose
// explanation runs them again. Past the budget the file is refused.
func (r *run) withValue(loc *profile.Location, elem *dataelem.DataElement, path string) (Cause, error) {
	if r.en.valueCost == 0 || !patternVRs[loc.VR] {
		return "", nil
	}
	b, ok := elem.Bytes()
	switch {
	case !ok:
		loc.ValueUnread = true
		return CauseBadValue, nil
	case len(b) > profile.MaxPatternValue:
		loc.ValueUnread = true
		return CauseTooLarge, nil
	case bytes.IndexByte(b, esc) >= 0:
		loc.ValueUnread = true
		return CauseBadValue, nil
	}
	passes := int64(1)
	if r.locs != nil {
		passes = 2
	}
	if !r.spend(len(b), passes*r.en.valueCost) {
		return "", patternWorkFailure(path)
	}
	loc.Value = naming.TrimValue(loc.VR, string(b))
	return "", nil
}

// patternFailure refuses the file when a value pattern could not read the
// element's value (Decision.PatternUnread: a value passed unread could be
// what the pattern was written to catch), or matched more distinct texts
// than the bound (PatternOver: a needle dropped would let a copy stay);
// nil otherwise.
func patternFailure(dec Decision, unread Cause, path, rule string) error {
	switch {
	case dec.PatternUnread:
		return &Failure{Cause: unread, Path: path}
	case dec.PatternOver:
		return &Failure{Cause: CauseTooManyMatches, Path: path, Rule: rule}
	}
	return nil
}

// patternNeedles keeps as identifier needles what a rule's value pattern
// matched in a value the rule's action removes or changes: a person's rule
// chose those texts as identifying. A redact's own cuts are its needles
// (actRedact), not what its selector matched, since redact keeps what it
// does not cut and a needle of kept text would hold every file.
func (r *run) patternNeedles(dec Decision, act Action, vr, value, path string) {
	if dec.Rule < 0 || len(dec.PatternMatches) == 0 || act.Kind == ActRedact || !changesValue(act, vr, value) {
		return
	}
	for _, m := range dec.PatternMatches {
		r.patternNeedle(m, path)
	}
}

// patternNeedle keeps a text a pattern took out of a value as an
// identifier needle: whole, since it comes from one value; and taken only
// as a whole word when it begins or ends with a letter, so a cut name or
// code is not found inside a longer word ("Smith" in "Smithson"). One that
// begins and ends with a digit is held to digit edges by the check
// already.
func (r *run) patternNeedle(text, path string) {
	text = strings.Trim(text, " \x00")
	if text == "" {
		return
	}
	edge := EdgeAny
	first, _ := utf8.DecodeRuneInString(text)
	last, _ := utf8.DecodeLastRuneInString(text)
	if unicode.IsLetter(first) || unicode.IsLetter(last) {
		edge = EdgeWord
	}
	r.record.Needles = append(r.record.Needles, Needle{Kind: NeedleText, Value: text, Path: path, Edge: edge, Whole: true})
}

// actRedact cuts the matches of the rule's pattern out of a text value
// and keeps the rest. Each text cut is an identifier needle, so a copy of
// it kept anywhere in the file holds the file.
func (r *run) actRedact(c *target, a Action) error {
	if !patternVRs[c.vr] || a.Pattern == nil {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	switch {
	case err != nil || strings.IndexByte(orig, esc) >= 0:
		return c.fail(CauseBadValue)
	case len(orig) > profile.MaxPatternValue:
		return c.fail(CauseTooLarge)
	}
	out, cuts, count, cause := redactValue(a.Pattern, a.Replacement, c.vr, orig, func(n int) bool { return r.spend(n, a.PatternCost) })
	if cause != "" {
		return c.fail(cause)
	}
	if count == 0 {
		return nil
	}
	r.record.Entries[c.entry].Redactions = count
	for _, cut := range cuts {
		r.patternNeedle(cut, c.path)
	}
	return r.write(c, out)
}

// redactValue cuts the matches of re out of a value of VR vr, each match
// becoming repl, and returns what is left and the texts cut, or the cause
// of a refusal. It cuts part by part (profile.ValueParts), so no match
// spans the backslash between two values; a PN part component by
// component, each between its "^" and "=" (PS3.5 6.2.1), so no match
// removes a delimiter. Each piece is cut again until the pattern no longer
// matches it, so cutting cannot join two halves into a new match. Past
// profile.MaxRedactMatches matches in all it refuses (CauseTooManyMatches),
// and when what is left is not a value of the VR or does not fit its
// length field (CauseRedactInvalid). cuts are the texts cut, each once
// (past MaxRedactMatches distinct ones it refuses, CauseTooManyMatches);
// count is every match cut.
// Each pass over a piece first asks spend for its length, and refuses
// (CauseTooLarge) when spend says the file's budget is gone.
//
// The value is cut in NFC (profile.NFC), the form value patterns match: a
// name written with combining marks is cut by a pattern that writes it
// composed, and what is left is written in NFC. A value with nothing cut
// is not written (count is 0), so it stays as it came.
func redactValue(re *regexp.Regexp, repl, vr, value string, spend func(n int) bool) (out string, cuts []string, count int, cause Cause) {
	value = profile.NFC(value)
	seen := map[string]bool{}
	var over bool
	cut := func(s string) (string, bool) {
		for {
			if !spend(len(s)) {
				over = true
				return "", false
			}
			var b strings.Builder
			last, n := 0, 0
			for _, m := range re.FindAllStringIndex(s, -1) {
				if m[1] == m[0] {
					continue
				}
				n++
				if t := s[m[0]:m[1]]; !seen[t] {
					if len(cuts) == profile.MaxRedactMatches {
						return "", false
					}
					seen[t] = true
					cuts = append(cuts, t)
				}
				b.WriteString(s[last:m[0]])
				b.WriteString(repl)
				last = m[1]
			}
			if n == 0 {
				return s, true
			}
			count += n
			b.WriteString(s[last:])
			s = b.String()
		}
	}
	parts := profile.ValueParts(vr, value)
	for i, part := range parts {
		if vr != "PN" {
			var ok bool
			if parts[i], ok = cut(part); !ok {
				return "", nil, 0, tooMuch(over)
			}
			continue
		}
		groups := strings.Split(part, "=")
		for g, group := range groups {
			comps := strings.Split(group, "^")
			for k, comp := range comps {
				var ok bool
				if comps[k], ok = cut(comp); !ok {
					return "", nil, 0, tooMuch(over)
				}
			}
			groups[g] = strings.Join(comps, "^")
		}
		parts[i] = strings.Join(groups, "=")
	}
	out = strings.Join(parts, `\`)
	if count > 0 && (!validText(vr, out) || int64(len(out)) > maxLengthField(vr)) {
		return "", nil, 0, CauseRedactInvalid
	}
	return out, cuts, count, ""
}

// maxLengthField is the longest value an element of VR vr can be written
// with in explicit VR: 65,534 bytes (the even length under 0xFFFF) for a
// VR of a 16-bit length field, and 2^32-2 for UC and UT, whose field is
// 32 bits (PS3.5 7.1.2). Every value of a multi-valued element may be
// valid alone and the whole still not fit.
func maxLengthField(vr string) int64 {
	if vr == "UC" || vr == "UT" {
		return 1<<32 - 2
	}
	return 0xFFFE
}

// tooMuch is the cause of a cut stopped part way: the file's pattern
// budget, or the matches' bound.
func tooMuch(overBudget bool) Cause {
	if overBudget {
		return CauseTooLarge
	}
	return CauseTooManyMatches
}

// redactNeedles keeps as identifier needles what a redact would cut from a
// value inside a sequence that is removed: the value goes with the
// sequence, and what the rule would have cut where it stood is still a
// text a person's rule chose as identifying, searched for in what the
// file keeps. A value redact would refuse for its matches or its validity
// gives none here, the sequence's removal taking it out; past the file's
// pattern budget the file is refused, as where the value stands.
func (r *run) redactNeedles(act Action, vr, value, path string) error {
	if act.Kind != ActRedact || act.Pattern == nil || !patternVRs[vr] || len(value) > profile.MaxPatternValue {
		return nil
	}
	_, cuts, _, cause := redactValue(act.Pattern, act.Replacement, vr, value, func(n int) bool { return r.spend(n, act.PatternCost) })
	switch cause {
	case "":
		for _, cut := range cuts {
			r.patternNeedle(cut, path)
		}
	case CauseTooLarge:
		return patternWorkFailure(path)
	}
	return nil
}
