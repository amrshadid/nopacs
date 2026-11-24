package server

import (
	"net"
	"sync"
)

// The bound on network connections. Every connection costs the process a
// file descriptor, and when they run out nothing can be accepted, not even
// on the local socket, nor a database file opened: a flood of connections
// from callers with no key would lock out the operator. So the network
// listener holds at most this many connections at once, a number below the
// process's file limit that leaves a reserve; further callers wait in the
// system's queue of pending connections, which costs the process nothing.
const (
	// fileReserve is what the network may never take of the file limit:
	// the local socket's connections, the master and the tenants' database
	// files, the log, the lock, with room to spare.
	fileReserve = 64
	// minNetworkConns is the least the network is given, on a system with
	// a very low file limit.
	minNetworkConns = 16
	// defaultNetworkConns is the bound where the file limit cannot be read
	// (Windows, which counts handles and has no small per-process limit).
	defaultNetworkConns = 4096
)

// maxNetworkConns returns the bound on network connections for this
// process: its file limit less the reserve, or the default where the limit
// is not known.
func maxNetworkConns() int {
	limit := fileLimit()
	if limit <= 0 {
		return defaultNetworkConns
	}
	return max(minNetworkConns, limit-fileReserve)
}

// limitListener holds at most cap(slots) connections open at once: Accept
// waits for a slot before it takes the next connection from the system,
// and a connection's Close gives its slot back.
type limitListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

// newLimitListener bounds ln to n connections at once.
func newLimitListener(ln net.Listener, n int) *limitListener {
	return &limitListener{Listener: ln, slots: make(chan struct{}, n), done: make(chan struct{})}
}

// Accept waits for a free slot, then accepts.
func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

// Close closes the listener and ends an Accept waiting for a slot.
func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitConn gives its slot back once, when it is closed.
type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

// Close closes the connection and frees its slot.
func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
