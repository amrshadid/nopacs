package query

import (
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/tag"
)

// QIDOParam is one parameter of a QIDO-RS search's query, as the request
// gives it.
type QIDOParam struct {
	// Key is the parameter's name: a keyword, a tag as eight hexadecimal
	// digits, or a path of them joined by "." (PS3.18 8.3.4.1).
	Key string
	// Value is its value, decoded from the URL.
	Value string
}

// QIDOError is a QIDO-RS query the mapping refuses, which the service
// answers 400 (PS3.18 8.3.4). Its text names the parameter, never its
// value.
type QIDOError struct {
	// Key is the parameter refused.
	Key string
	// Why says why.
	Why string
}

// Error returns the key and why.
func (e *QIDOError) Error() string { return "query: parameter " + strconv.Quote(e.Key) + ": " + e.Why }

// Unwrap returns ErrInvalid.
func (e *QIDOError) Unwrap() error { return ErrInvalid }

// WarningFuzzy is the warning FromQIDO returns when a search asks for fuzzy
// matching of person names, which this mapping does not do: names match as
// PS3.4 C.2.2.2.1 matches them, without regard to case. The service sends it
// as a "Warning: 299" header (PS3.18 8.3.4.1).
const WarningFuzzy = "fuzzy matching is not supported; person names are matched as written, without regard to case"

// qidoReserved are the query parameters that are not matching keys
// (PS3.18 8.3.4).
var qidoReserved = map[string]bool{
	"limit": true, "offset": true, "includefield": true, "fuzzymatching": true,
	"emptyvaluematching": true, "multiplevaluematching": true,
}

// FromQIDO maps a QIDO-RS search's matching keys onto a condition, by the
// matching rules of PS3.4 C.2.2.2 that PS3.18 8.3.4 refers to:
//
//   - a key with an empty value, a "*" alone, or a sequence key alone is
//     universal matching (C.2.2.2.3): no leaf;
//   - a UID, or a list of UIDs separated by "," or "\" (C.2.2.2.2), is
//     "==" or "in";
//   - a date, time or date-time "a-b", "-b" or "a-" is a range (C.2.2.2.5),
//     inclusive at both ends; one value is an equality;
//   - a value with "*" or "?" is a pattern (C.2.2.2.4), "like";
//   - any other value is single value matching (C.2.2.2.1): "==", or for
//     a person's name "=", which compares without regard to case as PS3.4
//     permits; a number of a numeric VR compares as a number;
//   - a key through sequences ("Seq.Attr") matches any item (C.2.2.2.6);
//   - a private data element is named with its creator's element as another
//     key in the same query, as PS3.18 8.3.4.1 asks; the leaf names it by
//     creator;
//   - with emptyvaluematching=true, the value `""` matches an absent or
//     empty attribute; with multiplevaluematching=true, values separated by
//     "\" match any of them; both are refused without the parameter;
//   - fuzzymatching=true is answered with WarningFuzzy.
//
// The keys are joined by "and" in the order given; no key gives a nil
// condition, every row. Parameters that are not matching keys (limit,
// offset, includefield) are left to the caller. The tree is checked
// (Check); a key given twice, an unknown keyword, or a value the attribute's
// VR cannot take is a *QIDOError.
func FromQIDO(params []QIDOParam) (Node, []string, error) {
	var warnings []string
	emptyMatch, multiMatch := false, false
	for _, p := range params {
		on := strings.EqualFold(p.Value, "true")
		switch strings.ToLower(p.Key) {
		case "fuzzymatching":
			if on {
				warnings = append(warnings, WarningFuzzy)
			}
		case "emptyvaluematching":
			emptyMatch = on
		case "multiplevaluematching":
			multiMatch = on
		}
	}
	type key struct {
		path []uint32
		text string
	}
	var keys []key
	creators := map[string]string{}
	seen := map[string]bool{}
	for _, p := range params {
		if qidoReserved[strings.ToLower(p.Key)] {
			continue
		}
		path, err := qidoPath(p.Key)
		if err != nil {
			return nil, nil, err
		}
		id := qidoPathString(path)
		if seen[id] {
			return nil, nil, &QIDOError{Key: p.Key, Why: "it is given twice"}
		}
		seen[id] = true
		last := path[len(path)-1]
		if isCreatorTag(last) {
			creators[id] = strings.TrimRight(p.Value, " ")
			continue
		}
		keys = append(keys, key{path: path, text: p.Key})
	}
	var leaves []Node
	for _, k := range keys {
		f, err := qidoField(k.path, k.text, creators)
		if err != nil {
			return nil, nil, err
		}
		v := paramValue(params, k.text)
		n, err := qidoLeaf(f, k.path[len(k.path)-1], k.text, v, emptyMatch, multiMatch)
		if err != nil {
			return nil, nil, err
		}
		if n != nil {
			leaves = append(leaves, n)
		}
	}
	var out Node
	switch len(leaves) {
	case 0:
		return nil, warnings, nil
	case 1:
		out = leaves[0]
	default:
		out = &And{Children: leaves}
	}
	if err := Check(out); err != nil {
		return nil, nil, err
	}
	return out, warnings, nil
}

