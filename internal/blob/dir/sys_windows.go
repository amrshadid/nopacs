//go:build windows

package dir

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// syncDirectory does nothing on Windows. A directory there cannot be
// flushed through the os package (FlushFileBuffers needs a handle opened
// for writing, and a directory is opened for reading), and NTFS writes its
// changes of names through its own journal, so a rename is either done or
// not after a crash, never half done. What is not promised is that the last
// rename before a power loss survives it; that is the remaining gap against
// Unix, where the directory is synced.
func syncDirectory(*os.Root, string) error { return nil }

// errorHandleDiskFull and errorDiskFull are ERROR_HANDLE_DISK_FULL and
// ERROR_DISK_FULL, which package syscall does not name.
const (
	errorHandleDiskFull syscall.Errno = 39
	errorDiskFull       syscall.Errno = 112
)

// isFull reports whether err says the volume has no space left.
func isFull(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull)
}

// errorSharingViolation is ERROR_SHARING_VIOLATION, which package syscall
// does not name.
const errorSharingViolation syscall.Errno = 32

// isBusy reports whether err is Windows saying another program holds the
// file open in a way that forbids the change: a virus scanner or an indexer
// does that for moments at a time, and a second try usually succeeds. Go
// opens files with FILE_SHARE_DELETE and renames with POSIX semantics, so
// NoPACs' own readers never cause it.
func isBusy(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}

// sameFileSystem cannot tell on Windows: the volume a file is on is not in
// what os.Lstat returns there, so Open does not compare the temporary
// directory's volume with the root's. A temporary directory that is a
// mount point or a junction is still refused, since Go reports it as a
// link or an irregular file and not as a plain directory. What is left
// unchecked, a volume mounted below the root, makes a Put into it fail at
// its rename with ERROR_NOT_SAME_DEVICE, which Put reports as such.
func sameFileSystem(fs.FileInfo, fs.FileInfo) (same, known bool) { return false, false }

// errorNotSameDevice is ERROR_NOT_SAME_DEVICE, which package syscall does
// not name.
const errorNotSameDevice syscall.Errno = 17

// isCrossDevice reports whether err is a rename refused because it would
// cross volumes.
func isCrossDevice(err error) bool { return errors.Is(err, errorNotSameDevice) }

// openNonblock is nothing on Windows, where a name in a directory cannot be
// a FIFO that blocks its open.
const openNonblock = 0

// isUnsupported is always false on Windows, where a directory is not synced
// at all.
func isUnsupported(error) bool { return false }

// isVanished is false on Windows, where a removed directory is reported as
// not found.
func isVanished(error) bool { return false }
