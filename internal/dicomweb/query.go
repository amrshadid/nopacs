package dicomweb

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/tag"
)

// The bounds of a search's page (PS3.18 8.3.4.4).
const (
	// DefaultLimit is the most results a search answers when it names no
	// limit: a page a client shows at once.
	DefaultLimit = 100
	// MaxLimit is the most results one answer holds, whatever limit is
	// asked: each result of a study or a series may cost a read of a
	// metadata file, so a page past this is asked for in several.
	MaxLimit = 1000
	// MaxOffset is the largest offset served. The catalog pages by a
	// cursor, so an offset is reached by reading every row before it; past
	// this a search is refused rather than read a large part of a tenant,
	// and a client narrows it (a date range, a modality) instead.
	MaxOffset = 10000
	// MaxUIDList is the most UIDs one key matches (UID list matching,
	// PS3.4 C.2.2.2.2); each is one lookup in the catalog.
	MaxUIDList = 100
)

// The parameters of a search that are not attributes (PS3.18 8.3.4.1,
// 8.3.3.1).
const (
	paramIncludeField  = "includefield"
	paramLimit         = "limit"
	paramOffset        = "offset"
	paramFuzzyMatching = "fuzzymatching"
	paramAccept        = "accept"
	paramCharset       = "charset"
)

// Match is how a key's value is matched (PS3.4 C.2.2.2).
type Match int

// The kinds of matching.
const (
	// MatchSingle is single value matching: the value as it is.
	MatchSingle Match = iota + 1
	// MatchList is UID list matching: one of several UIDs.
	MatchList
	// MatchWildcard is wild card matching: "*" any run of characters, "?"
	// one character.
	MatchWildcard
	// MatchRange is range matching of a date, a time or a date and time:
	// from Low to High, either open.
	MatchRange
)

// Key is one attribute a search matches.
type Key struct {
	// Tag is the attribute; Keyword its keyword (PS3.6) and VR its value
	// representation, the dictionary's first when it gives two.
	Tag     tag.Tag
	Keyword string
	VR      string
	// Match is how it is matched.
	Match Match
	// Value is the value of a single or a wild card match.
	Value string
	// UIDs are the UIDs of a list match, sorted, each once.
	UIDs []string
	// Low and High bound a range match; an empty one is open.
	Low, High string
}

// Query is a search: its level and the study and series its path names,
// the keys it matches, the attributes it returns, and its page.
type Query struct {
	// Level is the level searched.
	Level Level
	// Study and Series are the UIDs the path names, each empty when it
	// names none.
	Study, Series string
	// Keys are the attributes matched, in order of tag, each once.
	Keys []Key
	// Include are the attributes asked for with includefield, besides
	// those a search returns by default; IncludeAll is includefield=all.
	Include    []tag.Tag
	IncludeAll bool
	// Limit and Offset are the page: at most Limit results after the first
	// Offset. Capped says the limit asked for was more than MaxLimit and
	// is answered as MaxLimit.
	Limit, Offset int
	Capped        bool
	// Fuzzy says fuzzy matching was asked for, which is not done: the
	// answer says so (PS3.18 10.6.1.2.2).
	Fuzzy bool
}

// Key returns the key of the attribute t, and whether the search matches
// it.
func (q Query) Key(t tag.Tag) (Key, bool) {
	for _, k := range q.Keys {
		if k.Tag == t {
			return k, true
		}
	}
	return Key{}, false
}

// Keywords returns the keywords of the keys matched, joined by commas, as
// an audit entry records a search: which attributes, never their values.
func (q Query) Keywords() string {
	names := make([]string, 0, len(q.Keys))
	for _, k := range q.Keys {
		names = append(names, k.Keyword)
	}
	return strings.Join(names, ",")
}

