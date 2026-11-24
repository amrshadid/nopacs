package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// SocketName is the local socket's file name inside <data-dir>/run.
const SocketName = "nopacs.sock"

// RunDir returns the directory of the local socket and the lock:
// <data-dir>/run, private to the user that runs the server.
func RunDir(dataDir string) string { return filepath.Join(dataDir, "run") }

// TLSDir returns the directory of a tenant's TLS files, which its receivers
// and senders name (receiver.proto, ReceiverTls; sender.proto, SenderTls):
// <data-dir>/tls/<tenant>. A backup holds none of them: they are the site's
// credentials, given again after a restore onto a new data directory.
func TLSDir(dataDir, tenantName string) string { return filepath.Join(dataDir, "tls", tenantName) }

// SocketPath returns the local socket's path for a data directory.
func SocketPath(dataDir string) string { return filepath.Join(RunDir(dataDir), SocketName) }

// maxSocketPath is the longest socket path every system accepts: sun_path
// holds 104 bytes on macOS and 108 on Linux, with the terminating NUL. A
// longer path fails with a message that does not name the cause, so it is
// refused here first.
const maxSocketPath = 103

// staleDialTimeout bounds the probe of a socket file left in place: a
// server that answers does so at once on the same machine.
const staleDialTimeout = time.Second

// localPeer is the operating system user a local socket connection came
// from, once the peer check has shown it is the server's own user.
type localPeer struct {
	user string
}

// localKey is the context key of a connection's localPeer.
type localKey struct{}

// localFrom returns the local peer a request's connection came from, and
// false for a network connection.
func localFrom(ctx context.Context) (localPeer, bool) {
	p, ok := ctx.Value(localKey{}).(localPeer)
	return p, ok
}

// peerConn is a local socket connection with its checked user.
type peerConn struct {
	net.Conn
	peer localPeer
}

// peerListener accepts local socket connections and keeps only those whose
// peer is the server's own operating system user, who can read the
// installation's files anyway and so is made the local operator: any other is
// closed at once, before a byte is read, and logged.
type peerListener struct {
	net.Listener
	// self is the user the peer must be.
	self string
	// peerUser reads a connection's peer user; it is the system's call
	// (peer_*.go) except in tests.
	peerUser func(net.Conn) (string, error)
	log      *slog.Logger
}

// Accept returns the next connection whose peer is the server's user.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		user, err := l.peerUser(c)
		if err != nil || user != l.self {
			// The user id is not a secret and tells the admin who tried.
			l.log.Warn("server: a local socket connection from another user was closed",
				slog.String("peer_user", user), slog.Any("error", err))
			_ = c.Close()
			continue
		}
		return &peerConn{Conn: c, peer: localPeer{user: user}}, nil
	}
}

// connContext puts a local connection's peer in the context of each of its
// requests.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if pc, ok := c.(*peerConn); ok {
		return context.WithValue(ctx, localKey{}, pc.peer)
	}
	return ctx
}

// listenSocket makes <data-dir>/run private, removes a socket file left by
// a process that is gone (only after confirming nothing answers on it), and
// listens on the local socket. The socket file is made readable and
// writable by its owner only, which Linux enforces on connect; the private
// directory is the guard on every system.
func listenSocket(dataDir string) (net.Listener, error) {
	dir := RunDir(dataDir)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	path := SocketPath(dataDir)
	if len(path) > maxSocketPath {
		return nil, fault.New(fault.Invalid, "server: the local socket's path "+path+" is "+strconv.Itoa(len(path))+
			" bytes, longer than the "+strconv.Itoa(maxSocketPath)+" a system accepts; use a data directory with a shorter path")
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fault.Wrap(fault.Unavailable, err, "server: cannot listen on the local socket "+path+": "+err.Error())
	}
	if err := restrictSocket(path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// removeStale removes a socket file at path left by a process that is gone.
// It first dials it: when something answers, another server is serving
// there and the file is left alone. Anything that is not a socket (a
// directory, a regular file) is never removed.
func removeStale(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fault.Wrap(fault.Internal, err, "server: cannot look at "+path)
	}
	if !isSocketFile(info) {
		return fault.New(fault.Invalid, "server: "+path+" exists and is not a socket; move it away")
	}
	c, err := net.DialTimeout("unix", path, staleDialTimeout)
	if err == nil {
		_ = c.Close()
		return fault.New(fault.Conflict, "server: another process answers on the local socket "+path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault.Wrap(fault.Internal, err, "server: cannot remove the stale socket "+path)
	}
	return nil
}

// tlsConfig is the network listener's TLS: TLS 1.2 or later (Go's default
// minimum, stated here so a change of default cannot lower it), with Go's
// default cipher suites and key exchanges, which include the post-quantum
// hybrid since Go 1.24.
//
// A client certificate is asked for and never required
// (tls.RequestClientCert), and not checked against any authority: a key
// that must come with one names the certificate by its SHA-256, which the
// check compares, so the pin is the trust and no authority is needed. The
// handshake still proves the client holds the certificate's private key
// (its CertificateVerify, RFC 8446 4.4.3, which Go verifies whenever a
// certificate is sent). A client that sends none connects as before.
//
// The server's certificate comes from certs on each handshake, so a
// certificate renewed in its files is served without a restart.
func tlsConfig(certs *certFiles) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: certs.get,
		ClientAuth:     tls.RequestClientCert,
	}
}

// checkListen refuses a network address that would send keys in clear
// text: without a certificate only a loopback address is accepted, where
// no other machine can read the traffic.
func checkListen(addr string, haveCert bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "server: --listen takes HOST:PORT, such as 127.0.0.1:8443")
	}
	if haveCert || isLoopbackHost(host) {
		return nil
	}
	return fault.New(fault.Invalid, "server: listening on "+addr+" without TLS is refused, since keys would cross the network in clear text; "+
		"give --tls-cert and --tls-key, or listen on a loopback address such as 127.0.0.1")
}

// isLoopbackHost reports whether host is a loopback address, written as an
// address. A name is not accepted, not even localhost: a name resolves
// through files and servers the server does not control, and could name
// another machine. An empty host (all addresses) is not loopback.
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
