package query

import (
	"database/sql"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"
	// The compiled SQL is checked against each database itself; nothing the
	// binary runs imports this test, and the drivers keep their own
	// libraries' only use outside it.
	_ "github.com/jackc/pgx/v5/stdlib" //nolint:depguard // the check runs the compiled SQL on Postgres itself
	_ "modernc.org/sqlite"             //nolint:depguard // the check runs the compiled SQL on SQLite itself
)

// scanRow is one row of the scan search table the database checks fill.
type scanRow struct {
	id, session     int
	description     string
	modality        string
	thickness       *float64
	protocol, maker string
	imageType       string // CS values separated by "\"
	operators       string // a PN value, its groups separated by "="
}

// sessionRow is one row of the session search table; date is "" for none.
type sessionRow struct {
	id, subject int
	date        string
}

// fixture is the rows the database checks hold, and the data set routing
// reads for each scan.
type fixture struct {
	subjects []int
	sessions []sessionRow
	scans    []scanRow
	sets     map[int]*dataset.Dataset
}

// fillerScans is how many plain rows follow the rows written by hand.
const fillerScans = 3000

// newFixture returns the rows: text with case, accents (composed and
// decomposed), "_", "%", "[", Greek with a final sigma and Han; NULL
// numbers, absent makers, protocols and dates; multi-valued attributes
// and person names with three component groups; then fillerScans plain
// rows, a third without a slice thickness.
func newFixture() *fixture {
	f := func(v float64) *float64 { return &v }
	fx := &fixture{
		sessions: []sessionRow{{1, 1, "20240115"}, {2, 1, "20230101"}, {3, 2, "20261101"}, {4, 2, "20250601"}, {5, 3, ""}},
		scans: []scanRow{
			{1, 1, "T1_MPRAGE", "MR", f(1), "T1W sag", "SIEMENS", `ORIGINAL\PRIMARY\AXIAL`, "Doe^John=山田^太郎=やまだ^たろう"},
			{2, 1, "Localizer", "MR", f(5), "loc", "", `ORIGINAL\PRIMARY\LOCALIZER`, ""},
			{3, 2, "CT Head 5mm", "CT", f(5), "head", "GE", `ORIGINAL\PRIMARY\AXIAL`, "Roe^Ann"},
			{4, 2, "t1 mprage repeat", "MR", nil, "T1W axial", "GE", `DERIVED\SECONDARY`, ""},
			{5, 3, "Ünï Täst 100%", "PT", f(2), "pet", "GE", "", "Doe^Jane"},
			{6, 3, "T1XMPRAGE", "MR", f(0.9), "T1W sag", "SIEMENS", "", ""},
			{7, 4, "Müller Straße", "MR", nil, "", "", "", ""},
			{8, 4, "[MPRAGE] ΟΔΟΣ", "MR", nil, "", "", `DERIVED\PRIMARY`, ""},
			{9, 4, "山田太郎 οδος", "CT", nil, "", "", "", ""},
			{10, 5, "plain scan", "MR", f(3), "plain", "GE", "", ""},
		},
		subjects: []int{1, 2, 3},
		sets:     map[int]*dataset.Dataset{},
	}
	modalities := []string{"MR", "CT", "PT", "US", "XA", "NM"}
	for i := range fillerScans {
		session := 101 + i/3
		if i%3 == 0 {
			date := "2020" + pad2(1+i%12) + pad2(1+i%28)
			fx.sessions = append(fx.sessions, sessionRow{session, 10 + session%40, date})
		}
		r := scanRow{id: 1001 + i, session: session, description: "filler series " + strconv.Itoa(i), modality: modalities[i%6]}
		if i%3 != 0 {
			r.thickness = f(float64(i%7) / 2)
		}
		if i%4 != 0 {
			r.protocol = "proto " + strconv.Itoa(i%13)
		}
		if i%2 != 0 {
			r.maker = "VENDOR" + strconv.Itoa(i%5)
		}
		if i%5 != 0 {
			r.imageType = `ORIGINAL\PRIMARY\OTHER`
		}
		if i%11 == 0 {
			r.operators = "Filler^Op" + strconv.Itoa(i%7)
		}
		fx.scans = append(fx.scans, r)
	}
	for s := 10; s < 50; s++ {
		fx.subjects = append(fx.subjects, s)
	}
	for _, r := range fx.scans {
		fx.sets[r.id] = r.dataSet()
	}
	return fx
}

