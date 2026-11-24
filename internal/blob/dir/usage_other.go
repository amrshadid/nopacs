//go:build !linux && !darwin && !windows

package dir

import "github.com/amrshadid/nopacs/internal/blob"

// usageKnown says whether volumeUsage can report this system's volumes. On
// the systems NoPACs is not shipped for, the call that would tell differs
// from one to the next, so the driver says it does not know.
const usageKnown = false

// volumeUsage reports that the space is not known here.
func volumeUsage(string) (blob.Usage, error) {
	return blob.Usage{}, nil
}
