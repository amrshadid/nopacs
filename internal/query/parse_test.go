package query

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParsePrintRoundTrip guards the canonical form: every condition reads
// to a tree, Print writes it in its canonical form, and that form reads to
// the same tree and prints the same again.
func TestParsePrintRoundTrip(t *testing.T) {
	cases := map[string]string{
		`Modality = "CT"`:                            `Modality = "CT"`,
		`Modality=="CT"`:                             `Modality == "CT"`,
		`Modality nocase == "ct"`:                    `Modality = "ct"`,
		`Modality != "CT"`:                           `not Modality = "CT"`,
		`Modality in ("CT","MR")`:                    `Modality in ("CT", "MR")`,
		`Modality nocase in ("ct")`:                  `Modality nocase in ("ct")`,
		`SeriesNumber in (1234,5678)`:                `SeriesNumber in (1234, 5678)`,
		`SliceThickness between 0.5 and 1.25`:        `SliceThickness between 0.5 and 1.25`,
		`SliceThickness >= -1e+21`:                   `SliceThickness >= -1e+21`,
		`SliceThickness < 2`:                         `SliceThickness < 2`,
		`StudyDate >= 2024-01-31`:                    `StudyDate >= 2024-01-31`,
		`StudyDate last 30 days`:                     `StudyDate >= -30d`,
		`StudyDate last 2 years`:                     `StudyDate >= -2y`,
		`StudyDate = today`:                          `StudyDate = today`,
		`StudyDate between -6m and -1w`:              `StudyDate between -6m and -1w`,
		`StudyTime > 10:30`:                          `StudyTime > 10:30:00`,
		`AcquisitionDateTime <= 2024-01-31T10:30:15`: `AcquisitionDateTime <= 2024-01-31T10:30:15`,
		`SeriesDescription prefix "T1"`:              `SeriesDescription prefix "T1"`,
		`SeriesDescription nocase prefix "t1"`:       `SeriesDescription nocase prefix "t1"`,
		`SeriesDescription like "T1*_a?"`:            `SeriesDescription like "T1*_a?"`,
		`SeriesDescription nocase contains "mprage"`: `SeriesDescription contains "mprage"`,
		`ProtocolName words "t1 sag"`:                `ProtocolName words "t1 sag"`,
		`Manufacturer matches "^SIE(MENS)?$"`:        `Manufacturer matches "^SIE(MENS)?$"`,
		`Manufacturer matches "(?i)siemens"`:         `Manufacturer matches "(?i)siemens"`,
		`SoftwareVersions present`:                   `SoftwareVersions present`,
		`SoftwareVersions absent`:                    `SoftwareVersions absent`,
		`(0018,0050) = 5`:                            `SliceThickness = 5`,
		`( 0018 , 0050 ) = 5`:                        `SliceThickness = 5`,
		`(0009,1004) == "x"`:                         `(0009,1004) == "x"`,
		`(0029,"SIEMENS CSA HEADER",10) present`:     `(0029,"SIEMENS CSA HEADER",10) present`,
		`(0029,"SIEMENS CSA HEADER ",10) present`:    `(0029,"SIEMENS CSA HEADER",10) present`,
		`(0009,"A \"Q\" B",04) present`:              `(0009,"A \"Q\" B",04) present`,
		`OtherPatientIDsSequence.PatientID = "x"`:    `OtherPatientIDsSequence.PatientID = "x"`,
		`(0010,1002).(0010,0020) = "x"`:              `OtherPatientIDsSequence.PatientID = "x"`,
		`SharedFunctionalGroupsSequence.PixelMeasuresSequence.SliceThickness = 1`: `SharedFunctionalGroupsSequence.PixelMeasuresSequence.SliceThickness = 1`,
		`ImageType[2] == "AXIAL"`:                                              `ImageType[2] == "AXIAL"`,
		`calling_ae == "SCANNER1"`:                                             `calling_ae == "SCANNER1"`,
		`session.date >= 2024-01-01`:                                           `session.date >= 2024-01-01`,
		`a1 = "x" and b2 = "y" or c3 = "z"`:                                    `a1 = "x" and b2 = "y" or c3 = "z"`,
		`a1 = "x" and (b2 = "y" or c3 = "z")`:                                  `a1 = "x" and (b2 = "y" or c3 = "z")`,
		`a1 = "x" and (b2 = "y" and c3 = "z")`:                                 `a1 = "x" and (b2 = "y" and c3 = "z")`,
		`(a1 = "x" or b2 = "y") or c3 = "z"`:                                   `(a1 = "x" or b2 = "y") or c3 = "z"`,
		`((a1 = "x"))`:                                                         `a1 = "x"`,
		`not not a1 present`:                                                   `not not a1 present`,
		`not (a1 present and b2 absent)`:                                       `not (a1 present and b2 absent)`,
		`has scan (Modality == "MR" and SliceThickness <= 1)`:                  `has scan (Modality == "MR" and SliceThickness <= 1)`,
		`every scan (Modality == "MR") and has session (session.date >= -30d)`: `every scan (Modality == "MR") and has session (session.date >= -30d)`,
		"Modality\t=\n\"CT\"":                                                  `Modality = "CT"`,
		`PatientName = "Müller"`:                                               `PatientName = "Müller"`,
	}
	for in, want := range cases {
		n := mustParse(t, in)
		got := Print(n)
		if got != want {
			t.Errorf("Print(Parse(%q)) = %q, want %q", in, got, want)
			continue
		}
		again := mustParse(t, got)
		if !reflect.DeepEqual(n, again) {
			t.Errorf("Parse(Print(t)) != t for %q", in)
		}
		if Print(again) != got {
			t.Errorf("printing %q again gave %q", got, Print(again))
		}
	}
}

