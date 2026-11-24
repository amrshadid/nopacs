//go:build unix && !darwin

package dir

// isVanished is false on Unix systems other than macOS, which report a
// directory removed under a call as ENOENT, already recognized as not
// found.
func isVanished(error) bool { return false }
