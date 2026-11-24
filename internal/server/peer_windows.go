//go:build windows

package server

import (
	"net"
	"syscall"
)

// peerUser returns the server's own user for every local socket connection
// on Windows: the peer is not read, and the private directory is the guard.
//
// Why: Windows has no SO_PEERCRED for AF_UNIX sockets. What it offers is
// the ioctl SIO_AF_UNIX_GETPEERPID (Windows 10 1803 and later), which gives
// the peer's process id, not its user. Turning that into a user means
// opening the process and its token, and a process id can be reused by
// another process between the connect and the lookup, so the answer is not
// tied to the connection the way SO_PEERCRED's is. That is not reliable
// enough to be called a check, so it is not used.
//
// The guard on Windows is therefore <data-dir>\run itself: ensurePrivateDir
// gives it, at every start, a protected access list that grants the
// server's user alone, which the socket file inherits; a process of another
// user cannot open the socket to connect. The documentation of the package
// says so plainly.
func peerUser(net.Conn) (string, error) {
	return systemUser(), nil
}

// systemUser returns the security identifier (SID) of the user the server
// runs as, which access.LocalOperator records in the audit log.
func systemUser() string {
	sid, err := currentSID()
	if err != nil {
		return ""
	}
	return sid
}

// currentSID returns the SID of the process's user, as a string.
func currentSID() (string, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}

// peerCheck says, for the documentation and the log, how the local socket's
// caller is checked on this system.
const peerCheck = "none: the private directory is the guard"
