package api

import (
	"math"
	"strconv"
	"strings"
)

// The parts of a plain HTTP download that are read from its request alone:
// the window its query names, the Range it asks for, and its conditions
// (data.proto, "One instance over plain HTTP").

// window is a part of an instance a download's query names, which the
// answer treats as its whole body.
type window struct {
	given          bool
	offset, length int64
}

// messageBadWindow answers a query that does not name a window as the
// contract writes one.
const messageBadWindow = "the query is offset=<offset>&length=<length>, each in decimal digits with no sign or leading zero, length at least 1, each once, and nothing else"

// windowOf reads the query of a download, as sent: empty for none, or
// exactly the two parameters offset and length, each once, each a decimal
// with no sign or leading zero other than "0" itself, length at least 1
// and offset plus length at most 2^63 - 1. Anything else is Invalid: a
// parameter ignored would let two URIs name one part, and a second value
// would let one URI name two.
func windowOf(raw string) (window, error) {
	if raw == "" {
		return window{}, nil
	}
	w := window{given: true}
	var haveOffset, haveLength bool
	for part := range strings.SplitSeq(raw, "&") {
		k, v, ok := strings.Cut(part, "=")
		n, digits := decimal(v)
		switch {
		case !ok || !digits:
			return window{}, invalid("query", messageBadWindow)
		case k == "offset" && !haveOffset:
			w.offset, haveOffset = n, true
		case k == "length" && !haveLength:
			w.length, haveLength = n, true
		default:
			return window{}, invalid("query", messageBadWindow)
		}
	}
	if !haveOffset || !haveLength || w.length < 1 || w.offset > math.MaxInt64-w.length {
		return window{}, invalid("query", messageBadWindow)
	}
	return w, nil
}

// decimal reads s as the contract writes a number: decimal digits, no
// sign, no leading zero but in "0" itself, and no more than an int64
// holds.
func decimal(s string) (int64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// rangeKind is what a Range header asks of a body.
type rangeKind int

// The kinds.
const (
	// rangeNone is no Range, or one ignored: the whole body is answered
	// 200 (RFC 9110 14.2 lets a server ignore a Range).
	rangeNone rangeKind = iota
	// rangeOne is one satisfiable range, answered 206.
	rangeOne
	// rangeUnsatisfiable is a range no byte of the body is in, answered
	// 416 (RFC 9110 15.5.17).
	rangeUnsatisfiable
)

// byteRange is the first and last byte, both counted, of a range of a body.
type byteRange struct {
	first, last int64
}

// byteRangeOf reads a Range header against a body of size bytes (RFC 9110
// 14.1.2, 14.2): one range per request, "bytes=<first>-",
// "bytes=<first>-<last>" with a last at or past the end taken as the end,
// or "bytes=-<suffix>". A first at or past the end, any range of an empty
// body, and a suffix of zero are unsatisfiable. A header that does not
// parse, names another unit, has a last before its first, or holds more
// than one range is ignored. The unit is compared without regard to case
// (RFC 9110 14.1); a number too long for an int64 is past any end.
func byteRangeOf(h string, size int64) (byteRange, rangeKind) {
	unit, spec, ok := strings.Cut(h, "=")
	if !ok || !strings.EqualFold(unit, "bytes") || strings.Contains(spec, ",") {
		return byteRange{}, rangeNone
	}
	spec = strings.Trim(spec, " \t")
	firstText, lastText, ok := strings.Cut(spec, "-")
	if !ok {
		return byteRange{}, rangeNone
	}
	if firstText == "" {
		suffix, ok := saturated(lastText)
		switch {
		case !ok:
			return byteRange{}, rangeNone
		case suffix == 0 || size == 0:
			return byteRange{}, rangeUnsatisfiable
		}
		return byteRange{first: size - min(suffix, size), last: size - 1}, rangeOne
	}
	first, ok := saturated(firstText)
	if !ok {
		return byteRange{}, rangeNone
	}
	last := int64(math.MaxInt64)
	if lastText != "" {
		if last, ok = saturated(lastText); !ok || last < first {
			return byteRange{}, rangeNone
		}
	}
	if first >= size {
		return byteRange{}, rangeUnsatisfiable
	}
	return byteRange{first: first, last: min(last, size-1)}, rangeOne
}

// saturated reads 1*DIGIT (RFC 9110 14.1.1), leading zeros allowed, as an
// int64, with a number past the largest int64 read as the largest.
func saturated(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return n, true
}

// strongTag returns an instance's entity tag: its SHA-256 in lowercase
// hexadecimal in quotes, a strong validator (RFC 9110 8.8.3).
func strongTag(sha string) string { return `"` + sha + `"` }

// ifRangeHolds reports whether a request's If-Range lets its Range apply
// (RFC 9110 13.1.5): when it has none, or when it is exactly the strong
// tag. A date or a weak tag ("W/") never does, since only a strong tag
// proves the bytes are the same.
func ifRangeHolds(values []string, tag string) bool {
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && strings.Trim(values[0], " \t") == tag
}

// noneMatchFails reports whether a request's If-None-Match names the
// instance's tag, or "*", which any instance matches: the answer is then
// 304 Not Modified (RFC 9110 13.1.2), compared weakly, as that section
// says.
func noneMatchFails(values []string, tag string) bool {
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			item = strings.TrimPrefix(strings.Trim(item, " \t"), "W/")
			if item == "*" || item == tag {
				return true
			}
		}
	}
	return false
}
