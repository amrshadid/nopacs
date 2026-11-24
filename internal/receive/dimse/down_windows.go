//go:build windows

package dimse

import "syscall"

// The Winsock errors of bind (learn.microsoft.com, "Windows Sockets Error
// Codes"). Go's syscall.EADDRINUSE and its kin on Windows are values of its
// own that a socket call never returns, so the Winsock numbers are compared.
const (
	wsaEACCES        = syscall.Errno(10013)
	wsaEADDRINUSE    = syscall.Errno(10048)
	wsaEADDRNOTAVAIL = syscall.Errno(10049)
)

// addrInUse reports a port another socket holds (WSAEADDRINUSE).
func addrInUse(err error) bool { return isErrno(err, wsaEADDRINUSE) }

// permissionDenied reports a port the process may not bind (WSAEACCES).
func permissionDenied(err error) bool { return isErrno(err, wsaEACCES) }

// addrNotAvailable reports an address that is not one of the machine's
// (WSAEADDRNOTAVAIL).
func addrNotAvailable(err error) bool { return isErrno(err, wsaEADDRNOTAVAIL) }
