package header

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// read opens a test file and reads its header, failing the test on error.
func read(t *testing.T, name string) (*Header, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	h, err := Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return h, data
}

// TestReadPlacesEveryCorpusFile guards that every public test file gives its
// four placing UIDs, well formed, and a transfer syntax.
func TestReadPlacesEveryCorpusFile(t *testing.T) {
	names, _ := filepath.Glob(filepath.Join("..", "testdata", "*.dcm"))
	if len(names) < 10 {
		t.Fatalf("expected the corpus files, found %d", len(names))
	}
	for _, p := range names {
		name := filepath.Base(p)
		if name == "MR_truncated.dcm" {
			continue
		}
		h, _ := read(t, name)
		for _, a := range []Attribute{SOPClassUID, SOPInstanceUID, StudyUID, SeriesUID} {
			v, ok := h.UID(a)
			if !ok || uid.Check(v) != nil {
				t.Errorf("%s: %s is %q (present %v)", name, a, v, ok)
			}
		}
		if uid.Check(h.TransferSyntaxUID) != nil {
			t.Errorf("%s: transfer syntax %q", name, h.TransferSyntaxUID)
		}
	}
}

// TestPixelDataOffsetPointsAtTheElement guards the sidecar's pixel data
// offset: the bytes at it are the Pixel Data tag in the file's byte order,
// and the span ends inside the file, for native and encapsulated pixel data.
func TestPixelDataOffsetPointsAtTheElement(t *testing.T) {
	for _, name := range []string{"MR_small.dcm", "MR_small_RLE.dcm", "MR_small_jp2klossless.dcm", "ct5n-2062.dcm", "liver_1frame.dcm"} {
		h, data := read(t, name)
		if !h.HasPixelData {
			t.Fatalf("%s: no pixel data found", name)
		}
		off := h.PixelData.Offset
		if off <= 132 || off+8 > int64(len(data)) {
			t.Fatalf("%s: offset %d outside the file", name, off)
		}
		group := binary.LittleEndian.Uint16(data[off:])
		element := binary.LittleEndian.Uint16(data[off+2:])
		if group != 0x7FE0 || element != 0x0010 {
			t.Errorf("%s: bytes at %d are (%04X,%04X), not (7FE0,0010)", name, off, group, element)
		}
		if end := off + h.PixelData.Length; end > int64(len(data)) || h.PixelData.Length < 8 {
			t.Errorf("%s: span %+v ends past the file of %d bytes", name, h.PixelData, len(data))
		}
	}
}

// TestNotDICOM guards that text, an empty file, a cut file and a data set
// without its preamble are refused as not DICOM, with an offset.
func TestNotDICOM(t *testing.T) {
	mr, err := os.ReadFile(filepath.Join("..", "testdata", "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"text":      []byte("this is not a DICOM file, only some words\n"),
		"empty":     {},
		"preamble":  append(make([]byte, 128), []byte("DICM")...),
		"cut":       mr[:len(mr)-100],
		"no prefix": mr[132:],
	}
	for name, data := range cases {
		_, err := Read(bytes.NewReader(data), int64(len(data)))
		var nd *NotDICOMError
		if !errors.As(err, &nd) {
			t.Errorf("%s: got %v, want a NotDICOMError", name, err)
		}
	}
	trunc, err := os.ReadFile(filepath.Join("..", "testdata", "MR_truncated.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	var nd *NotDICOMError
	if _, err := Read(bytes.NewReader(trunc), int64(len(trunc))); !errors.As(err, &nd) {
		t.Errorf("MR_truncated.dcm: got %v, want a NotDICOMError", err)
	}
}

// TestLookupForNaming guards that the naming rule reads the values it
// needs, padding removed, and that the default rule makes labels.
func TestLookupForNaming(t *testing.T) {
	h, _ := read(t, "MR_small.dcm")
	if v, ok := h.Lookup(naming.NewTag(0x0010, 0x0020)); !ok || v != "4MR1" {
		t.Errorf("PatientID = %q, %v", v, ok)
	}
	if _, ok := h.Lookup(naming.NewTag(0x0010, 0x4000)); ok {
		t.Error("PatientComments is not in the file but was found")
	}
	key := bytes.Repeat([]byte{7}, naming.MinKeyLength)
	hasher, err := naming.NewHasher(key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := naming.Default().Labels(h, hasher)
	if err != nil {
		t.Fatal(err)
	}
	if p.Check() != nil {
		t.Errorf("labels %+v are not valid", p)
	}
}

// TestDICOMJSONHasNoBulkData guards the sidecar's data set: valid JSON
// keyed by tag, holding the UIDs, and without the pixel data's value.
func TestDICOMJSONHasNoBulkData(t *testing.T) {
	h, data := read(t, "MR_small.dcm")
	js, err := h.DICOMJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	inst, _ := h.UID(SOPInstanceUID)
	if v, ok := m["00080018"]["Value"].([]any); !ok || len(v) != 1 || v[0] != inst {
		t.Errorf("SOP Instance UID in the JSON is %v", m["00080018"])
	}
	px, ok := m["7FE00010"]
	if !ok {
		t.Fatal("the pixel data element is not listed")
	}
	if _, has := px["InlineBinary"]; has {
		t.Error("the pixel data's value is in the JSON")
	}
	if len(js) >= len(data) {
		t.Errorf("the JSON of %d bytes is not smaller than the file of %d", len(js), len(data))
	}
	again, _ := h.DICOMJSON()
	if !bytes.Equal(js, again) {
		t.Error("the same header gave two different JSON texts")
	}
}