// TestParseTree guards the trees of the grammar's forms: the private tag by
// creator, the sequence path, the value index, folding, "!=" as "not", and
// the "and" inside "between".
func TestParseTree(t *testing.T) {
	cases := map[string]Node{
		`(0029,"SIEMENS CSA HEADER",10) = "x"`: &Leaf{Field: Field{Path: []Step{{Tag: 0x00290010, Creator: "SIEMENS CSA HEADER"}}},
			Op: OpEqual, Fold: true, Values: []Value{{Kind: KindText, Text: "x"}}},
		`OtherPatientIDsSequence.PatientID == "x"`: &Leaf{Field: Field{Path: []Step{{Tag: 0x00101002}, {Tag: 0x00100020}}},
			Op: OpEqual, Values: []Value{{Kind: KindText, Text: "x"}}},
		`ImageType[2] = 3`: &Leaf{Field: Field{Path: []Step{{Tag: 0x00080008}}, Indexed: true, Index: 2},
			Op: OpEqual, Values: []Value{{Kind: KindNumber, Number: 3}}},
		`Modality != "CT"`: &Not{Child: &Leaf{Field: Field{Path: []Step{{Tag: 0x00080060}}},
			Op: OpEqual, Fold: true, Values: []Value{{Kind: KindText, Text: "CT"}}}},
		`SliceThickness between 1 and 2 and Modality present`: &And{Children: []Node{
			&Leaf{Field: Field{Path: []Step{{Tag: 0x00180050}}}, Op: OpBetween, Values: []Value{{Kind: KindNumber, Number: 1}, {Kind: KindNumber, Number: 2}}},
			&Leaf{Field: Field{Path: []Step{{Tag: 0x00080060}}}, Op: OpPresent},
		}},
		`StudyDate last 30 days`: &Leaf{Field: Field{Path: []Step{{Tag: 0x00080020}}}, Op: OpGreaterEqual,
			Values: []Value{{Kind: KindRelative, Relative: 30, Unit: 'd'}}},
	}
	for in, want := range cases {
		if got := mustParse(t, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %#v, want %#v", in, got, want)
		}
	}
}

