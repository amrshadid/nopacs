package header

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// privateOB returns an explicit VR little endian OB element (PS3.5 7.1.2)
// of n zero bytes with the tag (0049,10xx).
func privateOB(element byte, n int) []byte {
	// The element numbers used are below 0x100, so the high byte is 0x10.
	b := []byte{0x49, 0x00, element, 0x10, 'O', 'B', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[8:], uint32(n)) // #nosec G115 -- a test's small length.
	return append(b, make([]byte, n)...)
}

// TestDICOMJSONShortLeavesOutPixelDataAndLongValues guards the header a
// person is shown: a binary value of exactly 1 KiB stays inline, one byte
// longer is left out with no BulkDataURI and no InlineBinary, and Pixel Data
// is left out, while ordinary attributes are kept.
func TestDICOMJSONShortLeavesOutPixelDataAndLongValues(t *testing.T) {
	_, data := read(t, "ct2n-6293.dcm")
	at := bytes.Index(data, []byte{0xE0, 0x7F, 0x10, 0x00})
	if at < 0 {
		t.Fatal("no Pixel Data in the corpus file")
	}
	long := append(append(append(append([]byte{}, data[:at]...), privateOB(0x98, ShortValueLimit)...), privateOB(0x99, ShortValueLimit+2)...), data[at:]...)
	h, err := Read(bytes.NewReader(long), int64(len(long)))
	if err != nil {
		t.Fatal(err)
	}
	text, err := h.DICOMJSONShort()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(text, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["00491098"]; !ok {
		t.Error("a value of exactly 1 KiB was left out")
	}
	if _, ok := m["00491099"]; ok {
		t.Error("a value over 1 KiB was kept")
	}
	if _, ok := m["7FE00010"]; ok {
		t.Error("Pixel Data was kept")
	}
	if _, ok := m["00100010"]; !ok {
		t.Error("an ordinary attribute was left out")
	}
	if bytes.Contains(text, []byte("BulkDataURI")) {
		t.Error("the header refers to bulk data")
	}
}
