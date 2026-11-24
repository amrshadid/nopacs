package profile_test

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestVersionIsImmutable guards that a version cannot be changed: what its
// accessors answer are copies, a next version leaves it as it was, and its
// texts and checksum stay what they were when it was made.
func TestVersionIsImmutable(t *testing.T) {
	cat := testCatalog(t)
	v, err := profile.New("research", 1, richDocument(), cat)
	if err != nil {
		t.Fatal(err)
	}
	sum, source, expanded := v.Checksum(), v.Source(), v.ExpandedText()

	// Changing what the accessors answer changes nothing.
	v.Source()[0] = 'x'
	v.ExpandedText()[0] = 'x'
	v.Document().Rules[0].Action = remove()
	v.Expanded().Rules[0].Origin = "changed"

	// A next version is a new value.
	other := richDocument()
	other.Rules = other.Rules[1:]
	next, err := v.Next(other, cat)
	if err != nil {
		t.Fatal(err)
	}
	if next.Number() != 2 || next.Name() != "research" || next.Ref() != "research@2" {
		t.Errorf("next is %s", next.Ref())
	}
	if next.Checksum() == sum {
		t.Error("a different document has the same checksum")
	}

	if v.Number() != 1 || v.Checksum() != sum || !bytes.Equal(v.Source(), source) || !bytes.Equal(v.ExpandedText(), expanded) {
		t.Error("the version changed")
	}
	if !proto.Equal(v.Document(), richDocument()) {
		t.Error("the version's document is not the one given")
	}
	if !v.SameDocument(richDocument()) || v.SameDocument(other) {
		t.Error("SameDocument does not tell the document from another")
	}
}

// TestVersionChangesNothingGiven guards that a document given to New may
// be changed after, by its caller, without changing the version.
func TestVersionChangesNothingGiven(t *testing.T) {
	doc := richDocument()
	v, err := profile.New("research", 1, doc, testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	sum := v.Checksum()
	doc.Rules[0].Action = remove()
	if !proto.Equal(v.Document(), richDocument()) || v.Checksum() != sum {
		t.Error("changing the given document changed the version")
	}
}

// TestLoadChecksTheChecksum guards that a stored version reads back equal,
// and that one whose expanded text was changed after it was made is
// refused, as is one with a malformed name or number.
func TestLoadChecksTheChecksum(t *testing.T) {
	v, err := profile.New("research", 3, richDocument(), testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	back, err := profile.Load("research", 3, v.Source(), v.ExpandedText(), v.Checksum())
	if err != nil {
		t.Fatal(err)
	}
	if back.Ref() != "research@3" || !proto.Equal(back.Expanded(), v.Expanded()) || !proto.Equal(back.Document(), v.Document()) {
		t.Error("the loaded version differs")
	}

	changed := bytes.Replace(v.ExpandedText(), []byte(`"hash"`), []byte(`"keep"`), 1)
	if bytes.Equal(changed, v.ExpandedText()) {
		t.Fatal("the test changed nothing")
	}
	if _, err := profile.Load("research", 3, v.Source(), changed, v.Checksum()); !errors.Is(err, profile.ErrChecksum) {
		t.Errorf("a changed expanded text: %v", err)
	}
	for _, c := range []struct {
		name   string
		number int32
	}{{"Research", 3}, {"research", 0}, {"research", profile.MaxVersions + 1}} {
		if _, err := profile.Load(c.name, c.number, v.Source(), v.ExpandedText(), v.Checksum()); err == nil {
			t.Errorf("%s@%d loaded", c.name, c.number)
		}
	}
}

// TestNewRefuses guards what New refuses: a malformed name, a number out
// of bounds, a document with problems (as Problems), and a version past
// the most a profile may have.
func TestNewRefuses(t *testing.T) {
	cat := testCatalog(t)
	if _, err := profile.New("Research", 1, &v1.ProfileDocument{}, cat); err == nil {
		t.Error("a malformed name")
	}
	if _, err := profile.New("research", 0, &v1.ProfileDocument{}, cat); err == nil {
		t.Error("version 0")
	}
	_, err := profile.New("research", 1, &v1.ProfileDocument{Bases: []string{baseGone}}, cat)
	var problems profile.Problems
	if !errors.As(err, &problems) || problems[0].Code != profile.CodeBaseUnavailable {
		t.Errorf("a document with problems: %v", err)
	}
	last, err := profile.New("research", profile.MaxVersions, &v1.ProfileDocument{}, cat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := last.Next(&v1.ProfileDocument{}, cat); err == nil {
		t.Error("a version past the most")
	}
}

// TestParseRef guards the form of a reference to a version, "name@number"
// or "name" for the latest.
func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		in   string
		want profile.Ref
		ok   bool
	}{
		{"research", profile.Ref{Name: "research"}, true},
		{"research@1", profile.Ref{Name: "research", Version: 1}, true},
		{"research@10000", profile.Ref{Name: "research", Version: 10000}, true},
		{"research@0", profile.Ref{}, false},
		{"research@01", profile.Ref{}, false},
		{"research@+1", profile.Ref{}, false},
		{"research@", profile.Ref{}, false},
		{"research@10001", profile.Ref{}, false},
		{"Research@1", profile.Ref{}, false},
		{"@1", profile.Ref{}, false},
		{"con", profile.Ref{}, false},
	} {
		got, err := profile.ParseRef(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %+v, %v", c.in, got, err)
		}
		if c.ok && got.String() != c.in {
			t.Errorf("%q written back as %q", c.in, got.String())
		}
	}
}
