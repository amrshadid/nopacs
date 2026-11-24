package header

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestIdentifyFromAPrefix guards what a stream source reads before it
// decides where a file goes: for every corpus file, the whole file gives
// the same four UIDs and transfer syntax as Read, and complete; every
// prefix cut at any of a spread of points gives each UID either as Read
// gives it or empty, never a value cut short, and calls itself complete
// only once its data set was read past (0020,000E), when all four are
// there.
func TestIdentifyFromAPrefix(t *testing.T) {
	names, _ := filepath.Glob(filepath.Join("..", "testdata", "*.dcm"))
	for _, p := range names {
		name := filepath.Base(p)
		if name == "MR_truncated.dcm" {
			continue
		}
		h, data := read(t, name)
		want := Identity{TransferSyntaxUID: h.TransferSyntaxUID, Complete: true}
		want.SOPClassUID, _ = h.UID(SOPClassUID)
		want.SOPInstanceUID, _ = h.UID(SOPInstanceUID)
		want.StudyUID, _ = h.UID(StudyUID)
		want.SeriesUID, _ = h.UID(SeriesUID)
		got, err := Identify(data, true)
		if err != nil || got != want {
			t.Errorf("%s whole: %+v, %v; want %+v", name, got, err, want)
		}
		for cut := 132; cut < len(data); cut += 1 + cut/3 {
			got, err := Identify(data[:cut], false)
			if err != nil {
				// A cut inside the file meta information leaves no transfer
				// syntax to read the data set by.
				if !errors.Is(err, ErrNotDICOM) {
					t.Errorf("%s cut at %d: %v", name, cut, err)
				}
				continue
			}
			for _, c := range []struct{ got, want string }{
				{got.SOPClassUID, want.SOPClassUID}, {got.SOPInstanceUID, want.SOPInstanceUID},
				{got.StudyUID, want.StudyUID}, {got.SeriesUID, want.SeriesUID},
			} {
				if c.got != "" && c.got != c.want {
					t.Errorf("%s cut at %d: a UID read as %q, want %q or nothing", name, cut, c.got, c.want)
				}
			}
			if got.Complete && got != want {
				t.Errorf("%s cut at %d: complete with %+v, want %+v", name, cut, got, want)
			}
		}
	}
}

// TestIdentifyRefusesWhatIsNotDICOM guards that bytes without the preamble
// and DICM prefix are refused as not DICOM, whether whole or cut.
func TestIdentifyRefusesWhatIsNotDICOM(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("not a DICOM file at all"), make([]byte, 4096)} {
		for _, whole := range []bool{true, false} {
			if _, err := Identify(b, whole); !errors.Is(err, ErrNotDICOM) {
				t.Errorf("Identify(%d bytes, whole %v) = %v, want ErrNotDICOM", len(b), whole, err)
			}
		}
	}
}
