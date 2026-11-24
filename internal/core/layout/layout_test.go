package layout

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
)

var scan = label.Path{Subject: "SUB-001", Session: "20240131-1.2.3", Scan: "3-1.2.4"}

// TestBuild guards the three key forms and the prefix, exactly as the
// layout states them.
func TestBuild(t *testing.T) {
	for _, c := range []struct {
		got  func() (string, error)
		want string
	}{
		{func() (string, error) { return InstanceKey(scan, "1.2.5.6") }, "data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.dcm"},
		{func() (string, error) { return ManifestKey(scan) }, "meta/SUB-001/20240131-1.2.3/3-1.2.4/manifest.json"},
		{func() (string, error) { return MetadataKey(scan) }, "meta/SUB-001/20240131-1.2.3/3-1.2.4/metadata.json"},
		{func() (string, error) { return InstancePrefix(scan) }, "data/SUB-001/20240131-1.2.3/3-1.2.4/"},
	} {
		got, err := c.got()
		if err != nil || got != c.want {
			t.Errorf("got %q %v, want %q", got, err, c.want)
		}
	}
}

// TestBuildRefuses guards that no key is built from a part that is not
// valid: each label, the UID, the kind, and a UID on a meta key.
func TestBuildRefuses(t *testing.T) {
	bad := []Key{
		{Kind: Instance, Path: label.Path{Subject: "..", Session: "a", Scan: "b"}, SOPInstanceUID: "1.2"},
		{Kind: Instance, Path: label.Path{Subject: "a", Session: "b/c", Scan: "d"}, SOPInstanceUID: "1.2"},
		{Kind: Instance, Path: label.Path{Subject: "a", Session: "b", Scan: ""}, SOPInstanceUID: "1.2"},
		{Kind: Instance, Path: label.Path{Subject: "a", Session: "b", Scan: "NUL"}, SOPInstanceUID: "1.2"},
		{Kind: Instance, Path: scan, SOPInstanceUID: ""},
		{Kind: Instance, Path: scan, SOPInstanceUID: "1.02"},
		{Kind: Instance, Path: scan, SOPInstanceUID: "../../x"},
		{Kind: Manifest, Path: scan, SOPInstanceUID: "1.2"},
		{Kind: 0, Path: scan},
		{Kind: 9, Path: scan},
	}
	for _, k := range bad {
		if s, err := k.String(); err == nil || !errors.Is(err, ErrKey) {
			t.Errorf("%+v: built %q, %v", k, s, err)
		}
	}
	if _, err := InstancePrefix(label.Path{}); err == nil {
		t.Error("prefix of empty path built")
	}
}

// TestParse guards the parser on keys it must take and on near misses it
// must refuse because they do not build back to themselves.
func TestParse(t *testing.T) {
	k, err := Parse("data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.dcm")
	if err != nil || k != (Key{Kind: Instance, Path: scan, SOPInstanceUID: "1.2.5.6"}) {
		t.Fatalf("%+v %v", k, err)
	}
	k, err = Parse("meta/SUB-001/20240131-1.2.3/3-1.2.4/manifest.json")
	if err != nil || k != (Key{Kind: Manifest, Path: scan}) {
		t.Fatalf("%+v %v", k, err)
	}
	k, err = Parse("meta/SUB-001/20240131-1.2.3/3-1.2.4/metadata.json")
	if err != nil || k != (Key{Kind: Metadata, Path: scan}) {
		t.Fatalf("%+v %v", k, err)
	}
	for _, s := range []string{
		"",
		"data",
		"data/",
		"/data/a/b/c/1.2.dcm",
		"data/a/b/c/1.2.dcm/",
		"data//a/b/c/1.2.dcm",
		"data/a/b//1.2.dcm",
		"data/a/b/c/d/1.2.dcm",
		"data/a/b/1.2.dcm",
		"data/a/b/c/1.2.DCM",
		"data/a/b/c/1.2",
		"data/a/b/c/.dcm",
		"data/a/b/c/1.02.dcm",
		"data/a/b/c/1.2.dcm.dcm",
		"data/a/b/c/manifest.json",
		"meta/a/b/c/1.2.dcm",
		"meta/a/b/c/Manifest.json",
		"meta/a/b/c/manifest.json ",
		"Data/a/b/c/1.2.dcm",
		"data/../b/c/1.2.dcm",
		"data/./b/c/1.2.dcm",
		"data/a/b/c\\d/1.2.dcm",
		"data/con/b/c/1.2.dcm",
		"data/a/b./c/1.2.dcm",
		"data/a/b/c/1.2.dcm\x00",
		"data/" + strings.Repeat("a", 129) + "/b/c/1.2.dcm",
		strings.Repeat("a/", 3000),
	} {
		if k, err := Parse(s); err == nil || !errors.Is(err, ErrKey) {
			t.Errorf("%.60q: parsed as %+v, %v", s, k, err)
		}
	}
}

