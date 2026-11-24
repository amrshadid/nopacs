package manifest

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
)

var scan = label.Path{Subject: "SUB-001", Session: "20240131-1.2.3", Scan: "3-1.2.4"}

func file(t testing.TB, id string, size int64) File {
	t.Helper()
	k, err := layout.InstanceKey(scan, id)
	if err != nil {
		t.Fatal(err)
	}
	return File{Key: k, Offset: 0, Length: size, Size: size, SHA256: strings.Repeat("ab", 32), SOPInstanceUID: id}
}

const golden = `{"version":1,"files":[
{"key":"data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.10.dcm","offset":0,"length":20,"size":20,"sha256":"abababababababababababababababababababababababababababababababab","sop_instance_uid":"1.2.5.10"},
{"key":"data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.9.dcm","offset":0,"length":10,"size":10,"sha256":"abababababababababababababababababababababababababababababababab","sop_instance_uid":"1.2.5.9"}
]}
`

// TestEncodeGolden guards the encoded form byte for byte, and that the
// order the files are given in does not change it.
func TestEncodeGolden(t *testing.T) {
	a, b := file(t, "1.2.5.9", 10), file(t, "1.2.5.10", 20)
	for _, files := range [][]File{{a, b}, {b, a}} {
		got, err := Encode(Manifest{Files: files})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != golden {
			t.Fatalf("got\n%s\nwant\n%s", got, golden)
		}
	}
	m, err := Decode(strings.NewReader(golden))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files[0] != b || m.Files[1] != a {
		t.Fatalf("%+v", m.Files)
	}
}

// TestEncodeRefuses guards each rule of a file and of a manifest on the
// writing side, so no invalid manifest is ever put on a storage.
func TestEncodeRefuses(t *testing.T) {
	ok := file(t, "1.2.5.9", 10)
	other := ok
	other.Key, _ = layout.InstanceKey(label.Path{Subject: "x", Session: "y", Scan: "z"}, "1.2.5.8")
	other.SOPInstanceUID = "1.2.5.8"
	mod := func(f func(*File)) File { g := ok; f(&g); return g }
	for name, files := range map[string][]File{
		"no files":       {},
		"meta key":       {mod(func(f *File) { f.Key, _ = layout.ManifestKey(scan) })},
		"bad key":        {mod(func(f *File) { f.Key = "data/a//b/1.2.dcm" })},
		"uid mismatch":   {mod(func(f *File) { f.SOPInstanceUID = "1.2.5.7" })},
		"bad uid":        {mod(func(f *File) { f.SOPInstanceUID = "1.02" })},
		"offset":         {mod(func(f *File) { f.Offset = 1 })},
		"negative":       {mod(func(f *File) { f.Length, f.Size = -1, -1 })},
		"zero length":    {mod(func(f *File) { f.Length, f.Size = 0, 0 })},
		"size":           {mod(func(f *File) { f.Size = 11 })},
		"sha upper":      {mod(func(f *File) { f.SHA256 = strings.Repeat("AB", 32) })},
		"sha short":      {mod(func(f *File) { f.SHA256 = "ab" })},
		"duplicate":      {ok, ok},
		"two scans":      {ok, other},
		"dup uid differ": {ok, mod(func(f *File) { f.Length, f.Size = 11, 11 })},
	} {
		if b, err := Encode(Manifest{Files: files}); err == nil {
			t.Errorf("%s: encoded\n%s", name, b)
		}
	}
	files := make([]File, MaxFiles+1)
	if _, err := Encode(Manifest{Files: files}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too many files: %v", err)
	}
}

