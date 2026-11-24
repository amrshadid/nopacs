//go:build unix && !darwin

package secret

import "os"

// permitACL reports false on Unix systems other than macOS. On Linux a POSIX
// access list is reflected in the mode bits (see checkPrivate), so the mode
// check already covers it. On the BSDs, which no release of NoPACs
// targets, access lists are not read.
func permitACL(*os.File) (bool, error) {
	return false, nil
}
