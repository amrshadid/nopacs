package dir

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Read returns a range of key as a stream; see blob.Store.
func (s *Store) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	if err := blob.CheckKey(key); err != nil {
		return nil, err
	}
	if err := blob.CheckRange(offset, length); err != nil {
		return nil, err
	}
	dir, name, err := s.openKeyDir(key)
	if err != nil {
		return nil, err
	}
	f, info, err := s.openCheckedIn(dir, name, false)
	if dir != s.root {
		_ = dir.Close() // Only read; f stays open.
	}
	if err != nil {
		return nil, s.objectError(err, "the storage could not open the object")
	}
	size := info.Size()
	if offset > size {
		_ = f.Close() // Only read; the range error is the answer.
		return nil, blob.ErrPastEnd()
	}
	n := size - offset
	if length >= 0 && length < n {
		n = length
	}
	return &section{r: io.NewSectionReader(f, offset, n), f: f, s: s, base: offset}, nil
}

// section is a range of an open file that closes the file. Its errors are
// rewritten like every other error of the store, so none names the root.
type section struct {
	r *io.SectionReader
	f *os.File
	s *Store
	// base is the offset in the file of the range's first byte.
	base int64
}

// Remaining returns how many bytes of the range are not yet read: the
// range as Read cut it at the end of the file when it was opened.
func (r *section) Remaining() int64 {
	pos, _ := r.r.Seek(0, io.SeekCurrent) // A SectionReader's Seek to its current place never fails.
	return r.r.Size() - pos
}

// SendTo writes the next n bytes of the range to w, or the rest of it when
// fewer are left, and moves the range past what was written: what Read
// would have given. The bytes are copied from the file itself by io.Copy,
// so a writer whose ReadFrom takes a file (net/http's answer over a plain
// TCP or Unix connection) has the system copy them from file to socket
// (sendfile on Linux, macOS and the BSDs, TransmitFile on Windows) without
// their passing through this process. A file cut short since the open
// gives fewer bytes and no error, as Read gives io.EOF early.
func (r *section) SendTo(w io.Writer, n int64) (int64, error) {
	pos, _ := r.r.Seek(0, io.SeekCurrent) // As in Remaining.
	n = min(n, r.r.Size()-pos)
	if n <= 0 {
		return 0, nil
	}
	if _, err := r.f.Seek(r.base+pos, io.SeekStart); err != nil {
		return 0, r.s.wrap(fault.Unavailable, err, "the storage could not read the object")
	}
	sent, err := io.Copy(w, &io.LimitedReader{R: r.f, N: n})
	_, _ = r.r.Seek(pos+sent, io.SeekStart) // Within the section: sent is at most n.
	return sent, err
}

// Read reads the range.
func (r *section) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = r.s.wrap(fault.Unavailable, err, "the storage could not read the object")
	}
	return n, err
}

// Close closes the file under the stream.
func (r *section) Close() error {
	if err := r.f.Close(); err != nil {
		return r.s.wrap(fault.Unavailable, err, "the storage could not close the object")
	}
	return nil
}

// Errors of openChecked that are about what a name holds, not about the
// file system.
var (
	errWrongType     = errors.New("the name holds something of another type than was opened for")
	errLinked        = errors.New("the name is a symbolic link")
	errKeptReplacing = errors.New("the name kept being replaced while it was opened")
)

// openChecked opens name for reading and returns it with what the open
// descriptor is, only when that is a regular file (or, with wantDir, a
// directory) and is the very file the name holds, examined without
// following a link.
//
// The open comes first, and does not block (O_NONBLOCK on Unix), so a FIFO
// or a device swapped in for the name cannot hang it; the type is then
// taken from the descriptor, never from an earlier look at the name. The
// name is examined after the open and compared with the descriptor: a
// symbolic link there is refused, and an object replaced by a Put in
// between is opened again. Each try is spent only when a Put won that race,
// so the tries are not a timing guess.
func (s *Store) openChecked(name string, wantDir bool) (*os.File, fs.FileInfo, error) {
	return s.openCheckedIn(s.root, name, wantDir)
}

