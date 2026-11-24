//go:build windows

package server

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Windows keeps a directory's permissions in an access list (DACL), not in
// mode bits, and os.Mkdir's mode is ignored there. The syscall package does
// not export the functions that build and set an access list, so the two
// used here are taken from advapi32.dll through syscall.NewLazyDLL, as
// package secret does; advapi32.dll is a system DLL the syscall package
// loads only from System32. So golang.org/x/sys/windows is not needed.
var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDacl                            = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetNamedSecurityInfoW                                = advapi32.NewProc("SetNamedSecurityInfoW")
)

// Values from the Windows SDK (sddl.h, accctrl.h, winnt.h).
const (
	sddlRevision1                  = 1
	seFileObject                   = 1 // SE_OBJECT_TYPE: a file or a directory
	daclSecurityInformation        = 0x4
	protectedDACLSecurityInfo      = 0x80000000
	errorSuccess              uint = 0
)

// EnsurePrivateDir makes dir when it does not exist, and in every case sets
// its access list to grant full control to the server's user alone,
// protected from what its parent would pass down, and inherited by what is
// made inside it (the socket file, the lock). It is set at every start,
// rather than checked, so a directory someone widened is narrowed again.
// This access list is the only guard of the local socket on Windows (see
// peer_windows.go). The parent must exist.
func EnsurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fault.Wrap(fault.Internal, err, "server: cannot make the directory "+dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "server: cannot look at "+dir)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fault.New(fault.Invalid, "server: "+dir+" is not a directory (a link is not accepted); move it away")
	}
	if err := setPrivateDACL(dir); err != nil {
		return fault.Wrap(fault.Internal, err, "server: cannot make "+dir+" private to the user running nopacs")
	}
	return nil
}

// ensurePrivateDir is EnsurePrivateDir, under the name the listener uses.
func ensurePrivateDir(dir string) error { return EnsurePrivateDir(dir) }

// setPrivateDACL sets on path a protected DACL with one entry: full control
// for the current user, inherited by files and directories inside (OICI).
func setPrivateDACL(path string) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	sddl, err := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return err
	}
	var sd uintptr
	if r, _, e := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return e
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(sd)) }()
	var present, defaulted int32
	var dacl uintptr
	if r, _, e := procGetSecurityDescriptorDacl.Call(sd,
		uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); r == 0 {
		return e
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, _ := procSetNamedSecurityInfoW.Call(uintptr(unsafe.Pointer(p)), seFileObject,
		daclSecurityInformation|protectedDACLSecurityInfo, 0, 0, dacl, 0)
	if uint(r) != errorSuccess {
		return syscall.Errno(r)
	}
	return nil
}

// restrictSocket does nothing on Windows: the socket file inherits the
// directory's private access list.
func restrictSocket(string) error { return nil }

// isSocketFile reports whether info may be a socket. Windows reports an
// AF_UNIX socket file as a reparse point, which Go has shown as different
// modes across releases, so anything that is not a directory counts; the
// dial before removal is what protects a live socket.
func isSocketFile(info fs.FileInfo) bool { return !info.IsDir() }

// dirGuard says how the local socket's directory is kept private on this
// system.
const dirGuard = "a protected access list granting the server's user alone"