// pad2 writes n, from 0 to 99, in two digits.
func pad2(n int) string { return strconv.Itoa(100 + n)[1:] }

// dataSet returns a row as the data set routing would read.
func (r scanRow) dataSet() *dataset.Dataset {
	ds := dataset.NewDataset()
	add := func(g, e uint16, vr dataelem.VR, v string) {
		if v != "" {
			_ = ds.Add(dataelem.NewDataElement(tag.New(g, e), vr, []byte(v)))
		}
	}
	add(0x0008, 0x103E, dataelem.LO, r.description)
	add(0x0008, 0x0060, dataelem.CS, r.modality)
	add(0x0008, 0x0070, dataelem.LO, r.maker)
	add(0x0008, 0x0008, dataelem.CS, r.imageType)
	add(0x0008, 0x1070, dataelem.PN, r.operators)
	add(0x0018, 0x1030, dataelem.LO, r.protocol)
	if r.thickness != nil {
		add(0x0018, 0x0050, dataelem.DS, strconv.FormatFloat(*r.thickness, 'g', -1, 64))
	}
	return ds
}

// session returns a session's row.
func (fx *fixture) session(id int) sessionRow {
	for _, s := range fx.sessions {
		if s.id == id {
			return s
		}
	}
	return sessionRow{}
}

// related returns the ids of the rows at level to that belong with the row
// id at level from.
func (fx *fixture) related(from, to Level, id int) []int {
	var out []int
	switch {
	case from == LevelSession && to == LevelScan:
		for _, r := range fx.scans {
			if r.session == id {
				out = append(out, r.id)
			}
		}
	case from == LevelScan && to == LevelSession:
		for _, r := range fx.scans {
			if r.id == id {
				out = append(out, r.session)
			}
		}
	case from == LevelSubject && to == LevelSession:
		for _, s := range fx.sessions {
			if s.subject == id {
				out = append(out, s.id)
			}
		}
	case from == LevelSession && to == LevelSubject:
		out = append(out, fx.session(id).subject)
	}
	return out
}

// holds is the reference answer: a leaf as routing evaluates it over the
// row (the scan's data set, or the session's and subject's catalog
// fields), and "has" over the rows that belong with it.
func (fx *fixture) holds(t *testing.T, n Node, level Level, id int) bool {
	t.Helper()
	switch n := n.(type) {
	case *And:
		for _, c := range n.Children {
			if !fx.holds(t, c, level, id) {
				return false
			}
		}
		return true
	case *Or:
		for _, c := range n.Children {
			if fx.holds(t, c, level, id) {
				return true
			}
		}
		return false
	case *Not:
		return !fx.holds(t, n.Child, level, id)
	case *Has:
		some := false
		for _, r := range fx.related(level, n.Level, id) {
			ok := fx.holds(t, n.Child, n.Level, r)
			some = some || ok
			if n.Every && !ok {
				return false
			}
		}
		return some
	case *Leaf:
		s := Subject{Dataset: dataset.NewDataset(), Catalog: func(name string) (string, bool, error) {
			switch {
			case level == LevelSession && name == "session.date":
				d := fx.session(id).date
				return d, d != "", nil
			case level == LevelSubject && name == "subject.label":
				return "SUBJ-" + strconv.Itoa(id), true, nil
			}
			return "", false, errors.New("no such field")
		}}
		if level == LevelScan {
			s.Dataset = fx.sets[id]
		}
		r, err := Eval(n, s)
		if err != nil {
			t.Fatal(err)
		}
		return r.Truth == True
	}
	t.Fatalf("a node is missing")
	return false
}

// ids returns the ids of the rows at a level, in order.
func (fx *fixture) ids(level Level) []int {
	var out []int
	switch level {
	case LevelScan:
		for _, r := range fx.scans {
			out = append(out, r.id)
		}
	case LevelSession:
		for _, s := range fx.sessions {
			out = append(out, s.id)
		}
	case LevelSubject:
		out = append(out, fx.subjects...)
	}
	slices.Sort(out)
	return out
}

// checked is a condition the databases answer, at the level of its rows.
type checked struct {
	cond  string
	level Level
}