// ParseQuery reads the query string of a search of resource r (PS3.18
// 8.3.4, 10.6.1.2). A UID the path names is a key of the query, and may
// not be given again. Anything the query cannot mean is 400: a parameter
// that is neither a parameter of a search nor an attribute by keyword or
// tag, an attribute inside a sequence, a key given twice, a limit or an
// offset that is not a count, an offset past MaxOffset, a list of more than
// MaxUIDList UIDs, a range of an attribute that is not a date or a time.
// A charset other than UTF-8 is 406 (PS3.18 8.3.3.1).
func ParseQuery(r Resource, rawQuery string) (Query, *Error) {
	q := Query{Level: r.Level, Study: r.Study, Series: r.Series, Limit: DefaultLimit}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return q, refuse(http.StatusBadRequest, "the query string is not well formed")
	}
	given := map[tag.Tag]bool{}
	for _, p := range []struct {
		t tag.Tag
		v string
	}{{TagStudyInstanceUID, r.Study}, {TagSeriesInstanceUID, r.Series}} {
		if p.v != "" {
			given[p.t] = true
			q.Keys = append(q.Keys, Key{Tag: p.t, Keyword: p.t.GetKeyword(), VR: "UI", Match: MatchSingle, Value: p.v})
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		vs := values[name]
		last := vs[len(vs)-1]
		switch name {
		case paramIncludeField:
			for _, v := range vs {
				for field := range strings.SplitSeq(v, ",") {
					field = strings.TrimSpace(field)
					if field == "" {
						continue
					}
					if field == "all" {
						q.IncludeAll = true
						continue
					}
					t, ok := attributeTag(field)
					if !ok {
						return q, refuse(http.StatusBadRequest, "includefield names "+quoted(field)+", which is not an attribute by keyword or tag")
					}
					if !slices.Contains(q.Include, t) {
						q.Include = append(q.Include, t)
					}
				}
			}
		case paramLimit, paramOffset:
			n, err := strconv.Atoi(last)
			if err != nil || n < 0 || last[0] == '+' || (name == paramLimit && n == 0) {
				return q, refuse(http.StatusBadRequest, name+" is not a count of results")
			}
			if name == paramOffset {
				if n > MaxOffset {
					return q, refuse(http.StatusBadRequest, "an offset past "+strconv.Itoa(MaxOffset)+" is not served; narrow the search instead")
				}
				q.Offset = n
				continue
			}
			q.Limit = n
			if n > MaxLimit {
				q.Limit, q.Capped = MaxLimit, true
			}
		case paramFuzzyMatching:
			switch last {
			case "true":
				q.Fuzzy = true
			case "false":
			default:
				return q, refuse(http.StatusBadRequest, "fuzzymatching is true or false")
			}
		case paramAccept:
			// Read with the Accept header (AcceptJSON).
		case paramCharset:
			if !strings.EqualFold(last, "utf-8") {
				return q, refuse(http.StatusNotAcceptable, "a search is answered in UTF-8 only")
			}
		default:
			k, include, err := parseKey(name, vs)
			if err != nil {
				return q, err
			}
			if given[k.Tag] {
				return q, refuse(http.StatusBadRequest, name+" is given more than once, or by the path as well")
			}
			given[k.Tag] = true
			if include {
				// A universal match ("*" or nothing) returns the attribute
				// and keeps every result (PS3.4 C.2.2.2.3).
				if !slices.Contains(q.Include, k.Tag) {
					q.Include = append(q.Include, k.Tag)
				}
				continue
			}
			q.Keys = append(q.Keys, k)
		}
	}
	slices.SortFunc(q.Keys, func(a, b Key) int { return cmp.Compare(a.Tag, b.Tag) })
	return q, nil
}

