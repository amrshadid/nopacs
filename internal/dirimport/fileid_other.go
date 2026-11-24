//go:build !unix

package dirimport

import "io/fs"

// deviceOf reports that the system gives no file system of an entry
// through fs.FileInfo (on Windows the volume is known only from an open
// handle), so no entry is skipped for lying on another one.
func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }

// linkCount reports that the system gives no count of an entry's names
// through fs.FileInfo, so no file is skipped for having several.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