// openCheckedIn is openChecked of name in the directory dir, which is the
// root or a directory under it.
func (s *Store) openCheckedIn(dir *os.Root, name string, wantDir bool) (*os.File, fs.FileInfo, error) {
	for range 64 {
		f, err := s.open(dir, name)
		if err != nil {
			return nil, nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close() // Only read; the Stat error is the answer.
			return nil, nil, err
		}
		if (wantDir && !info.IsDir()) || (!wantDir && !info.Mode().IsRegular()) {
			_ = f.Close() // Only read; not what was asked for.
			return nil, nil, errWrongType
		}
		named, err := dir.Lstat(name)
		if notFound(err) {
			_ = f.Close() // Only read; removed or replaced since the open.
			continue
		}
		if err != nil {
			_ = f.Close() // Only read; the Lstat error is the answer.
			return nil, nil, err
		}
		if os.SameFile(named, info) {
			return f, info, nil
		}
		_ = f.Close() // Only read; the name now holds another file.
		if named.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, errLinked
		}
	}
	return nil, nil, errKeptReplacing
}

// openKeyDir opens the directory key is in and returns it, the root itself
// for a key at the top, with the object's name in it; the caller closes a
// directory that is not the root. It goes down one directory at a time,
// each examined with Lstat in the one above before it is opened, so a
// symbolic link planted where a directory belongs is refused, as
// checkDirs refuses it, and each name is looked up once: a lookup by the
// whole path from the root (checkDirs, and the root's own Lstat and open)
// goes through every directory above again, which for an instance's key
// was most of what a small read cost. A directory missing or a file is
// NotFound.
func (s *Store) openKeyDir(key string) (*os.Root, string, error) {
	dirs, name := path.Split(key)
	dir := s.root
	fail := func(err error) (*os.Root, string, error) {
		if dir != s.root {
			_ = dir.Close() // Only read.
		}
		if notFound(err) {
			return nil, "", notFoundError()
		}
		return nil, "", s.fail(err, "the storage could not examine the object's directories")
	}
	if dirs == "" {
		return dir, native(name), nil
	}
	for seg := range strings.SplitSeq(strings.TrimSuffix(dirs, "/"), "/") {
		info, err := dir.Lstat(seg)
		switch {
		case err != nil:
			return fail(err)
		case info.Mode()&fs.ModeSymlink != 0:
			return fail(linkError())
		case !info.IsDir():
			return fail(syscall.ENOTDIR)
		}
		next, err := dir.OpenRoot(seg)
		if err != nil {
			return fail(err)
		}
		if dir != s.root {
			_ = dir.Close() // Only read.
		}
		dir = next
	}
	return dir, native(name), nil
}

// objectError turns an error from opening or examining an object into one
// with a kind: nothing there, or something there that is not an object (a
// directory, a link, a FIFO), is NotFound.
func (s *Store) objectError(err error, message string) error {
	switch {
	case notFound(err), errors.Is(err, errWrongType), errors.Is(err, errLinked):
		return notFoundError()
	case errors.Is(err, errKeptReplacing):
		return fault.New(fault.Unavailable, "the object kept being replaced while it was opened")
	}
	return s.fail(err, message)
}

// lstatObject returns what is at key, examined without following a link, or
// NotFound when that is nothing, a directory, or not a regular file.
func (s *Store) lstatObject(key string) (fs.FileInfo, error) {
	dir, name, err := s.openKeyDir(key)
	if err != nil {
		return nil, err
	}
	info, err := dir.Lstat(name)
	if dir != s.root {
		_ = dir.Close() // Only read.
	}
	if err != nil {
		return nil, s.objectError(err, "the storage could not examine the object")
	}
	if !info.Mode().IsRegular() {
		return nil, notFoundError()
	}
	return info, nil
}

// Stat returns the size of key; see blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Info{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Info{}, err
	}
	info, err := s.lstatObject(key)
	if err != nil {
		return blob.Info{}, err
	}
	return blob.Info{Size: info.Size()}, nil
}
