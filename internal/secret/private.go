package secret

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
)

// CheckPrivateDir refuses a directory in which another user could replace a
// private file: on Linux and macOS one that is not owned by the current user,
// that the group or other users can write (with noRead, also read), or that
// has a macOS access list entry granting anything. It is skipped on Windows,
// where the files made by CreatePrivateTemp carry their own access list. A
// missing directory is NotFound. The message says what is wrong and not how
// to fix it, which depends on what the directory is for.
func CheckPrivateDir(dir string, noRead bool) error { return system.checkPrivateDir(dir, noRead) }

// CreatePrivateTemp creates a new file in dir whose name is pattern with its
// first "*" replaced by random characters (appended when there is none), that
// only the current user can open, from its first byte: mode 0600 on Linux and
// macOS, and on Windows a protected access list naming only the user (the
// directory's inherited entries, which OpenPrivate would refuse, are not
// passed on). It never opens an existing file. The caller writes, syncs and
// closes it, and renames or removes it.
func CreatePrivateTemp(dir, pattern string) (*os.File, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return nil, fault.New(fault.Internal, "secret: a temporary file's pattern holds a separator")
	}
	for range 10 {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return nil, fault.Wrap(fault.Internal, err, "secret: no random name could be made")
		}
		name := hex.EncodeToString(r[:])
		if i := strings.Index(pattern, "*"); i >= 0 {
			name = pattern[:i] + name + pattern[i+1:]
		} else {
			name = pattern + name
		}
		f, err := createPrivate(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "secret: a private file cannot be created in "+dir)
		}
		return f, nil
	}
	return nil, fault.New(fault.Internal, "secret: no unused name for a private file in "+dir)
}
