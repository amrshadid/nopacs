package testkit

import (
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive/header"
)

// Generator makes DICOM files for scale tests and benchmarks from one public
// file with a long-form OW or OB pixel data element, such as pydicom's
// MR_small.dcm: its header as it is, with its SOP Instance UID made
// distinct per file and its pixel data made the length asked for, filled
// with zeros, streamed so a file of any size costs no memory. Where the
// pixel data element is and how long its header is are read by the
// pipeline's header reader (on go-dicom); the bytes are only copied and
// counted.
type Generator struct {
	prefix []byte // up to and including the pixel data element's VR field
	suffix []byte // the elements after the pixel data, as they are
	uid    []byte // the SOP Instance UID as it is in the file
	uidAt  []int  // where it is in prefix
}

// NewGenerator reads file, a whole Part 10 file, and finds what it changes:
// its pixel data element, and its SOP Instance UID in the file meta
// information and in the data set (PS3.10 7.1), whose last four digits
// File replaces.
func NewGenerator(t testing.TB, file []byte) *Generator {
	t.Helper()
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	off := h.PixelData.Offset
	if vr := string(file[off+4 : off+6]); vr != "OW" && vr != "OB" {
		t.Fatalf("the pixel data's VR is %q, not a long-form OW or OB (PS3.5 7.1.2)", vr)
	}
	g := &Generator{prefix: append([]byte{}, file[:off+8]...), suffix: file[off+h.PixelData.Length:]}
	u, _ := h.UID(header.SOPInstanceUID)
	g.uid = []byte(u)
	for i := 0; ; {
		j := bytes.Index(g.prefix[i:], g.uid)
		if j < 0 {
			break
		}
		g.uidAt = append(g.uidAt, i+j)
		i += j + len(g.uid)
	}
	if len(g.uidAt) != 2 || len(g.uid) < 4 {
		t.Fatalf("the SOP Instance UID is in the file %d times, want 2 (meta and data set)", len(g.uidAt))
	}
	return g
}

// File returns the n-th file (0 <= n < 9000) with pixelBytes bytes of pixel
// data (even, below 4 GiB), as a stream, and its size. Files of different n
// have different SOP Instance UIDs.
func (g *Generator) File(n int, pixelBytes int64) (io.Reader, int64) {
	p := append([]byte{}, g.prefix...)
	// The UID's last four digits become 1000+n: same length, so no
	// element's length changes, and no component starts with a zero.
	digits := []byte(strconv.Itoa(1000 + n))
	for _, at := range g.uidAt {
		copy(p[at+len(g.uid)-4:], digits)
	}
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(pixelBytes)) // #nosec G115 -- a test's sizes are far below 4 GiB.
	p = append(p, l[:]...)
	size := int64(len(p)) + pixelBytes + int64(len(g.suffix))
	return io.MultiReader(bytes.NewReader(p), io.LimitReader(Zeros{}, pixelBytes), bytes.NewReader(g.suffix)), size
}

// Zeros yields zero bytes forever.
type Zeros struct{}

// Read fills p with zeros.
func (Zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
