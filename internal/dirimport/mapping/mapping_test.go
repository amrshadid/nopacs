package mapping

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/importpath"
)

// The map is what an import's layout looks a part up in.
var _ importpath.KeyedMap = (*Map)(nil)

// cols is the configuration most tests read by: a session's subject by
// project and session.
var cols = Columns{Key: []string{"project", "session"}, Value: "subject"}

// TestParse guards reading a map by configured columns: extra columns
// ignored, columns in any order, quoted cells, CRLF line ends, a byte
// order mark, two keys sharing a cell, and a key listed twice with one
// value.
func TestParse(t *testing.T) {
	data := "\xef\xbb\xbfid,subject,project,session,date\r\n" +
		"e1,S01,P1,MR1,2020-01-01\r\n" +
		"e2,\"S,02\",P2,MR1,\r\n" +
		"\r\n" +
		"e3,S01,P1,MR1,2020-01-01\r\n"
	m, err := Parse([]byte(data), MaxInlineBytes, cols)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ project, session, want string }{
		{"P1", "MR1", "S01"}, {"P2", "MR1", "S,02"},
	} {
		if got, ok := m.Lookup([]string{tc.project, tc.session}); !ok || got != tc.want {
			t.Errorf("Lookup(%s, %s) = %q, %v", tc.project, tc.session, got, ok)
		}
	}
	for _, key := range [][]string{{"P3", "MR1"}, {"P1MR", "1"}, {"P1"}, {"P1", "MR1", "x"}, {"MR1", "P1"}} {
		if _, ok := m.Lookup(key); ok {
			t.Errorf("Lookup(%q) found an unlisted key", key)
		}
	}
	if m.Len() != 2 {
		t.Errorf("Len = %d", m.Len())
	}
	// One key column is enough.
	one, err := Parse([]byte("k,v\na,1\nb,2\n"), MaxInlineBytes, Columns{Key: []string{"k"}, Value: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := one.Lookup([]string{"b"}); !ok || v != "2" {
		t.Errorf("one key column: %q %v", v, ok)
	}
}

// TestParseXNATStyleExportExample guards, on the example of a CSV export
// of experiments as XNAT writes one, that the map is read as another CSV
// reader reads it: the values below are what Python's csv.DictReader gives
// for testdata/experiments.csv (quoted commas, doubled quotes, a newline
// inside a cell, non-ASCII labels, columns beyond those configured).
func TestParseXNATStyleExportExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "experiments.csv"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(data, MaxFileBytes, Columns{Key: []string{"project", "label"}, Value: "subject_label"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[[2]string]string{
		{"trial_a", "sub01_MR1"}: "sub01",
		{"trial_a", "sub01_MR2"}: "sub01",
		{"trial_b", "sub01_MR1"}: "sub,02",
		{"trial_b", "MR 3"}:      `say "x"`,
		{"trial_b", "Größe"}:     "two\nlines",
	}
	for k, v := range want {
		if got, ok := m.Lookup(k[:]); !ok || got != v {
			t.Errorf("Lookup(%q) = %q, %v; want %q", k, got, ok, v)
		}
	}
	if m.Len() != len(want) {
		t.Errorf("Len = %d, want %d", m.Len(), len(want))
	}
}

// secret is the text every refused map below holds in a cell, the header
// or both; no error may show it.
const secret = "DoeJane"

// TestParseRefuses guards each refusal, at the line it names, and that no
// error shows a cell, the header or a path.
func TestParseRefuses(t *testing.T) {
	h := "project,session,subject\n"
	for _, tc := range []struct {
		name  string
		data  string
		limit int
		line  int
		first int
		fault Fault
	}{
		{"too large", h + "P,S," + secret + strings.Repeat("x", 100) + "\n", 64, 0, 0, TooLarge},
		{"not utf8", h + "P,S,ok\nP,T," + secret + "\xff\n", MaxInlineBytes, 3, 0, NotUTF8},
		{"empty", "", MaxInlineBytes, 1, 0, NoHeader},
		{"missing column", "project,session," + secret + "\nP,S,X\n", MaxInlineBytes, 1, 0, MissingColumn},
		{"case differs", "Project,session,subject\n", MaxInlineBytes, 1, 0, MissingColumn},
		{"duplicate column", "project,session,subject,session\nP,S,X," + secret + "\n", MaxInlineBytes, 1, 0, DuplicateColumn},
		{"bare quote", h + "P,S,ok\nP,S2,a\"" + secret + "\n", MaxInlineBytes, 3, 0, Malformed},
		{"open quote", h + "P,S,\"" + secret + "\n", MaxInlineBytes, 2, 0, Malformed},
		{"cells", h + "P,S,X\nP,T," + secret + ",extra\n", MaxInlineBytes, 3, 0, WrongCellCount},
		{"empty value", h + "P," + secret + ",\n", MaxInlineBytes, 2, 0, EmptyCell},
		{"empty key cell", h + "," + secret + ",X\n", MaxInlineBytes, 2, 0, EmptyCell},
		{"two values", h + "P,S,A\nQ,S,B\n\"P\",S," + secret + "\n", MaxInlineBytes, 4, 2, TwoValues},
		{"two values across a quoted newline", h + "P,S,\"A\nA\"\nP,S," + secret + "\n", MaxInlineBytes, 4, 2, TwoValues},
	} {
		_, err := Parse([]byte(tc.data), tc.limit, cols)
		var e *Error
		if !errors.As(err, &e) || e.Line != tc.line || e.FirstLine != tc.first || e.Fault != tc.fault {
			t.Errorf("%s: got %#v (%v), want line %d first %d %v", tc.name, e, err, tc.line, tc.first, tc.fault)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "project,") {
			t.Errorf("%s: the error shows the map's text: %v", tc.name, err)
		}
	}
}

// TestParseRefusesColumns guards that a configuration that cannot read a
// map is refused before the map is read, naming no column's text.
func TestParseRefusesColumns(t *testing.T) {
	data := []byte("a,b\n1,2\n")
	for _, c := range []Columns{
		{Key: nil, Value: "b"},
		{Key: []string{"a"}, Value: ""},
		{Key: []string{secret, secret}, Value: "b"},
		{Key: []string{""}, Value: "b"},
		{Key: []string{secret}, Value: secret},
		{Key: strings.Split("a,b,c,d,e,f,g,h,i", ","), Value: "z"},
	} {
		_, err := Parse(data, MaxInlineBytes, c)
		var ce *ConfigError
		if !errors.As(err, &ce) || strings.Contains(err.Error(), secret) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// TestParseLimits guards that a map is taken up to its limit and the
// limits are the contract's: 3 MiB inline, 64 MiB from a file.
func TestParseLimits(t *testing.T) {
	if MaxInlineBytes != 3*1024*1024 || MaxFileBytes != 64*1024*1024 {
		t.Fatal("limits differ from the contract's")
	}
	data := []byte("project,session,subject\nP,S,X\n")
	if _, err := Parse(data, len(data), cols); err != nil {
		t.Errorf("a map at its limit: %v", err)
	}
	if _, err := Parse(data, len(data)-1, cols); err == nil {
		t.Error("a map past its limit was taken")
	}
}

// errorShape is every text an error of this package may have, once its
// numbers are taken out.
var errorShape = func() *regexp.Regexp {
	alts := ""
	for f := TooLarge; f <= TwoValues; f++ {
		if alts != "" {
			alts += "|"
		}
		alts += regexp.QuoteMeta(f.String())
	}
	return regexp.MustCompile(`\Amapping: (line [0-9]+: )?(` + alts + `)(, line [0-9]+)?\z`)
}()

// FuzzParse guards that no input makes Parse fail other than with an
// *Error whose text is a fixed sentence and line numbers, so no cell can
// reach a log, and that no key or value taken is empty.
func FuzzParse(f *testing.F) {
	f.Add([]byte("project,session,subject\nP,S,X\n"))
	f.Add([]byte("\xef\xbb\xbfsubject,project,session\r\n\"a\"\"b\",P,S\r\n"))
	f.Add([]byte("project,session,subject\nP,S,A\nP,S,B\n"))
	f.Add([]byte("project,session\n\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Parse(data, 1<<16, cols)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || !errorShape.MatchString(err.Error()) {
				t.Fatalf("error %q is not of the fixed shape", err)
			}
			return
		}
		for k, e := range m.entries {
			cells := splitKey(t, k)
			if e.value == "" || len(cells) != len(cols.Key) || slices.Contains(cells, "") {
				t.Fatalf("an empty cell was taken: %q %+v", cells, e)
			}
			if v, ok := m.Lookup(cells); !ok || v != e.value {
				t.Fatalf("Lookup(%q) = %q, %v", cells, v, ok)
			}
		}
	})
}

// splitKey undoes joinKey, failing on text joinKey cannot have made.
func splitKey(t *testing.T, k string) []string {
	var out []string
	for k != "" {
		n, rest, ok := strings.Cut(k, ":")
		size, err := strconv.Atoi(n)
		if !ok || err != nil || size > len(rest) {
			t.Fatalf("key %q is not joinKey's", k)
		}
		out = append(out, rest[:size])
		k = rest[size:]
	}
	return out
}
