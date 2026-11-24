package place

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/pathname"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MaxDirectoryLength bounds the path of a directory storage's directory on
// the server, root, tenant and path joined, in bytes, after the root's
// symbolic links are followed. The directory driver hands this path whole to
// the system when it opens the storage (stat, then open), and macOS refuses
// a path of more than 1023 bytes (PATH_MAX, 1024 with its final NUL) with
// ENAMETOOLONG; Linux allows 4095, and on Windows Go passes a long absolute
// path with the \\?\ prefix, which allows about 32,000 characters. A key
// below the directory does not count toward it: the driver reaches every
// file through an os.Root, which the system walks one segment at a time,
// each segment at most 255 bytes (blob.MaxSegmentLength), whatever the
// length of the whole. So a storage this bound accepts can be opened on
// every one of the three systems, and its files written whatever their keys.
const MaxDirectoryLength = 1023

// directoryMode is the mode of a directory the server makes for a storage:
// only the user the server runs as may list, enter or write it, so no other
// local user reads a tenant's archive.
const directoryMode = 0o700

// Directory is where a directory storage's directory is on the server.
type Directory struct {
	// root is the root's directory, every symbolic link in the root's own
	// path resolved: the root's path is the installation's to choose, and
	// may pass through one.
	root string
	// segments are the directory's below the root: the tenant's name, then
	// the storage's path.
	segments []string
	// existing is how many of the segments exist, from the first.
	existing int
}

// Path returns the directory's path. It is the installation's and is never
// put in an error or a log.
func (d Directory) Path() string {
	return filepath.Join(append([]string{d.root}, d.segments...)...)
}

// Exists reports whether the directory itself exists.
func (d Directory) Exists() bool { return d.existing == len(d.segments) }

// errNoRoot answers a root the installation does not list.
func errNoRoot(name string) error {
	return fault.New(fault.Precondition, "storage.directory.root: the installation lists no directory root named "+name)
}

// Resolve returns where the directory of a directory storage of the tenant
// named tenantName is, under the installation's roots, and refuses, as a
// precondition, a root the installation does not list, that is not there,
// or that lies at, inside or around another root once both are resolved
// (resolveRoot); a directory whose path would pass MaxDirectoryLength; and a
// path below the root that passes through a symbolic link or a file. Each
// existing segment is looked at with Lstat, which follows no link, so a link
// anywhere below the root is found whatever it points at, inside the root or
// outside it. No message holds a path.
//
// A link made later, after the storage is added, is found the same way:
// Opener.Open runs this again every time it opens the directory, and the
// directory driver then follows no link below the directory it is given
// (package blob/dir). A link swapped in between this check and the open is
// the window left; the driver's os.Root still keeps every file it opens
// inside the directory it opened.
func Resolve(roots settings.DirectoryRoots, tenantName string, ds tenant.DirectoryStorage) (Directory, error) {
	root, ok := roots.Lookup(ds.Root)
	if !ok {
		return Directory{}, errNoRoot(ds.Root)
	}
	real, err := resolveRoot(roots, root)
	if err != nil {
		return Directory{}, err
	}
	d := Directory{root: real, segments: append([]string{tenantName}, tenant.DirectorySegments(ds.Path)...)}
	if len(d.Path()) > MaxDirectoryLength {
		// The length itself is not said: with the path's, it would tell the
		// root's.
		return Directory{}, fault.New(fault.Precondition, "storage.directory.path: the directory's path on this server, its root's included, would be longer than the "+
			strconv.Itoa(MaxDirectoryLength)+" bytes every system the server runs on can open; choose a shorter path")
	}
	cur := real
	for _, seg := range d.segments {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return d, nil
		case err != nil:
			return Directory{}, fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not examine the storage's directory")
		case info.Mode()&fs.ModeSymlink != 0:
			return Directory{}, fault.New(fault.Precondition, "storage.directory.path: the directory's path passes through a symbolic link below the root, which is refused")
		case !info.IsDir():
			return Directory{}, fault.New(fault.Precondition, "storage.directory.path: a file that is not a directory is where the directory's path goes")
		}
		d.existing++
	}
	return d, nil
}

// CheckDirectory checks the directory a directory storage s of the tenant
// named tenantName is to be at, as adding the storage, or changing where its
// files are, does: everything Resolve checks, the directory absent or empty,
// and no other directory storage of the tenant (others; s itself, by name,
// is passed over, so a storage may move inside its own former directory) at,
// inside or around it once resolved. It returns the directory, to be made
// (Directory.Make) once every other check has passed.
func CheckDirectory(roots settings.DirectoryRoots, tenantName string, s tenant.Storage, others []tenant.Storage) (Directory, error) {
	d, err := Resolve(roots, tenantName, s.Directory)
	if err != nil {
		return Directory{}, err
	}
	if err := d.checkEmpty(); err != nil {
		return Directory{}, err
	}
	mine := segments(d.Path())
	for _, o := range others {
		if o.Type != tenant.StorageDirectory || o.Name == s.Name {
			continue
		}
		root, ok := roots.Lookup(o.Directory.Root)
		if !ok || !filepath.IsAbs(root.Path) {
			continue
		}
		real, err := filepath.EvalSymlinks(root.Path)
		if err != nil {
			continue
		}
		other := filepath.Join(append([]string{real, tenantName}, tenant.DirectorySegments(o.Directory.Path)...)...)
		if pathname.Nest(mine, segments(other)) {
			return Directory{}, fault.New(fault.Precondition, "storage.directory: the directory is, contains or lies inside the directory of the storage "+o.Name)
		}
	}
	return d, nil
}

// checkEmpty refuses, as a precondition, a directory that exists and holds
// anything: a storage is added on a directory that is absent or empty, so it
// never takes over files it did not write.
func (d Directory) checkEmpty() error {
	if !d.Exists() {
		return nil
	}
	f, err := os.Open(d.Path())
	if err != nil {
		return fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not read the storage's directory")
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	switch {
	case errors.Is(err, io.EOF) || (err == nil && len(names) == 0):
		return nil
	case err != nil:
		return fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not read the storage's directory")
	}
	return fault.New(fault.Precondition, "storage.directory: the directory is not empty; a storage is added on a directory that is absent or empty")
}

// Make makes the segments of d that do not exist, each with directoryMode,
// through an os.Root on the root, which refuses a path that would leave it.
// Each segment is looked at again after it is made, with Lstat, so a link
// that took a segment's place meanwhile is refused.
func (d Directory) Make() error {
	r, err := os.OpenRoot(d.root)
	if err != nil {
		return fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not open the root's directory")
	}
	defer func() { _ = r.Close() }()
	for i := d.existing; i < len(d.segments); i++ {
		rel := filepath.Join(d.segments[:i+1]...)
		if err := r.Mkdir(rel, directoryMode); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrPermission) {
				return fault.Wrap(fault.Precondition, pathless(err), "storage.directory: the server may not make the storage's directory under the root")
			}
			return fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not make the storage's directory")
		}
		info, err := r.Lstat(rel)
		if err != nil {
			return fault.Wrap(fault.Unavailable, pathless(err), "storage.directory: the server could not examine the directory it made")
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return fault.New(fault.Precondition, "storage.directory.path: the directory's path passes through a symbolic link below the root, which is refused")
		}
	}
	return nil
}
