package importpath

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestCheckTakes guards that paths the rule allows are taken: any UTF-8,
// spaces, dots inside a name, and segments and paths at their bounds.
func TestCheckTakes(t *testing.T) {
	for _, p := range []string{
		"a", "a/b/c", "proj 1/arc001/Sess.01", "über/日本", "...x", "a.", ".hidden",
		strings.Repeat("x", MaxSegmentLength),
		strings.Repeat("ab/", (MaxPathLength-1)/3) + "c",
		pathOf(MaxPathLength),
	} {
		if err := Check(p); err != nil {
			t.Errorf("Check(%.40q) = %v", p, err)
		}
	}
	if err := Check(pathOf(MaxPathLength + 1)); err == nil {
		t.Errorf("Check took a path of %d bytes", MaxPathLength+1)
	}
}

// pathOf returns a valid path of n bytes, of segments as long as the rule
// allows.
func pathOf(n int) string {
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		seg := min(MaxSegmentLength, n-b.Len())
		if n-b.Len()-seg == 1 {
			seg-- // never leave room for a "/" alone
		}
		b.WriteString(strings.Repeat("s", seg))
	}
	return b.String()
}

// TestBoundFitsAnIndexRow guards the reason for MaxPathLength: a path at
// the bound with the widest index's other columns (a job id of 64 bytes,
// a unit id, the longest kind) stays below the 2704 bytes a Postgres
// B-tree takes in one index row, with room for the row's header.
func TestBoundFitsAnIndexRow(t *testing.T) {
	const btreeMax, jobID, unitID, kind, header = 2704, 64, 21, len("quarantined"), 64
	if MaxPathLength+jobID+unitID+kind+header > btreeMax {
		t.Fatalf("a path of %d bytes leaves no room in an index row of %d", MaxPathLength, btreeMax)
	}
	if MaxEntryPathLength != MaxPathLength+1+MaxSegmentLength {
		t.Fatal("an entry's path is not a fitting directory's path, \"/\" and a name")
	}
}

// TestCheckEntry guards the rule of an entry a walk meets: up to
// MaxEntryPathLength bytes is taken, reported fitting only up to
// MaxPathLength; past it, or breaking another rule, it is refused.
func TestCheckEntry(t *testing.T) {
	for _, tc := range []struct {
		p    string
		fits bool
	}{
		{"a/b", true}, {pathOf(MaxPathLength), true}, {pathOf(MaxPathLength + 1), false},
		{pathOf(MaxEntryPathLength), false},
	} {
		fits, err := CheckEntry(tc.p)
		if err != nil || fits != tc.fits {
			t.Errorf("CheckEntry(%d bytes) = %v, %v, want %v", len(tc.p), fits, err, tc.fits)
		}
	}
	for _, tc := range []struct {
		p     string
		fault Fault
	}{
		{pathOf(MaxEntryPathLength + 1), EntryTooLong}, {"", Empty}, {pathOf(MaxPathLength) + "/..", DotSegment},
		{"/" + pathOf(MaxPathLength), Absolute},
	} {
		_, err := CheckEntry(tc.p)
		var pe *PathError
		if !errors.As(err, &pe) || pe.Fault != tc.fault {
			t.Errorf("CheckEntry(%d bytes) = %v, want %v", len(tc.p), err, tc.fault)
		}
	}
}

// TestFitting guards where an entry too long is recorded: itself when it
// fits, otherwise its deepest ancestor that fits, cut at a "/".
func TestFitting(t *testing.T) {
	dir := pathOf(MaxPathLength - 3)
	for _, tc := range []struct{ p, want string }{
		{"a/b", "a/b"},
		{pathOf(MaxPathLength), pathOf(MaxPathLength)},
		{dir + "/" + strings.Repeat("n", 200), dir},
		{dir + "/ab", dir + "/ab"},
		{strings.Repeat("x", MaxPathLength+1), ""},
	} {
		got := Fitting(tc.p)
		if got != tc.want {
			t.Errorf("Fitting(%d bytes) = %d bytes, want %d", len(tc.p), len(got), len(tc.want))
		}
		if len(got) > MaxPathLength {
			t.Errorf("Fitting gave %d bytes", len(got))
		}
	}
}

