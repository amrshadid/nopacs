package importpath

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// keyed is a map for tests: the key's values joined by "/" to the value.
type keyed map[string]string

func (k keyed) Lookup(key []string) (string, bool) {
	v, ok := k[strings.Join(key, "/")]
	return v, ok
}

func mustLayout(t testing.TB, k Kind, pattern string, depth int) *Layout {
	t.Helper()
	l, err := New(k, pattern, depth)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// xnatExamplePattern is an example of a pattern describing a real tree:
// XNAT's archive, <project>/arc<nnn>/<session>/SCANS/<scan>/DICOM/<file>,
// which keeps no subject folder.
const xnatExamplePattern = `^(?P<project>[^/]+)/arc[0-9]{3}/(?P<session>[^/]+)/SCANS/(?P<scan>[^/]+)/DICOM/[^/]+$`

// xnatExampleUse is how a map fills that tree's missing subject: by
// project and session.
var xnatExampleUse = MapUse{Key: []string{"project", "session"}, Gives: "subject"}

// TestPatternReadsXNATShapedTreeExample guards, on the example of an
// XNAT-shaped tree, that a pattern and a map together give every part: a
// file under a scan's DICOM folder gives project, session and scan, and
// the subject by project and session; a file under a session's other
// folders, another resource, or a session the map does not list is
// skipped with the contract's code.
func TestPatternReadsXNATShapedTreeExample(t *testing.T) {
	l := mustLayout(t, Pattern, xnatExamplePattern, 3)
	if !slices.Equal(l.Parts(), []string{"project", "session", "scan"}) {
		t.Fatalf("parts %v", l.Parts())
	}
	r, err := l.Resolver(keyed{"P1/MR1": "S01", "P2/MR1": "S77"}, xnatExampleUse)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want map[string]string
		skip Skip
	}{
		{"P1/arc001/MR1/SCANS/4/DICOM/1.dcm", map[string]string{"project": "P1", "session": "MR1", "scan": "4", "subject": "S01"}, NoSkip},
		// Two projects sharing a session label get each its own subject.
		{"P2/arc001/MR1/SCANS/1/DICOM/x", map[string]string{"project": "P2", "session": "MR1", "scan": "1", "subject": "S77"}, NoSkip},
		{"P1/arc001/MR1/RESOURCES/nii/a.nii", nil, OutsideLayout},
		{"P1/arc001/MR1/SCANS/4/secondary/1.dcm", nil, OutsideLayout},
		{"P1/arc001/MR1/SCANS/4/DICOM/sub/1.dcm", nil, OutsideLayout},
		{"x/P1/arc001/MR1/SCANS/4/DICOM/1.dcm", nil, OutsideLayout},
		{"P1/arc001/MR1/SCANS/4/DICOM/", nil, OutsideLayout},
		{"P1/arc001/GONE/SCANS/4/DICOM/1.dcm", nil, UnmappedSession},
		// The map is read by its whole key.
		{"P3/arc001/MR1/SCANS/4/DICOM/1.dcm", nil, UnmappedSession},
	} {
		got, skip := r.Resolve(tc.path)
		if skip != tc.skip || !maps.Equal(got, tc.want) {
			t.Errorf("Resolve(%q) = %v, %v; want %v, %v", tc.path, got, skip, tc.want, tc.skip)
		}
	}
	if u, ok := l.Unit("P1/arc001/MR1/SCANS/4/DICOM/1.dcm"); !ok || u != "P1/arc001/MR1" {
		t.Errorf("Unit = %q, %v", u, ok)
	}
	if _, ok := l.Unit("P1/arc001/MR1"); ok {
		t.Error("a path at the unit's depth was given a unit")
	}
}

