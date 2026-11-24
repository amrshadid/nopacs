package api

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// The DICOM JSON members of an element (PS3.18 F.2.2).
const (
	memberVR      = "vr"
	memberValue   = "Value"
	memberInline  = "InlineBinary"
	memberBulkURI = "BulkDataURI"
)

// keyPaddingMember is Data Set Trailing Padding (FFFC,FFFC) as a member
// name; an answer leaves it out (PS3.5 7.1.3, PS3.10 7.2).
const keyPaddingMember = "FFFCFFFC"

// pixelMembers are the member names of the elements that hold an image's
// pixels: Pixel Data, Float Pixel Data and Double Float Pixel Data (PS3.3
// C.7.6.3).
var pixelMembers = map[string]bool{"7FE00010": true, "7FE00008": true, "7FE00009": true}

// datasetFromSidecar returns the DICOM JSON of the instance the metadata
// file's line in describes, answered as the contract says, with a
// BulkDataURI in place of the pixel data the line points at. It returns nil
// when the line does not settle the answer and the instance's file must be
// read: the line holds no data set, its Pixel Data cannot be placed, or it
// holds an element that may be bulk data whose place the file does not
// keep. The file records the place of Pixel Data and of no other value, so
// any other value it did not hold (a binary value over 1 KiB), and any
// text that may pass 64 KiB as the file holds it, is found in the file.
//
// The line's data set is go-dicom's DICOM JSON, the text the instance's
// file gives: members in order of their tags, written as encoding/json
// writes them. Each element the answer keeps is passed on as the line
// holds it, found by a walk over the text once encoding/json has found it
// valid, and only Pixel Data is written anew. Decoding the data set into
// values and writing it again cost some 90 us an instance, most of the
// time of a scan's page.
func datasetFromSidecar(in sidecar.Instance, ts string, uri func(offset, length int64) string) []byte {
	hdr, known := headerLength(ts)
	if !known {
		return nil
	}
	// The first JSON value of the line's data set, checked to be valid
	// JSON, so the walks below may assume it is. A line is one value; the
	// decoder, which also passes over what follows the first, is needed
	// only for a line that is not.
	raw := bytes.TrimSpace(in.Dataset)
	if !json.Valid(raw) {
		var first json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(in.Dataset)).Decode(&first); err != nil {
			return nil
		}
		raw = first
	}
	set := make([]member, 0, 64)
	if !eachMember(raw, func(key, value []byte) bool {
		set = append(set, member{key: decodedKey(key), value: value})
		return true
	}) {
		return nil
	}
	// One member per name, the last as a decoder keeps it, in the order of
	// their names, as encoding/json writes a map.
	slices.SortStableFunc(set, func(a, b member) int { return bytes.Compare(a.key, b.key) })
	kept := set[:0]
	for i, m := range set {
		if i+1 < len(set) && bytes.Equal(set[i+1].key, m.key) {
			continue
		}
		if string(m.key) != keyPaddingMember {
			kept = append(kept, m)
		}
	}
	pixelAt := -1
	var pixel []byte
	size := 2
	for i, m := range kept {
		size += len(m.key) + len(m.value) + 4
		el, ok := elementOf(m.value)
		if !ok {
			return nil
		}
		switch settled, place := el.settle(m.key, 0); {
		case !settled:
			return nil
		case place:
			if in.PixelData == nil || pixel != nil || in.PixelData.Length < hdr {
				return nil
			}
			// As encoding/json writes the map of the two members.
			pixel = []byte(`{"` + memberBulkURI + `":`)
			pixel = appendJSONString(pixel, []byte(uri(in.PixelData.Offset+hdr, in.PixelData.Length-hdr)))
			pixel = append(pixel, `,"`+memberVR+`":`...)
			pixel = append(appendJSONString(pixel, []byte(el.vr)), '}')
			pixelAt = i
		}
	}
	if in.PixelData != nil && pixel == nil {
		return nil
	}
	out := make([]byte, 0, size+len(pixel))
	out = append(out, '{')
	for i, m := range kept {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(appendJSONString(out, m.key), ':')
		if i == pixelAt {
			out = append(out, pixel...)
		} else {
			out = append(out, m.value...)
		}
	}
	return append(out, '}')
}

// member is one member of a JSON object: its name, decoded, and its value
// as written.
type member struct {
	key, value []byte
}

// element is one element of a DICOM JSON data set, as far as settle needs
// it: its VR, and its Value and InlineBinary as written (the last of each
// when written twice, as a decoder keeps it).
type element struct {
	vr            string
	value, inline []byte
	hasValue      bool
	inlineIsText  bool
	hasURI        bool
}

// elementOf reads an element: it must be a JSON object. A VR that is not a
// string is read as none.
func elementOf(raw []byte) (element, bool) {
	var el element
	ok := eachMember(raw, func(key, value []byte) bool {
		switch string(decodedKey(key)) {
		case memberVR:
			el.vr = ""
			if value[0] == '"' {
				el.vr = jsonText(value)
			}
		case memberValue:
			el.value, el.hasValue = value, true
		case memberInline:
			el.inline, el.inlineIsText = value, value[0] == '"'
		case memberBulkURI:
			el.hasURI = true
		}
		return true
	})
	return el, ok
}

