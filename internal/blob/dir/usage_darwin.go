//go:build darwin

package dir

import (
	"syscall"

	"github.com/amrshadid/nopacs/internal/blob"
)

// usageKnown says whether volumeUsage can report this system's volumes.
const usageKnown = true

// volumeUsage reports the volume holding path, from statfs(2), whose block
// counts are in units of f_bsize on macOS. Free is f_bavail, what an
// unprivileged process may still write.
func volumeUsage(path string) (blob.Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return blob.Usage{}, err
	}
	unit := int64(st.Bsize)
	return blob.Usage{
		Known: true,
		Total: int64(st.Blocks) * unit,          // #nosec G115 -- a block count is far below 2^63.
		Used:  int64(st.Blocks-st.Bfree) * unit, // #nosec G115 -- as above.
		Free:  int64(st.Bavail) * unit,          // #nosec G115 -- as above.
	}, nil
}
