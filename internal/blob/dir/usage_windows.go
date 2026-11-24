//go:build windows

package dir

import (
	"syscall"
	"unsafe"

	"github.com/amrshadid/nopacs/internal/blob"
)

// usageKnown says whether volumeUsage can report this system's volumes.
const usageKnown = true

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// volumeUsage reports the volume holding path, from GetDiskFreeSpaceExW
// (learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-getdiskfreespaceexw).
// Free is the bytes available to this process, which honors quotas, and
// Used is the volume's size less its free bytes. Package syscall does not
// wrap the call, so it is made through kernel32.dll, which every Windows
// process has loaded.
func volumeUsage(path string) (blob.Usage, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return blob.Usage{}, err
	}
	var avail, total, free uint64
	r, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r == 0 {
		return blob.Usage{}, err
	}
	return blob.Usage{
		Known: true,
		Total: int64(total),        // #nosec G115 -- a volume is far below 2^63 bytes.
		Used:  int64(total - free), // #nosec G115 -- as above.
		Free:  int64(avail),        // #nosec G115 -- as above.
	}, nil
}
