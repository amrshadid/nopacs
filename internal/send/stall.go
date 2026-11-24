package send

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// A send is cut when the peer stops taking bytes, never for being slow:
// the bound is the sender's response timeout without progress, not a total
// for an instance, so a peer behind a slow link, or a sender whose rate is
// shared by its associations, sends instances of any size.

// stallCtx is the context of one C-STORE: its deadline is always the last
// progress plus the timeout. go-dicom sets the connection's write deadline
// from the context's deadline at each PDU it writes, and reads the next
// fragment of the data set just before writing the one it holds, so every
// fragment taken from the reader (progress) moves the bound of the writes
// after it: a peer that stops reading is cut a timeout after the last
// fragment it took. A fragment is at most the peer's maximum PDU and 1
// MiB, and must be written within the timeout.
type stallCtx struct {
	context.Context
	timeout time.Duration
	last    atomic.Int64
}

// newStallCtx returns the stall context of a send over parent, its
// progress now.
func newStallCtx(parent context.Context, timeout time.Duration) *stallCtx {
	c := &stallCtx{Context: parent, timeout: timeout}
	c.touch()
	return c
}

// Deadline is the last progress plus the timeout.
func (c *stallCtx) Deadline() (time.Time, bool) {
	return time.Unix(0, c.last.Load()).Add(c.timeout), true
}

// touch records progress now.
func (c *stallCtx) touch() { c.last.Store(time.Now().UnixNano()) }

// stalled reports whether no progress was made for the timeout.
func (c *stallCtx) stalled() bool { d, _ := c.Deadline(); return !time.Now().Before(d) }

// progressReader records progress on its context each time a read yields
// bytes.
type progressReader struct {
	r   io.Reader
	ctx *stallCtx
}

// Read reads, and records progress.
func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.ctx.touch()
	}
	return n, err
}

// stallConn is a connection whose every write must make progress within
// timeout: each piece of a write sets its deadline afresh, so a peer that
// takes bytes slowly but steadily is never cut, and one that stops is cut a
// timeout after it stopped. Reads are left without a deadline: an HTTP
// transport reads for the answer while the body is still being written,
// and the wait for the answer is bounded by the request (its response
// header timeout, and the answer's own bound in stowRequest).
type stallConn struct {
	net.Conn
	timeout time.Duration
}

// Write writes, each piece taken within the timeout of the one before.
func (c *stallConn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return written, err
		}
		end := min(len(b), written+stallChunk)
		n, err := c.Conn.Write(b[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// stallChunk is the most of one write given to the connection at once, so
// a large write is bounded piece by piece and not as a whole.
const stallChunk = 64 << 10
