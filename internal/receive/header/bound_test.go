package header

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
)

// virtual is a file made of parts, read at any offset without holding its
// zeros in memory.
type virtual struct {
	parts [][]byte
	zeros int64 // after parts[0]
	size  int64
}

func newVirtual(head []byte, zeros int64, tail []byte) *virtual {
	return &virtual{parts: [][]byte{head, tail}, zeros: zeros, size: int64(len(head)) + zeros + int64(len(tail))}
}

func (v *virtual) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) && off < v.size {
		head, tail := int64(len(v.parts[0])), int64(len(v.parts[0]))+v.zeros
		var c int
		switch {
		case off < head:
			c = copy(p[n:], v.parts[0][off:])
		case off < tail:
			c = int(min(int64(len(p)-n), tail-off))
			clear(p[n : n+c])
		default:
			c = copy(p[n:], v.parts[1][off-tail:])
		}
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// mrSmall returns MR_small.dcm and where its data set and pixel data start,
// as go-dicom reads them.
func mrSmall(t *testing.T) (b []byte, dataSet int64, h *Header) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	df, err := filereader.ReadDICOMFile(filebase.NewFileReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatal(err)
	}
	h, err = Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return b, df.DataSetOffset, h
}

// TestHeaderOverTheBoundIsRefusedWithoutAllocating guards the bound: a file
// whose first element after the meta information declares a text value of
// 1 GiB, with the bytes there, is refused as a header too large while the
// live heap stays under 64 MiB, where go-dicom alone would allocate the
// value whole.
func TestHeaderOverTheBoundIsRefusedWithoutAllocating(t *testing.T) {
	b, ds, _ := mrSmall(t)
	head := append([]byte{}, b[:ds]...)
	// (0010,4000) written with the explicit VR UT, whose length is four
	// bytes (PS3.5 7.1.2), declaring 1 GiB.
	el := []byte{0x10, 0x00, 0x00, 0x40, 'U', 'T', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(el[8:], 1<<30)
	head = append(head, el...)
	v := newVirtual(head, 1<<30, nil)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Read(v, v.size)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("reading the header allocated %d MiB", grew>>20)
	}
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Limit != MaxHeader {
		t.Fatalf("got %v, want a TooLargeError", err)
	}
}

// TestABoundRaisedReadsALargerHeader guards ReadWithin: a header of 2 MiB
// (a text value that long before the pixel data) is refused within a bound
// of 1 MiB, with that bound in the error, and read within one of 4 MiB.
func TestABoundRaisedReadsALargerHeader(t *testing.T) {
	b, ds, _ := mrSmall(t)
	head := append([]byte{}, b[:ds]...)
	const long = 2 << 20
	// (0010,4000) Patient Comments, UT with a four-byte length (PS3.5
	// 7.1.2), long spaces: a value the reader keeps in the header.
	el := []byte{0x10, 0x00, 0x00, 0x40, 'U', 'T', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(el[8:], long)
	head = append(head, el...)
	head = append(head, bytes.Repeat([]byte{' '}, long)...)
	head = append(head, b[ds:]...)
	_, err := ReadWithin(bytes.NewReader(head), int64(len(head)), 1<<20)
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Limit != 1<<20 {
		t.Fatalf("within 1 MiB: %v, want a TooLargeError of that bound", err)
	}
	h, err := ReadWithin(bytes.NewReader(head), int64(len(head)), 4<<20)
	if err != nil {
		t.Fatalf("within 4 MiB: %v", err)
	}
	if _, ok := h.UID(SOPInstanceUID); !ok {
		t.Fatal("the header read within the raised bound has no SOP Instance UID")
	}
}

// TestPixelDataPastTheBoundIsRead guards that the bound refuses only a large
// header: a file whose pixel data runs far past MaxHeader is read, with its
// pixel data where it is.
func TestPixelDataPastTheBoundIsRead(t *testing.T) {
	b, _, h0 := mrSmall(t)
	off := h0.PixelData.Offset
	head := append([]byte{}, b[:off+12]...)
	pixels := int64(MaxHeader + 4<<20)
	binary.LittleEndian.PutUint32(head[off+8:], uint32(pixels))
	v := newVirtual(head, pixels, b[off+h0.PixelData.Length:])
	h, err := Read(v, v.size)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasPixelData || h.PixelData.Offset != off || h.PixelData.Length != 12+pixels {
		t.Fatalf("pixel data %+v", h.PixelData)
	}
}

// TestAValueAfterThePixelDataIsHeldToTheBound guards the elements after a
// pixel data longer than the bound: the file is then read whole, and a text
// value of 2 MiB after the pixel data is refused within a bound of 1 MiB, as
// it would be before the pixel data, rather than held in memory.
func TestAValueAfterThePixelDataIsHeldToTheBound(t *testing.T) {
	b, _, h0 := mrSmall(t)
	off := h0.PixelData.Offset
	head := append([]byte{}, b[:off+12]...)
	const pixels = 2 << 20
	binary.LittleEndian.PutUint32(head[off+8:], pixels)
	const long = 2 << 20
	// (7FE1,1001), after the pixel data in tag order, written with the
	// explicit VR UT and its four-byte length (PS3.5 7.1.2).
	tail := []byte{0xE1, 0x7F, 0x01, 0x10, 'U', 'T', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(tail[8:], long)
	tail = append(tail, bytes.Repeat([]byte{' '}, long)...)
	v := newVirtual(head, pixels, tail)
	_, err := ReadWithin(v, v.size, 1<<20)
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Limit != 1<<20 {
		t.Fatalf("got %v, want a TooLargeError of 1 MiB", err)
	}
}

// The child's environment: set, the test reads damaged files and exits.
const quietChildEnv = "NOPACS_HEADER_QUIET_CHILD"

// TestQuietChild reads a text file and a cut DICOM file, which go-dicom
// would log about. Run alone, it does nothing.
func TestQuietChild(t *testing.T) {
	if os.Getenv(quietChildEnv) == "" {
		t.Skip("the child of TestNothingIsWrittenToStderr")
	}
	text := []byte("this is not a DICOM file, only some words\n")
	_, _ = Read(bytes.NewReader(text), int64(len(text)))
	cut, err := os.ReadFile(filepath.Join("..", "testdata", "MR_truncated.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = Read(bytes.NewReader(cut), int64(len(cut)))
}

// TestNothingIsWrittenToStderr guards that reading damaged files writes
// nothing to the process's standard error: go-dicom's log lines about them
// hold bytes of the file.
func TestNothingIsWrittenToStderr(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestQuietChild$", "-test.count=1") // #nosec G204 G702 -- the test binary itself.
	cmd.Env = append(os.Environ(), quietChildEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("reading damaged files wrote to standard error:\n%s", stderr.String())
	}
}
