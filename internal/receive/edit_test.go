package receive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// setName is a change that gives Patient's Name (0010,0010) v.
func setName(v string) func(ds *dataset.Dataset) error {
	return func(ds *dataset.Dataset) error {
		elem, ok := ds.Get(tag.Tag(0x00100010))
		if !ok {
			return errors.New("the file has no patient's name")
		}
		elem.SetValue([]byte(v))
		return nil
	}
}

// readTestFile reads a public corpus file of testdata.
func readTestFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRewriteFileChangesOnlyWhatItIsAsked guards RewriteFile on public
// files, one native and one of two RLE frames: the file written back is in
// the transfer syntax the file had, its data set holds the new value, its
// pixel data element is the original's byte for byte, and its SOP Instance
// UID is the original's.
func TestRewriteFileChangesOnlyWhatItIsAsked(t *testing.T) {
	for _, name := range []string{"ct2n-6293.dcm", "SC_rgb_rle_2frame.dcm"} {
		file := readTestFile(t, name)
		before, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), 0)
		if err != nil {
			t.Fatal(err)
		}
		out, err := RewriteFile(file, 0, setName("Edited^Name"))
		if err != nil {
			t.Fatalf("%s: RewriteFile = %v", name, err)
		}
		after, err := header.ReadWithin(bytes.NewReader(out), int64(len(out)), 0)
		if err != nil {
			t.Fatalf("%s: the rewritten file cannot be read: %v", name, err)
		}
		if after.TransferSyntaxUID != before.TransferSyntaxUID {
			t.Errorf("%s: the rewritten file is in %s, the original in %s", name, after.TransferSyntaxUID, before.TransferSyntaxUID)
		}
		js, err := after.DICOMJSON()
		if err != nil || !strings.Contains(string(js), `"Edited^Name"`) {
			t.Errorf("%s: the rewritten data set does not hold the new value (%v)", name, err)
		}
		bs, as := before.PixelData, after.PixelData
		if !before.HasPixelData || !after.HasPixelData ||
			!bytes.Equal(file[bs.Offset:bs.Offset+bs.Length], out[as.Offset:as.Offset+as.Length]) {
			t.Errorf("%s: the rewritten file's pixel data element is not the original's", name)
		}
		bu, _ := before.UID(header.SOPInstanceUID)
		au, _ := after.UID(header.SOPInstanceUID)
		if au != bu || au == "" {
			t.Errorf("%s: the rewritten file's SOP Instance UID is %q, the original's %q", name, au, bu)
		}
	}
}

// TestRewriteFileRefusesWhatItCannotRead guards RewriteFile's failures: a
// file that is not DICOM is ErrUnreadable, and an error of the change is
// answered as it is.
func TestRewriteFileRefusesWhatItCannotRead(t *testing.T) {
	if _, err := RewriteFile([]byte("not a DICOM file at all"), 0, setName("x")); !errors.Is(err, ErrUnreadable) {
		t.Errorf("RewriteFile of bytes that are not DICOM = %v, want ErrUnreadable", err)
	}
	stop := errors.New("the change refused")
	if _, err := RewriteFile(readTestFile(t, "ct2n-6293.dcm"), 0, func(*dataset.Dataset) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("RewriteFile whose change failed = %v, want the change's error", err)
	}
}

// implicitLatin1 returns a public CT file (ISO_IR 100, private elements)
// written again in Implicit VR Little Endian with its Specific Character
// Set kept as ISO_IR 100, so its private elements read back as UN.
func implicitLatin1(t *testing.T) []byte {
	t.Helper()
	file := readTestFile(t, "ct2n-6293.dcm")
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), 0)
	if err != nil {
		t.Fatal(err)
	}
	ds, source, little, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	cs, ok := ds.Get(tag.Tag(0x00080005))
	if !ok {
		t.Fatal("the file declares no character set")
	}
	cs.SetValue([]byte("ISO_IR 100"))
	var w memFile
	if err := writeDataset(&w, ds, "1.2.840.10008.1.2", source, little); err != nil {
		t.Fatal(err)
	}
	return w.b
}

// TestCheckRewritableRefusesRawTextUnderAnotherCharacterSet guards the
// files an edit may write again: one in Latin-1 (ISO_IR 100) whose private
// elements read back as UN (an implicit-VR file) is refused, since the
// rewrite would keep their Latin-1 bytes under a UTF-8 declaration; the
// same file explicit, its private elements with their VRs, and a file that
// declares no character set are rewritable.
func TestCheckRewritableRefusesRawTextUnderAnotherCharacterSet(t *testing.T) {
	raw := implicitLatin1(t)
	if h, err := header.ReadWithin(bytes.NewReader(raw), int64(len(raw)), 0); err != nil || *h.CharacterSet() != "ISO_IR 100" ||
		h.TransferSyntaxUID != "1.2.840.10008.1.2" {
		t.Fatalf("the crafted file is not implicit Latin-1 (%v)", err)
	}
	if err := CheckRewritable(raw, 0); !errors.Is(err, ErrRawText) || fault.KindOf(err) != fault.Precondition {
		t.Fatalf("an implicit Latin-1 file with UN elements = %v, want ErrRawText as a precondition failure", err)
	}
	for _, name := range []string{"ct2n-6293.dcm", "MR_small.dcm"} {
		if err := CheckRewritable(readTestFile(t, name), 0); err != nil {
			t.Errorf("%s: %v, want it rewritable", name, err)
		}
	}
}

