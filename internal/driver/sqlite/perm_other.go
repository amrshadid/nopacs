//go:build !unix

package sqlite

import (
	"errors"
	"io/fs"
	"os"

	"github.com/amrshadid/nopacs/internal/fault"
)

// checkPrivate does not check modes where there are no Unix modes. On Windows
// a file's access is its access list, inherited from its directory, so the
// directory the operator chooses decides who can open the database; reading
// and checking that list is not done here yet (the key file does it in
// package secret, and the same check may be shared here later).
//
// It does refuse anything at the database's path, or its -wal or -shm path,
// that is not a regular file, examined with Lstat, so a symbolic link
// planted there is not followed, as on Unix.
func checkPrivate(path string, name dbName) error {
	for _, f := range []struct{ path, what string }{
		{path, "the " + string(name) + " file"},
		{path + "-wal", "the " + string(name) + "'s -wal file"},
		{path + "-shm", "the " + string(name) + "'s -shm file"},
	} {
		info, err := os.Lstat(f.path) // #nosec G703 -- the operator's own choice of database path, only examined.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fault.New(fault.Unavailable, f.what+" cannot be checked")
		}
		if !info.Mode().IsRegular() {
			return fault.New(fault.Invalid, f.what+" is not a regular file")
		}
	}
	return nil
}