// TestCheckRefuses guards each rule of a path: every fault is found, with
// the segment it is in, so a job cannot name a path that leaves its root,
// means another directory on Windows, or cannot be opened.
func TestCheckRefuses(t *testing.T) {
	for _, tc := range []struct {
		p     string
		seg   int
		fault Fault
	}{
		{"", 0, Empty},
		{strings.Repeat("a/", MaxPathLength/2) + "a", 0, TooLong},
		{"/a", 0, Absolute},
		{"a/", 0, TrailingSlash},
		{"a//b", 2, EmptySegment},
		{"a/" + strings.Repeat("x", MaxSegmentLength+1), 2, SegmentTooLong},
		{".", 1, DotSegment},
		{"a/..", 2, DotSegment},
		{"a/./b", 2, DotSegment},
		{`a\b`, 1, BadByte},
		{"c:/x", 1, BadByte},
		{"a/b\x00c", 2, BadByte},
		{"a/\xff", 2, NotUTF8},
	} {
		err := Check(tc.p)
		var pe *PathError
		if !errors.As(err, &pe) || pe.Segment != tc.seg || pe.Fault != tc.fault {
			t.Errorf("Check(%.40q) = %v, want segment %d %v", tc.p, err, tc.seg, tc.fault)
		}
	}
}

// TestCheckErrorHoldsNoPath guards that a refusal names a segment by
// number only: a path can hold a patient's name, and an error reaches
// logs.
func TestCheckErrorHoldsNoPath(t *testing.T) {
	for _, p := range []string{"Doe^Jane/..", "Doe^Jane/a:b", "Doe^Jane//x", "/Doe^Jane", "Doe^Jane/\xff"} {
		err := Check(p)
		if err == nil || strings.Contains(err.Error(), "Doe") {
			t.Errorf("Check(%q) = %v", p, err)
		}
	}
}

// TestCompareWalkOrder guards the walk order: a directory's whole subtree
// before the next name in that directory, which plain byte order of whole
// paths reverses ("-" and "." are below "/").
func TestCompareWalkOrder(t *testing.T) {
	if Compare("a/b/c", "a/b.d") >= 0 || Compare("a/b.d", "a-c") >= 0 || Compare("a/b/c", "a-c") >= 0 {
		t.Fatal("a/b/c < a/b.d < a-c does not hold")
	}
	// Plain byte order gives the reverse, which is why it is not used.
	if "a-c" >= "a/b.d" || "a/b.d" >= "a/b/c" {
		t.Fatal("the premise about byte order is wrong")
	}
	got := []string{"a-c", "a/b.d", "a/b/c", "a", "a/b", "b", "a/b/c/d", "A"}
	slices.SortFunc(got, Compare)
	want := []string{"A", "a", "a/b", "a/b/c", "a/b/c/d", "a/b.d", "a-c", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("sorted %q, want %q", got, want)
	}
	for _, p := range want {
		if Compare(p, p) != 0 {
			t.Errorf("Compare(%q, itself) != 0", p)
		}
	}
}

// TestOverlap guards when two jobs read the same files: one path, or one
// under the other by whole segments; the empty path is the whole root.
func TestOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"a", "a", true}, {"a", "a/b", true}, {"a/b", "a", true}, {"", "x/y", true},
		{"a", "ab", false}, {"a/b", "a/c", false}, {"ab/c", "a", false},
	} {
		if got := Overlap(tc.a, tc.b); got != tc.want {
			t.Errorf("Overlap(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// segmentCompare is the walk order written the slow, obvious way: segment
// lists compared element by element, a prefix first.
func segmentCompare(a, b string) int {
	return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
}

// FuzzCompare guards that Compare is the segment-by-segment order on every
// pair of valid paths, and antisymmetric.
func FuzzCompare(f *testing.F) {
	for _, s := range [][2]string{{"a/b/c", "a/b.d"}, {"a/b.d", "a-c"}, {"a", "a/b"}, {"x", "x"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if Check(a) != nil || Check(b) != nil {
			return
		}
		got := Compare(a, b)
		if want := segmentCompare(a, b); got != want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", a, b, got, want)
		}
		if Compare(b, a) != -got {
			t.Fatalf("Compare is not antisymmetric on %q, %q", a, b)
		}
	})
}

// FuzzCheck guards that a path Check takes holds no segment that could
// leave the root or name another directory, and that a refusal never holds
// the path's text.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{"a/b", "a/../b", "/x", "a\\b", "c:", "a//b", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		err := Check(p)
		if err != nil {
			if len(p) >= 4 && strings.Contains(err.Error(), p) {
				t.Fatalf("the error %q holds the path", err)
			}
			return
		}
		if len(p) > MaxPathLength {
			t.Fatal("took a path too long")
		}
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "" || seg == "." || seg == ".." || len(seg) > MaxSegmentLength || strings.ContainsAny(seg, "\\:\x00") {
				t.Fatalf("took %q with segment %q", p, seg)
			}
		}
	})
}
