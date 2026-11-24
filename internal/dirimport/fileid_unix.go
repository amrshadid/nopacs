//go:build unix

package dirimport

import (
	"io/fs"
	"syscall"
)

// deviceOf returns the file system an entry lies on (st_dev, POSIX
// <sys/stat.h>), and whether the system reports it.
func deviceOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return widen(st.Dev), true
}

// linkCount returns how many names an entry has (st_nlink), and whether
// the system reports it.
func linkCount(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return widen(st.Nlink), true
}

// widen returns a field of syscall.Stat_t as a uint64, whatever its type
// on this system (st_dev is an int32 on macOS and a uint64 on Linux;
// st_nlink an int16 on AIX, a uint16, a uint32 or a uint64 elsewhere). The
// values are compared, never computed with, so the bits of a negative one
// stand as they are.
func widen[T ~int16 | ~int32 | ~int64 | ~uint16 | ~uint32 | ~uint64](v T) uint64 {
	return uint64(v) // #nosec G115 -- compared, never computed with.
}
