package dicomweb

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// Matches reports whether a value of an attribute matches the key (PS3.4
// C.2.2.2): one of values, the attribute's values as text, when it has
// several. An attribute with no value matches no key, since a key with a
// value is never a universal match here (ParseQuery keeps those apart).
// Text is compared as it is, but for trailing spaces, which a value's
// padding may leave (PS3.5 6.2); so matching is case sensitive, as single
// value matching is for every VR but PN (PS3.4 C.2.2.2.1), and PN is
// matched literally too since fuzzy matching is not done.
func (k Key) Matches(values ...string) bool {
	for _, v := range values {
		if k.matchOne(strings.TrimRight(v, " ")) {
			return true
		}
	}
	return false
}

// matchOne matches one value.
func (k Key) matchOne(v string) bool {
	switch k.Match {
	case MatchSingle:
		if isNumber(k.VR) {
			return sameNumber(k.Value, v)
		}
		return v == k.Value
	case MatchList:
		_, found := slices.BinarySearch(k.UIDs, v)
		return found
	case MatchWildcard:
		return Wildcard(k.Value, v)
	case MatchRange:
		return inRange(k.VR, k.Low, k.High, v)
	}
	return false
}

// sameNumber reports whether two numbers written as text are the same: an
// IS or a DS may be written with leading zeros, a sign or spaces (PS3.5
// 6.2), and a JSON number has none of them.
func sameNumber(a, b string) bool {
	return normalNumber(a) == normalNumber(b)
}

// normalNumber writes a number's text without spaces, a plus sign or
// leading zeros.
func normalNumber(s string) string {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	s = strings.TrimLeft(s, "0")
	if s == "" || s[0] == '.' {
		s = "0" + s
	}
	if neg && s != "0" {
		return "-" + s
	}
	return s
}

// inRange reports whether v lies from low to high, either open, comparing
// as PS3.4 C.2.2.2.5 does: a date YYYYMMDD by its digits; a time or a date
// and time by its text, the bound taken as the start of what it names, so
// a high bound "1200" keeps everything up to 12:00:59.999999.
func inRange(vr, low, high, v string) bool {
	if v == "" {
		return false
	}
	if low != "" && v < low {
		return false
	}
	if high == "" {
		return true
	}
	if vr == "DA" {
		return v <= high
	}
	// A time or a date and time of more precision than the high bound is
	// within it when it starts with the bound.
	return v <= high || strings.HasPrefix(v, high)
}

// Wildcard reports whether v matches pattern, where "*" is any run of
// characters, none included, and "?" one character (PS3.4 C.2.2.2.4).
func Wildcard(pattern, v string) bool {
	// The classic two-pointer match with one point to go back to.
	p, s := 0, 0
	star, mark := -1, 0
	for s < len(v) {
		switch {
		case p < len(pattern) && pattern[p] == '?':
			_, n := utf8.DecodeRuneInString(v[s:])
			p++
			s += n
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, s
			p++
		case p < len(pattern) && pattern[p] == v[s]:
			p++
			s++
		case star >= 0:
			p = star + 1
			_, n := utf8.DecodeRuneInString(v[mark:])
			mark += n
			s = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// LiteralPrefix returns the part of a wild card pattern before its first
// "*" or "?": every value the pattern matches starts with it.
func LiteralPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, "*?"); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// ElementValues returns the values of an element of DICOM JSON (PS3.18
// F.2.2) as text a key matches: a string as it is, a number as JSON writes
// it, a person name by its alphabetic group (PS3.18 F.2.2); nil for an
// element with no Value, or one that is not an element.
func ElementValues(raw json.RawMessage) []string {
	var el struct {
		Value []json.RawMessage `json:"Value"`
	}
	if json.Unmarshal(raw, &el) != nil {
		return nil
	}
	out := make([]string, 0, len(el.Value))
	for _, v := range el.Value {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out = append(out, s)
			continue
		}
		var pn struct {
			Alphabetic string `json:"Alphabetic"`
		}
		if json.Unmarshal(v, &pn) == nil && pn.Alphabetic != "" {
			out = append(out, pn.Alphabetic)
			continue
		}
		var n json.Number
		if json.Unmarshal(v, &n) == nil {
			out = append(out, n.String())
		}
	}
	return out
}