// TestRawUnderSeesAnItemsCharacterSet guards the walk of a data set's
// items: an element of VR UN inside an item that declares a character set
// of its own (PS3.5 7.5.3) is refused, whatever the file declares, since
// the library reads the item's declaration as UTF-8 and the one it had is
// not known; the same element in an item that declares none, in a file in
// the default repertoire, is not. The data sets are in the form the
// library reads such a file to (DICOM JSON, PS3.18 F.2).
func TestRawUnderSeesAnItemsCharacterSet(t *testing.T) {
	const declared = `{"00100010":{"Value":[{"Alphabetic":"Buc^Jerome"}],"vr":"PN"},` +
		`"00400275":{"Value":[{"00080005":{"Value":["ISO_IR 192"],"vr":"CS"},"00080100":{"Value":["X"],"vr":"SH"},` +
		`"00091011":{"InlineBinary":"SPRwaXRhbCDJbG/vc2U=","vr":"UN"}}],"vr":"SQ"}}`
	const undeclared = `{"00400275":{"Value":[{"00080100":{"Value":["X"],"vr":"SH"},` +
		`"00091011":{"InlineBinary":"QUNNRQ==","vr":"UN"}}],"vr":"SQ"}}`
	for _, c := range []struct {
		name, js string
		other    bool
		want     bool
	}{
		{"an item that declares its own", declared, false, true},
		{"an item that declares none", undeclared, false, false},
		{"an item under the file's other set", undeclared, true, true},
	} {
		var ds map[string]any
		if err := json.Unmarshal([]byte(c.js), &ds); err != nil {
			t.Fatal(err)
		}
		if got := rawUnder(ds, c.other); got != c.want {
			t.Errorf("%s: rawUnder = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEditedLineIsReadFromTheNewBytes guards EditedLine: what a file says
// of itself (size, checksum, transfer syntax, pixel data and each frame's
// checksum, data set) is read from the rewritten bytes, and what only the
// catalog knows (key, revision, received checksum) is the old line's.
func TestEditedLineIsReadFromTheNewBytes(t *testing.T) {
	file := readTestFile(t, "SC_rgb_rle_2frame.dcm")
	out, err := RewriteFile(file, 0, setName("Edited^Name"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := header.ReadWithin(bytes.NewReader(out), int64(len(out)), 0)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out)
	old := sidecar.Instance{SOPInstanceUID: "1.2.3", Key: "data/a/b/c/1.2.3.dcm", Revision: 1, ReceivedSHA256: strings.Repeat("a", 64),
		Size: 1, SHA256: strings.Repeat("b", 64), Dataset: []byte(`{"stale":true}`)}
	line, err := EditedLine(out, old, hex.EncodeToString(sum[:]), 0)
	if err != nil {
		t.Fatal(err)
	}
	if line.Size != int64(len(out)) || line.SHA256 != hex.EncodeToString(sum[:]) || line.TransferSyntax != h.TransferSyntaxUID {
		t.Errorf("the line says size %d, checksum %s, syntax %s of the new bytes", line.Size, line.SHA256, line.TransferSyntax)
	}
	if line.PixelData == nil || line.PixelData.Offset != h.PixelData.Offset || line.PixelData.Length != h.PixelData.Length {
		t.Errorf("the line's pixel data is %+v, the new file's %+v", line.PixelData, h.PixelData)
	}
	if len(line.Frames) != 2 || len(line.FrameSHA256) != 2 {
		t.Errorf("the line names %d frames and %d frame checksums, want 2 of each", len(line.Frames), len(line.FrameSHA256))
	}
	if line.DatasetOmitted || !strings.Contains(string(line.Dataset), `"Edited^Name"`) {
		t.Errorf("the line's data set is not the new file's: %.80s", line.Dataset)
	}
	if line.Key != old.Key || line.Revision != old.Revision || line.ReceivedSHA256 != old.ReceivedSHA256 || line.SOPInstanceUID != old.SOPInstanceUID {
		t.Errorf("the line did not keep what only the catalog knows: %+v", line)
	}
	if _, err := EditedLine([]byte("not a DICOM file"), old, "x", 0); !errors.Is(err, ErrUnreadable) {
		t.Errorf("EditedLine of bytes that are not DICOM = %v, want ErrUnreadable", err)
	}
}