// TestParseRefusals guards the errors of malformed conditions: each is a
// *SyntaxError at the byte where the text went wrong, or a tree Check
// refuses, wrapping ErrInvalid.
func TestParseRefusals(t *testing.T) {
	cases := []struct {
		in     string
		offset int // -1: a tree Check refuses
	}{
		{``, 0},
		{`   `, 0},
		{`Modality`, 8},
		{`Modality =`, 10},
		{`Modality = "CT`, 11},
		{`Modality = 'CT'`, 11},
		{`Modality = "C\T"`, 13},
		{`Modality = "a` + "\n" + `b"`, 13},
		{`Modality = "CT" and`, 19},
		{`Modality = "CT" Modality = "MR"`, 16},
		{`(Modality = "CT"`, 16},
		{`Modality = "CT")`, 15},
		{`NoSuchKeyword = "x"`, 0},
		{`modality = "x" and and = "y"`, 19},
		{`Modality ! "CT"`, 9},
		{`Modality = CT`, 11},
		{`Modality in "CT"`, 12},
		{`Modality in ("CT" "MR")`, 18},
		{`SliceThickness between 1 2`, 25},
		{`StudyDate last 30 fortnights`, 18},
		{`StudyDate = 30d`, 12},
		{`StudyDate = 2024-01-31x`, 12},
		{`StudyDate = 1e999`, 12},
		{`has study (Modality present)`, 4},
		{`has scan Modality present`, 9},
		{`(0029,"X") present`, 0},
		{`(0029,"X",1) present`, 0},
		{`(0018,005) present`, 0},
		{`ImageType[x] present`, 10},
		{`ImageType[1024] present`, 10},
		{`Modality nocase < 3`, 16},
		{`Modality nocase present`, 16},
		{`Modality = "a` + "\x01" + `"`, 13},
		{`Modality = "a` + "\xff" + `"`, 13},
		{`StudyDate = 2024-02-30`, -1},
		{`StudyTime = 25:00`, -1},
		{`Modality < "CT"`, -1},
		{`SliceThickness contains 5`, -1},
		{`Manufacturer matches "("`, -1},
		{`Manufacturer nocase matches "x"`, 20},
		{`Manufacturer matches "(ab|cd|ef){400}"`, -1},
		{`ProtocolName words "  "`, -1},
		{`Modality in ("CT", 3)`, -1},
		{`(0010,"CREATOR",10) present`, -1},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		var se *SyntaxError
		switch {
		case err == nil:
			t.Errorf("Parse(%q) took it", c.in)
		case c.offset < 0:
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Parse(%q) = %v, want an error wrapping ErrInvalid", c.in, err)
			}
		case !errors.As(err, &se):
			t.Errorf("Parse(%q) = %v, want a *SyntaxError", c.in, err)
		case se.Offset != c.offset:
			t.Errorf("Parse(%q) failed at offset %d (%v), want %d", c.in, se.Offset, err, c.offset)
		}
	}
}

