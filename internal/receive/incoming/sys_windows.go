//go:build windows

package incoming

import "os"

// syncDirectory does nothing on Windows. A directory there cannot be flushed
// through the os package (FlushFileBuffers needs a handle opened for
// writing, and a directory is opened for reading), and NTFS writes its
// changes of names through its own journal, so a rename is done or not after
// a crash, never half done. What is not promised is that the last rename
// before a power loss survives it: a sender answered just before may then
// find its file was not kept, the gap the directory storage states too.
func syncDirectory(*os.Root, string) error { return nil }

// openNonblock is nothing on Windows, where a name in a directory cannot be
// a FIFO that blocks its open.
const openNonblock = 0
