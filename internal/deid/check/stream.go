package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// window is the bytes of a stream (the raw bytes of the file, or the text
// of a long value) read and searched at a time, so a check holds a window
// and not the file.
const window = 1 << 20

// edgeRoom is the bytes kept on each side of a match so its edges can be
// judged inside the window that reports it: edgesFit reads at most a rune
// and one byte past a match (dateEdge), five bytes.
const edgeRoom = 8

// readerBuffer is the bytes the reader of the file's elements reads at a
// time: the library reads a tag, a length and a value by separate calls.
const readerBuffer = 64 << 10

// scanStream searches r for the patterns of m in windows of size bytes.
// Each window begins with the end of the one before, the longest pattern
// and edgeRoom more, so a match across the edge between two windows lies
// whole in the second; each match is reported once, by the window in which
// it ends, and only when the bytes on both sides of it that its edges are
// judged by are in that window too (edgeRoom). found is told the window,
// the pattern, the match's start and end in the window, and the stream's
// offset of the window's first byte. prepare, when not nil, is called with each window before it
// is searched, with the start of the bytes new in it and the range of ends
// it reports, [from, to). buf is the room the windows are read into, grown
// when it is too small; it is returned for the next stream.
//
// The work is the automaton's steps over each byte, once over the bytes of
// a window and again over the overlap, plus a unit of *budget for each
// pattern reported, as matcher.scan's; past the budget it returns
// errBudget and how far into the stream it had read.
func scanStream(ctx context.Context, m *matcher, r io.Reader, size int, buf []byte, budget *int64,
	prepare func(win []byte, fresh, from, to int, base int64), found func(win []byte, pat, start, end int, base int64),
) ([]byte, int64, error) {
	keep := m.longest + edgeRoom
	if need := max(size, 1) + keep + edgeRoom; cap(buf) < need {
		buf = make([]byte, 0, need)
	}
	buf = buf[:0]
	var base int64
	from := 0
	for {
		fresh := len(buf)
		eof := false
		for len(buf) < cap(buf) {
			n, err := r.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
			if errors.Is(err, io.EOF) {
				eof = true
				break
			}
			if err != nil {
				return buf, base, err
			}
		}
		to := len(buf)
		if !eof {
			to -= edgeRoom
		}
		if prepare != nil {
			prepare(buf, fresh, from, to, base)
		}
		at, err := m.scanRange(ctx, buf, from, to, budget, func(pat, end int) {
			found(buf, pat, end-m.lens[pat], end, base)
		})
		if err != nil {
			return buf, base + int64(at), err
		}
		if eof {
			return buf, base + int64(len(buf)), nil
		}
		start := max(0, to-keep)
		n := copy(buf, buf[start:])
		buf = buf[:n]
		base += int64(start)
		from = to - start
	}
}

// fileReader is an io.ReadSeeker over size bytes of an io.ReaderAt that
// reads readerBuffer bytes at a time, for the reader of the file's
// elements, which asks for a few bytes at a time. A read of src that
// fails, other than at its end, is kept in err, so a fault of the source
// is told apart from a file that cannot be read.
type fileReader struct {
	src  io.ReaderAt
	size int64
	pos  int64
	buf  []byte
	// at is the offset of buf's first byte in src.
	at  int64
	err error
}

// newFileReader returns a fileReader of size bytes of src.
func newFileReader(src io.ReaderAt, size int64) *fileReader {
	return &fileReader{src: src, size: size}
}

// Read fills p from the current offset; less than len(p) only at the end
// of the bytes, or when src fails.
func (f *fileReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if f.pos >= f.size {
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
		if f.pos >= f.at && f.pos < f.at+int64(len(f.buf)) {
			c := copy(p[n:], f.buf[f.pos-f.at:])
			n += c
			f.pos += int64(c)
			continue
		}
		if err := f.fill(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// fill reads the buffer from the current offset.
func (f *fileReader) fill() error {
	if f.buf == nil {
		f.buf = make([]byte, min(readerBuffer, f.size))
	}
	want := min(int64(cap(f.buf)), f.size-f.pos)
	n, err := f.src.ReadAt(f.buf[:want], f.pos)
	f.buf, f.at = f.buf[:n], f.pos
	if int64(n) == want {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		// The source holds fewer bytes than said: the file read ends here.
		f.size = f.pos + int64(n)
		return nil
	}
	f.err = err
	return err
}

// Seek sets the offset of the next Read.
func (f *fileReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		offset += f.size
	}
	if offset < 0 {
		return 0, errors.New("deid check: seek before the start of the file")
	}
	f.pos = offset
	return offset, nil
}

// sectionOf returns a reader of the bytes [off, end) of src that answers a
// source holding fewer of them than said as a file cut short (a Refusal,
// CauseUnreadable), and any other fault of src as an error of the source.
func sectionOf(src io.ReaderAt, off, end int64) io.Reader {
	return &exactReader{r: io.NewSectionReader(src, off, end-off), left: end - off}
}

// exactReader is a reader that must give left bytes.
type exactReader struct {
	r    io.Reader
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left == 0 {
		return 0, io.EOF
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	switch {
	case e.left == 0:
		return n, nil
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, &Refusal{Cause: CauseUnreadable}
	}
	return n, fmt.Errorf("deid check: %w", err)
}

// foldInto appends s, already in NFC, to dst folded as foldText folds it,
// *space saying whether the text appended before ended in white space, so
// a run of white space across two calls is still one space.
func foldInto(dst []byte, s []byte, space *bool) []byte {
	for len(s) > 0 {
		r, size := utf8.DecodeRune(s)
		s = s[size:]
		dst = foldRune(dst, r, space)
	}
	return dst
}

// foldReader reads the text of a stream of UTF-8 (already in NFC) folded
// as foldText folds it: the bytes of a rune cut by the end of one read are
// kept for the next, so a rune is never split into two U+FFFD.
type foldReader struct {
	src     io.Reader
	in      []byte
	out     []byte
	pending []byte
	space   bool
	eof     bool
}

// newFoldReader returns a foldReader of src.
func newFoldReader(src io.Reader) *foldReader {
	return &foldReader{src: src, in: make([]byte, 0, 32<<10)}
}

func (f *foldReader) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.eof {
			return 0, io.EOF
		}
		n, err := f.src.Read(f.in[len(f.in):cap(f.in)])
		f.in = f.in[:len(f.in)+n]
		if errors.Is(err, io.EOF) {
			f.eof = true
		} else if err != nil {
			return 0, err
		}
		whole := len(f.in)
		if !f.eof {
			// Keep a rune whose bytes are not all read yet.
			for i := 1; i <= utf8.UTFMax-1 && i <= whole; i++ {
				if utf8.RuneStart(f.in[whole-i]) {
					if !utf8.FullRune(f.in[whole-i:]) {
						whole -= i
					}
					break
				}
			}
		}
		f.out = foldInto(f.out[:0], f.in[:whole], &f.space)
		f.in = f.in[:copy(f.in, f.in[whole:])]
		f.pending = f.out
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}
