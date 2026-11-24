//go:build linux || darwin

package server

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens path (making it, mode 0600) and takes an exclusive flock(2)
// on it without waiting. It reports held when another process has it. A
// flock belongs to the open file and ends when the process does, so a
// killed server leaves no lock behind. flock on a network file system may
// not lock across machines; a data directory is local.
func lockFile(path string) (f *os.File, held bool, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the server's own lock file.
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return f, false, nil
}

// unlockFile drops the lock by closing the file, which ends the flock.
func unlockFile(f *os.File) error { return f.Close() }