// TestMapUseRefused guards that a map is refused when its key names a
// part the layout does not give, when it would fill a part the layout
// already gives, and when its use is malformed.
func TestMapUseRefused(t *testing.T) {
	l := mustLayout(t, Pattern, `(?P<project>[^/]+)/(?P<session>[^/]+)/.+`, 1)
	for _, tc := range []struct {
		use      MapUse
		sentinel bool
	}{
		{MapUse{Key: []string{"project", "visit"}, Gives: "subject"}, true},
		{MapUse{Key: []string{"project"}, Gives: "session"}, true},
		{MapUse{Key: []string{"project"}, Gives: "project"}, true},
		{MapUse{Key: nil, Gives: "subject"}, false},
		{MapUse{Key: []string{"project", "project"}, Gives: "subject"}, false},
		{MapUse{Key: []string{"project"}, Gives: "Subject"}, false},
	} {
		_, err := l.Resolver(keyed{}, tc.use)
		var ce *ConfigError
		if tc.sentinel && !errors.Is(err, ErrMapParts) || !tc.sentinel && !errors.As(err, &ce) {
			t.Errorf("%+v: %v", tc.use, err)
		}
	}
	if _, err := mustLayout(t, Plain, "", 0).Resolver(keyed{}, xnatExampleUse); !errors.Is(err, ErrMapParts) {
		t.Errorf("a map with a plain layout: %v", err)
	}
	if err := l.CheckMap(MapUse{Key: []string{"session", "project"}, Gives: "subject"}); err != nil {
		t.Errorf("a fitting use: %v", err)
	}
}

