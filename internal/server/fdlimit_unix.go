//go:build linux || darwin

package server

import (
	"math"
	"syscall"
)

// fileLimit returns the process's current limit on open files
// (RLIMIT_NOFILE), or 0 when it cannot be read. Go raises the soft limit to
// the hard one at start, so this is the limit the process actually has.
func fileLimit() int {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0
	}
	if r.Cur > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(r.Cur) //nolint:gosec // G115: bounded just above.
}
