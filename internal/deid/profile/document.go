package profile

import (
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// MaxDocumentBytes is the bound of a document's file form: the API's
// message limit, so a file the command line reads always fits a request.
const MaxDocumentBytes = 4 << 20

// ParseDocument reads a document in its file form, the protobuf JSON of
// ProfileDocument. A field the message does not have is refused, so a
// misspelled field is an error and never silently ignored. It checks the
// form only; Expand checks the rules.
func ParseDocument(b []byte) (*v1.ProfileDocument, error) {
	if len(b) > MaxDocumentBytes {
		return nil, fmt.Errorf("profile: a document is at most %d bytes", MaxDocumentBytes)
	}
	doc := &v1.ProfileDocument{}
	if err := (protojson.UnmarshalOptions{}).Unmarshal(b, doc); err != nil {
		return nil, fmt.Errorf("profile: not a profile document: %w", err)
	}
	return doc, nil
}

// Canonical returns a message's protobuf JSON in one fixed form: the field
// names of the JSON mapping, in the order of the message's fields, no
// field at its default, and no space. The protobuf library adds varying
// spaces to its JSON on purpose, so that no one relies on its bytes; this
// form removes them, so the same message gives the same bytes in every
// run, which a checksum needs.
func Canonical(m proto.Message) ([]byte, error) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	return out.Bytes(), nil
}

// FormatDocument returns a document's file form for a person: the
// canonical JSON indented by two spaces, with a final newline.
func FormatDocument(doc *v1.ProfileDocument) ([]byte, error) {
	c, err := Canonical(doc)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, c, "", "  "); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// cloneDocument returns a deep copy of a document, nil for nil.
func cloneDocument(doc *v1.ProfileDocument) *v1.ProfileDocument {
	if doc == nil {
		return nil
	}
	c, _ := proto.Clone(doc).(*v1.ProfileDocument)
	return c
}
