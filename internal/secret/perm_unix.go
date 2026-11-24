//go:build unix

package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/amrshadid/nopacs/internal/fault"
)

// privateMode is the only access a key file may give: read and write for its
// owner, nothing for the group or anyone else.
const privateMode = 0o600

// euid returns the user a private file and its directory must belong to:
// the process's effective user, unless a test names another.
func (fsys fileSystem) euid() int {
	if fsys.uid != nil {
		return fsys.uid()
	}
	return os.Geteuid()
}

// createPrivate creates a new file at path that only its owner may open. It
// fails if anything exists at path (O_EXCL), so it never opens a file or a
// symbolic link someone else placed there. The umask can only remove bits
// from 0600, never add them.
func createPrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateMode) //nolint:gosec // G304: the path is built by CreateKeyFile from the operator's own configuration.
}

// openKeyFile opens path for reading without blocking. Opening a FIFO for
// reading blocks until a writer appears, which would hang the server at
// start; with O_NONBLOCK the open returns at once and checkPrivate refuses
// the FIFO as not a regular file. On a regular file O_NONBLOCK changes
// nothing.
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the path is the operator's own configuration, read on purpose.
}

// checkPrivate refuses a key file that is not a regular file, that belongs
// to another user, whose mode gives the group or other users any access, or
// (on macOS) that carries an access list entry granting anything. Write
// access counts as much as read access: a user who can replace the key can
// make the archive seal new secrets under a key they know.
//
// On Linux a POSIX access list needs no separate check: a file with one
// shows the list's mask in its group mode bits, and an entry for another
// user or group grants nothing beyond that mask, so mode 0600 means no
// entry grants anything. macOS keeps its access lists apart from the mode,
// so permitACL reads them (acl_darwin.go).
func (fsys fileSystem) checkPrivate(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be checked")
	}
	if !info.Mode().IsRegular() {
		return fault.New(fault.Invalid, "secret: the key file "+path+" is not a regular file ("+info.Mode().Type().String()+")")
	}
	if err := checkOwner(info, path, "key file", "chown", fsys.euid()); err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fault.New(fault.Invalid, fmt.Sprintf(
			"secret: the key file %s may be opened by other users (mode %04o); "+
				"make it private to its owner with: chmod 600 %s", path, perm, path))
	}
	permit, err := permitACL(f)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the access list of the key file "+path+" cannot be read")
	}
	if permit {
		return fault.New(fault.Invalid, "secret: the key file "+path+" has an access list entry that grants access beyond its mode; "+
			"remove the access list with: chmod -N "+path)
	}
	return nil
}

// checkOwner refuses info unless it belongs to the user euid. A file
// owned by someone else is one they can make readable to anyone, or replace,
// whatever its mode says now.
func checkOwner(info fs.FileInfo, path, what, fix string, euid int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fault.New(fault.Internal, "secret: the owner of the "+what+" "+path+" cannot be read")
	}
	if uint64(st.Uid) != uint64(euid) { //nolint:gosec // G115: a uid is never negative; Geteuid returns -1 only on Windows.
		uid := strconv.FormatUint(uint64(st.Uid), 10)
		me := strconv.Itoa(euid)
		return fault.New(fault.Invalid, "secret: the "+what+" "+path+" is owned by uid "+uid+", not by uid "+me+
			" that nopacs runs as; give it to that user with: "+fix+" "+me+" "+path)
	}
	return nil
}

// checkDir refuses to keep a key file in a directory that another user could
// change: one owned by someone else, writable by its group or other users
// (which lets them rename the key file away and put their own in its place,
// even with the sticky bit, since they can then add a file of their own
// before it is created), or, on macOS, with an access list entry that grants
// anything. A missing directory is NotFound.
func (fsys fileSystem) checkDir(dir, path string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fault.Wrap(fault.NotFound, err, "secret: the directory of the key file "+path+" does not exist")
		}
		return fault.Wrap(fault.Internal, err, "secret: the directory of the key file "+path+" cannot be checked")
	}
	if !info.IsDir() {
		return fault.New(fault.Invalid, "secret: "+dir+", which should hold the key file "+path+", is not a directory")
	}
	if err := checkOwner(info, dir, "directory", "chown", fsys.euid()); err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fault.New(fault.Invalid, fmt.Sprintf(
			"secret: the directory %s that holds the key file may be changed by other users (mode %04o); "+
				"make it writable by its owner only with: chmod go-w %s", dir, perm, dir))
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the operator's own key file.
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the directory "+dir+" cannot be checked")
	}
	defer func() { _ = d.Close() }()
	permit, err := permitACL(d)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the access list of the directory "+dir+" cannot be read")
	}
	if permit {
		return fault.New(fault.Invalid, "secret: the directory "+dir+" that holds the key file has an access list entry that grants access beyond its mode; "+
			"remove the access list with: chmod -N "+dir)
	}
	return nil
}

// syncDir flushes the directory that holds a new key file, so the file's name
// survives a power loss as well as its contents: on Linux and macOS a new
// directory entry is durable only once the directory itself is synced.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the operator's own key file.
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// checkPrivateDir refuses a directory that another user could use to replace
// a private file in it: one owned by someone else, one the group or other
// users can write, with noRead also one they can read or enter, and on macOS
// one with an access list entry that grants anything. A missing directory is
// NotFound. The messages state the problem only; the caller knows what the
// directory is and how to fix it.
func (fsys fileSystem) checkPrivateDir(dir string, noRead bool) error {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fault.Wrap(fault.NotFound, err, "secret: the directory "+dir+" does not exist")
		}
		return fault.Wrap(fault.Internal, err, "secret: the directory "+dir+" cannot be checked")
	}
	if !info.IsDir() {
		return fault.New(fault.Invalid, "secret: "+dir+" is not a directory")
	}
	if err := checkOwner(info, dir, "directory", "chown", fsys.euid()); err != nil {
		return err
	}
	mask := fs.FileMode(0o022)
	if noRead {
		mask = 0o077
	}
	if perm := info.Mode().Perm(); perm&mask != 0 {
		return fault.New(fault.Invalid, fmt.Sprintf("secret: the directory %s can be reached by other users (mode %04o)", dir, perm))
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the person's own file.
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the directory "+dir+" cannot be checked")
	}
	defer func() { _ = d.Close() }()
	permit, err := permitACL(d)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the access list of the directory "+dir+" cannot be read")
	}
	if permit {
		return fault.New(fault.Invalid, "secret: the directory "+dir+" has an access list entry that grants access beyond its mode")
	}
	return nil
}
