package stow

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/receive/header"
)

// The bytes of a part read to identify it before it is handed on
// (header.Identify). The four UIDs that place a file come before
// (0020,000E) and so early in its data set: every public corpus file gives
// them in its first 64 KiB. A part whose first firstPrefix bytes do not
// give them is read up to maxPrefix, which bounds what a request holds in
// memory; one that does not give them even then is refused, since where it
// goes cannot be decided without holding more of it.
const (
	firstPrefix = 64 << 10
	maxPrefix   = 1 << 20
)

// The bounds of a request's multipart framing (RFC 2046 5.1.1), each
// refused past it so a client cannot make the endpoint hold or count
// without end what is not a file.
const (
	// MaxParts is the most parts one request holds. Each part costs the
	// request a little memory for its line in the answer and a turn of the
	// pipeline; a client with more instances sends more requests.
	MaxParts = 10000
	// MaxPartHeader is the most bytes of one part's header (its header
	// fields and the boundary line before them; for the first part, the
	// preamble too), as the server bounds a request's own header at 64
	// KiB. A part's header names its media type and little else.
	MaxPartHeader = 16 << 10
	// headerSlack is what the multipart reader may read past a part's
	// header in the same read, its buffer's size: the bytes taken while a
	// header is read are held to MaxPartHeader and this, and the header
	// read is then measured against MaxPartHeader itself.
	headerSlack = 4 << 10
)

// errStalled is a body whose client sent nothing within the read stall
// bound.
var errStalled = errors.New("stow: the request body stopped arriving")

// errCut is a body whose reads Stop ended.
var errCut = errors.New("stow: the receiver is stopping")

// errPartHeader is a part whose header is larger than MaxPartHeader.
var errPartHeader = errors.New("stow: a part's header is too large")

// stallReader reads a request's body, giving each read the read stall
// bound: a read that waits longer fails, and the connection with it. Once
// the request is cut (cut set, by Stop), or its context ends, every read
// fails at once and the deadline is no longer moved, so a client that
// keeps sending cannot hold a stopping receiver.
type stallReader struct {
	r      io.Reader
	ctx    context.Context
	cut    *atomic.Bool
	extend func()
}

// Read extends the deadline, then reads, unless the request is cut.
func (s *stallReader) Read(p []byte) (int, error) {
	if s.cut.Load() {
		return 0, errCut
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	s.extend()
	// A cut between the check above and the extension is undone by it:
	// checked again, so the deadline the cut set is the one that holds.
	if s.cut.Load() {
		return 0, errCut
	}
	n, err := s.r.Read(p)
	var to interface{ Timeout() bool }
	switch {
	case s.cut.Load():
		return n, errCut
	case err != nil && errors.As(err, &to) && to.Timeout():
		return n, errStalled
	}
	return n, err
}

// headerBound holds the bytes taken from a body while a part's header is
// read to MaxPartHeader and headerSlack; between headers it reads freely.
type headerBound struct {
	r io.Reader
	// left is what a header may still take, while reading is true.
	left    int
	reading bool
}

// Read reads from the body, failing with errPartHeader once a header has
// taken its bound.
func (h *headerBound) Read(p []byte) (int, error) {
	if !h.reading {
		return h.r.Read(p)
	}
	if h.left <= 0 {
		return 0, errPartHeader
	}
	if len(p) > h.left {
		p = p[:h.left]
	}
	n, err := h.r.Read(p)
	h.left -= n
	return n, err
}

// startHeader begins the read of a part's header.
func (h *headerBound) startHeader() { h.reading, h.left = true, MaxPartHeader+headerSlack }

// endHeader ends it: the part's body is read without a bound here.
func (h *headerBound) endHeader() { h.reading = false }

// headerSize returns about how many bytes a part's header held: each
// field's name and value with the separator and line end the multipart
// reader removed.
func headerSize(h map[string][]string) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

// countingReader counts the bytes read through it and keeps the first
// error other than the end.
type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

// Read reads from the part, counting.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

// prefixBuffer is the one buffer a request reads each part's first bytes
// into, grown once to what the largest prefix needs and used again for
// every part: a request of many small parts allocates it once, not once a
// part. What it holds is valid until the next part is read.
type prefixBuffer struct{ buf []byte }

// read reads the first bytes of a part, enough to identify it: up to
// firstPrefix, then up to maxPrefix when those do not give its identity,
// and never more than one byte past the receiver's largest file. whole
// says the part ended within what was read. A failure to read the body is
// returned, a body that ends inside the part included (the multipart
// reader's io.ErrUnexpectedEOF): such a part was cut, not sent whole.
func (b *prefixBuffer) read(r io.Reader, limit int64) (prefix []byte, whole bool, err error) {
	first := int(min(int64(firstPrefix), limit+1))
	b.grow(first)
	n, whole, err := fill(r, b.buf[:first])
	if err != nil || whole || int64(first) > limit {
		return b.buf[:n], whole, err
	}
	if id, err := header.Identify(b.buf[:n], false); err == nil && id.Complete {
		return b.buf[:n], false, nil
	}
	second := int(min(int64(maxPrefix), limit+1))
	b.grow(second)
	m, whole, err := fill(r, b.buf[n:second])
	return b.buf[:n+m], whole, err
}

// grow makes the buffer hold at least n bytes, keeping what it holds.
func (b *prefixBuffer) grow(n int) {
	if len(b.buf) >= n {
		return
	}
	nb := make([]byte, n)
	copy(nb, b.buf)
	b.buf = nb
}

// fill reads r into p until p is full or r ends. It reports whether r
// ended (io.EOF, the end of the part) and returns any other error.
func fill(r io.Reader, p []byte) (int, bool, error) {
	n := 0
	for n < len(p) {
		m, err := r.Read(p[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
	}
	return n, false, nil
}