// TestPatternLayout guards a pattern layout: anchored at both ends
// whatever is written, parts as named, unit at the given depth.
func TestPatternLayout(t *testing.T) {
	l := mustLayout(t, Pattern, `(?P<study>[^/]+)/(?P<subject>[^/]+)/(?P<session>[^/]+)/.+`, 2)
	r, err := l.Resolver(nil, MapUse{})
	if err != nil {
		t.Fatal(err)
	}
	got, skip := r.Resolve("trial/s1/v1/a/b.dcm")
	if skip != NoSkip || !maps.Equal(got, map[string]string{"study": "trial", "subject": "s1", "session": "v1"}) {
		t.Fatalf("got %v %v", got, skip)
	}
	if _, skip := r.Resolve("trial/s1"); skip != OutsideLayout {
		t.Errorf("a short path matched")
	}
	// A pattern that would match a part of the path only is held to the
	// whole path.
	r2, _ := mustLayout(t, Pattern, `(?P<x>dcm)`, 1).Resolver(nil, MapUse{})
	if _, skip := r2.Resolve("a/dcm"); skip != OutsideLayout {
		t.Error("the pattern was not anchored")
	}
	if u, ok := l.Unit("trial/s1/v1/a/b.dcm"); !ok || u != "trial/s1" {
		t.Errorf("Unit = %q %v", u, ok)
	}
	// An optional group that did not take part leaves its part absent, and
	// a key part absent is a key the map cannot list.
	l3 := mustLayout(t, Pattern, `(?P<a>x)?/?(?P<b>[^/]+)`, 1)
	r3, _ := l3.Resolver(nil, MapUse{})
	if got, _ := r3.Resolve("y"); !maps.Equal(got, map[string]string{"b": "y"}) {
		t.Errorf("optional group: %v", got)
	}
	r4, err := l3.Resolver(keyed{"": "never"}, MapUse{Key: []string{"a"}, Gives: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, skip := r4.Resolve("y"); skip != UnmappedSession {
		t.Errorf("absent key part: %v", skip)
	}
}

// TestPlainLayout guards that a plain layout reads nothing of a path and
// skips nothing but a path of no shape.
func TestPlainLayout(t *testing.T) {
	l := mustLayout(t, Plain, "", 0)
	r, err := l.Resolver(nil, MapUse{})
	if err != nil {
		t.Fatal(err)
	}
	if got, skip := r.Resolve("any/where/x.dcm"); skip != NoSkip || len(got) != 0 {
		t.Errorf("got %v %v", got, skip)
	}
	for _, p := range []string{"", "/a", "a/", "a//b"} {
		if _, skip := r.Resolve(p); skip != OutsideLayout {
			t.Errorf("Resolve(%q) = %v", p, skip)
		}
	}
	if l.UnitDepth() != 1 || len(r.Given()) != 0 {
		t.Error("plain depth or parts")
	}
}

// TestNewRefuses guards the refusals of a layout as written, each with its
// field, so a receiver cannot hold a layout that cannot read a path.
func TestNewRefuses(t *testing.T) {
	for _, tc := range []struct {
		k       Kind
		pattern string
		depth   int
		field   string
	}{
		{0, "", 0, "kind"},
		{3, "(?P<a>x)", 1, "kind"},
		{Plain, "x", 0, "pattern"},
		{Plain, "", 1, "unit depth"},
		{Pattern, "", 1, "pattern"},
		{Pattern, strings.Repeat("a", MaxPatternLength+1), 1, "pattern"},
		{Pattern, `(?P<a>x)`, 0, "unit depth"},
		{Pattern, `(?P<a>x)`, MaxUnitDepth + 1, "unit depth"},
		{Pattern, `[^/]+`, 1, "pattern"},
		{Pattern, `(?P<A>x)`, 1, "pattern"},
		{Pattern, `(?P<a>x)(?P<a>y)`, 1, "pattern"},
		{Pattern, `(?P<a>x`, 1, "pattern"},
	} {
		_, err := New(tc.k, tc.pattern, tc.depth)
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Field != tc.field {
			t.Errorf("New(%v, %.20q, %d) = %v, want field %q", tc.k, tc.pattern, tc.depth, err, tc.field)
		}
	}
}

// TestNamingRuleAgainstLayout guards the check made when an import
// receiver is set: a naming rule may use the parts its layout and map
// give (here with the project before the subject and session labels, as
// for a tree whose labels are unique only within a project), and is
// refused when it uses a part they never give.
func TestNamingRuleAgainstLayout(t *testing.T) {
	r, err := mustLayout(t, Pattern, xnatExamplePattern, 3).Resolver(keyed{"P1/MR1": "S01"}, xnatExampleUse)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := naming.ParseWithParts(naming.Spec{Subject: "{project}_{subject}", Session: "{project}_{session}", Scan: "{scan}"}, r.Given())
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := r.Resolve("P1/arc001/MR1/SCANS/4/DICOM/1.dcm")
	got, err := rule.LabelsWithParts(nil, parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "P1_S01" || got.Session != "P1_MR1" || got.Scan != "4" {
		t.Errorf("labels %+v", got)
	}
	plain, _ := mustLayout(t, Plain, "", 0).Resolver(nil, MapUse{})
	if _, err := naming.ParseWithParts(naming.Spec{Subject: "{subject}", Session: "s", Scan: "c"}, plain.Given()); err == nil {
		t.Error("a plain layout's rule used a part nobody gives")
	}
}

// FuzzResolve guards that a layout's pattern, as a person may write it,
// never makes Resolve fail or give a part that is not a piece of the path;
// and that a pattern of [^/]+ parts gives parts holding no "/".
func FuzzResolve(f *testing.F) {
	f.Add(`(?P<a>[^/]+)/.+`, "x/y")
	f.Add(xnatExamplePattern, "P/arc001/S/SCANS/1/DICOM/f")
	f.Add(`(?P<a>.*)`, "")
	segs, err := New(Pattern, xnatExamplePattern, 3)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, pattern, p string) {
		x, _ := segs.Resolver(nil, MapUse{})
		if parts, skip := x.Resolve(p); skip == NoSkip {
			for k, v := range parts {
				if v == "" || strings.Contains(v, "/") || !strings.Contains(p, v) {
					t.Fatalf("part %s = %q of %q", k, v, p)
				}
			}
		}
		l, err := New(Pattern, pattern, 1)
		if err != nil {
			return
		}
		r, err := l.Resolver(nil, MapUse{})
		if err != nil {
			t.Fatal(err)
		}
		parts, skip := r.Resolve(p)
		if skip != NoSkip {
			return
		}
		for k, v := range parts {
			if !slices.Contains(l.Parts(), k) || !strings.Contains(p, v) {
				t.Fatalf("part %s = %q is not of %q", k, v, p)
			}
		}
	})
}