// TestParseBounds guards each bound of a condition: one past it is refused
// at the offset where it is passed, and the bound itself is taken.
func TestParseBounds(t *testing.T) {
	leaf := `Modality present`
	chain := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = leaf
		}
		return strings.Join(parts, " and ")
	}
	nested := func(n int) string { return strings.Repeat("(", n) + leaf + strings.Repeat(")", n) }
	nots := func(n int) string { return strings.Repeat("not ", n) + leaf }
	list := func(n int) string {
		vs := make([]string, n)
		for i := range vs {
			vs[i] = strconv.Itoa(i)
		}
		return "SeriesNumber in (" + strings.Join(vs, ",") + ")"
	}
	path := func(n int) string {
		return strings.Repeat("ReferencedSeriesSequence.", n-1) + "SeriesInstanceUID present"
	}
	str := func(n int) string { return `Modality == "` + strings.Repeat("x", n) + `"` }
	cases := []struct {
		name     string
		ok, over string
		offset   int
	}{
		{"leaves", chain(MaxLeaves), chain(MaxLeaves + 1), MaxLeaves * len(leaf+" and ")},
		{"parentheses", nested(MaxDepth), nested(MaxDepth + 1), MaxDepth},
		{"not", nots(MaxDepth - 1), nots(MaxDepth), 0},
		{"list", list(MaxListValues), list(MaxListValues + 1), len(list(MaxListValues))},
		{"path", path(MaxPathSteps), path(MaxPathSteps + 1), MaxPathSteps * len("ReferencedSeriesSequence.")},
		{"value", str(MaxValueLength), str(MaxValueLength + 1), len(`Modality == `)},
	}
	for _, c := range cases {
		if _, err := Parse(c.ok); err != nil {
			t.Errorf("%s: the bound itself was refused: %v", c.name, err)
		}
		_, err := Parse(c.over)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("%s: one past the bound gave %v, want a *SyntaxError", c.name, err)
			continue
		}
		if se.Offset != c.offset {
			t.Errorf("%s: refused at %d (%v), want %d", c.name, se.Offset, err, c.offset)
		}
	}
	long := strings.Repeat(" ", MaxLength) + "x"
	if _, err := Parse(long); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("a condition longer than MaxLength gave %v", err)
	}
	deepTree := nots(MaxDepth-1) + " and " + leaf
	if _, err := Parse(deepTree); err == nil {
		t.Errorf("a tree deeper than MaxDepth was taken")
	}
}

// TestCheckRefusesBuiltTrees guards Check over trees built in code, which
// do not pass through the parser's bounds.
func TestCheckRefusesBuiltTrees(t *testing.T) {
	f := Field{Path: []Step{{Tag: 0x00080060}}}
	text := []Value{{Kind: KindText, Text: "CT"}}
	var deepNode Node = &Leaf{Field: f, Op: OpPresent}
	for range MaxDepth {
		deepNode = &Not{Child: deepNode}
	}
	many := &Or{}
	for range MaxLeaves + 1 {
		many.Children = append(many.Children, &Leaf{Field: f, Op: OpPresent})
	}
	cases := map[string]Node{
		"nil":            nil,
		"nil leaf":       (*Leaf)(nil),
		"one child":      &And{Children: []Node{&Leaf{Field: f, Op: OpPresent}}},
		"deep":           deepNode,
		"many":           many,
		"no op":          &Leaf{Field: f, Values: text},
		"arity":          &Leaf{Field: f, Op: OpBetween, Values: text},
		"no field":       &Leaf{Op: OpPresent},
		"fold number":    &Leaf{Field: f, Op: OpEqual, Fold: true, Values: []Value{{Kind: KindNumber, Number: 1}}},
		"contains exact": &Leaf{Field: f, Op: OpContains, Values: text},
		"bad level":      &Has{Level: "study", Child: &Leaf{Field: f, Op: OpPresent}},
		"bad unit":       &Leaf{Field: f, Op: OpEqual, Values: []Value{{Kind: KindRelative, Relative: 1, Unit: 'x'}}},
		"even private":   &Leaf{Field: Field{Path: []Step{{Tag: 0x00080010, Creator: "X"}}}, Op: OpPresent},
		"catalog path":   &Leaf{Field: Field{Catalog: "calling_ae", Path: []Step{{Tag: 1}}}, Op: OpPresent},
		"catalog name":   &Leaf{Field: Field{Catalog: "Calling"}, Op: OpPresent},
		"index alone":    &Leaf{Field: Field{Path: []Step{{Tag: 0x00080060}}, Index: 3}, Op: OpPresent},
		"control text":   &Leaf{Field: f, Op: OpEqual, Values: []Value{{Kind: KindText, Text: "a\nb"}}},
		"nocase matches": &Leaf{Field: f, Op: OpMatches, Fold: true, Values: []Value{{Kind: KindText, Text: "x"}}},
	}
	for name, n := range cases {
		if err := Check(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Check = %v, want an error wrapping ErrInvalid", name, err)
		}
	}
}

