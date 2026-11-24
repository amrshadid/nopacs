package dir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// tempDir is the directory at the root where every Put writes its bytes
// before renaming them into place. Its name begins with blob.ReservedPrefix,
// which no key segment may, so no key can name, list or remove it or
// anything in it.
const tempDir = blob.ReservedPrefix + "tmp"

// The modes of what the driver makes. Objects are patient data after
// de-identification and still not for other users of the machine: only the
// user the server runs as may read them.
const (
	fileMode = 0o600
	dirMode  = 0o700
)

// copyBufferSize is the size of the buffer a Put copies through. It is
// large enough that a 256 MB object takes a few hundred writes, and small
// enough that many Puts at once hold little memory.
const copyBufferSize = 1 << 20

var copyBuffers = sync.Pool{New: func() any {
	b := make([]byte, copyBufferSize)
	return &b
}}

// Store is a blob.Store on a directory. Make one with Open.
type Store struct {
	root      *os.Root
	rootPaths []string // the root as given and as an absolute path, kept out of every error
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
	durable   durableDirs
	listing   dirCache
	// readOnly is set by OpenForReading: Put and Delete are refused.
	readOnly bool
	steps
}

// steps are the calls whose order makes a Put whole and durable, and the
// opening and reading of files and directories, as fields so that a test
// in this package can watch them, count them or make one fail. Open uses
// realSteps.
type steps struct {
	syncFile func(*os.File) error
	syncDir  func(root *os.Root, dir string) error
	rename   func(root *os.Root, from, to string) error
	mkdir    func(root *os.Root, dir string) error
	// open opens a file or a directory for reading, without blocking on a
	// FIFO or a device that a name may have been swapped for.
	open    func(root *os.Root, name string) (*os.File, error)
	readDir func(f *os.File, n int) ([]fs.DirEntry, error)
	// sameFS reports whether a and b, examined with Lstat, are on one file
	// system, and whether the system can tell at all.
	sameFS func(a, b fs.FileInfo) (same, known bool)
}

func realSteps() steps {
	return steps{
		syncFile: (*os.File).Sync,
		syncDir:  syncDirectory,
		rename:   renameRetrying,
		mkdir:    func(root *os.Root, dir string) error { return root.Mkdir(dir, dirMode) },
		open: func(root *os.Root, name string) (*os.File, error) {
			return root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
		},
		readDir: (*os.File).ReadDir,
		sameFS:  sameFileSystem,
	}
}

var _ blob.Store = (*Store)(nil)

// Open opens the storage at the directory path, makes its temporary
// directory if it is missing, checks that the file system can make a rename
// durable, and removes every temporary file a crash left. It reads the
// temporary directory and nothing else, so its cost does not grow with the
// number of objects.
//
// It refuses, as Invalid, a path that does not exist, that is not a
// directory, or in which this process cannot create a file; a temporary
// directory that is not a plain directory or, where the system can tell, is
// on another file system than the root; and a file system that cannot sync
// a directory. A missing directory is not made: a storage on a volume that
// is not mounted would otherwise be written to the empty mount point, on
// another disk, and look healthy until the volume came back. The path is
// the operator's and may be a symbolic link; nothing below it is followed.
// No error names the path.
func Open(dirPath string) (*Store, error) {
	return open(dirPath, realSteps())
}

// OpenForReading opens the storage at the directory path for Read, Stat,
// List and Usage only; Put and Delete are refused. It writes nothing: no
// temporary directory is made, no file is probed and nothing a crash left
// is removed. Open removes every file in the temporary directory, which is
// right for the one store that writes after a crash and wrong for a reader
// opened beside it: a reader's open would remove the temporary file of a
// Put in progress, or the probe file of another Open, and fail that Put or
// that Open. A read must not need the right to write either. It refuses
// what Open refuses about the path itself: a path that does not exist or
// is not a directory.
func OpenForReading(dirPath string) (*Store, error) {
	s, err := openRoot(dirPath, realSteps())
	if err != nil {
		return nil, err
	}
	s.readOnly = true
	return s, nil
}

// OpenBeside is Open for a store opened beside the one that writes after a
// crash: it makes and checks the temporary directory and probes it as Open
// does, but removes nothing a crash left there, since every file in it may
// be a Put in progress of the store already open (Open's removal would
// fail that Put). Every store a process opens on a storage after its first
// opens with it (place.Cleanups), as does a user that writes a few objects
// while the server's own stores run (an owner record taken one generation
// further); only the first store of a process opens with Open, which does
// the cleanup.
func OpenBeside(dirPath string) (*Store, error) {
	return openWith(dirPath, realSteps(), false)
}

// errReadOnly answers a Put or a Delete on a store opened for reading: the
// caller's mistake, never the storage's.
var errReadOnly = fault.New(fault.Internal, "the storage was opened for reading only")

// open is Open with the steps given, for a test that watches them.
func open(dirPath string, st steps) (*Store, error) {
	return openWith(dirPath, st, true)
}

