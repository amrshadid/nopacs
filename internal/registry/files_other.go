//go:build !unix

package registry

// openFileLimit returns -1 where there is no per-process limit on open files
// to read (Windows limits handles per process far above what the tenants
// need).
func openFileLimit() int64 { return -1 }