// conditions are what each database answers as the reference does: every
// operator, NULL-heavy columns under "not", decomposed and non-Latin text,
// "[" in prefixes and patterns, multi-valued attributes and person names,
// and "has" and "every" up and down the levels.
var conditions = []checked{
	{`Modality == "MR"`, LevelScan},
	{`Modality = "mr"`, LevelScan},
	{`Modality in ("CT", "PT")`, LevelScan},
	{`SliceThickness >= 2`, LevelScan},
	{`SliceThickness between 0.9 and 1`, LevelScan},
	{`SliceThickness absent`, LevelScan},
	{`SliceThickness present and Modality == "MR"`, LevelScan},
	{`Modality == "MR" and not SliceThickness = 5`, LevelScan},
	{`Modality == "MR" and not SliceThickness in (1, 5)`, LevelScan},
	{`Modality == "CT" and not SliceThickness between 1 and 3`, LevelScan},
	{`Modality == "MR" and not (SliceThickness between 0.5 and 1 or Manufacturer in ("GE"))`, LevelScan},
	{`not Manufacturer == "GE" and Modality == "MR"`, LevelScan},
	{`SeriesDescription prefix "T1_"`, LevelScan},
	{`SeriesDescription prefix "T1"`, LevelScan},
	{`SeriesDescription prefix "[MPR"`, LevelScan},
	{`SeriesDescription nocase prefix "t1"`, LevelScan},
	{`SeriesDescription nocase prefix "müller"`, LevelScan},
	{`SeriesDescription = "MÜLLER STRASSE"`, LevelScan},
	{`SeriesDescription like "T1_*"`, LevelScan},
	{`SeriesDescription like "T1?MPRAGE"`, LevelScan},
	{`SeriesDescription like "M??ller*"`, LevelScan},
	{`SeriesDescription like "*Stra?e"`, LevelScan},
	{`SeriesDescription nocase like "*mprage*"`, LevelScan},
	{`SeriesDescription nocase like "*[mprage]*"`, LevelScan},
	{`SeriesDescription like "*MPRAGE"`, LevelScan},
	{`SeriesDescription contains "mprage"`, LevelScan},
	{`SeriesDescription contains "unï"`, LevelScan},
	{`SeriesDescription contains "100%"`, LevelScan},
	{`SeriesDescription contains "_mprage"`, LevelScan},
	{`SeriesDescription contains "[mpr"`, LevelScan},
	{`SeriesDescription contains "ΟΔΟΣ"`, LevelScan},
	{`SeriesDescription contains "山田太"`, LevelScan},
	{`ProtocolName words "sag t1w"`, LevelScan},
	{`ProtocolName present`, LevelScan},
	{`ProtocolName present and Modality == "MR"`, LevelScan},
	{`ProtocolName absent and Modality == "CT"`, LevelScan},
	{`Modality == "MR" or SliceThickness = 5`, LevelScan},
	{`ImageType == "AXIAL"`, LevelScan},
	{`ImageType nocase in ("derived", "localizer")`, LevelScan},
	{`ImageType prefix "LOC"`, LevelScan},
	{`Modality == "MR" and not ImageType == "PRIMARY"`, LevelScan},
	{`ImageType absent and Modality == "MR"`, LevelScan},
	{`OperatorsName == "山田^太郎"`, LevelScan},
	{`OperatorsName = "doe^jane"`, LevelScan},
	{`OperatorsName nocase prefix "doe"`, LevelScan},
	{`has session (session.date between 2024-01-01 and 2024-12-31) and Modality == "MR"`, LevelScan},
	{`has session (session.date >= 2026-01-01)`, LevelScan},
	{`has scan (Modality == "CT")`, LevelSession},
	{`every scan (Modality == "MR")`, LevelSession},
	{`has scan (Modality == "MR") and has scan (Modality == "CT")`, LevelSession},
	{`session.date >= 2024-01-01 and not has scan (Modality == "PT")`, LevelSession},
	{`every scan (SliceThickness <= 1 or SliceThickness absent)`, LevelSession},
	{`has scan (ImageType == "DERIVED")`, LevelSession},
	{`session.date absent and has scan (Modality == "MR")`, LevelSession},
	{`has subject (subject.label = "subj-1")`, LevelSession},
	{`has session (has scan (Modality == "CT"))`, LevelSubject},
	{`every session (session.date >= 2024-01-01)`, LevelSubject},
}

// tableKeys are the key column of each level's table.
var tableKeys = map[Level]string{LevelSubject: "subject_id", LevelSession: "session_id", LevelScan: "scan_id"}