// TestDecodeRefuses guards the reading side: unknown versions, unknown or
// repeated fields, duplicates, any form other than the encoded one, and the
// bound on what it reads.
func TestDecodeRefuses(t *testing.T) {
	line := strings.Split(golden, "\n")[1]
	for name, c := range map[string]struct {
		in   string
		want error
	}{
		"version 2":        {strings.Replace(golden, `"version":1`, `"version":2`, 1), ErrVersion},
		"version 1.0":      {strings.Replace(golden, `"version":1`, `"version":1.0`, 1), ErrVersion},
		"version string":   {strings.Replace(golden, `"version":1`, `"version":"1"`, 1), ErrVersion},
		"no version":       {strings.Replace(golden, `"version":1,`, ``, 1), ErrVersion},
		"not json":         {"nope", ErrInvalid},
		"empty":            {"", ErrInvalid},
		"unknown field":    {strings.Replace(golden, `"offset":0,"length":20`, `"offset":0,"extra":1,"length":20`, 1), ErrInvalid},
		"unknown top":      {strings.Replace(golden, `"version":1,`, `"version":1,"x":1,`, 1), ErrInvalid},
		"repeated field":   {strings.Replace(golden, `"version":1,`, `"version":1,"version":1,`, 1), ErrNotCanonical},
		"repeated in file": {strings.Replace(golden, `"length":20,`, `"length":20,"length":20,`, 1), ErrNotCanonical},
		"duplicate file":   {strings.Replace(golden, line, line+"\n"+line, 1), ErrInvalid},
		"unsorted": {"{\"version\":1,\"files\":[\n" + strings.Split(golden, "\n")[2] + ",\n" +
			strings.TrimSuffix(line, ",") + "\n]}\n", ErrNotCanonical},
		"spaces":      {strings.Replace(golden, `"offset":0`, `"offset": 0`, 1), ErrNotCanonical},
		"no newline":  {strings.TrimSuffix(golden, "\n"), ErrNotCanonical},
		"trailing":    {golden + "{}", ErrInvalid},
		"field order": {strings.Replace(golden, `"offset":0,"length":20`, `"length":20,"offset":0`, 1), ErrNotCanonical},
		"upper key":   {strings.Replace(golden, `{"key"`, `{"KEY"`, 1), ErrNotCanonical},
		"escaped":     {strings.Replace(golden, `"sop_instance_uid":"1.2.5.9"`, `"sop_instance_uid":"1.2.5\u002e9"`, 1), ErrNotCanonical},
		"no files":    {"{\"version\":1,\"files\":[\n]}\n", ErrInvalid},
		"null files":  {"{\"version\":1,\"files\":null}\n", ErrInvalid},
		"huge number": {strings.Replace(golden, `"length":20,"size":20`, `"length":99999999999999999999,"size":20`, 1), ErrInvalid},
		"too large":   {golden + strings.Repeat(" ", MaxBytes), ErrTooLarge},
	} {
		_, err := Decode(strings.NewReader(c.in))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

// TestDecodeReadsBounded guards that Decode stops reading at MaxBytes+1,
// whatever the reader has.
func TestDecodeReadsBounded(t *testing.T) {
	r := &countingReader{}
	if _, err := Decode(r); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if r.n > MaxBytes+1 {
		t.Fatalf("read %d bytes", r.n)
	}
}

// countingReader is an endless reader of spaces that counts what it gives.
type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	c.n += len(p)
	return len(p), nil
}

// TestMerge guards merging a late arrival: new files are added and listed,
// a file already listed the same is not added twice, a different file under
// a listed UID is a conflict, a file of another scan is refused, and the
// result encodes the same whatever order the files came in.
func TestMerge(t *testing.T) {
	a, b, c := file(t, "1.2.5.1", 10), file(t, "1.2.5.2", 20), file(t, "1.2.5.3", 30)
	m, added, err := Merge(Manifest{}, []File{b, a})
	if err != nil || len(m.Files) != 2 || len(added) != 2 {
		t.Fatalf("%+v %+v %v", m, added, err)
	}
	m2, added, err := Merge(m, []File{c, a})
	if err != nil || len(m2.Files) != 3 || len(added) != 1 || added[0] != c {
		t.Fatalf("%+v %+v %v", m2, added, err)
	}
	if len(m.Files) != 2 {
		t.Fatal("Merge changed its input")
	}
	again, added, err := Merge(m2, []File{a, b, c})
	if err != nil || len(added) != 0 || len(again.Files) != 3 {
		t.Fatalf("rerun: %+v %v", added, err)
	}
	direct, _, _ := Merge(Manifest{}, []File{c, b, a})
	e1, _ := Encode(m2)
	e2, _ := Encode(direct)
	if !bytes.Equal(e1, e2) {
		t.Fatal("merge order changed the encoding")
	}
	changed := a
	changed.SHA256 = strings.Repeat("cd", 32)
	if _, _, err := Merge(m2, []File{changed}); !errors.Is(err, ErrConflict) {
		t.Errorf("conflict: %v", err)
	}
	if _, _, err := Merge(m2, []File{changed, changed}); !errors.Is(err, ErrConflict) {
		t.Errorf("conflict twice: %v", err)
	}
	if _, _, err := Merge(Manifest{}, []File{a, func() File { x := a; x.Size = 1; return x }()}); !errors.Is(err, ErrConflict) {
		t.Errorf("conflict within late: %v", err)
	}
	other := a
	other.Key, _ = layout.InstanceKey(label.Path{Subject: "x", Session: "y", Scan: "z"}, "1.2.5.9")
	other.SOPInstanceUID = "1.2.5.9"
	if _, _, err := Merge(m2, []File{other}); !errors.Is(err, ErrInvalid) {
		t.Errorf("other scan: %v", err)
	}
}

// FuzzDecode guards the decoder: it never panics, and whatever it accepts
// encodes back to exactly the bytes it read, so it accepts one form only.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(golden))
	f.Add([]byte(strings.Replace(golden, `"version":1,`, `"version":1,"version":1,`, 1)))
	f.Add([]byte("{\"version\":1,\"files\":[\n]}\n"))
	f.Add([]byte(`{"version":2}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		out, err := Encode(m)
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("accepted %q, which encodes to %q, %v", data, out, err)
		}
	})
}