// TestRoundTrip guards the property the layout rests on: every key the
// builder makes parses back to the same parts, over many drawn parts,
// including labels and UIDs of the greatest length.
func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: a fixed seed, so a failure is reproduced; nothing secret is drawn.
	const alpha = "ABCXYZabcxyz0189._-"
	lab := func() string {
		for {
			n := 1 + r.IntN(label.MaxLength)
			if r.IntN(4) == 0 {
				n = label.MaxLength
			}
			b := make([]byte, n)
			for i := range b {
				b[i] = alpha[r.IntN(len(alpha))]
			}
			if label.Valid(string(b)) {
				return string(b)
			}
		}
	}
	id := func() string {
		var b strings.Builder
		b.WriteString("1")
		for r.IntN(12) != 0 {
			c := []string{"0", "2", "840", "10008", "99999999"}[r.IntN(5)]
			if b.Len()+1+len(c) > 64 {
				break
			}
			b.WriteString(".")
			b.WriteString(c)
		}
		return b.String()
	}
	longest := 0
	for range 20000 {
		k := Key{Kind: Kind(1 + r.IntN(3)), Path: label.Path{Subject: lab(), Session: lab(), Scan: lab()}}
		if k.Kind == Instance {
			k.SOPInstanceUID = id()
		}
		s, err := k.String()
		if err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
		longest = max(longest, len(s))
		back, err := Parse(s)
		if err != nil || back != k {
			t.Fatalf("%q: parsed as %+v, %v; built from %+v", s, back, err, k)
		}
	}
	if longest > MaxKeyLength {
		t.Fatalf("built a key of %d bytes, past MaxKeyLength %d", longest, MaxKeyLength)
	}
}

// FuzzParse guards the parser: it never panics, and whatever it accepts
// builds back to the same string, so it accepts only what the builder makes.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"data/a/b/c/1.2.dcm", "meta/a/b/c/manifest.json", "meta/a/b/c/metadata.json",
		"data//b/c/1.2.dcm", "data/a/b/c/1.02.dcm", "data/../b/c/1.dcm",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		k, err := Parse(s)
		if err != nil {
			return
		}
		back, err := k.String()
		if err != nil || back != s {
			t.Fatalf("%q parsed as %+v, which builds %q, %v", s, k, back, err)
		}
	})
}

// TestRevisionKey guards the revision key form: revision 1 keeps the plain
// key, later revisions add ".r<n>" before the extension, and Parse reads
// each back to its revision.
func TestRevisionKey(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
		rev  int
	}{
		{1, "data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.dcm", 0},
		{2, "data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.r2.dcm", 2},
		{37, "data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.r37.dcm", 37},
		{MaxRevision, "data/SUB-001/20240131-1.2.3/3-1.2.4/1.2.5.6.r1000000.dcm", MaxRevision},
	} {
		got, err := RevisionKey(scan, "1.2.5.6", c.n)
		if err != nil || got != c.want {
			t.Fatalf("revision %d: got %q %v, want %q", c.n, got, err, c.want)
		}
		k, err := Parse(got)
		if err != nil || k != (Key{Kind: Instance, Path: scan, SOPInstanceUID: "1.2.5.6", Revision: c.rev}) || k.Rev() != max(c.n, 1) {
			t.Fatalf("%q: parsed as %+v, %v", got, k, err)
		}
	}
	for _, n := range []int{0, -1, MaxRevision + 1} {
		if s, err := RevisionKey(scan, "1.2.5.6", n); err == nil || !errors.Is(err, ErrKey) {
			t.Errorf("revision %d: built %q, %v", n, s, err)
		}
	}
	if s, err := (Key{Kind: Manifest, Path: scan, Revision: 2}).String(); err == nil {
		t.Errorf("a manifest key with a revision was built: %q", s)
	}
}