// schema is the test's search tables, with an index for each index the
// registry declares and on each join column, in SQL both databases read;
// text is COLLATE "C" on Postgres, so its order and LIKE are by bytes.
func schema(collate string) []string {
	text := " TEXT" + collate
	stmts := []string{
		`CREATE TABLE subject_search (subject_id INTEGER PRIMARY KEY, label` + text + `, label_folded` + text + `)`,
		`CREATE TABLE session_search (session_id INTEGER PRIMARY KEY, subject_id INTEGER, study_date` + text + `, study_time` + text + `, calling_ae` + text + `)`,
		`CREATE TABLE scan_search (scan_id INTEGER PRIMARY KEY, session_id INTEGER, subject_id INTEGER,
			series_description` + text + `, series_description_folded` + text + `, modality` + text + `, modality_folded` + text + `,
			slice_thickness DOUBLE PRECISION, protocol_name` + text + `, protocol_name_folded` + text + `, manufacturer` + text + `,
			acquisition_datetime` + text + `, csa_0029_10` + text + `)`,
		`CREATE TABLE scan_search_image_type (scan_id INTEGER, image_type` + text + `, image_type_folded` + text + `)`,
		`CREATE TABLE scan_search_operators_name (scan_id INTEGER, operators_name` + text + `, operators_name_folded` + text + `)`,
	}
	for _, ix := range []struct {
		table string
		cols  []string
	}{
		{"subject_search", []string{"label", "label_folded"}},
		{"session_search", []string{"subject_id", "study_date", "study_time", "calling_ae"}},
		{"scan_search", []string{"session_id", "subject_id", "series_description", "series_description_folded", "modality", "modality_folded",
			"slice_thickness", "protocol_name_folded", "acquisition_datetime", "csa_0029_10"}},
		{"scan_search_image_type", []string{"scan_id", "image_type", "image_type_folded"}},
		{"scan_search_operators_name", []string{"scan_id", "operators_name", "operators_name_folded"}},
	} {
		for _, c := range ix.cols {
			stmts = append(stmts, `CREATE INDEX `+ix.table+`_`+c+` ON `+ix.table+` (`+c+`)`)
		}
	}
	return stmts
}

