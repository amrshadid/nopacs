package header

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// tagNumberOfFrames is Number of Frames (0028,0008), an IS (PS3.3
// C.7.6.6).
var tagNumberOfFrames = naming.NewTag(0x0028, 0x0008)

// ErrNoFrameRanges is pixel data whose frames cannot be given as byte
// ranges of the file: a deflated data set, whose positions count inflated
// bytes (PS3.5 A.5); a 1-bit image whose frames start inside a byte (PS3.5
// 8.1.1), where a range would hold bits of two frames; or pixel data that
// go-dicom cannot divide into the frames the data set states (an offset
// table that points nowhere, fewer fragments than frames, end-of-image
// markers that do not divide them), or whose Number of Frames is not a
// number. It is a fact of the file, not a failure
// to read it: a reader of such a frame reads the file's header and
// decodes, as it would without a table.
var ErrNoFrameRanges = errors.New("header: the frames of the pixel data are not byte ranges of the file")

// Frames returns where each frame of the file's pixel data lies in the file
// src holds, the file the header was read from: frame i is the bytes of
// result[i], in order, joined. For native pixel data that is one range a
// frame, Rows x Columns x Samples per Pixel samples of Bits Allocated each,
// in the file's byte order; for encapsulated pixel data (PS3.5 A.4) it is
// the content of each fragment of the frame, without the item headers
// between them, which is what PS3.18 8.7.3.3.2 answers for a frame as
// stored. go-dicom decides the division (filereader.DICOMFile.FrameRanges):
// the Extended Offset Table (7FE0,0001), then the Basic Offset Table, then
// the count of fragments against Number of Frames, then the end-of-image
// markers. It reads from src only what that needs: the offset tables, and
// the last bytes of a fragment when only a marker can say where a frame
// ends.
//
// A file without pixel data, or whose frames are not byte ranges of the
// file, is ErrNoFrameRanges; any other error is a failure to read src.
func (h *Header) Frames(src io.ReaderAt) ([][]Span, error) {
	if !h.HasPixelData || h.file.Deflated {
		return nil, ErrNoFrameRanges
	}
	pixels := h.file.PixelDataElement()
	if pixels == nil {
		return nil, ErrNoFrameRanges
	}
	// go-dicom reads a Number of Frames that is not a number (an IS holds
	// an integer, PS3.5 6.2) as one frame, and would then give the first
	// frame's bytes for a value that holds several: such pixel data has no
	// division to record.
	if v, ok := h.Lookup(tagNumberOfFrames); ok && v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			return nil, ErrNoFrameRanges
		}
	}
	r := &failRecorder{r: src}
	ranges, err := h.file.FrameRanges(r)
	if err != nil {
		if rerr := r.failure(); rerr != nil {
			return nil, fmt.Errorf("header: reading the pixel data's items: %w", rerr)
		}
		// go-dicom's text may name lengths and offsets of the file, which
		// are not values; it is still not kept, as no other text of the
		// library is.
		return nil, ErrNoFrameRanges
	}
	if !pixels.UndefinedLength && len(ranges) > 1 {
		if bit, err := h.file.FrameFirstBit(1); err != nil || bit != 0 {
			return nil, ErrNoFrameRanges
		}
	}
	out := make([][]Span, len(ranges))
	for i, frame := range ranges {
		if len(frame) == 0 {
			return nil, ErrNoFrameRanges
		}
		out[i] = make([]Span, len(frame))
		for j, part := range frame {
			out[i][j] = Span{Offset: part.Offset, Length: part.Length}
		}
	}
	if len(out) == 0 {
		return nil, ErrNoFrameRanges
	}
	return out, nil
}

// failRecorder keeps the first error of the reader beneath that is not the
// end of the file, so Frames can tell a storage that failed from pixel data
// go-dicom cannot divide, whose error is its own.
type failRecorder struct {
	r   io.ReaderAt
	mu  sync.Mutex
	err error
}

// ReadAt implements io.ReaderAt.
func (f *failRecorder) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		f.mu.Lock()
		if f.err == nil {
			f.err = err
		}
		f.mu.Unlock()
	}
	return n, err
}

// failure returns the first error kept.
func (f *failRecorder) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
