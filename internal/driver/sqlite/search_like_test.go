package sqlite

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
)

// TestLikeOnExactTextKeepsSpacesAtTheEnds guards "like" on an exact
// comparison against what routing's evaluator answers: a value with a space
// at either end, and a pattern with a space beside a wildcard or at an end,
// match as the pattern reads over the exact value, though the folded column
// that finds the rows has lost the value's spaces at its ends.
func TestLikeOnExactTextKeepsSpacesAtTheEnds(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, s := range []string{`CREATE TABLE subjects (id INTEGER PRIMARY KEY, label TEXT, folded TEXT)`,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY, subject_id INTEGER, study_date TEXT, receiver TEXT, calling_ae TEXT, called_ae TEXT)`,
		`CREATE TABLE scans (id INTEGER PRIMARY KEY, session_id INTEGER)`, search.DDL(search.SQLite)} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	values := []string{" T1 axial", "T1 axial ", "  Brain scan  ", "T1 axial", "T1  axial", "x y"}
	for i, v := range values {
		if _, err := db.Exec(`INSERT INTO scans (id, session_id) VALUES (?, 1)`, i+1); err != nil {
			t.Fatal(err)
		}
		row := search.Empty()
		row.Source = "1.2." + strconv.Itoa(i+1)
		row.Cells[1].Texts = []string{v}
		if _, err := db.Exec(search.UpsertSQL(search.Replace), search.Args(int64(i+1), row)...); err != nil {
			t.Fatal(err)
		}
	}
	glob := func(p string) *regexp.Regexp {
		var b strings.Builder
		b.WriteString("(?s)^")
		for _, ch := range p {
			switch ch {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		return regexp.MustCompile(b.String() + "$")
	}
	compared := 0
	for _, p := range []string{"* T1*", " T1*", "*axial ", "*al *", "* axial", "*1 a*", "* Brain*", " Brain*", "*scan *", "*n  *", "*T1 ax*", "T1*", "*xial"} {
		n, err := query.Parse(`SeriesDescription like ` + strconv.Quote(p))
		if err != nil {
			t.Fatal(err)
		}
		c := query.Compiler{Registry: search.Registry(), Dialect: query.SQLite, Now: time.Now()}
		q, err := c.Compile(n, query.LevelScan)
		if err != nil {
			continue // not served by an index: nothing to compare
		}
		// #nosec G202 -- the compiler's text: registry names and placeholders.
		rows, err := db.Query(`SELECT t0.scan_id FROM `+q.From+` WHERE `+q.Where, q.Args...)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int]bool{}
		for rows.Next() {
			var id int
			_ = rows.Scan(&id)
			got[id] = true
		}
		_ = rows.Close()
		for i, v := range values {
			if want := glob(p).MatchString(v); got[i+1] != want {
				t.Errorf("like %q over %q: search %v, routing %v", p, v, got[i+1], want)
			}
		}
		compared++
	}
	if compared < 8 {
		t.Errorf("only %d patterns were served and compared", compared)
	}
}
