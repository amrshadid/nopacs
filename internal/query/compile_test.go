package query

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testRegistry is a registry of the shape a search table takes: a table
// per level, text with an exact and a folded column, numbers, dates,
// times, a trigram index on two text fields, a private attribute and a
// field with no index.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry([]Table{
		{Level: LevelSubject, Name: "subject_search", Key: "subject_id"},
		{Level: LevelSession, Name: "session_search", Key: "session_id", Parents: map[Level]string{LevelSubject: "subject_id"}},
		{Level: LevelScan, Name: "scan_search", Key: "scan_id", Row: "rowid", Parents: map[Level]string{LevelSubject: "subject_id", LevelSession: "session_id"}},
	}, []Column{
		{Field: "SeriesDescription", Level: LevelScan, Type: TypeText, Exact: "series_description", Folded: "series_description_folded",
			Index: IndexExact | IndexFolded | IndexTrigram, Trigram: "scan_search_description_fts"},
		{Field: "Modality", Level: LevelScan, Type: TypeText, Exact: "modality", Folded: "modality_folded", Index: IndexExact | IndexFolded},
		{Field: "(0018,0050)", Level: LevelScan, Type: TypeNumber, Exact: "slice_thickness", Index: IndexExact},
		{Field: "ProtocolName", Level: LevelScan, Type: TypeText, Exact: "protocol_name", Folded: "protocol_name_folded",
			Index: IndexFolded | IndexTrigram, Trigram: "scan_search_protocol_fts"},
		{Field: "Manufacturer", Level: LevelScan, Type: TypeText, Exact: "manufacturer"},
		{Field: "AcquisitionDateTime", Level: LevelScan, Type: TypeDateTime, Exact: "acquisition_datetime", Index: IndexExact},
		{Field: `(0029,"SIEMENS CSA HEADER",10)`, Level: LevelScan, Type: TypeText, Exact: "csa_0029_10", Index: IndexExact},
		{Field: "session.date", Level: LevelSession, Type: TypeDate, Exact: "study_date", Index: IndexExact},
		{Field: "StudyTime", Level: LevelSession, Type: TypeTime, Exact: "study_time", Index: IndexExact},
		{Field: "calling_ae", Level: LevelSession, Type: TypeText, Exact: "calling_ae", Index: IndexExact},
		{Field: "subject.label", Level: LevelSubject, Type: TypeText, Exact: "label", Folded: "label_folded", Index: IndexExact | IndexFolded},
		{Field: "ImageType", Level: LevelScan, Type: TypeText, Exact: "image_type", Folded: "image_type_folded",
			Index: IndexExact | IndexFolded, Values: "scan_search_image_type"},
		{Field: "OperatorsName", Level: LevelScan, Type: TypeText, Exact: "operators_name", Folded: "operators_name_folded",
			Index: IndexExact | IndexFolded, Values: "scan_search_operators_name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// compiled is one condition's expected SQL on both dialects.
type compiled struct {
	cond     string
	level    Level
	pg, lite string
	args     []any
	liteArgs []any // when SQLite binds other values; nil: args
}

// TestCompileOperators guards the SQL of each operator on both dialects:
// the text holds only registry names, fixed fragments and placeholders,
// and every value is a parameter, folded when the leaf folds.
func TestCompileOperators(t *testing.T) {
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	cases := []compiled{
		{cond: `Modality == "MR"`, pg: `t0.modality = $1`, lite: `t0.modality = ?`, args: []any{"MR"}},
		{cond: `Modality = "Mr "`, pg: `t0.modality_folded = $1`, lite: `t0.modality_folded = ?`, args: []any{"mr"}},
		{cond: `Modality in ("CT", "MR")`, pg: `t0.modality IN ($1, $2)`, lite: `t0.modality IN (?, ?)`, args: []any{"CT", "MR"}},
		{cond: `Modality nocase in ("CT", "MR")`, pg: `t0.modality_folded IN ($1, $2)`, lite: `t0.modality_folded IN (?, ?)`, args: []any{"ct", "mr"}},
		{cond: `SliceThickness < 1.5`, pg: `t0.slice_thickness < $1`, lite: `t0.slice_thickness < ?`, args: []any{1.5}},
		{cond: `SliceThickness <= 1.5`, pg: `t0.slice_thickness <= $1`, lite: `t0.slice_thickness <= ?`, args: []any{1.5}},
		{cond: `SliceThickness > 1.5`, pg: `t0.slice_thickness > $1`, lite: `t0.slice_thickness > ?`, args: []any{1.5}},
		{cond: `SliceThickness >= 1.5`, pg: `t0.slice_thickness >= $1`, lite: `t0.slice_thickness >= ?`, args: []any{1.5}},
		{cond: `SliceThickness = 1`, pg: `t0.slice_thickness = $1`, lite: `t0.slice_thickness = ?`, args: []any{1.0}},
		{cond: `SliceThickness between 0.5 and 1`, pg: `t0.slice_thickness BETWEEN $1 AND $2`, lite: `t0.slice_thickness BETWEEN ? AND ?`, args: []any{0.5, 1.0}},
		{cond: `SliceThickness present`, pg: `t0.slice_thickness IS NOT NULL`, lite: `t0.slice_thickness IS NOT NULL`},
		{cond: `SliceThickness absent`, pg: `t0.slice_thickness IS NULL`, lite: `t0.slice_thickness IS NULL`},
		{cond: `AcquisitionDateTime >= 2024-01-31T10:00`, pg: `t0.acquisition_datetime >= $1`, lite: `t0.acquisition_datetime >= ?`, args: []any{"20240131100000"}},
		{cond: `session.date between 2024-01-01 and 2024-12-31`, level: LevelSession, pg: `t0.study_date BETWEEN $1 AND $2`, lite: `t0.study_date BETWEEN ? AND ?`, args: []any{"20240101", "20241231"}},
		{cond: `session.date last 30 days`, level: LevelSession, pg: `t0.study_date >= $1`, lite: `t0.study_date >= ?`, args: []any{"20261027"}},
		{cond: `StudyTime < 12:00`, level: LevelSession, pg: `t0.study_time < $1`, lite: `t0.study_time < ?`, args: []any{"120000"}},
		{cond: `SeriesDescription prefix "T1_%"`, pg: `t0.series_description LIKE $1 ESCAPE '\'`,
			lite: `(t0.series_description >= ? AND t0.series_description < ?)`,
			args: []any{`T1\_\%%`}, liteArgs: []any{`T1_%`, `T1_&`}},
		{cond: `SeriesDescription nocase prefix "T1_"`, pg: `t0.series_description_folded LIKE $1 ESCAPE '\'`,
			lite: `(t0.series_description_folded >= ? AND t0.series_description_folded < ?)`,
			args: []any{`t1\_%`}, liteArgs: []any{`t1_`, "t1`"}},
		{cond: `SeriesDescription prefix "[T"`, pg: `t0.series_description LIKE $1 ESCAPE '\'`,
			lite: `(t0.series_description >= ? AND t0.series_description < ?)`,
			args: []any{`[T%`}, liteArgs: []any{`[T`, `[U`}},
		{cond: `SeriesDescription like "T1*"`, pg: `t0.series_description LIKE $1 ESCAPE '\'`,
			lite: `(t0.series_description >= ? AND t0.series_description < ?)`,
			args: []any{`T1%`}, liteArgs: []any{`T1`, `T2`}},
		{cond: `SeriesDescription like "T1"`, pg: `t0.series_description = $1`, lite: `t0.series_description = ?`, args: []any{"T1"}},
		{cond: `SeriesDescription nocase like "*mprage*"`, pg: `t0.series_description_folded LIKE $1 ESCAPE '\'`,
			lite: `t0.rowid IN (SELECT rowid FROM scan_search_description_fts WHERE series_description_folded GLOB ?)`,
			args: []any{`%mprage%`}, liteArgs: []any{`*mprage*`}},
		{cond: `SeriesDescription like "*MPR?GE"`,
			pg:   `(t0.series_description_folded LIKE $1 ESCAPE '\' AND t0.series_description LIKE $2 ESCAPE '\')`,
			lite: `(t0.rowid IN (SELECT rowid FROM scan_search_description_fts WHERE series_description_folded GLOB ?) AND t0.series_description GLOB ?)`,
			args: []any{`%mpr%ge`, `%MPR_GE`}, liteArgs: []any{`*mpr*ge`, `*MPR?GE`}},
		{cond: `SeriesDescription contains "MPRAGE"`, pg: `t0.series_description_folded LIKE $1 ESCAPE '\'`,
			lite: `(t0.rowid IN (SELECT rowid FROM scan_search_description_fts WHERE scan_search_description_fts MATCH ?) AND instr(t0.series_description_folded, ?) > 0)`,
			args: []any{`%mprage%`}, liteArgs: []any{`"mprage"`, "mprage"}},
		{cond: `SeriesDescription contains "a\"bcd%e"`, pg: `t0.series_description_folded LIKE $1 ESCAPE '\'`,
			lite: `(t0.rowid IN (SELECT rowid FROM scan_search_description_fts WHERE scan_search_description_fts MATCH ?) AND instr(t0.series_description_folded, ?) > 0)`,
			args: []any{`%a"bcd\%e%`}, liteArgs: []any{`"a""bcd%e"`, `a"bcd%e`}},
		{cond: `ProtocolName words "T1W sag"`,
			pg: `(t0.protocol_name_folded LIKE $1 ESCAPE '\' AND t0.protocol_name_folded LIKE $2 ESCAPE '\')`,
			lite: `((t0.rowid IN (SELECT rowid FROM scan_search_protocol_fts WHERE scan_search_protocol_fts MATCH ?) AND instr(t0.protocol_name_folded, ?) > 0) AND ` +
				`(t0.rowid IN (SELECT rowid FROM scan_search_protocol_fts WHERE scan_search_protocol_fts MATCH ?) AND instr(t0.protocol_name_folded, ?) > 0))`,
			args: []any{`%t1w%`, `%sag%`}, liteArgs: []any{`"t1w"`, "t1w", `"sag"`, "sag"}},
		{cond: `ProtocolName present`, pg: `t0.protocol_name_folded IS NOT NULL`, lite: `t0.protocol_name_folded IS NOT NULL`},
		{cond: `ImageType == "AXIAL"`,
			pg:   `t0.scan_id IN (SELECT t0v.scan_id FROM scan_search_image_type t0v WHERE t0v.image_type = $1)`,
			lite: `t0.scan_id IN (SELECT t0v.scan_id FROM scan_search_image_type t0v WHERE t0v.image_type = ?)`, args: []any{"AXIAL"}},
		{cond: `OperatorsName = "Doe^Jane" and Modality == "MR"`,
			pg:   `(t0.scan_id IN (SELECT t0v.scan_id FROM scan_search_operators_name t0v WHERE t0v.operators_name_folded = $1) AND t0.modality = $2)`,
			lite: `(t0.scan_id IN (SELECT t0v.scan_id FROM scan_search_operators_name t0v WHERE t0v.operators_name_folded = ?) AND t0.modality = ?)`,
			args: []any{"doe^jane", "MR"}},
		{cond: `(0029,"SIEMENS CSA HEADER",10) == "x"`, pg: `t0.csa_0029_10 = $1`, lite: `t0.csa_0029_10 = ?`, args: []any{"x"}},
		{cond: `Modality == "MR" and not Manufacturer == "GE"`,
			pg:   `(t0.modality = $1 AND NOT COALESCE(t0.manufacturer = $2, FALSE))`,
			lite: `(t0.modality = ? AND NOT COALESCE(t0.manufacturer = ?, FALSE))`, args: []any{"MR", "GE"}},
		{cond: `Modality == "MR" or SliceThickness < 1`,
			pg: `(t0.modality = $1 OR t0.slice_thickness < $2)`, lite: `(t0.modality = ? OR t0.slice_thickness < ?)`, args: []any{"MR", 1.0}},
		{cond: `has scan (Modality == "MR" and SliceThickness <= 1)`, level: LevelSession,
			pg:   `t0.session_id IN (SELECT t1.session_id FROM scan_search t1 WHERE (t1.modality = $1 AND t1.slice_thickness <= $2))`,
			lite: `t0.session_id IN (SELECT t1.session_id FROM scan_search t1 WHERE (t1.modality = ? AND t1.slice_thickness <= ?))`,
			args: []any{"MR", 1.0}},
		{cond: `every scan (Modality == "MR")`, level: LevelSession,
			pg:   `(t0.session_id IN (SELECT t1.session_id FROM scan_search t1 WHERE t1.modality = $1) AND NOT EXISTS (SELECT 1 FROM scan_search t1 WHERE t1.session_id = t0.session_id AND NOT COALESCE(t1.modality = $2, FALSE)))`,
			lite: `(t0.session_id IN (SELECT t1.session_id FROM scan_search t1 WHERE t1.modality = ?) AND NOT EXISTS (SELECT 1 FROM scan_search t1 WHERE t1.session_id = t0.session_id AND NOT COALESCE(t1.modality = ?, FALSE)))`,
			args: []any{"MR", "MR"}},
		{cond: `has session (session.date >= 2024-01-01) and Modality == "MR"`,
			pg:   `(t0.session_id IN (SELECT t1.session_id FROM session_search t1 WHERE t1.study_date >= $1) AND t0.modality = $2)`,
			lite: `(t0.session_id IN (SELECT t1.session_id FROM session_search t1 WHERE t1.study_date >= ?) AND t0.modality = ?)`,
			args: []any{"20240101", "MR"}},
		{cond: `has session (has scan (Modality == "CT"))`, level: LevelSubject,
			pg:   `t0.subject_id IN (SELECT t1.subject_id FROM session_search t1 WHERE t1.session_id IN (SELECT t2.session_id FROM scan_search t2 WHERE t2.modality = $1))`,
			lite: `t0.subject_id IN (SELECT t1.subject_id FROM session_search t1 WHERE t1.session_id IN (SELECT t2.session_id FROM scan_search t2 WHERE t2.modality = ?))`,
			args: []any{"CT"}},
	}
	reg := testRegistry(t)
	for _, c := range cases {
		level := c.level
		if level == "" {
			level = LevelScan
		}
		n := mustParse(t, c.cond)
		for _, d := range []struct {
			name    string
			dialect Dialect
			want    string
			args    []any
		}{{"postgres", Postgres, c.pg, c.args}, {"sqlite", SQLite, c.lite, c.liteArgs}} {
			if d.args == nil {
				d.args = c.args
			}
			got, err := Compiler{Registry: reg, Dialect: d.dialect, Now: now}.Compile(n, level)
			if err != nil {
				t.Errorf("%s: %q: %v", d.name, c.cond, err)
				continue
			}
			if got.Where != d.want {
				t.Errorf("%s: %q\n got %s\nwant %s", d.name, c.cond, got.Where, d.want)
			}
			if !reflect.DeepEqual(got.Args, d.args) {
				t.Errorf("%s: %q: args %#v, want %#v", d.name, c.cond, got.Args, d.args)
			}
			table := map[Level]string{LevelSubject: "subject_search", LevelSession: "session_search", LevelScan: "scan_search"}[level]
			if got.From != table+" t0" || got.Alias != "t0" {
				t.Errorf("%s: From %q", d.name, got.From)
			}
		}
	}
}

// TestCompileServedRule guards the refusals of a search no index serves:
// a leaf without the index its operator needs, an "or" with one such
// child, a "not" alone, a short contains, a pattern without three letters
// in a row, a regular expression; each names the field to index. An "and"
// with one served child is taken.
func TestCompileServedRule(t *testing.T) {
	reg := testRegistry(t)
	c := Compiler{Registry: reg, Dialect: Postgres}
	refused := map[string]string{
		`Manufacturer == "GE"`:                          "Manufacturer",
		`Manufacturer present`:                          "Manufacturer",
		`ProtocolName == "x"`:                           "ProtocolName",
		`Modality == "MR" or Manufacturer == "GE"`:      "Manufacturer",
		`not Modality == "MR"`:                          "",
		`SeriesDescription contains "t1"`:               "SeriesDescription",
		`SeriesDescription contains "é1"`:               "SeriesDescription",
		`ProtocolName words "t1 sagittal"`:              "ProtocolName",
		`SeriesDescription like "*t1*"`:                 "SeriesDescription",
		`SeriesDescription like "*"`:                    "SeriesDescription",
		`Modality nocase like "*mr*"`:                   "Modality",
		`SeriesDescription matches "^T1"`:               "SeriesDescription",
		`Manufacturer == "GE" and not Modality == "MR"`: "Manufacturer",
		`has scan (Manufacturer == "GE")`:               "Manufacturer",
		`SeriesDescription prefix ""`:                   "SeriesDescription",
		`SeriesDescription nocase prefix " "`:           "SeriesDescription",
		`ImageType absent`:                              "ImageType",
		`SeriesDescription contains "a-b-c"`:            "SeriesDescription",
		`SeriesDescription like "*a-b*c-d*"`:            "SeriesDescription",
		`ProtocolName words "t1w s-g"`:                  "ProtocolName",
	}
	for cond, field := range refused {
		level := LevelScan
		if strings.HasPrefix(cond, "has") {
			level = LevelSession
		}
		_, err := c.Compile(mustParse(t, cond), level)
		var ns *NotServedError
		if !errors.As(err, &ns) || ns.Field != field {
			t.Errorf("%q: %v, want a *NotServedError naming %q", cond, err, field)
		}
	}
	for _, cond := range []string{
		`Modality == "MR" and Manufacturer == "GE"`,
		`Manufacturer == "GE" and (Modality == "MR" or SliceThickness < 1)`,
		`SeriesDescription contains "t1x"`,
		`SeriesDescription like "*mpr*"`,
		`Modality == "MR" and not SeriesDescription contains "loc"`,
		`ImageType absent and Modality == "MR"`,
		`SeriesDescription contains "a-bcd"`,
	} {
		if _, err := c.Compile(mustParse(t, cond), LevelScan); err != nil {
			t.Errorf("%q was refused: %v", cond, err)
		}
	}
}

// TestCompileTrigramRunPerDialect guards what each database's trigram
// index serves: pg_trgm makes trigrams only from runs of letters and
// digits, so a term or pattern without three of them in a row is refused
// on Postgres, while the FTS5 trigram tokenizer indexes any three
// characters and SQLite takes it. A term of fewer than three characters
// is refused on both.
func TestCompileTrigramRunPerDialect(t *testing.T) {
	reg := testRegistry(t)
	for cond, takes := range map[string]struct{ pg, lite bool }{
		`SeriesDescription contains "a-b-c"`:  {false, true},
		`SeriesDescription like "*a-b*c-d*"`:  {false, true},
		`ProtocolName words "t1w s-g"`:        {false, true},
		`SeriesDescription contains "a-b"`:    {false, true},
		`SeriesDescription contains "山田太"`:    {true, true},
		`SeriesDescription like "*a-b*cde*"`:  {true, true},
		`SeriesDescription contains "ab"`:     {false, false},
		`SeriesDescription like "*ab*cd*"`:    {false, false},
		`SeriesDescription contains "  ab  "`: {false, false},
	} {
		n := mustParse(t, cond)
		for _, d := range []struct {
			name    string
			dialect Dialect
			want    bool
		}{{"postgres", Postgres, takes.pg}, {"sqlite", SQLite, takes.lite}} {
			_, err := Compiler{Registry: reg, Dialect: d.dialect}.Compile(n, LevelScan)
			var ns *NotServedError
			if (err == nil) != d.want || err != nil && !errors.As(err, &ns) {
				t.Errorf("%s: %q: %v, want taken %v", d.name, cond, err, d.want)
			}
		}
	}
}

// TestCompileFieldRefusals guards the fields a search cannot compare: one
// not in the registry, one of another level, values of another kind, and
// folding a column with no folded form.
func TestCompileFieldRefusals(t *testing.T) {
	c := Compiler{Registry: testRegistry(t), Dialect: SQLite}
	if _, err := c.Compile(mustParse(t, `PatientName == "x"`), LevelScan); !errors.Is(err, ErrUnknownField) {
		t.Errorf("an unknown field gave %v", err)
	}
	for _, cond := range []string{
		`session.date >= 2024-01-01`,
		`SliceThickness == "1"`,
		`Modality = 3`,
		`Manufacturer = "ge"`,
		`AcquisitionDateTime >= 2024-01-01`,
	} {
		if _, err := c.Compile(mustParse(t, cond), LevelScan); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q gave %v, want an error wrapping ErrInvalid", cond, err)
		}
	}
	if _, err := c.Compile(mustParse(t, `has subject (subject.label present)`), LevelSubject); !errors.Is(err, ErrInvalid) {
		t.Errorf("has at its own level gave %v", err)
	}
	if _, err := c.Compile(mustParse(t, `Modality present`), LevelInstance); err == nil {
		t.Errorf("a level with no table was taken")
	}
	got, err := c.Compile(nil, LevelScan)
	if err != nil || got.Where != "1 = 1" || len(got.Args) != 0 {
		t.Errorf("no condition gave %+v, %v", got, err)
	}
}

// TestCompileInjectionStaysParameter guards that no text of a condition
// reaches the SQL: values meant to break out of a string literal or a
// LIKE pattern are parameters, and the SQL is the same whatever they are.
func TestCompileInjectionStaysParameter(t *testing.T) {
	c := Compiler{Registry: testRegistry(t), Dialect: Postgres}
	base, err := c.Compile(mustParse(t, `SeriesDescription == "plain" or SeriesDescription contains "plain"`), LevelScan)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{`'; DROP TABLE scan_search; --`, `x' OR '1'='1 plain`, `%_\ plain`, `") OR 1=1 -- plain`, `$1 $2 plain`} {
		got, err := c.Compile(mustParse(t, `SeriesDescription == `+quote(v)+` or SeriesDescription contains `+quote(v)), LevelScan)
		if err != nil {
			t.Fatal(err)
		}
		if got.Where != base.Where {
			t.Errorf("the value %q changed the SQL: %s", v, got.Where)
		}
		if got.Args[0] != v {
			t.Errorf("the value %q is not its parameter: %#v", v, got.Args)
		}
	}
}

// TestNewRegistryRefusals guards the registry's checks: every name a plain
// identifier, each field once and parsed, a table for its level, folding
// only of text.
func TestNewRegistryRefusals(t *testing.T) {
	tables := []Table{{Level: LevelScan, Name: "scan_search", Key: "scan_id"}}
	col := Column{Field: "Modality", Level: LevelScan, Type: TypeText, Exact: "modality", Index: IndexExact}
	bad := map[string]struct {
		tables []Table
		cols   []Column
	}{
		"quoted table":   {[]Table{{Level: LevelScan, Name: `scan"; DROP`, Key: "id"}}, nil},
		"upper column":   {tables, []Column{{Field: "Modality", Level: LevelScan, Type: TypeText, Exact: "Modality"}}},
		"twice":          {tables, []Column{col, col}},
		"bad field":      {tables, []Column{{Field: "NoSuchKeyword", Level: LevelScan, Type: TypeText, Exact: "x"}}},
		"no table":       {tables, []Column{{Field: "Modality", Level: LevelSession, Type: TypeText, Exact: "x"}}},
		"folded number":  {tables, []Column{{Field: "Rows", Level: LevelScan, Type: TypeNumber, Exact: "r", Folded: "rf"}}},
		"trigram unfold": {tables, []Column{{Field: "Modality", Level: LevelScan, Type: TypeText, Exact: "m", Index: IndexTrigram}}},
		"parent below":   {[]Table{{Level: LevelSession, Name: "s", Key: "id", Parents: map[Level]string{LevelScan: "scan_id"}}}, nil},
		"two tables":     {[]Table{tables[0], tables[0]}, nil},
		"no type":        {tables, []Column{{Field: "Modality", Level: LevelScan, Exact: "m"}}},
	}
	for name, b := range bad {
		if _, err := NewRegistry(b.tables, b.cols); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// TestEscapes guards the translation of literals and patterns into LIKE.
func TestEscapes(t *testing.T) {
	if got := EscapeLike(`a%b_c\d`); got != `a\%b\_c\\d` {
		t.Errorf("EscapeLike = %q", got)
	}
	if got := GlobToLike(`T1*_?%\`); got != `T1%\__\%\\` {
		t.Errorf("GlobToLike = %q", got)
	}
}
