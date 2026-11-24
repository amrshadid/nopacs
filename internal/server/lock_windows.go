//go:build windows

package server

import (
	"errors"
	"os"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION (winerror.h): another
// process has the file open in a way that excludes this open.
const errorSharingViolation syscall.Errno = 32

// lockFile opens path for reading and writing while letting other
// processes only read it: no second process can open it for writing until
// this handle is closed, which Windows does when the process ends, however
// it ends. That is the lock; it needs no LockFileEx, and the syscall
// package's CreateFile is enough, so golang.org/x/sys is not needed. The
// file stays readable, so a second server can name the first one's process.
func lockFile(path string) (f *os.File, held bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return os.NewFile(uintptr(h), path), false, nil
}

// unlockFile drops the lock by closing the handle.
func unlockFile(f *os.File) error { return f.Close() }
