package check

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// methodEngine returns an engine on the basic profile of PS3.15 Annex E
// that names its method, so it writes the record of the de-identification
// with a code, its meaning, the method and the versions.
func methodEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	k := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	en, err := engine.New(e, engine.Keys{UID: k('u'), Date: k('d'), Hash: k('h')}, engine.Options{Method: "research@3"})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// deidentifyName de-identifies a file of a patient of the name given with
// en, writes it, and returns its path and the engine's record.
func deidentifyName(t *testing.T, en *engine.Engine, name string, more ...el) (string, *engine.Record) {
	t.Helper()
	ds := build(t, append([]el{
		{0x00080016, "UI", testSOPClass},
		{0x00080018, "UI", testSOPInstance},
		{0x00080060, "CS", "OT"},
		{0x00100010, "PN", name},
		{0x00100020, "LO", "PID-58213"},
	}, more...)...)
	rec, err := en.Apply(context.Background(), ds, engine.Reception{})
	if err != nil {
		t.Fatal(err)
	}
	// The file meta information names the instance by its remapped UID, as
	// the pipeline's writer does.
	sop := strings.TrimRight(string(ds.GetValue(tag.Tag(0x00080018))), " \x00")
	return writeDS(t, ds, filewriter.FileMetaInfo{MediaStorageSOPInstanceUID: sop}), rec
}

// checkWritten checks the file at path as the pipeline checks a file the
// engine just made: with the run's needles and its record.
func checkWritten(t *testing.T, path string, needles engine.Needles, written engine.Written) (Report, error) {
	t.Helper()
	b := readBytes(t, path)
	return New(Options{}).CheckWritten(context.Background(), bytes.NewReader(b), int64(len(b)), needles, written, nil)
}

// TestTheWrittenMethodIsNoResidual guards a patient whose family name is a
// word of the record of the de-identification the engine writes (PS3.15
// E.1.1): "NoPACs", the name of the software in De-identification Method
// (0012,0063), and "Basic" and "Profile", words of the basic profile's
// code meaning there and in the code sequence (0012,0064). Checked with
// the run's record, the file passes; checked without it, every element
// searched, the same file is held at the record, which is the file held
// forever that the record prevents.
func TestTheWrittenMethodIsNoResidual(t *testing.T) {
	en := methodEngine(t)
	for _, name := range []string{"NoPACs^Maria", "Basic^Ann", "Profile^Lee"} {
		t.Run(name, func(t *testing.T) {
			path, rec := deidentifyName(t, en, name)
			if rec.Written.Method == "" || len(rec.Written.MethodCodes) != 1 {
				t.Fatalf("the engine recorded no method it wrote: %q, %d codes", rec.Written.Method, len(rec.Written.MethodCodes))
			}
			if _, err := checkWritten(t, path, rec.Needles, rec.Written); err != nil {
				t.Fatalf("held with the run's record: %v", err)
			}
			_, err := checkWritten(t, path, rec.Needles, engine.Written{})
			var res *Residual
			if !errors.As(err, &res) || res.First.Kind != KindName {
				t.Fatalf("without the record, want a name found in it, got %v", err)
			}
			if p := res.First.Path; p != "00120063" && p != "00120064[0]/00080104" {
				t.Errorf("without the record, found at %s", p)
			}
		})
	}
}

