//go:build !windows

package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/amrshadid/nopacs/internal/fault"
)

// EnsurePrivateDir makes dir, mode 0700, when it does not exist; when it
// does, it refuses it unless it is a directory (not a link to one) owned by
// the effective user with no permission for the group or others, and says
// how to fix it. A directory another user can enter could let that user
// reach the local socket, and one another user can write could let that
// user swap the socket or the lock for their own. The parent must exist.
func EnsurePrivateDir(dir string) error {
	err := os.Mkdir(dir, 0o700)
	switch {
	case err == nil:
		// The umask may have taken bits away, never added them; Chmod states
		// the mode exactly anyway.
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; 0700 is private to the owner.
			return fault.Wrap(fault.Internal, err, "server: cannot set the mode of "+dir)
		}
		return nil
	case !errors.Is(err, fs.ErrExist):
		return fault.Wrap(fault.Internal, err, "server: cannot make the directory "+dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "server: cannot look at "+dir)
	}
	if !info.IsDir() {
		return fault.New(fault.Invalid, "server: "+dir+" is not a directory (a link is not accepted); move it away")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fault.New(fault.Invalid, fmt.Sprintf("server: %s belongs to user %d, not to the user running nopacs (%d); fix with: chown %d %s",
			dir, st.Uid, os.Geteuid(), os.Geteuid(), dir))
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fault.New(fault.Invalid, fmt.Sprintf("server: %s may be used by other users (mode %04o); make it private with: chmod 700 %s",
			dir, perm, dir))
	}
	return nil
}

// ensurePrivateDir is EnsurePrivateDir, under the name the listener uses.
func ensurePrivateDir(dir string) error { return EnsurePrivateDir(dir) }

// restrictSocket makes the socket file readable and writable by its owner
// only. Linux checks write permission on the socket file at connect
// (unix(7)); other systems may not, which is why the directory is the
// guard everywhere.
func restrictSocket(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fault.Wrap(fault.Internal, err, "server: cannot set the mode of the local socket "+path)
	}
	return nil
}

// isSocketFile reports whether info is a socket.
func isSocketFile(info fs.FileInfo) bool { return info.Mode()&fs.ModeSocket != 0 }

// dirGuard says how the local socket's directory is kept private on this
// system.
const dirGuard = "mode 0700, owned by the server's user"
