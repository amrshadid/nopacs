package query

import (
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
)

// corpusEnv names pydicom's directory of public test files.
const corpusEnv = "NOPACS_PYDICOM_CORPUS"

// receiveTestdata holds the public files the receiver's tests use, always
// present.
const receiveTestdata = "../receive/testdata"

// readFile reads a DICOM file's data set as routing is given it.
func readFile(t *testing.T, path string) *dataset.Dataset {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 G703 -- a test's own corpus path.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(f), filereader.ReadOptions{
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return df.GetDataset()
}

// corpusFile reads a file of pydicom's corpus, skipping the test when the
// corpus is not given.
func corpusFile(t *testing.T, name string) *dataset.Dataset {
	t.Helper()
	dir := os.Getenv(corpusEnv)
	if dir == "" {
		t.Skip(corpusEnv + " is not set")
	}
	return readFile(t, filepath.Join(dir, name))
}

// mustField parses a field or fails the test.
func mustField(t *testing.T, s string) Field {
	t.Helper()
	f, err := ParseField(s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// answer runs a compiled search and returns the ids it selects.
func answer(t *testing.T, db *sql.DB, q SQL, key string) []int {
	t.Helper()
	rows, err := db.Query(`SELECT `+q.Alias+`.`+key+` FROM `+q.From+` WHERE `+q.Where+` ORDER BY 1`, q.Args...) // #nosec G202 -- the text is the compiler's: registry names and placeholders.
	if err != nil {
		t.Fatalf("%s: %v", q.Where, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// planLines returns the lines of a query plan: the last column of each
// row the query returns.
func planLines(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		var line string
		for i := range vals {
			vals[i] = new(any)
		}
		vals[len(vals)-1] = &line
		if err := rows.Scan(vals...); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// short returns the first ids of a list, enough to read in a failure.
func short(ids []int) []int {
	if len(ids) > 20 {
		return ids[:20]
	}
	return ids
}

// mustParse parses a condition or fails the test.
func mustParse(t testing.TB, text string) Node {
	t.Helper()
	n, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return n
}