// parseKey reads one attribute of a query and its values: the key, or
// that it is only to be returned.
func parseKey(name string, vs []string) (Key, bool, *Error) {
	if strings.Contains(name, ".") {
		return Key{}, false, refuse(http.StatusBadRequest, "matching inside a sequence is not served")
	}
	t, ok := attributeTag(name)
	if !ok {
		return Key{}, false, refuse(http.StatusBadRequest, quoted(name)+" is neither a parameter of a search nor an attribute by keyword or tag")
	}
	k := Key{Tag: t, Keyword: t.GetKeyword(), VR: attributeVR(t)}
	if k.Keyword == "" {
		k.Keyword = t.Hex()
	}
	if k.VR == "SQ" {
		return k, false, refuse(http.StatusBadRequest, "a sequence is not matched by a value")
	}
	if k.VR == "UI" {
		// Several UIDs: the parameter given more than once, or a list
		// separated by commas (PS3.18 8.3.4.1) or by backslashes, as a
		// C-FIND identifier writes one (PS3.4 C.2.2.2.2).
		// The list is refused as soon as it passes MaxUIDList, and its
		// repeats are found by a map, so a long query costs one pass over
		// its text, not a comparison of each UID with every other.
		var uids []string
		seen := map[string]bool{}
		for _, v := range vs {
			for u := range strings.SplitSeq(strings.ReplaceAll(v, `\`, ","), ",") {
				if u = strings.TrimSpace(u); u == "" || seen[u] {
					continue
				}
				if len(uids) == MaxUIDList {
					return k, false, refuse(http.StatusBadRequest, "a key matches at most "+strconv.Itoa(MaxUIDList)+" UIDs")
				}
				seen[u] = true
				uids = append(uids, u)
			}
		}
		switch {
		case len(uids) == 0:
			return k, true, nil
		case len(uids) == 1:
			k.Match, k.Value = MatchSingle, uids[0]
		default:
			slices.Sort(uids)
			k.Match, k.UIDs = MatchList, uids
		}
		return k, false, nil
	}
	if len(vs) != 1 {
		return k, false, refuse(http.StatusBadRequest, name+" is given more than once")
	}
	v := strings.TrimRight(vs[0], " ")
	switch {
	case v == "" || strings.Trim(v, "*") == "":
		return k, true, nil
	case isDateTime(k.VR) && strings.Contains(v, "-"):
		low, high, _ := strings.Cut(v, "-")
		if strings.Contains(high, "-") || (low == "" && high == "") {
			return k, false, refuse(http.StatusBadRequest, name+" is a range of one \"-\" between two bounds, either of them open")
		}
		k.Match, k.Low, k.High = MatchRange, low, high
	case strings.ContainsAny(v, "*?") && !isNumber(k.VR) && !isDateTime(k.VR):
		k.Match, k.Value = MatchWildcard, v
	default:
		k.Match, k.Value = MatchSingle, v
	}
	if (k.VR == "DA" || k.VR == "DT" || k.VR == "TM") && k.Match != MatchRange && strings.ContainsAny(v, "*?") {
		return k, false, refuse(http.StatusBadRequest, name+" is a date or a time, matched by a value or a range")
	}
	return k, false, nil
}

// attributeTag finds a standard attribute by keyword or by a tag of eight
// hexadecimal digits (PS3.18 8.3.4.1).
func attributeTag(name string) (tag.Tag, bool) {
	if t := tag.GlobalDictionary().GetByKeyword(name); t != 0 {
		return t, true
	}
	if len(name) != 8 {
		return 0, false
	}
	v, err := strconv.ParseUint(name, 16, 32)
	if err != nil {
		return 0, false
	}
	t := tag.Tag(v)
	// A tag the dictionary does not hold has no VR to match it by.
	if !t.Exists() {
		return 0, false
	}
	return t, true
}

// attributeVR is the dictionary's VR of a standard attribute, the first
// where it gives two ("US or SS").
func attributeVR(t tag.Tag) string {
	vr, _, _ := strings.Cut(t.GetVR(), " ")
	return vr
}

// isDateTime reports whether vr is a date, a time or a date and time,
// which range matching applies to (PS3.4 C.2.2.2.5).
func isDateTime(vr string) bool { return vr == "DA" || vr == "TM" || vr == "DT" }

// isNumber reports whether vr holds numbers, which only single value
// matching applies to.
func isNumber(vr string) bool {
	switch vr {
	case "IS", "DS", "US", "SS", "UL", "SL", "FL", "FD", "UV", "SV":
		return true
	}
	return false
}

// quoted returns s in quotes, cut to 64 bytes, for a message.
func quoted(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strconv.Quote(s)
}
