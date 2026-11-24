//go:build darwin

package dir

import (
	"errors"
	"syscall"
)

// isVanished reports whether err is what macOS answers when a directory is
// removed while a name is made or moved in it: besides ENOENT, it answers
// EINVAL from mkdirat and renameat in that case, observed under a race of
// a removal against a creation, where Linux answers ENOENT alone.
func isVanished(err error) bool { return errors.Is(err, syscall.EINVAL) }
