package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// SchemaVersion is the version of the JSON documents. It changes only when a
// field is removed or changes meaning, which the rules for the documents
// forbid within a major version of the command line.
const SchemaVersion = 1

// marshal options shared by every document: empty values are written, so the
// shape of a document does not depend on what is set; names are the lower
// camel case ones of the contract's JSON form.
var protoOptions = protojson.MarshalOptions{EmitUnpopulated: true}

// compactProto returns the protobuf JSON of m as compact JSON. protojson
// deliberately varies its white space between builds, so the output is
// compacted before it is used, which makes it identical every time.
func compactProto(m proto.Message) ([]byte, error) {
	raw, err := protoOptions.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding a message as JSON: %w", err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, fmt.Errorf("encoding a message as JSON: %w", err)
	}
	return out.Bytes(), nil
}

// writeDocument indents a compact JSON object and writes it with a newline.
func writeDocument(w io.Writer, compact []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return fmt.Errorf("encoding a document as JSON: %w", err)
	}
	out.WriteByte('\n')
	_, err := w.Write(out.Bytes())
	return err
}

// JSONMessage writes the answer of a call as one document: schemaVersion, then
// the message's own fields under the contract's JSON names.
func JSONMessage(w io.Writer, m proto.Message) error {
	inner, err := compactProto(m)
	if err != nil {
		return err
	}
	head := fmt.Sprintf(`{"schemaVersion":%d`, SchemaVersion)
	if string(inner) == "{}" {
		return writeDocument(w, []byte(head+"}"))
	}
	return writeDocument(w, []byte(head+","+string(inner[1:])))
}

// JSONList writes a list as one document: schemaVersion, the items under the
// given field name (the contract's name for the repeated field, and an empty
// array, never null, when there are none), and "more", which is true when the
// server had further items than were fetched.
func JSONList[T proto.Message](w io.Writer, field string, items []T, more bool) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"schemaVersion":%d,%q:[`, SchemaVersion, field)
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, err := compactProto(it)
		if err != nil {
			return err
		}
		b.Write(raw)
	}
	fmt.Fprintf(&b, `],"more":%t}`, more)
	return writeDocument(w, b.Bytes())
}

// JSONListTotal is JSONList with "totalCount" after "more": the number of
// rows that match, written only when the server knew it, since an absent
// count means not known and a zero would say there are none.
func JSONListTotal[T proto.Message](w io.Writer, field string, items []T, more bool, total *int64) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"schemaVersion":%d,%q:[`, SchemaVersion, field)
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, err := compactProto(it)
		if err != nil {
			return err
		}
		b.Write(raw)
	}
	fmt.Fprintf(&b, `],"more":%t`, more)
	if total != nil {
		fmt.Fprintf(&b, `,"totalCount":%d`, *total)
	}
	b.WriteByte('}')
	return writeDocument(w, b.Bytes())
}

// JSONValue writes any value that encoding/json can encode as a document with
// schemaVersion added first. The value must encode as an object.
func JSONValue(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding a document as JSON: %w", err)
	}
	if len(raw) < 2 || raw[0] != '{' {
		return fmt.Errorf("encoding a document as JSON: %T is not an object", v)
	}
	head := fmt.Sprintf(`{"schemaVersion":%d`, SchemaVersion)
	if string(raw) == "{}" {
		return writeDocument(w, []byte(head+"}"))
	}
	return writeDocument(w, []byte(head+","+string(raw[1:])))
}
