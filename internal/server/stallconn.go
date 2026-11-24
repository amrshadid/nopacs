package server

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
)

// stallListener gives each network connection a stallConn. It sits under
// TLS, on the bytes TLS sends.
type stallListener struct {
	net.Listener
}

// Accept accepts and wraps the connection.
func (l stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stallConn{Conn: c}, nil
}

// stallConn fails every write at once after one write ran past its
// deadline. A long method's response is cut when a write cannot finish
// within the stall timeout, and net/http then closes the connection; over
// TLS that Close first sends a close_notify alert under a deadline of its
// own of 5 seconds (crypto/tls), into the very buffers a client that stopped
// reading has filled, so the handler was held 5 seconds past the stall
// timeout. A connection whose write timed out has lost bytes of a record
// in the middle (RFC 8446 5, a record is sent whole or the stream is
// broken), so nothing written after it could be read anyway.
type stallConn struct {
	net.Conn
	timedOut atomic.Bool
}

// Write writes, or fails at once when an earlier write timed out.
func (c *stallConn) Write(b []byte) (int, error) {
	if c.timedOut.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.Conn.Write(b)
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		c.timedOut.Store(true)
	}
	return n, err
}