// TestAKeptMethodIsSearched guards a De-identification Method (0012,0063)
// the file was received with and the profile kept: a profile on no Annex
// E base that names no method writes none, so its record names none, and
// the received value, holding the patient's name, is a residual.
func TestAKeptMethodIsSearched(t *testing.T) {
	e, problems := profile.Expand(&v1.ProfileDocument{Rules: []*v1.ProfileRule{
		profileRule("names", vrSelector("PN"), actRemove),
	}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	k := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	en, err := engine.New(e, engine.Keys{UID: k('u'), Date: k('d'), Hash: k('h')}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path, rec := deidentifyName(t, en, "NoPACs^Maria", el{0x00120063, "LO", "Exported by NoPACs"})
	if rec.Written.Method != "" {
		t.Fatalf("the engine says it wrote %q", rec.Written.Method)
	}
	_, err = checkWritten(t, path, rec.Needles, rec.Written)
	var res *Residual
	if !errors.As(err, &res) || res.First.Path != "00120063" {
		t.Fatalf("want the kept method held, got %v", err)
	}
}

// writtenOf is a record of the de-identification as the engine writes one
// on the basic profile, for files made here by hand.
var writtenOf = engine.Written{
	Method: `Basic Application Confidentiality Profile\research@3\NoPACs unknown\go-dicom unknown`,
	MethodCodes: []anonymize.MethodCode{
		{Value: "113100", Scheme: "DCM", Meaning: "Basic Application Confidentiality Profile"},
	},
}

// codeItem is an item of De-identification Method Code Sequence, with
// more elements after the three of a code.
func codeItem(t *testing.T, c anonymize.MethodCode, more ...el) *dataset.Dataset {
	t.Helper()
	return build(t, append([]el{{0x00080100, "SH", c.Value}, {0x00080102, "SH", c.Scheme}, {0x00080104, "LO", c.Meaning}}, more...)...)
}

// TestOnlyTheExactRecordIsSkipped guards the bounds of what is not
// searched: a De-identification Method that is not exactly the one the
// run wrote, a code sequence with an element more, the run's very value
// inside a sequence item, and bytes in the raw search past the record are
// each searched and found; the run's record itself, at the top level, is
// not.
func TestOnlyTheExactRecordIsSkipped(t *testing.T) {
	nopacs := needle(engine.NeedleName, "NoPACs^Maria")
	method := el{0x00120063, "LO", writtenOf.Method}
	codes := el{0x00120064, "SQ", []*dataset.Dataset{codeItem(t, writtenOf.MethodCodes[0])}}
	cases := []struct {
		name string
		els  []el
		want string
		raw  bool
	}{
		{"the record as written", []el{method, codes}, "", false},
		{"a method with a value more", []el{{0x00120063, "LO", writtenOf.Method + `\NoPACs`}, codes}, "00120063", false},
		{"a method with a word changed", []el{{0x00120063, "LO", `Basic Application Confidentiality Profile\research@3\NoPACs 9`}, codes}, "00120063", false},
		{"a code item with an element more", []el{method, {0x00120064, "SQ", []*dataset.Dataset{
			codeItem(t, writtenOf.MethodCodes[0], el{0x00080103, "SH", "NoPACs"})}}}, "00120064[0]/00080103", false},
		{"a code item more", []el{method, {0x00120064, "SQ", []*dataset.Dataset{
			codeItem(t, writtenOf.MethodCodes[0]), codeItem(t, anonymize.MethodCode{Value: "1", Scheme: "99X", Meaning: "NoPACs"})}}}, "00120064[1]/00080104", false},
		{"the record's value in an item", []el{method, codes, {0x00400275, "SQ", []*dataset.Dataset{
			build(t, method)}}}, "00400275[0]/00120063", false},
		{"bytes past the record in the raw search", []el{method, codes, {0x00130010, "LO", "ACME 1.0"},
			{0x00131010, "OW", []byte("\x01NoPACs\x01\x01")}}, "00131010", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, tc.els...)
			rep, err := checkWritten(t, path, engine.Needles{nopacs}, writtenOf)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want a pass, got %v: %+v", err, rep.Findings)
				}
				return
			}
			var res *Residual
			if !errors.As(err, &res) {
				t.Fatalf("want a residual at %s, got %v", tc.want, err)
			}
			for _, f := range rep.Findings {
				if f.Path == tc.want && f.Raw == tc.raw {
					return
				}
			}
			t.Fatalf("no finding at %s (raw %v): %+v", tc.want, tc.raw, rep.Findings)
		})
	}
}