// TestCheckStopsAtMaxDepth guards that Check refuses a tree past MaxDepth
// before it looks further down, for every kind of node: a chain of "not"
// or "has" a million deep, whose bottom leaf is also invalid, is refused
// for its depth, quickly, and never for the leaf it does not reach.
func TestCheckStopsAtMaxDepth(t *testing.T) {
	bad := &Leaf{Op: OpPresent} // no field: invalid if ever reached
	for name, wrap := range map[string]func(Node) Node{
		"not": func(n Node) Node { return &Not{Child: n} },
		"has": func(n Node) Node { return &Has{Level: LevelScan, Child: n} },
		"and": func(n Node) Node { return &And{Children: []Node{n, &Leaf{Field: Field{Catalog: "x"}, Op: OpPresent}}} },
	} {
		var n Node = bad
		for range 1_000_000 {
			n = wrap(n)
		}
		start := time.Now()
		err := Check(n)
		if err == nil || !strings.Contains(err.Error(), "levels deep") {
			t.Errorf("%s: Check = %v, want a refusal for depth", name, err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("%s: Check took %v", name, d)
		}
	}
}

// TestMatchesProgramBound guards the bound on a pattern's compiled size:
// a short pattern whose program is large is refused by Check, one within
// the bound taken, and a pattern compiled once is reused.
func TestMatchesProgramBound(t *testing.T) {
	leaf := func(p string) *Leaf {
		return &Leaf{Field: Field{Path: []Step{{Tag: 0x00080070}}}, Op: OpMatches, Values: []Value{{Kind: KindText, Text: p}}}
	}
	for p, ok := range map[string]bool{
		`^(GE|SIEMENS)$`:           true,
		`[a-z]{500}`:               true,
		`(ab|cd|ef){400}`:          false,
		`[a-z]{999}[a-z]{999}x{9}`: false,
		`(?i)toshiba_[a-z]+$`:      true,
	} {
		err := Check(leaf(p))
		if (err == nil) != ok || err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: Check = %v, want taken %v", p, err, ok)
		}
	}
	a, err1 := compilePattern(`^x+$`)
	b, err2 := compilePattern(`^x+$`)
	if err1 != nil || err2 != nil || a != b {
		t.Errorf("a pattern was compiled twice: %p %p %v %v", a, b, err1, err2)
	}
}

// TestInjectionStaysValue guards that text meant to escape a value, the
// condition or SQL stays one value: each reads to a single leaf whose
// value is the text, and prints back to a condition that reads the same.
func TestInjectionStaysValue(t *testing.T) {
	values := []string{
		`CT" or Modality present or "`,
		`x") or (Modality present`,
		`'; DROP TABLE scans; --`,
		`%_\`,
		`" and not "`,
		`*?[]`,
		`$1`,
	}
	for _, v := range values {
		text := `Modality == ` + quote(v)
		n := mustParse(t, text)
		l, ok := n.(*Leaf)
		if !ok || len(l.Values) != 1 || l.Values[0].Text != v {
			t.Errorf("Parse(%q) = %#v, want one leaf holding the text", text, n)
			continue
		}
		if again := mustParse(t, Print(n)); !reflect.DeepEqual(again, n) {
			t.Errorf("the value %q did not survive printing", v)
		}
	}
	// Unescaped, the same text is refused, never read as more condition.
	if _, err := Parse(`Modality == "CT" or Modality present or ""x`); err == nil {
		t.Errorf("text after a closed string was taken")
	}
}

// TestSyntaxErrorQuotesNoValue guards that a refusal never quotes a value
// of the condition, which in a routing rule may be a patient's name.
func TestSyntaxErrorQuotesNoValue(t *testing.T) {
	for _, in := range []string{
		`PatientName = "DOE^JANE" PatientID`,
		`PatientName = "DOE^JANE" and`,
		`PatientName == "DOE^JANE` + strings.Repeat("x", MaxValueLength) + `"`,
	} {
		_, err := Parse(in)
		if err == nil || strings.Contains(err.Error(), "DOE") {
			t.Errorf("Parse(%q) = %v", in, err)
		}
	}
}
