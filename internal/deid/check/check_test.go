package check

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filewriter"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// One needle of each kind, and the text it is planted as.
var kinds = []struct {
	kind   Kind
	needle engine.Needle
	text   string
}{
	{KindName, needle(engine.NeedleName, "DOE^JOHN"), "seen by John Doe"},
	{KindIdentifier, needle(engine.NeedleText, "MRN-778812"), "record MRN-778812 follows"},
	{KindDate, needle(engine.NeedleDate, "20240315"), "scanned 15.03.2024 at noon"},
	{KindUID, needle(engine.NeedleUID, "1.2.826.0.1.3680043.2.1125.77"), "ref 1.2.826.0.1.3680043.2.1125.77"},
}

// overlay is Overlay Data (6000,3000), a standard element of VR OW whose
// value is never decoded as text: what is planted in it is found only by
// the raw search.
const overlay = 0x60003000

// TestEachKindIsFoundInEachPlace guards that a value of each kind is found
// wherever a file can hold it: a top-level element, an element of a nested
// sequence's item, a private element, a private value of VR UN, the file
// meta information, the preamble, and the value of a standard binary
// element (only by the raw search); and that the finding names the element
// and the kind.
func TestEachKindIsFoundInEachPlace(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k.kind), func(t *testing.T) {
			t.Run("top level", func(t *testing.T) {
				p := writeFile(t, el{0x00204000, "LT", k.text})
				mustFind(t, p, k.kind, "00204000", k.needle)
			})
			t.Run("nested sequence", func(t *testing.T) {
				inner := build(t, el{0x00081030, "LO", k.text})
				outer := build(t, el{0x00081110, "SQ", []*dataset.Dataset{inner}})
				p := writeFile(t, el{0x00081115, "SQ", []*dataset.Dataset{build(t), outer}})
				mustFind(t, p, k.kind, "00081115[1]/00081110[0]/00081030", k.needle)
			})
			t.Run("private", func(t *testing.T) {
				p := writeFile(t, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091001, "LO", k.text})
				mustFind(t, p, k.kind, "00091001", k.needle)
			})
			t.Run("unknown VR", func(t *testing.T) {
				p := writeFile(t, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091002, "UN", k.text + " "})
				mustFind(t, p, k.kind, "00091002", k.needle)
			})
			t.Run("file meta", func(t *testing.T) {
				// The Media Storage SOP Instance UID holds the UID; the
				// Source AE Title, which nothing copies from the data set,
				// every other value.
				meta := filewriter.FileMetaInfo{SourceApplicationEntityTitle: k.needle.Value}
				want := "00020016"
				if k.kind == KindUID {
					meta = filewriter.FileMetaInfo{MediaStorageSOPInstanceUID: k.needle.Value}
					want = "00020003"
				}
				p := writeDS(t, build(t, base()...), meta)
				mustFind(t, p, k.kind, want, k.needle)
			})
			t.Run("preamble", func(t *testing.T) {
				p := writeFile(t)
				patch(t, p, 10, []byte(k.text))
				mustFind(t, p, k.kind, "preamble", k.needle)
			})
			t.Run("standard binary", func(t *testing.T) {
				p := writeFile(t, el{overlay, "OW", even(" " + k.text + " ")})
				rep := mustFind(t, p, k.kind, "60003000", k.needle)
				if !rep.Findings[0].Raw {
					t.Errorf("found other than in the raw bytes: %+v", rep.Findings)
				}
			})
		})
	}
}

// even returns s as bytes padded with a space to an even length, as a
// value of VR OW is.
func even(s string) []byte {
	if len(s)%2 == 1 {
		s += " "
	}
	return []byte(s)
}

