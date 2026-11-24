//go:build !linux && !darwin && !windows

package server

import (
	"errors"
	"os"
)

// lockFile refuses on a system no release targets: without a lock written
// for it, two servers could share one data directory.
func lockFile(string) (*os.File, bool, error) {
	return nil, false, errors.New("locking a data directory is not written for this system")
}

// unlockFile closes f.
func unlockFile(f *os.File) error { return f.Close() }
