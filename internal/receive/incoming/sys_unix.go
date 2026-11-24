//go:build unix

package incoming

import (
	"errors"
	"os"
	"syscall"
)

// syncDirectory syncs the directory dir below root, so that a name made or
// replaced in it survives a crash of the machine: on POSIX a file's fsync
// does not make its directory entry durable (fsync(2)). On macOS
// os.File.Sync is F_FULLFSYNC, which also empties the drive's cache, since a
// plain fsync there does not (fcntl(2) on macOS).
func syncDirectory(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	serr := f.Sync()
	return errors.Join(serr, f.Close())
}

// openNonblock is added to every open for reading, so opening a name that
// something swapped for a FIFO returns at once instead of waiting for a
// writer (open(2): O_NONBLOCK); Open then checks it is a regular file.
const openNonblock = syscall.O_NONBLOCK