// TestCleanFilePasses guards that a file holding none of the needles
// passes, with the forms searched for and the short ones counted.
func TestCleanFilePasses(t *testing.T) {
	p := writeFile(t, el{0x00204000, "LT", "nothing to see here"}, el{0x00080020, "DA", "20190101"})
	rep := mustPass(t, p, needle(engine.NeedleName, "DOE^JO"), needle(engine.NeedleDate, "20240315"),
		needle(engine.NeedleText, "AB"))
	// DOE^JO and its long forms, the forms of the date; JO, DOE and AB
	// too short.
	if rep.Needles != 3 || rep.Searched < 10 || rep.Skipped != 3 {
		t.Errorf("report %+v", rep)
	}
}

// TestShortValuesAreSkippedAndCounted guards the minimum: a value under
// MinLength is not searched for and is counted, a lower minimum searches
// it, and a name of one Han character is searched for at any length as the
// engine cleans it.
func TestShortValuesAreSkippedAndCounted(t *testing.T) {
	p := writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00204000, "LT", "see ABC and 李"})
	rep := mustPass(t, p, needle(engine.NeedleText, "ABC"))
	if rep.Skipped != 1 || rep.Searched != 0 {
		t.Errorf("report %+v", rep)
	}
	if _, err := New(Options{MinLength: 3}).checkPath(t, t.Context(), p, engine.Needles{needle(engine.NeedleText, "ABC")}, nil); err == nil {
		t.Error("a minimum of 3 did not search for a 3-letter value")
	}
	mustFind(t, p, KindName, "00204000", needle(engine.NeedleName, "李^小龍"))
}

// TestFindingsHoldNoValue guards that neither the report nor the error's
// text, reason or detail carries the value found.
func TestFindingsHoldNoValue(t *testing.T) {
	p := writeFile(t, el{0x00204000, "LT", "DOE^JOHN"}, el{0x00081030, "LO", "MRN-778812"})
	rep, err := run(t, p, needle(engine.NeedleName, "DOE^JOHN"), needle(engine.NeedleText, "MRN-778812"))
	var res *Residual
	if !errors.As(err, &res) {
		t.Fatalf("got %v", err)
	}
	all := fmt.Sprintf("%v %+v %#v %s %s", err, rep, rep, res.Reason(), res.Detail())
	for _, v := range []string{"DOE", "JOHN", "778812"} {
		if strings.Contains(strings.ToUpper(all), v) {
			t.Errorf("%q in %s", v, all)
		}
	}
	if res.Total != 2 || !strings.Contains(res.Detail(), "(2 found)") {
		t.Errorf("detail %q, total %d", res.Detail(), res.Total)
	}
}

// imageElements are the Image Pixel module of an 8-bit 10-pixel image
// (PS3.3 C.7.6.3), Pixel Data aside.
var imageElements = []el{
	{0x00280002, "US", []uint16{1}}, {0x00280004, "CS", "MONOCHROME2"}, {0x00280010, "US", []uint16{8}},
	{0x00280011, "US", []uint16{10}}, {0x00280100, "US", []uint16{8}}, {0x00280101, "US", []uint16{8}},
	{0x00280102, "US", []uint16{7}}, {0x00280103, "US", []uint16{0}},
}

// TestPixelDataIsNotSearched guards that the bytes of the top level's
// Pixel Data are left out of both searches (an image whose bytes happen
// to spell a needle passes), and that Pixel Data in an item of a sequence
// (an icon image, PS3.3 C.7.6.1.1.6), which de-identification never keeps,
// is a finding whatever it holds, at its path.
func TestPixelDataIsNotSearched(t *testing.T) {
	px := []byte(strings.Repeat("MRN-778812", 8))
	n := needle(engine.NeedleText, "MRN-778812")
	mustPass(t, writeFile(t, append(imageElements, el{0x7FE00010, "OB", px})...), n)
	icon := build(t, append(imageElements, el{0x7FE00010, "OB", []byte("plain pixels")})...)
	mustFind(t, writeFile(t, el{0x00880200, "SQ", []*dataset.Dataset{icon}}), KindIdentifier, "00880200[0]/7FE00010", n)
}
