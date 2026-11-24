//go:build !windows

package dimse

import "syscall"

// addrInUse reports a port another socket holds (bind(2): EADDRINUSE).
func addrInUse(err error) bool { return isErrno(err, syscall.EADDRINUSE) }

// permissionDenied reports a port the process may not bind, as one below
// 1024 without the privilege (bind(2): EACCES; EPERM on some systems).
func permissionDenied(err error) bool { return isErrno(err, syscall.EACCES, syscall.EPERM) }

// addrNotAvailable reports an address that is not one of the machine's
// (bind(2): EADDRNOTAVAIL).
func addrNotAvailable(err error) bool { return isErrno(err, syscall.EADDRNOTAVAIL) }
