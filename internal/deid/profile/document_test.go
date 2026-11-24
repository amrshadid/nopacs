package profile_test

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestDocumentRoundTrip guards the file form: a document written as JSON
// and read back is equal to the proto message, field by field, whether the
// JSON is the canonical text, the text for people, or what the protobuf
// library writes; and its canonical text is the same each way.
func TestDocumentRoundTrip(t *testing.T) {
	doc := richDocument()
	canonical, err := profile.Canonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := profile.FormatDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	library, err := protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"canonical": canonical, "formatted": formatted, "proto names": library} {
		back, err := profile.ParseDocument(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !proto.Equal(back, doc) {
			t.Errorf("%s: the document read back differs", name)
		}
		again, err := profile.Canonical(back)
		if err != nil || !bytes.Equal(again, canonical) {
			t.Errorf("%s: canonical text %s, want %s", name, again, canonical)
		}
	}
	if !bytes.HasSuffix(formatted, []byte("}\n")) || !bytes.Contains(formatted, []byte("\n  \"bases\": [")) {
		t.Errorf("the text for people is not indented by two spaces:\n%s", formatted)
	}
}

// TestExpandedRoundTrip guards that an expanded form read back from its
// canonical text is equal to the one expanded, so a stored version does
// what it did when it was made.
func TestExpandedRoundTrip(t *testing.T) {
	e := mustExpand(t, richDocument(), testCatalog(t))
	text, _, err := profile.Checksum(e)
	if err != nil {
		t.Fatal(err)
	}
	back := &v1.ExpandedProfile{}
	if err := protojson.Unmarshal(text, back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back, e) {
		t.Error("the expanded form read back differs")
	}
}

// TestDocumentFileForm guards what the file form refuses: a field the
// message does not have (a misspelling is an error, never ignored),
// something that is not JSON, and a file past the bound. It also shows the
// form a person writes, field names in lower camel case and enumerations
// by name.
func TestDocumentFileForm(t *testing.T) {
	written := `{
	  "bases": ["test/base-a:1"],
	  "unnamed": "UNNAMED_ELEMENTS_REMOVE",
	  "dateShiftDays": 30,
	  "rules": [
	    {"name": "names", "selector": {"vr": "PN"}, "action": {"hash": {}}},
	    {"selector": {"keyword": "StudyDate"}, "when": [{"subject": "CONDITION_SUBJECT_MODALITY", "equals": "CT"}], "action": {"shift": {}}},
	    {"selector": {"privateElement": {"creator": "ACME", "group": "0019", "element": "xx"}}, "action": {"keep": {}}},
	    {"selector": {"path": "**/ContentSequence[*]/TextValue"}, "action": {"clean": {}}}
	  ]
	}`
	doc, err := profile.ParseDocument([]byte(written))
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := profile.Expand(doc, testCatalog(t)); len(problems) > 0 {
		t.Errorf("the written document: %v", problems)
	}
	for name, b := range map[string]string{
		"misspelled field": `{"rules": [{"selecter": {"vr": "PN"}, "action": {"hash": {}}}]}`,
		"unknown action":   `{"rules": [{"selector": {"vr": "PN"}, "action": {"forward": {}}}]}`,
		"two kinds":        `{"rules": [{"selector": {"vr": "PN", "tag": "00100010"}, "action": {"hash": {}}}]}`,
		"not json":         `rules: []`,
		"unknown enum":     `{"unnamed": "UNNAMED_ELEMENTS_SOMETIMES"}`,
	} {
		if _, err := profile.ParseDocument([]byte(b)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	big := `{"description": "` + strings.Repeat("x", profile.MaxDocumentBytes) + `"}`
	if _, err := profile.ParseDocument([]byte(big)); err == nil {
		t.Error("a file past the bound was read")
	}
}