// recordBytes is the record of the de-identification writtenOf names,
// encoded by hand in Explicit VR Little Endian (PS3.5 7.1.2, 7.5.2): the
// method, then its code sequence of undefined length with one item of
// undefined length. itemEnd and seqEnd are the 4-byte lengths of the Item
// and Sequence Delimitation Items (zero in a well-made file); un, when not
// nil, writes the Code Value as UN with these two reserved bytes and a
// 4-byte length, as an intermediary that did not know the tag would.
func recordBytes(t *testing.T, itemEnd, seqEnd, un []byte) []byte {
	t.Helper()
	var b []byte
	head := func(group, elem uint16, vr string) {
		b = binary.LittleEndian.AppendUint16(b, group)
		b = binary.LittleEndian.AppendUint16(b, elem)
		b = append(b, vr...)
	}
	// length is the length of v, which fits the 2-byte length field of a
	// short explicit VR (PS3.5 7.1.2).
	length := func(v string) uint16 {
		n := len(v)
		if n < 0 || n > math.MaxUint16 {
			t.Fatalf("a value of %d bytes", n)
			return 0
		}
		return uint16(n)
	}
	short := func(group, elem uint16, vr, v string) {
		if len(v)%2 == 1 {
			v += " "
		}
		head(group, elem, vr)
		b = binary.LittleEndian.AppendUint16(b, length(v))
		b = append(b, v...)
	}
	delim := func(elem uint16, n []byte) {
		head(0xFFFE, elem, "")
		b = append(b, n...)
	}
	c := writtenOf.MethodCodes[0]
	short(0x0012, 0x0063, "LO", writtenOf.Method)
	head(0x0012, 0x0064, "SQ")
	b = append(b, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF)
	delim(0xE000, []byte{0xFF, 0xFF, 0xFF, 0xFF})
	if un != nil {
		head(0x0008, 0x0100, "UN")
		b = append(b, un...)
		b = binary.LittleEndian.AppendUint32(b, uint32(length(c.Value)))
		b = append(b, c.Value...)
	} else {
		short(0x0008, 0x0100, "SH", c.Value)
	}
	short(0x0008, 0x0102, "SH", c.Scheme)
	short(0x0008, 0x0104, "LO", c.Meaning)
	delim(0xE00D, itemEnd)
	delim(0xE0DD, seqEnd)
	return b
}

// TestOnlyTheComparedValuesLeaveTheRawSearch guards the bytes of the code
// sequence (0012,0064) that were never compared with what the engine
// wrote: the lengths of its Item and Sequence Delimitation Items, and the
// header of a Code Value written as UN with its reserved bytes, are each
// searched raw with the run's record given, and a Code Value written as
// UN is not taken for the one the engine wrote (SH), so the item's text is
// searched too. The well-made record, the control, passes for a word of
// its code meaning.
func TestOnlyTheComparedValuesLeaveTheRawSearch(t *testing.T) {
	zero := []byte{0, 0, 0, 0}
	cases := []struct {
		name   string
		record []byte
		needle engine.Needle
		want   bool
	}{
		// A word of the code meaning: the record is taken for the run's,
		// or the control would be found too.
		{"the record well made", recordBytes(t, zero, zero, nil), needle(engine.NeedleName, "Basic^Ann"), false},
		{"an item delimiter's length", recordBytes(t, []byte("4711"), zero, nil), needle(engine.NeedleText, "4711"), true},
		{"the sequence delimiter's length", recordBytes(t, zero, []byte("4711"), nil), needle(engine.NeedleText, "4711"), true},
		{"a code value as UN, its reserved bytes", recordBytes(t, zero, zero, []byte("12")), needle(engine.NeedleText, "UN12"), true},
		{"a code value as UN, the item's text", recordBytes(t, zero, zero, []byte{0, 0}), needle(engine.NeedleName, "Basic^Ann"), true},
	}
	head := readBytes(t, writeFile(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := append(append([]byte{}, head...), tc.record...)
			rep, err := New(Options{}).CheckWritten(context.Background(), bytes.NewReader(b), int64(len(b)), engine.Needles{tc.needle}, writtenOf, nil)
			if !tc.want {
				if err != nil {
					t.Fatalf("want a pass, got %v: %+v", err, rep.Findings)
				}
				return
			}
			var res *Residual
			if !errors.As(err, &res) {
				t.Fatalf("want a residual with the record given, got %v: %+v", err, rep.Findings)
			}
			if p := res.First.Path; !strings.HasPrefix(p, "00120064") {
				t.Errorf("found at %s, want in the code sequence", p)
			}
		})
	}
}
