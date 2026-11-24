package sqlkit

import (
	"crypto/sha256"
	"io/fs"
	"testing"
	"testing/fstest"
)

// TestLoad guards the reading of a migration directory: files in order of
// their number, the no-transaction mark read from the first line only, and
// the checksum taken of the text.
func TestLoad(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0001_init.sql":  {Data: []byte("CREATE TABLE a (x INTEGER);\n")},
		"m/0002_index.sql": {Data: []byte(NoTransactionMark + "\nCREATE INDEX i ON a (x);\n")},
		"m/0003_more.sql":  {Data: []byte("SELECT 1;\n" + NoTransactionMark + "\n")},
	}
	got, err := Load(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("loaded %d migrations, want 3", len(got))
	}
	for i, want := range []struct {
		name string
		noTx bool
	}{{"0001_init.sql", false}, {"0002_index.sql", true}, {"0003_more.sql", false}} {
		if got[i].Version != i+1 || got[i].Name != want.name || got[i].NoTransaction != want.noTx {
			t.Errorf("migration %d = %d %s noTx=%v, want %d %s noTx=%v",
				i, got[i].Version, got[i].Name, got[i].NoTransaction, i+1, want.name, want.noTx)
		}
	}
	if got[0].Sum != sha256.Sum256([]byte("CREATE TABLE a (x INTEGER);\n")) {
		t.Error("the checksum is not the SHA-256 of the file")
	}
}

// TestLoadLineEndings guards that a file checked out with CRLF line endings,
// as git may do on Windows, has the same checksum as the same file with LF,
// so binaries built on the two systems agree about an applied migration.
func TestLoadLineEndings(t *testing.T) {
	lf, err := Load(fstest.MapFS{"m/0001_a.sql": {Data: []byte("-- a\nSELECT 1;\n")}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := Load(fstest.MapFS{"m/0001_a.sql": {Data: []byte("-- a\r\nSELECT 1;\r\n")}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if lf[0].Sum != crlf[0].Sum || lf[0].SQL != crlf[0].SQL {
		t.Error("CRLF and LF copies of one file differ in checksum or text")
	}
	noTx, err := Load(fstest.MapFS{"m/0001_a.sql": {Data: []byte(NoTransactionMark + "\r\nSELECT 1;\r\n")}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !noTx[0].NoTransaction {
		t.Error("the no-transaction mark is not seen before a CRLF")
	}
}

// TestLoadRefuses guards that a directory that is not a clean sequence is
// refused rather than partly applied: a gap, a duplicate, a misnamed file, a
// subdirectory, and an empty directory.
func TestLoadRefuses(t *testing.T) {
	sql := &fstest.MapFile{Data: []byte("SELECT 1;")}
	cases := map[string]fstest.MapFS{
		"gap":          {"m/0001_a.sql": sql, "m/0003_c.sql": sql},
		"starts at 2":  {"m/0002_a.sql": sql},
		"duplicate":    {"m/0001_a.sql": sql, "m/0001_b.sql": sql},
		"misnamed":     {"m/0001_a.sql": sql, "m/2_b.sql": sql},
		"upper case":   {"m/0001_A.sql": sql},
		"other file":   {"m/0001_a.sql": sql, "m/README": sql},
		"subdirectory": {"m/0001_a.sql": sql, "m/0002_b.sql/x": sql},
		"empty":        {"m": &fstest.MapFile{Mode: fs.ModeDir}},
	}
	for name, fsys := range cases {
		if _, err := Load(fsys, "m"); err == nil {
			t.Errorf("%s: loaded, want an error", name)
		}
	}
}
