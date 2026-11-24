//go:build !linux && !darwin

package server

// fileLimit returns 0: the system has no small per-process limit on open
// files that this package reads (Windows counts handles in the millions),
// so the default bound on network connections applies.
func fileLimit() int { return 0 }
