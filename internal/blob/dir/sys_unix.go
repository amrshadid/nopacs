//go:build unix

package dir

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// syncDirectory syncs the directory dir below root, so that a name made,
// replaced or removed in it survives a crash of the machine: on POSIX a
// file's fsync does not make its directory entry durable (fsync(2), and the
// rename section of the ext4 documentation). On macOS os.File.Sync is
// F_FULLFSYNC, which also empties the drive's cache, since a plain fsync
// there does not (fcntl(2) on macOS).
func syncDirectory(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	serr := f.Sync()
	return errors.Join(serr, f.Close())
}

// isFull reports whether err says the volume has no space left.
func isFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// isBusy is always false on Unix: a rename or a removal never waits for a
// reader there, since an open file keeps its bytes after its name changes.
func isBusy(error) bool { return false }

// sameFileSystem reports whether a and b are on one file system, by their
// device numbers (stat(2): st_dev), which Unix always gives.
func sameFileSystem(a, b fs.FileInfo) (same, known bool) {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return false, false
	}
	return sa.Dev == sb.Dev, true
}

// isCrossDevice reports whether err is a rename refused because it would
// cross file systems (rename(2): EXDEV).
func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// openNonblock is added to every open for reading, so that opening a name
// that something swapped for a FIFO or a device returns at once instead of
// waiting for a writer (open(2): O_NONBLOCK); what was opened is then
// checked on the descriptor. On a regular file or a directory it changes
// nothing.
const openNonblock = syscall.O_NONBLOCK

// isUnsupported reports whether err, from a sync of a directory, says the
// file system cannot do it (fsync(2): EINVAL, "fd is bound to a special file
// ... which does not support synchronization"; ENOTSUP on some network file
// systems).
func isUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
