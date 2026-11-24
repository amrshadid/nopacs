package header

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestReadTransferSyntaxMatchesTheWholeRead guards that the bounded read of
// a file's start states the transfer syntax the whole header read does,
// for every public test file, and reads nothing past the bound it names.
func TestReadTransferSyntaxMatchesTheWholeRead(t *testing.T) {
	names, _ := filepath.Glob(filepath.Join("..", "testdata", "*.dcm"))
	for _, p := range names {
		name := filepath.Base(p)
		if name == "MR_truncated.dcm" {
			continue
		}
		h, data := read(t, name)
		got, err := ReadTransferSyntax(&limitedAt{r: bytes.NewReader(data)}, int64(len(data)))
		if err != nil || got != h.TransferSyntaxUID {
			t.Errorf("%s: transfer syntax %q (%v), want %q", name, got, err, h.TransferSyntaxUID)
		}
	}
}

// limitedAt fails any read that reaches past MaxMetaPrefix.
type limitedAt struct{ r *bytes.Reader }

// ReadAt reads, or fails past the bound.
func (l *limitedAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > MaxMetaPrefix {
		return 0, fmt.Errorf("read of %d bytes at %d passes the bound", len(p), off)
	}
	return l.r.ReadAt(p, off)
}

// TestReadTransferSyntaxRefusesWhatIsNotDICOM guards that bytes that are
// not a DICOM file, and a file cut inside its file meta information, are a
// *NotDICOMError, never a transfer syntax.
func TestReadTransferSyntaxRefusesWhatIsNotDICOM(t *testing.T) {
	_, data := read(t, "MR_small.dcm")
	for name, b := range map[string][]byte{
		"text":         bytes.Repeat([]byte("not dicom "), 40),
		"no prefix":    append(make([]byte, 128), bytes.Repeat([]byte{1}, 200)...),
		"cut in meta":  data[:140],
		"only a start": data[:10],
	} {
		var nd *NotDICOMError
		if got, err := ReadTransferSyntax(bytes.NewReader(b), int64(len(b))); !errors.As(err, &nd) || got != "" {
			t.Errorf("%s: %q, %v, want a NotDICOMError", name, got, err)
		}
	}
}

// bulkJSON returns the file's answer with references, and the references
// that were made as offset and length pairs.
func bulkJSON(t *testing.T, name string, omit bool) (map[string]map[string]any, [][2]int64, []byte) {
	t.Helper()
	h, data := read(t, name)
	var refs [][2]int64
	js, err := h.DICOMJSONBulk(func(o, l int64) string {
		refs = append(refs, [2]int64{o, l})
		return fmt.Sprintf("ref/%d/%d", o, l)
	}, omit)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	return m, refs, data
}

// TestDICOMJSONBulkReferencesPixelData guards that Pixel Data of a native
// file is a reference to its value field, whose bytes are the element's
// stated length, and that of an encapsulated file runs from its first item
// through the Sequence Delimitation Item, which a data set element after it
// does not pass.
func TestDICOMJSONBulkReferencesPixelData(t *testing.T) {
	m, refs, data := bulkJSON(t, "MR_small.dcm", false)
	px := m["7FE00010"]
	if len(refs) != 1 || px["BulkDataURI"] != fmt.Sprintf("ref/%d/%d", refs[0][0], refs[0][1]) || px["Value"] != nil {
		t.Fatalf("Pixel Data %v, references %v, want one reference alone", px, refs)
	}
	if refs[0][1] != 8192 || refs[0][0]+refs[0][1] > int64(len(data)) {
		t.Errorf("the reference %v is not the 8192 bytes of the value in a file of %d", refs[0], len(data))
	}
	m, refs, data = bulkJSON(t, "MR_small_RLE.dcm", false)
	value := data[refs[0][0] : refs[0][0]+refs[0][1]]
	if len(refs) != 1 || !bytes.HasPrefix(value, []byte{0xFE, 0xFF, 0x00, 0xE0}) || !bytes.HasSuffix(value, []byte{0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0}) || m["7FE00010"]["BulkDataURI"] == nil {
		t.Errorf("encapsulated Pixel Data: references %v in a file of %d bytes", refs, len(data))
	}
}

// TestDICOMJSONBulkOmitsWhenAsked guards the deflated case: with omit set
// no reference is made and no attribute that would be bulk data is
// answered, while the rest of the header is.
func TestDICOMJSONBulkOmitsWhenAsked(t *testing.T) {
	m, refs, _ := bulkJSON(t, "MR_small.dcm", true)
	if len(refs) != 0 || m["7FE00010"] != nil || m["00100020"] == nil {
		t.Errorf("references %v, Pixel Data %v, Patient ID %v", refs, m["7FE00010"], m["00100020"])
	}
}
