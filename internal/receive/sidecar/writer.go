package sidecar

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Writer writes a sidecar as a stream.
type Writer struct {
	w     *bufio.Writer
	last  string
	count int
	err   error
	// version is the version written: Version with a scan header,
	// VersionFrames, or 1 for a merge into a sidecar of version 1 (Merge).
	version int
	// scan is the header of a sidecar of version 3.
	scan ScanHeader
	// off counts the bytes written; idx holds the index entries so far.
	off int64
	idx []IndexEntry
}

// NewWriter returns a writer of a sidecar of version VersionFrames to w: the
// form without the scan's header, for a caller that does not know the
// catalog's values of the scan. A scan that is archived is written with
// NewWriterV3.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64<<10), version: VersionFrames}
}

// NewWriterV3 returns a writer of a sidecar of version 3 to w, with the scan
// header h. A header that is not valid is ErrFormat from the first Add, and
// from Close.
func NewWriterV3(w io.Writer, h ScanHeader) *Writer {
	wr := &Writer{w: bufio.NewWriterSize(w, 64<<10), version: Version, scan: h}
	if err := h.Validate(); err != nil {
		wr.err = err
	}
	return wr
}

// Add writes one instance's line. Instances must come in increasing order of
// SOP Instance UID (byte order), each once. A data set longer than
// MaxDataset is omitted, and a frame table of more than MaxFrameRanges
// ranges is left empty. An instance with pixel data and no frame table is
// ErrIncomplete.
func (w *Writer) Add(in Instance) error {
	if w.err != nil {
		return w.err
	}
	if len(in.Dataset) > MaxDataset {
		in.Dataset, in.DatasetOmitted = nil, true
	}
	if in.Dataset == nil {
		in.DatasetOmitted = true
	}
	if frameRanges(in.Frames) > MaxFrameRanges {
		in.Frames, in.FrameSHA256 = [][]Range{}, nil
	}
	if err := check(in, w.version); err != nil {
		return err
	}
	if in.NeedsFrames() {
		return ErrIncomplete
	}
	if w.count > 0 && in.SOPInstanceUID <= w.last {
		return ErrOrder
	}
	line, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%w: an instance cannot be encoded", ErrFormat)
	}
	return w.addLine(in.SOPInstanceUID, line)
}

// addLine writes an encoded line.
func (w *Writer) addLine(sopUID string, line []byte) error {
	if w.count == 0 {
		switch w.version {
		case 1:
			w.write([]byte(headerV1))
		case VersionFrames:
			w.write([]byte(header))
		default:
			enc, err := json.Marshal(w.scan)
			if err != nil {
				w.err = fmt.Errorf("%w: the scan's header cannot be encoded", ErrFormat)
				return w.err
			}
			w.write([]byte(headV3 + string(enc) + tailV3 + "\n"))
		}
	} else {
		w.write([]byte(",\n"))
	}
	if w.count%IndexEvery == 0 {
		w.idx = append(w.idx, IndexEntry{UID: sopUID, Offset: w.off})
	}
	w.write(line)
	w.last = sopUID
	w.count++
	return w.err
}

// write writes b, keeping the first error.
func (w *Writer) write(b []byte) {
	if w.err == nil {
		var n int
		n, w.err = w.w.Write(b)
		w.off += int64(n)
	}
}

// Close ends the sidecar and flushes it. A sidecar with no instance is an
// error: a scan is made with what it holds.
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.count == 0 {
		return fmt.Errorf("%w: no instances", ErrFormat)
	}
	w.write([]byte("\n" + footer))
	if len(w.idx) > 1 {
		line, err := json.Marshal(indexLine{Index: w.idx})
		if err != nil {
			return fmt.Errorf("%w: the index cannot be encoded", ErrFormat)
		}
		w.write(line)
		w.write([]byte("\n"))
	}
	if w.err == nil {
		w.err = w.w.Flush()
	}
	return w.err
}
