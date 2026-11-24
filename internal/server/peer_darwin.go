//go:build darwin

package server

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// The socket option that reads a local socket peer's credentials on macOS,
// from <sys/un.h> and <sys/ucred.h>. The standard library's syscall package
// has no wrapper for it, but it has syscall.Syscall6 and SYS_GETSOCKOPT on
// darwin, without cgo, as package secret already uses for fgetattrlist. So
// golang.org/x/sys is not needed.
const (
	solLocal      = 0 // SOL_LOCAL
	localPeerCred = 1 // LOCAL_PEERCRED
	xucredVersion = 0 // XUCRED_VERSION
	ngroups       = 16
)

// xucred is struct xucred from <sys/ucred.h>. Recent releases add a union
// of a pointer and the peer's pid after the groups; the trailing bytes make
// room for it, and the kernel copies at most the size given.
type xucred struct {
	version uint32
	uid     uint32
	ngroups int16
	_       int16
	groups  [ngroups]uint32
	_       [12]byte
}

// peerUser returns the effective user id of the process at the other end
// of a local socket connection: getsockopt(LOCAL_PEERCRED), which is what
// getpeereid(3) calls. The manual page says the credentials are those the
// peer had when it connected, and "there is no way for either side to
// influence the credentials returned to its peer" except by connecting as
// another user.
func peerUser(c net.Conn) (string, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return "", errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred xucred
	var errno syscall.Errno
	size := uint32(unsafe.Sizeof(cred))
	if err := raw.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerCred,
			uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&size)), 0)
	}); err != nil {
		return "", err
	}
	if errno != 0 {
		return "", errno
	}
	if cred.version != xucredVersion || size < uint32(unsafe.Offsetof(cred.ngroups)) {
		return "", errors.New("the kernel's peer credentials are not in the expected layout")
	}
	return strconv.FormatUint(uint64(cred.uid), 10), nil
}

// systemUser returns the server's own effective user id, which a local
// socket peer must have.
func systemUser() string { return strconv.Itoa(os.Geteuid()) }

// peerCheck says, for the documentation and the log, how the local socket's
// caller is checked on this system.
const peerCheck = "LOCAL_PEERCRED"
