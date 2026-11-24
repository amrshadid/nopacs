package header

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// failingAt is a file whose reads fail from an offset on, as a storage
// that stops answering midway does.
type failingAt struct {
	data []byte
	from int64
}

var errStorage = errors.New("the storage stopped answering")

func (f failingAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.from {
		return 0, errStorage
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

// TestFramesOfEncapsulatedFiles guards the frame ranges of encapsulated
// pixel data: each frame's ranges are the contents of its fragments,
// exactly the bytes after each item header (FFFE,E000) and its length, for
// one frame in one fragment (RLE, JPEG 2000), two frames with a Basic
// Offset Table (RLE), and three frames of three fragments each with an
// empty Basic Offset Table, where only the end-of-image markers divide
// them.
func TestFramesOfEncapsulatedFiles(t *testing.T) {
	for _, c := range []struct {
		name               string
		frames, perFrame   int
		jpegStart, jpegEnd bool
	}{
		{"MR_small_RLE.dcm", 1, 1, false, false},
		{"MR_small_jp2klossless.dcm", 1, 1, false, false},
		{"SC_rgb_rle_2frame.dcm", 2, 1, false, false},
		{"jpeg_3frame_nobot.dcm", 3, 3, true, true},
	} {
		h, data := read(t, c.name)
		frames, err := h.Frames(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(frames) != c.frames {
			t.Fatalf("%s: %d frames, want %d", c.name, len(frames), c.frames)
		}
		for i, f := range frames {
			if len(f) != c.perFrame {
				t.Errorf("%s frame %d: %d ranges, want %d", c.name, i+1, len(f), c.perFrame)
			}
			for _, r := range f {
				head := data[r.Offset-8:]
				if binary.LittleEndian.Uint16(head) != 0xFFFE || binary.LittleEndian.Uint16(head[2:]) != 0xE000 ||
					int64(binary.LittleEndian.Uint32(head[4:])) != r.Length {
					t.Errorf("%s frame %d: range %+v does not follow an item header of its length", c.name, i+1, r)
				}
				if r.Offset < h.PixelData.Offset || r.Offset+r.Length > h.PixelData.Offset+h.PixelData.Length {
					t.Errorf("%s frame %d: range %+v outside the pixel data", c.name, i+1, r)
				}
			}
			first, last := f[0], f[len(f)-1]
			if c.jpegStart && !bytes.HasPrefix(data[first.Offset:], []byte{0xFF, 0xD8}) {
				t.Errorf("%s frame %d does not start with a JPEG start-of-image marker", c.name, i+1)
			}
			tail := bytes.TrimSuffix(data[last.Offset:last.Offset+last.Length], []byte{0})
			if c.jpegEnd && !bytes.HasSuffix(tail, []byte{0xFF, 0xD9}) {
				t.Errorf("%s frame %d does not end with a JPEG end-of-image marker", c.name, i+1)
			}
		}
	}
}

// TestFramesOfNativeFiles guards native pixel data: one range a frame,
// starting at the value's first byte, Rows x Columns x Samples x Bits
// Allocated / 8 long (64 x 64 x 16 bits for MR_small).
func TestFramesOfNativeFiles(t *testing.T) {
	h, data := read(t, "MR_small.dcm")
	frames, err := h.Frames(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := Span{Offset: h.PixelData.Offset + 12, Length: 64 * 64 * 2}
	if len(frames) != 1 || len(frames[0]) != 1 || frames[0][0] != want {
		t.Fatalf("frames %+v, want one of %+v", frames, want)
	}
}

// TestFramesTellAFailureFromAFact guards that a storage failing while the
// offset table is read is returned as that failure, which a caller tries
// again, and never as ErrNoFrameRanges, which a caller records for good.
func TestFramesTellAFailureFromAFact(t *testing.T) {
	h, data := read(t, "SC_rgb_rle_2frame.dcm")
	_, err := h.Frames(failingAt{data: data, from: h.PixelData.Offset})
	if !errors.Is(err, errStorage) || errors.Is(err, ErrNoFrameRanges) {
		t.Fatalf("a failing storage gave %v", err)
	}
	h, data = read(t, "ct5n-2062.dcm")
	h.HasPixelData = false
	if _, err := h.Frames(bytes.NewReader(data)); !errors.Is(err, ErrNoFrameRanges) {
		t.Errorf("no pixel data gave %v", err)
	}
}

// TestFramesRefuseANumberOfFramesThatIsNot guards a Number of Frames that
// is not a number (pydicom's badVR.dcm has "1A"): go-dicom reads it as one
// frame and would give the first frame's bytes for a value that holds two,
// so the file has no frame ranges, as pydicom also cannot divide it.
func TestFramesRefuseANumberOfFramesThatIsNot(t *testing.T) {
	h, data := read(t, "SC_rgb_rle_2frame.dcm")
	if _, err := h.Frames(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	element := []byte{0x28, 0x00, 0x08, 0x00, 'I', 'S', 0x02, 0x00}
	at := bytes.Index(data, element)
	if at < 0 {
		t.Fatal("the file has no Number of Frames of two bytes")
	}
	bad := bytes.Clone(data)
	copy(bad[at+len(element):], "1A")
	h, err := Read(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Frames(bytes.NewReader(bad)); !errors.Is(err, ErrNoFrameRanges) {
		t.Fatalf("a Number of Frames of 1A gave %v", err)
	}
}