// openWith is Open with the steps given, removing what a crash left in the
// temporary directory only when clean is set (OpenBeside).
func openWith(dirPath string, st steps, clean bool) (*Store, error) {
	s, err := openRoot(dirPath, st)
	if err != nil {
		return nil, err
	}
	todo := []func() error{s.prepareTemp, s.checkDirSync, s.probe}
	if clean {
		todo = append(todo, s.cleanTemp)
	}
	for _, step := range todo {
		if err := step(); err != nil {
			_ = s.root.Close() // The step's error is the one that matters.
			return nil, err
		}
	}
	return s, nil
}

// openRoot opens the directory at dirPath as a store's root, changing
// nothing in it.
func openRoot(dirPath string, st steps) (*Store, error) {
	s := &Store{steps: st}
	s.rootPaths = []string{dirPath}
	if abs, err := filepath.Abs(dirPath); err == nil && abs != dirPath {
		s.rootPaths = append(s.rootPaths, abs)
	}
	info, err := os.Stat(dirPath) // #nosec G703 -- the operator's own choice of storage directory, only examined.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, s.wrap(fault.Invalid, err, "the storage's directory does not exist; make it, or mount its volume")
	case err != nil:
		return nil, s.wrapUnreachable(err, "the storage's directory cannot be examined")
	case !info.IsDir():
		return nil, fault.New(fault.Invalid, "the storage's path is not a directory")
	}
	root, err := os.OpenRoot(dirPath)
	if err != nil {
		return nil, s.wrapUnreachable(err, "the storage's directory cannot be opened")
	}
	s.root = root
	s.durable.init()
	s.listing.init()
	return s, nil
}

// prepareTemp makes the temporary directory when it is missing and checks
// that it is a plain directory on the root's file system.
//
// A rename cannot cross file systems (rename(2): EXDEV), and copying instead
// would not be whole or nothing, so the temporary directory and the objects
// must share one. Where the system gives a device number (Unix), a temporary
// directory on another device than the root, such as a mount, is refused
// here. A directory below the root that is a mount cannot be found without
// walking the tree; a Put into it fails at its rename, saying why.
func (s *Store) prepareTemp() error {
	info, err := s.root.Lstat(tempDir)
	if notFound(err) {
		if err := s.mkdir(s.root, tempDir); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrPermission) {
				return s.wrap(fault.Invalid, err, "the storage's directory cannot be written by the user nopacs runs as")
			}
			return s.wrap(fault.Unavailable, err, "the storage could not make its temporary directory")
		}
		info, err = s.root.Lstat(tempDir)
	}
	if err != nil {
		return s.wrap(fault.Unavailable, err, "the storage could not examine its temporary directory")
	}
	if info.Mode().Type() != fs.ModeDir {
		return fault.New(fault.Invalid, "the storage's temporary directory "+tempDir+" is not a plain directory (a link, a mount point or a file); remove it")
	}
	rootInfo, err := s.root.Lstat(".")
	if err != nil {
		return s.wrap(fault.Unavailable, err, "the storage could not examine its directory")
	}
	if same, known := s.sameFS(rootInfo, info); known && !same {
		return fault.New(fault.Invalid, "the storage's temporary directory "+tempDir+" is on another file system than the storage's directory; a storage's directory must be one file system")
	}
	return nil
}

// checkDirSync syncs the root and the temporary directory once. Besides
// making the temporary directory's own entry durable, it finds out whether
// the file system can sync a directory at all: some network file systems
// answer EINVAL, and on such a one no rename could be made durable, so
// every Put would fail after its object was already in place. Refusing the
// storage at Open says so once, instead of at every Put.
func (s *Store) checkDirSync() error {
	for _, dir := range []string{".", tempDir} {
		if err := s.syncDir(s.root, dir); err != nil {
			if isUnsupported(err) {
				return s.wrap(fault.Invalid, err, "the storage's file system cannot sync a directory, so it cannot make a rename durable; use a file system that can")
			}
			return s.wrap(fault.Unavailable, err, "the storage could not sync its directory")
		}
	}
	return nil
}

// probe creates, syncs and removes a file in the temporary directory, so
// that a root this process cannot write is refused at Open and not at the
// first reception.
func (s *Store) probe() error {
	name, err := tempName()
	if err != nil {
		return err
	}
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return s.wrap(fault.Invalid, err, "the storage's directory cannot be written by the user nopacs runs as")
		}
		return s.wrap(fault.Unavailable, err, "the storage's directory cannot be written")
	}
	_, werr := f.Write([]byte{0})
	serr := f.Sync()
	cerr := f.Close()
	rerr := s.root.Remove(name)
	if err := errors.Join(werr, serr, cerr, rerr); err != nil {
		return s.wrap(fault.Unavailable, err, "the storage's directory cannot be written")
	}
	return nil
}

// Close closes the root. A stream from Read that is still open stays
// readable. Closing again returns what the first Close did.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if err := s.root.Close(); err != nil {
			s.closeErr = s.wrap(fault.Unavailable, err, "the storage could not be closed")
		}
	})
	return s.closeErr
}

