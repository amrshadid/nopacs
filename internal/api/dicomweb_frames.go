package api

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// frames answers frames of an instance (PS3.18 10.4.1.1.4) as they are
// stored: each frame one part of a multipart body, its bytes the ranges of
// the stored file its frame table names (metaSource.framesOf), read by
// ranged reads and joined, never the whole file and never decoded, and
// checked against the frame's SHA-256 as it is sent when the line records
// one: a frame whose bytes do not match is cut before its last piece, as a
// whole instance is. The table is found once for the request, under the
// caller's open read, however many frames it names; a list of more frames
// than the instance has is refused. Frames that are not byte ranges of the
// file (a deflated data set, 1-bit pixels that start inside a byte) can be
// answered only decoded, which is not served: 406. An instance without
// pixel data, or a frame number past its frames, is 404.
func (x *webRequest) frames(ctx context.Context) error {
	if err := x.holdRead(); err != nil {
		return x.fail(err)
	}
	t, err := x.locate(ctx)
	if err != nil {
		return x.fail(err)
	}
	if err := x.marked(ctx, t); err != nil {
		return x.fail(err)
	}
	in := *t.instance
	ls := x.newLines()
	defer ls.closeAll()
	sl, err := ls.of(ctx, t.scan)
	if err != nil {
		return x.fail(err)
	}
	line, err := sl.line(ctx, in.SOPInstanceUID)
	if err != nil {
		return x.fail(err)
	}
	ts, plan, err := sl.src.planFrames(ctx, in, line, x.res.FrameNumbers)
	if perr := (*dicomweb.Error)(nil); errors.As(err, &perr) {
		return x.refuse(perr)
	}
	if fault.KindOf(err) == fault.Precondition {
		return x.refuse(&dicomweb.Error{Status: http.StatusNotAcceptable, Message: fault.Message(err)})
	}
	if err != nil {
		return x.fail(err)
	}
	media, syntax, perr := dicomweb.AcceptFrames(x.r, ts)
	if perr != nil {
		return x.refuse(perr)
	}
	var mp *dicomweb.Multipart
	stream, stop, err := x.openStream(ctx, func() {
		x.w.Header().Set("Content-Type", mp.ContentType())
		x.w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	mp = dicomweb.NewMultipart(stream, media)
	if x.r.Method == http.MethodHead {
		return stream.end(nil, 0)
	}
	ctx = stream.ctx
	longest := int64(1)
	for _, p := range plan {
		longest = max(longest, p.length)
	}
	chunk := min(longest, httpChunk)
	bufs := [2][]byte{make([]byte, chunk), make([]byte, chunk)}
	for i, p := range plan {
		var part io.Writer
		open := func() (io.Writer, error) {
			if part == nil {
				w, err := mp.Part(media, syntax)
				if err != nil {
					return nil, err
				}
				part = w
			}
			return part, nil
		}
		// The table counts from the instance's first byte, which lies at
		// its offset in the object at its key.
		src := sl.src.file(in)
		body := &rangesReader{ctx: ctx, src: src, base: in.Offset, ranges: p.ranges}
		_, err := checkedread.CopyChecked(body, p.length, p.sum, bufs,
			func(b []byte) error {
				w, err := open()
				if err != nil {
					return err
				}
				_, err = w.Write(b)
				return err
			})
		_ = body.Close()
		if err != nil {
			x.c.mismatched(ctx, src.Mismatch, err)
			return stream.end(instanceFailure(err, pathOf(t.scan)), i)
		}
		if part == nil {
			// A frame of no bytes still has its part.
			if _, err := open(); err != nil {
				return stream.end(err, i)
			}
		}
	}
	if err := mp.Close(); err != nil {
		return stream.end(err, len(plan))
	}
	return stream.end(nil, len(plan))
}

// plannedFrame is one frame a request names, as it is read: its ranges of
// the instance's file, how many bytes they hold, and its checksum, nil for
// a frame whose line records none.
type plannedFrame struct {
	ranges []sidecar.Range
	length int64
	sum    *tenant.SHA256
}

// planFrames returns the Transfer Syntax UID of the instance held and the
// frames numbers names, in order, from its frame table found once
// (framesOf). A list of more frames than the instance has is refused with
// 400; a number past its frames is NotFound; a range outside the instance,
// or a checksum not in its form, is data loss.
func (s *metaSource) planFrames(ctx context.Context, held tenant.ScanInstance, line sidecar.Instance, numbers []int) (string, []plannedFrame, error) {
	all, err := s.framesOf(ctx, held, line)
	if err != nil {
		return "", nil, err
	}
	if len(numbers) > len(all.table) {
		return "", nil, &dicomweb.Error{Status: http.StatusBadRequest,
			Message: "the request names " + strconv.Itoa(len(numbers)) + " frames; the instance has " + strconv.Itoa(len(all.table))}
	}
	plan := make([]plannedFrame, 0, len(numbers))
	for _, n := range numbers {
		f, err := all.frame(n)
		if err != nil {
			return "", nil, err
		}
		p := plannedFrame{ranges: f.Ranges}
		for _, r := range f.Ranges {
			if r.Offset < 0 || r.Length < 0 || r.Offset+r.Length > held.Length {
				return "", nil, fault.New(fault.DataLoss, messageBadSidecar)
			}
			p.length += r.Length
		}
		if f.SHA256 != "" {
			sum, err := parseSHA256(f.SHA256)
			if err != nil {
				return "", nil, fault.New(fault.DataLoss, messageBadSidecar)
			}
			p.sum = &sum
		}
		plan = append(plan, p)
	}
	return all.transferSyntax, plan, nil
}

// rangesReader reads the ranges of an instance's file, each from base, one
// after another as one stream: a frame's bytes, the contents of its
// fragments without the item headers between them. Each range is opened
// only when the one before has ended, so one read is open at a time, and
// every range from the storage the file's first read chose.
type rangesReader struct {
	ctx    context.Context
	src    instanceFile
	base   int64
	ranges []sidecar.Range
	cur    io.ReadCloser
}

// Read reads the next bytes of the ranges.
func (r *rangesReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.ranges) == 0 {
				return 0, io.EOF
			}
			rg := r.ranges[0]
			r.ranges = r.ranges[1:]
			if rg.Length == 0 {
				continue
			}
			body, err := r.src.Read(r.ctx, r.base+rg.Offset, rg.Length)
			if err != nil {
				return 0, err
			}
			// A storage that answers more than the range asked is cut to
			// it; one that answers fewer leaves the frame short, which
			// checkedread.CopyChecked reports.
			r.cur = struct {
				io.Reader
				io.Closer
			}{io.LimitReader(body, rg.Length), body}
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

// Close closes the range open.
func (r *rangesReader) Close() error {
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil
		return err
	}
	return nil
}

// parseSHA256 reads a checksum written as 64 lowercase hex digits.
func parseSHA256(s string) (tenant.SHA256, error) {
	var out tenant.SHA256
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(out) {
		return out, errors.New("api: not a SHA-256 in hex")
	}
	copy(out[:], b)
	return out, nil
}