// fill writes the rows in one transaction, each multi-valued attribute's
// values (Values) in its values table.
func fill(t *testing.T, db *sql.DB, fx *fixture, d Dialect) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	insert := func(table, cols string, vals ...any) {
		ph := make([]string, len(vals))
		for i := range vals {
			ph[i] = d.Placeholder(i + 1)
		}
		if _, err := tx.Exec(`INSERT INTO `+table+` (`+cols+`) VALUES (`+strings.Join(ph, ", ")+`)`, vals...); err != nil { // #nosec G202 -- the text is the test's names and placeholders.
			t.Fatal(err)
		}
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	folded := func(s string) any { return nullable(Fold(s)) }
	for _, s := range fx.subjects {
		label := "SUBJ-" + strconv.Itoa(s)
		insert("subject_search", "subject_id, label, label_folded", s, label, Fold(label))
	}
	for _, s := range fx.sessions {
		insert("session_search", "session_id, subject_id, study_date", s.id, s.subject, nullable(s.date))
	}
	multi := []struct {
		table, cols string
		field       Field
	}{
		{"scan_search_image_type", "scan_id, image_type, image_type_folded", mustField(t, "ImageType")},
		{"scan_search_operators_name", "scan_id, operators_name, operators_name_folded", mustField(t, "OperatorsName")},
	}
	for _, r := range fx.scans {
		var th any
		if r.thickness != nil {
			th = *r.thickness
		}
		insert("scan_search", "scan_id, session_id, subject_id, series_description, series_description_folded, modality, modality_folded, "+
			"slice_thickness, protocol_name, protocol_name_folded, manufacturer",
			r.id, r.session, fx.session(r.session).subject, nullable(r.description), folded(r.description), nullable(r.modality), folded(r.modality),
			th, nullable(r.protocol), folded(r.protocol), nullable(r.maker))
		for _, m := range multi {
			vals, err := Values(fx.sets[r.id], m.field, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				insert(m.table, m.cols, r.id, v, Fold(v))
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// checkDatabase fills the database (prepare finishes what the database
// needs after), compares its answers with the reference's, so search and
// routing mean the same by one condition, and checks with scans, which
// returns the lines of a condition's plan that read a table whole, that no
// condition the compiler serves is answered so.
func checkDatabase(t *testing.T, db *sql.DB, d Dialect, prepare func(), scans func(q SQL) []string) {
	fx := newFixture()
	fill(t, db, fx, d)
	prepare()
	c := Compiler{Registry: testRegistry(t), Dialect: d, Now: time.Now()}
	for _, k := range conditions {
		n := mustParse(t, k.cond)
		q, err := c.Compile(n, k.level)
		if err != nil {
			t.Errorf("%q: %v", k.cond, err)
			continue
		}
		var want []int
		all := fx.ids(k.level)
		for _, id := range all {
			if fx.holds(t, n, k.level, id) {
				want = append(want, id)
			}
		}
		if len(want) == 0 || len(want) == len(all) {
			t.Errorf("%q selects %d of %d rows: a condition that keeps no row or every row proves nothing", k.cond, len(want), len(all))
		}
		if got := answer(t, db, q, tableKeys[k.level]); !slices.Equal(got, want) {
			t.Errorf("%q: the database selects %v, routing %v", k.cond, short(got), short(want))
		}
		if bad := scans(q); len(bad) > 0 {
			t.Errorf("%q reads a table whole: %s\n%s", k.cond, strings.Join(bad, "; "), q.Where)
		}
	}
}

// TestSQLiteAnswersAsRouting guards the compiled SQL against SQLite itself
// (modernc, with FTS5's trigram tokenizer): it runs, it selects the rows
// routing selects, and its query plan reads no table whole (no "SCAN" of a
// table; an FTS5 table is searched through its index). The tables are not
// analyzed, so the planner takes an index wherever one can serve, and a
// scan in the plan means none could.
func TestSQLiteAnswersAsRouting(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	stmts := append(schema(""),
		`CREATE VIRTUAL TABLE scan_search_description_fts USING fts5(series_description_folded, content='scan_search', content_rowid='scan_id', tokenize='trigram')`,
		`CREATE VIRTUAL TABLE scan_search_protocol_fts USING fts5(protocol_name_folded, content='scan_search', content_rowid='scan_id', tokenize='trigram')`)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	prepare := func() {
		for _, s := range []string{
			`INSERT INTO scan_search_description_fts(scan_search_description_fts) VALUES ('rebuild')`,
			`INSERT INTO scan_search_protocol_fts(scan_search_protocol_fts) VALUES ('rebuild')`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkDatabase(t, db, SQLite, prepare, func(q SQL) []string {
		var bad []string
		for _, line := range planLines(t, db, `EXPLAIN QUERY PLAN SELECT 1 FROM `+q.From+` WHERE `+q.Where, q.Args) {
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "VIRTUAL TABLE INDEX") {
				bad = append(bad, line)
			}
		}
		return bad
	})
}

// TestPostgresAnswersAsRouting is the same check against Postgres with
// pg_trgm, in a schema of its own dropped after, when NOPACS_TEST_POSTGRES
// names a server. Its plan check turns sequential scans off: Postgres
// then takes one only where no index can serve, so a "Seq Scan" in a plan
// means none could.
func TestPostgresAnswersAsRouting(t *testing.T) {
	dsn := os.Getenv("NOPACS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NOPACS_TEST_POSTGRES is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	name := "query_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := db.Exec(`CREATE SCHEMA ` + name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + name + ` CASCADE`) }()
	if _, err := db.Exec(`SET search_path TO ` + name + `, public`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	stmts := append(schema(` COLLATE "C"`),
		`CREATE INDEX ON scan_search USING gin (series_description_folded gin_trgm_ops)`,
		`CREATE INDEX ON scan_search USING gin (protocol_name_folded gin_trgm_ops)`)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	prepare := func() {
		for _, s := range []string{`ANALYZE subject_search, session_search, scan_search, scan_search_image_type, scan_search_operators_name`, `SET enable_seqscan = off`} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkDatabase(t, db, Postgres, prepare, func(q SQL) []string {
		var bad []string
		for _, line := range planLines(t, db, `EXPLAIN SELECT 1 FROM `+q.From+` WHERE `+q.Where, q.Args) {
			if strings.Contains(line, "Seq Scan") {
				bad = append(bad, strings.TrimSpace(line))
			}
		}
		return bad
	})
}
