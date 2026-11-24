//go:build !linux && !darwin && !windows

package server

import (
	"errors"
	"net"
	"os"
	"strconv"
)

// peerUser refuses every local socket connection on a system no release
// targets: its way of reading a peer's user is not written, and a socket
// nobody can check is not opened to anyone.
func peerUser(net.Conn) (string, error) {
	return "", errors.New("the local socket's peer cannot be checked on this system")
}

// systemUser returns the server's own effective user id.
func systemUser() string { return strconv.Itoa(os.Geteuid()) }

// peerCheck says how the local socket's caller is checked on this system.
const peerCheck = "unsupported: every connection is refused"