// beginWrite is begin for a call that writes, which a store opened for
// reading refuses.
func (s *Store) beginWrite(ctx context.Context) error {
	if s.readOnly {
		return errReadOnly
	}
	return s.begin(ctx)
}

// begin refuses a call on a closed store or with an ended context.
func (s *Store) begin(ctx context.Context) error {
	if s.closed.Load() {
		return fault.New(fault.Unavailable, "the storage is closed")
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the storage call was canceled")
	}
	return nil
}

// Delete removes key; see blob.Store. It removes the one object and
// nothing else: the directory it was in stays, since removing it could
// race a Put into it (see Put), and an empty directory is never listed. A
// symbolic link where the object would be is not an object, and is left
// alone.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.beginWrite(ctx); err != nil {
		return err
	}
	if err := blob.CheckKey(key); err != nil {
		return err
	}
	if _, err := s.lstatObject(key); err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return nil
		}
		return err
	}
	dir := parent(key)
	if err := removeRetrying(s.root, native(key)); err != nil && !notFound(err) {
		return s.wrap(fault.Unavailable, err, "the storage could not remove the object")
	}
	s.listing.invalidate(dir)
	// Without it a crash could bring the object back.
	if err := s.syncDir(s.root, native(dir)); err != nil && !notFound(err) {
		return s.wrap(fault.Unavailable, err, "the storage removed the object but could not sync its directory")
	}
	return nil
}

// Purge removes key's file; see blob.Store. A directory keeps no versions,
// so an object that was there answers one Removal, removed, once its
// directory is synced: the bytes are gone from the storage (a copy the
// file system or the host keeps of its own, a snapshot, is outside it).
func (s *Store) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	if err := s.beginWrite(ctx); err != nil {
		return nil, err
	}
	if err := blob.CheckKey(key); err != nil {
		return nil, err
	}
	if _, err := s.lstatObject(key); err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return nil, nil
		}
		return nil, err
	}
	if err := s.Delete(ctx, key); err != nil {
		return nil, err
	}
	return []blob.Removal{{Disposition: blob.Removed}}, nil
}

// PurgeExpected implements blob.PurgeExpecter: a directory removes.
func (s *Store) PurgeExpected(context.Context) (blob.Disposition, string) {
	return blob.Removed, "directory"
}

// Usage reports the space of the volume the root is on; see blob.Store.
func (s *Store) Usage(ctx context.Context) (blob.Usage, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Usage{}, err
	}
	u, err := volumeUsage(s.root.Name())
	if err != nil {
		return blob.Usage{}, s.wrap(fault.Unavailable, err, "the storage could not read its volume's free space")
	}
	return u, nil
}

// fail turns an error from the file system into one with a kind.
func (s *Store) fail(err error, message string) error {
	var fe *fault.Error
	switch {
	case errors.As(err, &fe):
		return err
	case errors.Is(err, os.ErrClosed) || s.closed.Load():
		return s.wrap(fault.Unavailable, err, "the storage is closed")
	case notFound(err):
		return notFoundError()
	}
	return s.wrap(fault.Unavailable, err, message)
}

func notFoundError() error {
	return fault.New(fault.NotFound, "the storage holds no object at this key")
}

// notFound reports whether err means that nothing is at a path: it does not
// exist, or a directory on the way to it is a file.
func notFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// goneMidway reports whether err, from a call that makes or moves a name in
// a directory, means that the directory itself was removed during the call:
// notFound, or what macOS answers in that case (see isVanished).
func goneMidway(err error) bool {
	return notFound(err) || isVanished(err)
}

// parent returns the key of the directory key is in, "." at the top.
func parent(key string) string {
	return path.Dir(key)
}

// native returns key as a path of this system, relative to the root.
func native(key string) string {
	return filepath.FromSlash(key)
}

// tempName returns a new temporary file's path relative to the root, in the
// temporary directory. Its 64 random bits make a clash between two Puts too
// unlikely to plan for, and O_EXCL refuses one anyway.
func tempName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fault.Wrap(fault.Internal, err, "no random bytes for a temporary name")
	}
	return filepath.Join(tempDir, hex.EncodeToString(b[:])), nil
}

// renameRetrying renames from to to, trying again for a short while when
// the system says a file is busy (see isBusy), which on Windows a virus
// scanner or an indexer holding the old file causes for moments at a time.
// On other systems isBusy is always false and this is one rename.
func renameRetrying(root *os.Root, from, to string) error {
	return retryBusy(func() error { return root.Rename(from, to) })
}

// removeRetrying removes name, trying again as renameRetrying does.
func removeRetrying(root *os.Root, name string) error {
	return retryBusy(func() error { return root.Remove(name) })
}

func retryBusy(op func() error) error {
	wait := time.Millisecond
	for {
		err := op()
		if err == nil || !isBusy(err) || wait > time.Second {
			return err
		}
		time.Sleep(wait)
		wait *= 2
	}
}