// settle reports whether the element named key, at depth (0 the data set's
// top), is answered as the line holds it, or, with place, whether it is
// the top level's Pixel Data, skipped when the file was written, whose
// BulkDataURI the caller writes. It reports false when the element needs
// the instance's file, as Pixel Data in a sequence's item does.
func (el element) settle(key []byte, depth int) (settled, place bool) {
	if el.vr == "SQ" {
		// A Value that is not an array holds no items; every item must be
		// a data set whose every element is settled.
		ok := !el.hasValue || el.value[0] != '[' || eachItem(el.value, func(item []byte) bool {
			return eachMember(item, func(k, v []byte) bool {
				child, ok := elementOf(v)
				if !ok {
					return false
				}
				settled, place := child.settle(decodedKey(k), depth+1)
				return settled && !place
			})
		})
		return ok, false
	}
	switch {
	case el.hasURI:
		return false, false
	case el.inlineIsText && textLength(el.inline) > trustedInline:
		return false, false
	case el.hasValue && valueSize(el.value) > trustedInline:
		return false, false
	case el.hasValue || el.inlineIsText:
		// Held in the file as it is answered.
		return true, false
	case pixelMembers[string(key)]:
		// Skipped when the file was written, whatever its length; only the
		// top level's is placed.
		return depth == 0, depth == 0
	case isBinary(el.vr):
		// A binary value skipped for its length, or an empty one: the
		// file cannot tell them apart and does not keep its place.
		return false, false
	}
	return true, false
}

// textLength returns the length of the JSON string raw once decoded. The
// text as written is never shorter than that plus its quotes, so it is
// decoded only when it may pass trustedInline.
func textLength(raw []byte) int {
	if len(raw)-2 <= trustedInline {
		return len(raw) - 2
	}
	return len(jsonText(raw))
}

// valueSize returns sizeOf the Value raw. The text as written is never
// shorter than what sizeOf counts, so it is decoded only when it may pass
// trustedInline.
func valueSize(raw []byte) int {
	if len(raw) <= trustedInline {
		return len(raw)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return len(raw)
	}
	return sizeOf(v)
}

// The walks below read JSON text that encoding/json has already found
// valid, so they look only at what separates its values.

// eachMember calls fn with each member of the JSON object raw, its name as
// written with its quotes and its value, in the order written, until fn
// returns false. It reports false when raw is not an object or fn returned
// false.
func eachMember(raw []byte, fn func(key, value []byte) bool) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	i := skipSpace(raw, 1)
	for i < len(raw) && raw[i] != '}' {
		kEnd := skipValue(raw, i)
		key := raw[i:kEnd]
		i = skipSpace(raw, kEnd)
		i = skipSpace(raw, i+1) // The colon.
		vEnd := skipValue(raw, i)
		if !fn(key, raw[i:vEnd]) {
			return false
		}
		i = skipSpace(raw, vEnd)
		if i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		}
	}
	return true
}

// eachItem calls fn with each value of the JSON array raw until fn returns
// false, and reports whether every call returned true.
func eachItem(raw []byte, fn func(item []byte) bool) bool {
	i := skipSpace(raw, 1)
	for i < len(raw) && raw[i] != ']' {
		end := skipValue(raw, i)
		if !fn(raw[i:end]) {
			return false
		}
		i = skipSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		}
	}
	return true
}

// skipValue returns the end of the JSON value that starts at i.
func skipValue(raw []byte, i int) int {
	switch raw[i] {
	case '"':
		for j := i + 1; j < len(raw); j++ {
			switch raw[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return len(raw)
	case '{', '[':
		depth := 0
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case '"':
				j = skipValue(raw, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return len(raw)
	}
	j := i
	for j < len(raw) && !strings.ContainsRune(",}] \t\r\n", rune(raw[j])) {
		j++
	}
	return j
}

// skipSpace returns the first index from i that is not JSON white space.
func skipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	return i
}

// decodedKey returns a member's name, written with its quotes, decoded: the
// text between the quotes itself unless it holds an escape.
func decodedKey(raw []byte) []byte {
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return inner
	}
	return []byte(jsonText(raw))
}

// jsonText decodes the JSON string raw, written with its quotes.
func jsonText(raw []byte) string {
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner)
	}
	var s string
	_ = json.Unmarshal(raw, &s) // Valid JSON: a string always decodes.
	return s
}

// appendJSONString appends s as encoding/json writes a string: as it is,
// quoted, when nothing in it is escaped, and through encoding/json itself
// when something is ('&' in a URI, say).
func appendJSONString(out, s []byte) []byte {
	plain := true
	for i := 0; i < len(s) && plain; i++ {
		c := s[i]
		plain = c >= 0x20 && c < 0x80 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&'
	}
	if plain {
		out = append(out, '"')
		out = append(out, s...)
		return append(out, '"')
	}
	b, _ := json.Marshal(string(s)) // A string always marshals.
	return append(out, b...)
}

// isBinary reports whether vr is a representation PS3.18 F.2.2 carries as
// InlineBinary.
func isBinary(vr string) bool {
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "UN":
		return true
	}
	return false
}

// sizeOf returns about how many bytes the JSON text of a Value takes.
func sizeOf(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case json.Number:
		return len(x)
	case []any:
		n := 0
		for _, e := range x {
			n += sizeOf(e) + 1
		}
		return n
	case map[string]any:
		n := 0
		for k, e := range x {
			n += len(k) + sizeOf(e) + 4
		}
		return n
	}
	return 4
}
