//go:build unix

package registry

import (
	"math"
	"syscall"
)

// openFileLimit returns the process's soft limit on open files (RLIMIT_NOFILE),
// which Go raises to the hard limit at start, or -1 when it cannot be read.
func openFileLimit() int64 {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err != nil {
		return -1
	}
	if l.Cur > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(l.Cur) //nolint:gosec // G115: checked against MaxInt64 above.
}
