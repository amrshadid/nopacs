//go:build linux

package server

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
)

// peerUser returns the effective user id of the process at the other end
// of a local socket connection, as the kernel recorded it when that process
// connected: SO_PEERCRED (unix(7)). The peer cannot change what is
// returned except by connecting as another user. The standard library's
// syscall package has GetsockoptUcred on Linux, so golang.org/x/sys is not
// needed.
func peerUser(c net.Conn) (string, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return "", errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return "", err
	}
	if serr != nil {
		return "", serr
	}
	return strconv.FormatUint(uint64(cred.Uid), 10), nil
}

// systemUser returns the server's own effective user id, which a local
// socket peer must have.
func systemUser() string { return strconv.Itoa(os.Geteuid()) }

// peerCheck says, for the documentation and the log, how the local socket's
// caller is checked on this system.
const peerCheck = "SO_PEERCRED"
