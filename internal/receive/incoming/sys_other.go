//go:build !unix && !windows

package incoming

import "os"

// syncDirectory does nothing on a system that is neither Unix nor Windows,
// where NoPACs is not shipped; it is here so the package builds there.
func syncDirectory(*os.Root, string) error { return nil }

// openNonblock is nothing where it is not known to be needed.
const openNonblock = 0
