//go:build unix

package registry

import (
	"runtime"
	"syscall"
)

// peakRSS returns the most memory the process has held at once, in bytes,
// as the system counts it. It includes what the SQLite library allocates
// outside the Go heap, which runtime.MemStats does not see.
func peakRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	// Maxrss is an int32 on 32-bit Linux and an int64 elsewhere, so the
	// conversion is needed on some systems and not others.
	rss := int64(ru.Maxrss) //nolint:unconvert // see above
	// macOS counts bytes; Linux and the BSDs count KiB (getrusage(2)).
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return rss
	}
	return rss * 1024
}
