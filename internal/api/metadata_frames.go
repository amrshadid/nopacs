package api

import (
	"context"
	"errors"
	"io"

	"github.com/amrshadid/nopacs/internal/blob/readat"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The messages of a frame that cannot be answered by range.
const (
	messageNoPixels    = "the instance has no pixel data"
	messageNoFrame     = "the instance has no frame with this number"
	messageNotByRanges = "the instance's frames are not byte ranges of its file; a frame is read from its decoded pixel data"
)

// StoredFrame is where one frame of an instance lies in its file on the storage,
// for a route that answers frames (WADO-RS, PS3.18 10.4.1.1.4) to read it
// by range without reading the file's header.
type StoredFrame struct {
	// TransferSyntax is the file's Transfer Syntax UID, which says what the
	// bytes are: a compressed frame as its syntax codes it (PS3.18
	// 8.7.3.3.2), or native samples in the file's byte order.
	TransferSyntax string
	// Count is how many frames the instance has.
	Count int
	// Ranges are the frame's byte ranges in the file, which joined in order
	// are the frame as stored: for encapsulated pixel data (PS3.5 A.4) the
	// contents of its fragments, without the item headers between them.
	Ranges []sidecar.Range
	// SHA256 is the frame's checksum as the line records it, empty for a
	// frame found from the file's header because the line holds no table.
	SHA256 string
}

// storedFrames is where every frame of an instance lies in its file, as
// frameOf reads it once for a request of several frames.
type storedFrames struct {
	transferSyntax string
	table          [][]sidecar.Range
	// sums is each frame's checksum, nil for a table read from the file.
	sums []string
}

// frame answers where frame n of the instance held lies in its file,
// counted from 1 as a request names frames. The scan's metadata file
// answers it when the instance's line holds a frame table (recorded when
// it was archived, or given by the pass for scans archived before); for a
// line written before tables existed it is read from the instance's file
// by bounded ranged reads, its header and its pixel data's item headers,
// never the whole file. An instance without pixel data, or n not one of
// its frames, is NotFound; frames that are not byte ranges of the file (a
// deflated data set, a 1-bit image whose frames start inside a byte, pixel
// data go-dicom cannot divide) are a precondition failure, for the route
// to answer from the decoded pixel data instead.
func (s *metaSource) frame(ctx context.Context, held tenant.ScanInstance, n int) (StoredFrame, error) {
	_, rc, in, err := s.lineOf(ctx, held.SOPInstanceUID, nil)
	if err != nil {
		return StoredFrame{}, err
	}
	_ = rc.Close()
	return s.frameOf(ctx, held, in, n)
}

// frameOf answers frame n of the instance held from its line of the scan's
// metadata file, in, or from its file when the line holds no table
// (framesOf).
func (s *metaSource) frameOf(ctx context.Context, held tenant.ScanInstance, in sidecar.Instance, n int) (StoredFrame, error) {
	all, err := s.framesOf(ctx, held, in)
	if err != nil {
		return StoredFrame{}, err
	}
	return all.frame(n)
}

// frame returns frame n, counted from 1; n not one of the frames is
// NotFound.
func (f storedFrames) frame(n int) (StoredFrame, error) {
	if n < 1 || n > len(f.table) {
		return StoredFrame{}, fault.New(fault.NotFound, messageNoFrame)
	}
	out := StoredFrame{TransferSyntax: f.transferSyntax, Count: len(f.table), Ranges: f.table[n-1]}
	if f.sums != nil {
		out.SHA256 = f.sums[n-1]
	}
	return out, nil
}

// framesOf answers where every frame of the instance held lies in its file,
// from its line of the scan's metadata file, in, or from its file when the
// line holds no table: its header and its pixel data's item headers, read
// once however many frames a request names. The line must be the
// instance's: the same key, size and archived checksum the catalog holds
// (of the bytes stored, which for a de-identified instance are not those
// received). An instance without pixel data is NotFound; frames that are
// not byte ranges of the file are a precondition failure.
func (s *metaSource) framesOf(ctx context.Context, held tenant.ScanInstance, in sidecar.Instance) (storedFrames, error) {
	if in.Key != held.Key || in.Size != held.Size || (!held.ArchivedSHA256.IsZero() && in.SHA256 != held.ArchivedSHA256.String()) {
		return storedFrames{}, fault.New(fault.DataLoss, messageBadSidecar)
	}
	// A frame table counts from the instance's first byte (package
	// sidecar), and so is the file read here: the instance's window of its
	// object, which is the whole object while each instance is one.
	size := held.Length
	ra := io.NewSectionReader(readat.New(ctx, s.store, held.Key, held.Offset+size), held.Offset, size)
	ts, table, sums := in.TransferSyntax, in.Frames, in.FrameSHA256
	if in.PixelData == nil {
		// No pixel data in the line: none in the file, or a deflated file,
		// whose pixel data has no place in the file (PS3.5 A.5).
		if ts == "" {
			var err error
			if ts, err = header.ReadTransferSyntax(ra, size); err != nil {
				return storedFrames{}, fileFailure(err)
			}
		}
		if ts == transferSyntaxDeflated {
			return storedFrames{}, fault.New(fault.Precondition, messageNotByRanges)
		}
		return storedFrames{}, fault.New(fault.NotFound, messageNoPixels)
	}
	if table == nil {
		h, err := header.ReadWithin(ra, size, s.limit)
		if err != nil {
			var tooLarge *header.TooLargeError
			if errors.As(err, &tooLarge) {
				return storedFrames{}, fault.New(fault.Precondition, messageNotByRanges)
			}
			return storedFrames{}, fileFailure(err)
		}
		ts, sums = h.TransferSyntaxUID, nil
		if table, err = frames.Table(h, ra); err != nil {
			return storedFrames{}, fileFailure(err)
		}
	}
	if len(table) == 0 {
		return storedFrames{}, fault.New(fault.Precondition, messageNotByRanges)
	}
	if ts == "" {
		var err error
		if ts, err = header.ReadTransferSyntax(ra, size); err != nil {
			return storedFrames{}, fileFailure(err)
		}
	}
	return storedFrames{transferSyntax: ts, table: table, sums: sums}, nil
}
