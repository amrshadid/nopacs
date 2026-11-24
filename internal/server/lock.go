package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/amrshadid/nopacs/internal/fault"
)

// LockName is the lock file's name inside <data-dir>/run.
const LockName = "serve.lock"

// maxLockFile bounds what is read of a lock file to name its holder: a
// process id is a few bytes.
const maxLockFile = 64

// Lock is the hold one server has on a data directory, so a second server is
// refused instead of writing the same databases and making the first one's
// calls wait on its locks: an operating system file lock, taken at start and
// held for the life of the process. The system drops it when the process
// ends, however it ends, including a kill with signal 9 or a panic, so it is
// never left stale; the file itself stays, and a later start locks it again.
// The process id written in the file only names the holder in the refusal of
// a second server; it decides nothing.
type Lock struct {
	path string
	once sync.Once
	f    *os.File
}

// TakeLock locks the data directory dataDir for this process, making
// <data-dir>/run private first. When another process holds the lock it
// refuses with Conflict, naming that process's id when the file holds one.
func TakeLock(dataDir string) (*Lock, error) {
	dir := RunDir(dataDir)
	if err := EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockName)
	f, held, err := lockFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "server: cannot lock "+path)
	}
	if held {
		msg := "server: another nopacs-server serve is running on the data directory " + dataDir
		if pid := holderPID(path); pid != "" {
			msg += " (process " + pid + ")"
		}
		return nil, fault.New(fault.Conflict, msg+"; stop it first, or use another --data-dir")
	}
	// The pid is written after the lock is held; a reader racing the write
	// at worst names no process.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{path: path, f: f}, nil
}

// Path returns the lock file's path.
func (l *Lock) Path() string { return l.path }

// Release empties the lock file and drops the lock. The process id is
// emptied first so the file names no process once none holds it, and so a
// start that is refused leaves the data directory as it found it. It is
// safe to call more than once and on a nil Lock.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() {
		_ = l.f.Truncate(0)
		err = unlockFile(l.f)
	})
	return err
}

// holderPID returns the process id written in the lock file, or "" when it
// cannot be read or is not a number.
func holderPID(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: the server's own lock file.
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxLockFile)
	n, _ := f.Read(buf)
	s := string(bytes.TrimSpace(buf[:n]))
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return ""
	}
	return s
}
