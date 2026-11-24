package frames

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// Table returns the frame table of the file src holds, whose header h was
// read from it, as a sidecar line keeps it: nil for a file without pixel
// data or whose data set is deflated (whose line has no pixel data, PS3.5
// A.5); an empty table for pixel data whose frames are not byte ranges of
// the file (header.ErrNoFrameRanges); and otherwise each frame's ranges.
// An error is a failure to read src, never a fact of the file.
func Table(h *header.Header, src io.ReaderAt) ([][]sidecar.Range, error) {
	if !h.HasPixelData || h.TransferSyntaxUID == header.TransferSyntaxDeflated {
		return nil, nil
	}
	spans, err := h.Frames(src)
	if errors.Is(err, header.ErrNoFrameRanges) {
		return [][]sidecar.Range{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([][]sidecar.Range, len(spans))
	for i, frame := range spans {
		out[i] = make([]sidecar.Range, len(frame))
		for j, s := range frame {
			out[i][j] = sidecar.Range{Offset: s.Offset, Length: s.Length}
		}
	}
	return out, nil
}

// sumBuffer is the size of the buffer Checksums reads a file through.
const sumBuffer = 256 << 10

// Checksums returns the SHA-256 of each frame of table, in lowercase hex,
// frame 1 first, and the SHA-256 of the whole file, from one read of the
// file r holds from its first byte to its end: each frame's checksum is
// of its ranges' bytes joined in order, which is the frame as stored and
// as a request for it is answered (PS3.18 8.7.3.3.2). A caller compares
// the file's checksum with the one the file was archived under, so the
// frames' checksums are known to be of the bytes archived. The ranges of
// a table come in order and do not overlap (sidecar.Instance.Frames); it
// holds one buffer, never the file. A table whose ranges pass the end of
// the file is io.ErrUnexpectedEOF.
func Checksums(r io.Reader, table [][]sidecar.Range) ([]string, [sha256.Size]byte, error) {
	type span struct {
		frame       int
		start, stop int64
	}
	var spans []span
	for i, f := range table {
		for _, rg := range f {
			spans = append(spans, span{frame: i, start: rg.Offset, stop: rg.Offset + rg.Length})
		}
	}
	sums := make([]hash.Hash, len(table))
	for i := range sums {
		sums[i] = sha256.New()
	}
	file := sha256.New()
	buf := make([]byte, sumBuffer)
	pos := int64(0)
	next := 0
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b := buf[:n]
			_, _ = file.Write(b) // A hash never fails.
			end := pos + int64(n)
			for next < len(spans) && spans[next].start < end {
				s := spans[next]
				from, to := max(s.start, pos), min(s.stop, end)
				if from < to {
					_, _ = sums[s.frame].Write(b[from-pos : to-pos])
				}
				if s.stop > end {
					break
				}
				next++
			}
			pos = end
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, [sha256.Size]byte{}, err
		}
	}
	for next < len(spans) && spans[next].stop <= spans[next].start {
		// A range of no bytes at the very end of the file.
		next++
	}
	if next < len(spans) {
		return nil, [sha256.Size]byte{}, io.ErrUnexpectedEOF
	}
	out := make([]string, len(sums))
	for i, h := range sums {
		out[i] = hex.EncodeToString(h.Sum(nil))
	}
	var whole [sha256.Size]byte
	copy(whole[:], file.Sum(nil))
	return out, whole, nil
}
