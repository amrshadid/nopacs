package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
)

// lines splits a plaintext into its lines, each without its "\n".
func lines(plain []byte) [][]byte {
	out := bytes.Split(plain, []byte("\n"))
	return out[:len(out)-1] // The empty piece after the last "\n".
}

// lineKind returns the kind of an object line, or "" for a row.
func lineKind(t *testing.T, l []byte) string {
	t.Helper()
	k, err := kindOf(l)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// reseal recomputes every section's SHA-256 and writes them into the
// footer, so a plaintext a test changed is one a writer could have made:
// what a backup made with the right key file can carry.
func reseal(t *testing.T, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var sums []string
	h := sha256.New()
	in := false
	for _, l := range lines(plain) {
		switch lineKind(t, l) {
		case kindSection:
			h.Reset()
			in = true
		case kindFooter:
			var f Footer
			if err := json.Unmarshal(l, &f); err != nil || len(f.Sections) != len(sums) {
				t.Fatalf("the footer %s does not list %d sections (%v)", l, len(sums), err)
			}
			for i := range f.Sections {
				f.Sections[i].SHA256 = sums[i]
			}
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			l = b
		}
		if in {
			h.Write(l)
			h.Write([]byte("\n"))
		}
		if lineKind(t, l) == kindEnd {
			sums = append(sums, hex.EncodeToString(h.Sum(nil)))
			in = false
		}
		out.Write(l)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// editRows calls fn with every row of the table named table, by column
// name, and writes back what fn left in it.
func editRows(t *testing.T, plain []byte, table string, fn func(col map[string]int, row []json.RawMessage)) []byte {
	t.Helper()
	var out bytes.Buffer
	var col map[string]int
	edited := 0
	for _, l := range lines(plain) {
		switch lineKind(t, l) {
		case kindTable:
			var tl tableLine
			if err := json.Unmarshal(l, &tl); err != nil {
				t.Fatal(err)
			}
			col = nil
			if tl.Name == table {
				col = map[string]int{}
				for i, c := range tl.Columns {
					col[c[0]] = i
				}
			}
		case "":
			if col != nil {
				var row []json.RawMessage
				if err := json.Unmarshal(l, &row); err != nil {
					t.Fatal(err)
				}
				fn(col, row)
				b, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				l = b
				edited++
			}
		default:
			col = nil
		}
		out.Write(l)
		out.WriteByte('\n')
	}
	if edited == 0 {
		t.Fatalf("the plaintext holds no row of %s", table)
	}
	return out.Bytes()
}

// editEnds calls fn with every section's end line and writes back what fn
// left in it.
func editEnds(t *testing.T, plain []byte, fn func(e *SectionEnd)) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, l := range lines(plain) {
		if lineKind(t, l) == kindEnd {
			var e SectionEnd
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatal(err)
			}
			fn(&e)
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			l = b
		}
		out.Write(l)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// crafted writes plain, resealed and encrypted with the rig's key file, to
// a new file and returns its path.
func (r *rig) crafted(plain []byte) string {
	r.t.Helper()
	t := r.t.(*testing.T)
	p := filepath.Join(t.TempDir(), "crafted.nopacs")
	if err := os.WriteFile(p, encrypt(t, reseal(t, plain), r.keys), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// plainOf reads and decrypts the backup at path.
func (r *rig) plainOf(path string) []byte {
	r.t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		r.t.Fatal(err)
	}
	return plaintext(r.t.(*testing.T), b, r.keys)
}

// forgeRecord rewrites the correction record of id on the rig's storage
// with what fn makes of it, authenticated under the rig's key file: a
// record this installation could have written at another time than the
// backup's, which verifies and still says another thing than the row.
func (r *rig) forgeRecord(id string, fn func(c *record.Correction)) {
	r.t.Helper()
	r.rewriteRecord(id, func(c record.Correction) []byte {
		fn(&c)
		b, err := record.EncodeCorrection(c, tenantID, r.keys)
		if err != nil {
			r.t.Fatal(err)
		}
		return b
	})
}

// rewriteRecord replaces the correction record of id on the rig's storage
// with the bytes write makes of the record there, whatever they are: what
// anyone who can write to the storage, with or without the key file, could
// put there.
func (r *rig) rewriteRecord(id string, write func(c record.Correction) []byte) {
	r.t.Helper()
	key, err := layout.CorrectionKey(id)
	if err != nil {
		r.t.Fatal(err)
	}
	rc, err := r.storage.Read(context.Background(), key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := record.DecodeCorrection(rc, tenantID, r.keys)
	_ = rc.Close()
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.storage.Put(context.Background(), key, bytes.NewReader(write(c))); err != nil {
		r.t.Fatal(err)
	}
}
