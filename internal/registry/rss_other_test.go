//go:build !unix

package registry

// peakRSS is not read on systems without getrusage.
func peakRSS() int64 { return -1 }