// paramValue returns the value of the parameter named key.
func paramValue(params []QIDOParam, key string) string {
	for _, p := range params {
		if p.Key == key {
			return p.Value
		}
	}
	return ""
}

// isCreatorTag reports whether t is a private creator element (gggg,0010)
// to (gggg,00FF) of an odd group (PS3.5 7.8.1).
func isCreatorTag(t uint32) bool {
	return (t>>16)%2 == 1 && t>>16 > 7 && t&0xFFFF >= 0x10 && t&0xFFFF <= 0xFF
}

// qidoPath reads a key: steps joined by ".", each a keyword or eight
// hexadecimal digits.
func qidoPath(key string) ([]uint32, error) {
	steps := strings.Split(key, ".")
	if len(steps) > MaxPathSteps {
		return nil, &QIDOError{Key: key, Why: "the path has more than " + strconv.Itoa(MaxPathSteps) + " steps"}
	}
	out := make([]uint32, len(steps))
	for i, s := range steps {
		if len(s) == 8 {
			if v, err := strconv.ParseUint(s, 16, 32); err == nil {
				out[i] = uint32(v)
				continue
			}
		}
		t, ok := keywordTag(s)
		if !ok {
			return nil, &QIDOError{Key: key, Why: "it is not a keyword or a tag"}
		}
		out[i] = t
	}
	return out, nil
}

// qidoPathString writes a path of tags, the key a creator is found by.
func qidoPathString(path []uint32) string {
	parts := make([]string, len(path))
	for i, t := range path {
		parts[i] = hex4(t>>16) + hex4(t&0xFFFF)
	}
	return strings.Join(parts, ".")
}

// qidoField makes the field of a path, naming each private data element by
// the creator the query gives for its block at the same place of the path.
func qidoField(path []uint32, key string, creators map[string]string) (Field, error) {
	f := Field{Path: make([]Step, len(path))}
	for i, t := range path {
		f.Path[i] = Step{Tag: t}
		if (t>>16)%2 == 0 || t&0xFFFF < 0x1000 {
			continue
		}
		block := t>>16<<16 | t>>8&0xFF
		at := append(append([]uint32(nil), path[:i]...), block)
		c, ok := creators[qidoPathString(at)]
		if !ok || c == "" {
			return Field{}, &QIDOError{Key: key, Why: "a private data element needs its private creator as another key"}
		}
		f.Path[i] = Step{Tag: t>>16<<16 | t&0xFF, Creator: c}
	}
	return f, nil
}

