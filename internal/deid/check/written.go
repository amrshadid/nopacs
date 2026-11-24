package check

import (
	"strings"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// The elements of the record of the de-identification (PS3.15 E.1.1,
// E.3.6), and those of an item of its code sequence (PS3.3 8.8).
const (
	tagDeidMethod     tag.Tag = 0x00120063
	tagDeidMethodCode tag.Tag = 0x00120064
	tagCodeValue      tag.Tag = 0x00080100
	tagCodingScheme   tag.Tag = 0x00080102
	tagCodeMeaning    tag.Tag = 0x00080104
)

// writtenElements returns the elements of the top level of a data set that
// hold exactly the record of the de-identification the engine wrote in the
// run that made the file (engine.Written): De-identification Method
// (0012,0063) whose value is, byte for byte but for its padding, the value
// written, and De-identification Method Code Sequence (0012,0064) whose
// items are, one for one and in order, the codes written, each item holding
// Code Value, Coding Scheme Designator and Code Meaning and nothing else.
// Neither is searched as text, nor are the raw bytes of the values compared
// (writtenValues).
//
// This lets no received value through. PS3.15 E.1.1 and E.3.6 make these
// elements the de-identifier's own statement of what it did: the engine
// replaces whatever the file carried in them with its own (planMethod runs
// after every rule, so its value is the one written), and every word of
// what it writes comes from somewhere other than the file: a code meaning
// of PS3.16 CID 7050 the DICOM library holds, the name of the profile
// version a person gave, and the versions of the build. A word of it that
// equals a removed value ("NoPACs", "Basic", "Profile") is in every file the
// version makes, whoever the patient is, so it says nothing of anyone. What
// is skipped is an element whose value equals that statement exactly: an
// element carrying anything else (a received value the profile kept, a
// value a rule changed after, a byte more) is unlike it and is searched as
// before, as is one inside a sequence, one in the file meta information,
// and every one when the engine wrote none (a zero engine.Written).
//
// Each element compared must also carry the VR the engine writes (PS3.6:
// LO for the method and a Code Meaning, SH for a Code Value and a Coding
// Scheme Designator, SQ for the sequence), in the short explicit form when
// the file wrote a VR (PS3.5 7.1.2): the reader turns a value written as UN
// into its dictionary VR (PS3.5 6.2.2 Note 2), and such a value, with its
// two reserved bytes, is not what the engine wrote.
func writtenElements(elements []*attributes.DataElementValue, w engine.Written) map[*attributes.DataElementValue]bool {
	out := map[*attributes.DataElementValue]bool{}
	for _, e := range elements {
		switch {
		case e.Tag == tagDeidMethod && w.Method != "" && heldText(e, "LO", w.Method):
			out[e] = true
		case e.Tag == tagDeidMethodCode && len(w.MethodCodes) > 0 && sameCodes(e, w.MethodCodes):
			out[e] = true
		}
	}
	return out
}

// writtenValues returns the ranges of raw bytes of the elements of written
// (writtenElements) kept out of the raw search: exactly the values heldText
// compared, by the offsets the reader gave for each, and nothing else. For
// the method that is its value; for the code sequence it is the value of
// each Code Value, Coding Scheme Designator and Code Meaning, so the
// headers, VRs, reserved bytes and lengths of the sequence, of its items, of
// their elements and of every delimitation item (PS3.5 7.5) are still
// searched: none of those bytes was compared with what the engine wrote.
func writtenValues(written map[*attributes.DataElementValue]bool) [][2]int64 {
	var out [][2]int64
	add := func(e *attributes.DataElementValue) {
		if pos := e.Position; pos.End > pos.ValueOffset {
			out = append(out, [2]int64{pos.ValueOffset, pos.End})
		}
	}
	for e := range written {
		if e.Items == nil {
			add(e)
			continue
		}
		for _, it := range e.Items {
			for _, x := range it.Elements {
				add(x)
			}
		}
	}
	return out
}

// heldText reports whether e is a value of VR vr held whole that equals
// want, the space padding of a text value (PS3.5 6.2) aside, written with
// a short explicit VR when the file wrote one.
func heldText(e *attributes.DataElementValue, vr, want string) bool {
	if e.VR != vr || e.Items != nil || e.UndefinedLength || e.ValueSkipped || int64(len(e.Value)) != int64(e.Length) {
		return false
	}
	// The bytes cut from the raw search are [ValueOffset, End): exactly the
	// value compared, so its length must be the one the element states.
	if pos := e.Position; (pos.ExplicitVR && pos.LengthSize != 2) || pos.End-pos.ValueOffset != int64(e.Length) {
		return false
	}
	return strings.TrimRight(string(e.Value), " ") == strings.TrimRight(want, " ")
}

// sameCodes reports whether the sequence e holds exactly codes: an item
// per code, in order, each of the three elements of a code and no other.
func sameCodes(e *attributes.DataElementValue, codes []anonymize.MethodCode) bool {
	if e.VR != "SQ" || len(e.Items) != len(codes) {
		return false
	}
	for i, it := range e.Items {
		c := codes[i]
		want := []struct {
			tag   tag.Tag
			vr    string
			value string
		}{{tagCodeValue, "SH", c.Value}, {tagCodingScheme, "SH", c.Scheme}, {tagCodeMeaning, "LO", c.Meaning}}
		if len(it.Elements) != len(want) {
			return false
		}
		for j, x := range it.Elements {
			if x.Tag != want[j].tag || want[j].value == "" || !heldText(x, want[j].vr, want[j].value) {
				return false
			}
		}
	}
	return true
}