// TestParseRefusesBadRevisions guards that only the form String writes is
// taken: no ".r1", no leading zero, no sign, no text, no revision past the
// bound, and a UID that is not valid before the mark.
func TestParseRefusesBadRevisions(t *testing.T) {
	const dir = "data/SUB-001/20240131-1.2.3/3-1.2.4/"
	for _, name := range []string{
		"1.2.5.6.r1.dcm", "1.2.5.6.r0.dcm", "1.2.5.6.r02.dcm", "1.2.5.6.r+2.dcm", "1.2.5.6.r-2.dcm",
		"1.2.5.6.r.dcm", "1.2.5.6.rx.dcm", "1.2.5.6.r2x.dcm", "1.2.5.6.r1000001.dcm", "1.2.5.6.r99999999.dcm",
		"1.2.5.6.r2.r3.dcm", ".r2.dcm", "1.2.5.6r2.dcm", "1.2.5.6." + strings.ToUpper("r2") + ".dcm", "1.2.5.6.r2.dcm.dcm",
	} {
		if k, err := Parse(dir + name); err == nil || !errors.Is(err, ErrKey) {
			t.Errorf("%q parsed as %+v, %v", name, k, err)
		}
	}
	if k, err := Parse("meta/SUB-001/20240131-1.2.3/3-1.2.4/manifest.r2.json"); err == nil {
		t.Errorf("a manifest revision parsed as %+v", k)
	}
}

// TestRevisionKeyLongest guards MaxKeyLength: the longest revision key of
// the longest parts still parses.
func TestRevisionKeyLongest(t *testing.T) {
	long := label.Path{Subject: strings.Repeat("a", label.MaxLength), Session: strings.Repeat("b", label.MaxLength), Scan: strings.Repeat("c", label.MaxLength)}
	u := "1" + strings.Repeat(".2", 31)
	got, err := RevisionKey(long, u, MaxRevision)
	if err != nil || len(got) > MaxKeyLength {
		t.Fatalf("%d bytes past %d, %v", len(got), MaxKeyLength, err)
	}
	if _, err := Parse(got); err != nil {
		t.Fatal(err)
	}
}

// TestCorrectionKey guards the correction record's key and operation id, a
// lowercase ULID: built and parsed back, and refused for any other length,
// a letter Crockford's base 32 leaves out, a capital, a first character
// past "7", and anything a key segment must not hold.
func TestCorrectionKey(t *testing.T) {
	const op = "01jd5x7k2m3n4p5q6r7s8t9v0w"
	got, err := CorrectionKey(op)
	if err != nil || got != "meta/.corrections/"+op+".json" {
		t.Fatalf("%q %v", got, err)
	}
	if id, err := ParseCorrectionKey(got); err != nil || id != op {
		t.Fatalf("%q %v", id, err)
	}
	for _, id := range []string{"", op[:25], op + "0", strings.ToUpper(op), "01jd5x7k2m3n4p5q6r7s8t9v0i", "01jd5x7k2m3n4p5q6r7s8t9v0l",
		"01jd5x7k2m3n4p5q6r7s8t9v0o", "01jd5x7k2m3n4p5q6r7s8t9v0u", "81jd5x7k2m3n4p5q6r7s8t9v0w", "01jd5x7k2m3n4p5q6r7s8t9v-w",
		"01jd5x7k2m3n4p5q6r7s8t9v/w", "01jd5x7k2m3n4p5q6r7s8t9v.w", "01jd5x7k2m3n4p5q6r7s8t9v\x00w", "op-7f3a-0001"} {
		if s, err := CorrectionKey(id); err == nil || !errors.Is(err, ErrKey) {
			t.Errorf("%.30q: built %q, %v", id, s, err)
		}
	}
	for _, id := range []string{"00000000000000000000000000", "7zzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if _, err := CorrectionKey(id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	for _, s := range []string{"", "meta/.corrections/.json", "meta/.corrections/" + op + ".JSON", "meta/.corrections/a/" + op + ".json",
		"meta/.corrections/" + op + ".json.json", "meta/corrections/" + op + ".json", "meta/.owner.json", "data/.corrections/" + op + ".json",
		"meta/.corrections/" + strings.ToUpper(op) + ".json"} {
		if id, err := ParseCorrectionKey(s); err == nil {
			t.Errorf("%q parsed as %q", s, id)
		}
	}
	// Neither record's key is a scan's key.
	for _, s := range []string{OwnerKey, got} {
		if k, err := Parse(s); err == nil {
			t.Errorf("%q parsed as a scan file: %+v", s, k)
		}
	}
}