// qidoLeaf maps one key's value onto a leaf, nil for universal matching.
func qidoLeaf(f Field, t uint32, key, v string, emptyMatch, multiMatch bool) (Node, error) {
	vr := "UN"
	if (t>>16)%2 == 0 {
		vr = tag.Tag(t).GetVR()
	}
	refuse := func(why string) (Node, error) { return nil, &QIDOError{Key: key, Why: why} }
	switch {
	case v == "" || v == "*":
		// Universal matching (PS3.4 C.2.2.2.3) whatever the VR: a "*"
		// alone matches every value, and every absent one.
		return nil, nil
	case v == `""`:
		if !emptyMatch {
			return refuse(`the value "" matches an empty value only with emptyvaluematching=true`)
		}
		return &Leaf{Field: f, Op: OpAbsent}, nil
	case vr == "SQ":
		return refuse("a sequence key takes no value; name an attribute inside it")
	}
	switch vr {
	case "UI":
		if strings.ContainsAny(v, "*?") {
			return refuse("a UID is not matched by a pattern")
		}
		uids := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\\' })
		l := &Leaf{Field: f, Op: OpEqual}
		for _, u := range uids {
			l.Values = append(l.Values, Value{Kind: KindText, Text: u})
		}
		if len(uids) == 0 {
			return nil, nil
		}
		if len(uids) > 1 {
			l.Op = OpIn
		}
		return l, nil
	case "DA", "TM", "DT":
		return qidoRange(f, vr, key, v)
	}
	values := []string{v}
	if strings.Contains(v, `\`) {
		if !multiMatch {
			return refuse(`values separated by "\" match only with multiplevaluematching=true`)
		}
		values = strings.Split(v, `\`)
	}
	var leaves []Node
	for _, s := range values {
		l, err := qidoSingle(f, vr, key, s)
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, nil // one value universal: the key matches every row.
		}
		leaves = append(leaves, l)
	}
	if len(leaves) == 1 {
		return leaves[0], nil
	}
	return &Or{Children: leaves}, nil
}

// numericVRs are the VRs whose values are numbers.
var numericVRs = map[string]bool{
	"IS": true, "DS": true, "US": true, "SS": true, "UL": true, "SL": true,
	"FL": true, "FD": true, "UV": true, "SV": true,
}

// qidoSingle maps one value of a key that is not a UID, date or time.
func qidoSingle(f Field, vr, key, v string) (*Leaf, error) {
	if v == "*" {
		return nil, nil
	}
	if numericVRs[vr] {
		n, ok := parseNumber(v)
		if !ok {
			return nil, &QIDOError{Key: key, Why: "the attribute holds numbers"}
		}
		return &Leaf{Field: f, Op: OpEqual, Values: []Value{{Kind: KindNumber, Number: n}}}, nil
	}
	l := &Leaf{Field: f, Op: OpEqual, Fold: vr == "PN", Values: []Value{{Kind: KindText, Text: v}}}
	if strings.ContainsAny(v, "*?") {
		l.Op = OpLike
	}
	return l, nil
}

// dropOffsets returns a DT value or range without the offsets from UTC its
// date-times carry ("&ZZXX", PS3.5 6.2, DT), so the "-" left is the range's
// (PS3.4 C.2.2.2.5); the search compares date-times as written, offsets
// dropped, as routing reads them. A "+" is always an offset's. A "-" is an
// offset's when it follows a date-time with at least a full date, and four
// digits that are an offset (hours to 14, minutes to 59) follow it, then
// the end or the range's "-": "20240101120000-0500" is one value, and
// "20240101-20240201" and "2024-2025" are ranges. It returns false for a
// "+" that no offset follows.
func dropOffsets(v string) (string, bool) {
	var b strings.Builder
	start := 0 // where the current date-time begins
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '+' && c != '-' {
			b.WriteByte(c)
			continue
		}
		offset := i+5 <= len(v) && (i+5 == len(v) || v[i+5] == '-') && isOffset(v[i+1:i+5])
		switch {
		case c == '+' && !offset:
			return "", false
		case offset && (c == '+' || i-start >= 8):
			i += 4
		default:
			b.WriteByte(c)
			start = i + 1
		}
	}
	return b.String(), true
}

// isOffset reports whether s is HHMM of an offset from UTC: four digits,
// hours to 14, minutes to 59.
func isOffset(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[2]-'0')*10+int(s[3]-'0')
	return h <= 14 && m <= 59
}

// qidoRange maps a date, time or date-time value: one value, or a range.
func qidoRange(f Field, vr, key, v string) (Node, error) {
	parse := func(s string) (Value, bool) {
		switch vr {
		case "DA":
			if len(s) != 8 {
				return Value{}, false
			}
			d, ok := normalDate(s)
			return Value{Kind: KindDate, Text: d}, ok
		case "TM":
			t, ok := normalTime(s)
			return Value{Kind: KindTime, Text: t}, ok
		}
		dt, ok := normalDateTime(s)
		return Value{Kind: KindDateTime, Text: dt}, ok
	}
	bad := &QIDOError{Key: key, Why: "the value is not a " + vr + " value or a range of them (PS3.4 C.2.2.2.5)"}
	if strings.ContainsAny(v, "*?") {
		return nil, bad
	}
	if vr == "DT" {
		var ok bool
		if v, ok = dropOffsets(v); !ok {
			return nil, bad
		}
	}
	lo, hi, isRange := strings.Cut(v, "-")
	if !isRange {
		x, ok := parse(v)
		if !ok {
			return nil, bad
		}
		return &Leaf{Field: f, Op: OpEqual, Values: []Value{x}}, nil
	}
	if strings.Contains(hi, "-") || lo == "" && hi == "" {
		return nil, bad
	}
	var a, b Value
	var ok bool
	if lo != "" {
		if a, ok = parse(lo); !ok {
			return nil, bad
		}
	}
	if hi != "" {
		if b, ok = parse(hi); !ok {
			return nil, bad
		}
	}
	switch {
	case lo == "":
		return &Leaf{Field: f, Op: OpLessEqual, Values: []Value{b}}, nil
	case hi == "":
		return &Leaf{Field: f, Op: OpGreaterEqual, Values: []Value{a}}, nil
	}
	return &Leaf{Field: f, Op: OpBetween, Values: []Value{a, b}}, nil
}
