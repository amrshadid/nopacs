//go:build unix

package sqlite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/amrshadid/nopacs/internal/fault"
)

// checkPrivate refuses a database that must not be used, by the same rule
// as the installation's key file: the database file, its -wal and -shm files
// when they exist, and its directory must belong to the user nopacs runs
// as; nobody else may read or write the files, and nobody else may write the
// directory, where they could replace the file or plant a -wal file of their
// own. The master holds key hashes and sealed database addresses, and a user
// who can write the file can grant themselves any role; a tenant's database
// holds its catalog and its audit log. A directory others
// can list is allowed, as for the key file: it shows the names, and the
// files themselves stay closed. A file that does not exist yet is made
// private by createPrivate.
//
// The message says how to fix it, without the path: a database address never
// appears in an error, and the operator knows the path they gave.
func checkPrivate(path string, name dbName) error {
	return checkPrivateAs(path, name, os.Geteuid())
}

// checkPrivateAs is checkPrivate for the files of the user euid: the
// process's own effective user, but in a test, which cannot give a file away
// without being root and so names another user instead.
func checkPrivateAs(path string, name dbName, euid int) error {
	dir := filepath.Dir(path)
	if err := checkOne(dir, true, "the "+string(name)+"'s directory", "chmod go-w", euid); err != nil {
		return err
	}
	for _, f := range []struct{ path, what string }{
		{path, "the " + string(name) + " file"},
		{path + "-wal", "the " + string(name) + "'s -wal file"},
		{path + "-shm", "the " + string(name) + "'s -shm file"},
	} {
		if err := checkOne(f.path, false, f.what, "chmod 600", euid); err != nil {
			return err
		}
	}
	return nil
}

// checkOne refuses path when it exists and belongs to a user other than euid, or when
// it gives the group or others read or write access (a file) or write access
// (a directory). A path that does not exist is fine, except a directory,
// which must.
//
// A file is examined with Lstat, so a symbolic link planted where the
// database or its -wal or -shm file belongs is refused as "not a regular
// file" instead of being followed to a file of the planter's choosing. The
// directory is followed: a data directory that is a link the operator made is
// theirs to make.
func checkOne(path string, isDir bool, what, fix string, euid int) error {
	stat := os.Lstat
	if isDir {
		stat = os.Stat
	}
	info, err := stat(path) // #nosec G703 -- the operator's own choice of database path, only examined.
	if errors.Is(err, fs.ErrNotExist) && !isDir {
		return nil
	}
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return fault.New(fault.Unavailable, what+" cannot be checked: "+pathErr.Err.Error())
		}
		return fault.New(fault.Unavailable, what+" cannot be checked")
	}
	if isDir && !info.IsDir() {
		return fault.New(fault.Invalid, what+" is not a directory")
	}
	if !isDir && !info.Mode().IsRegular() {
		return fault.New(fault.Invalid, what+" is not a regular file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int64(st.Uid) != int64(euid) {
		return fault.New(fault.Invalid, what+" belongs to another user; run nopacs as its owner, or chown it to the user nopacs runs as")
	}
	if isDir && info.Mode().Perm()&0o022 != 0 {
		return fault.New(fault.Invalid, what+" can be written by users other than its owner ("+info.Mode().Perm().String()+"); run "+fix+" on it")
	}
	if !isDir && info.Mode().Perm()&0o066 != 0 {
		return fault.New(fault.Invalid, what+" can be read or written by users other than its owner ("+info.Mode().Perm().String()+"); run "+fix+" on it")
	}
	return nil
}
