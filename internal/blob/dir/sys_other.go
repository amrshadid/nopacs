//go:build !unix && !windows

package dir

import (
	"io/fs"
	"os"
)

// syncDirectory does nothing on a system that is neither Unix nor Windows,
// where NoPACs is not shipped; it is here so the package builds there.
func syncDirectory(*os.Root, string) error { return nil }

// isFull is always false where the system's error for a full volume is not
// known; the write still fails, with a less specific message.
func isFull(error) bool { return false }

// isBusy is always false where no retry is known to be needed.
func isBusy(error) bool { return false }

// sameFileSystem cannot tell where no way to ask is known.
func sameFileSystem(fs.FileInfo, fs.FileInfo) (same, known bool) { return false, false }

// isCrossDevice is always false where the system's error for it is not
// known; the rename still fails, with a less specific message.
func isCrossDevice(error) bool { return false }

// openNonblock is nothing where it is not known to be needed.
const openNonblock = 0

// isUnsupported is always false where directories are not synced.
func isUnsupported(error) bool { return false }

// isVanished is false where no special answer is known.
func isVanished(error) bool { return false }
