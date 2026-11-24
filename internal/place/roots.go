package place

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxLinkDepth bounds the symbolic links followed while a root that does
// not exist yet is resolved, as the systems themselves bound a chain of
// links (Linux's 40, ELOOP), so a loop of links ends.
const maxLinkDepth = 40

// CheckRoots applies the rules of storage.directory-roots that need this
// server's file system, beside the rule of its text
// (settings.ParseDirectoryRoots): each root's path is absolute on this
// server's system (Invalid), and no two roots are one directory or lie one
// inside the other once every symbolic link in their paths is followed
// (Precondition, as it depends on what is on the disk). A root that does not
// exist yet is compared where it would be once made: its longest existing
// part resolved and the rest added, since what does not exist cannot be a
// link; whether it has become one is checked again whenever a root is used
// (Resolve). A path the server cannot examine is a precondition too: where
// it leads cannot be known. Two roots are compared by name and by the
// identity of their directories (overlap), so two spellings of one
// directory, or one directory reached through a bind mount, are one root.
// No message holds a path.
func CheckRoots(roots settings.DirectoryRoots) error {
	resolved := make([]string, len(roots))
	for i, r := range roots {
		if !filepath.IsAbs(r.Path) {
			return fault.New(fault.Invalid, "has a path for the root "+r.Name+" that is not absolute on this server's system")
		}
		real, err := resolveLoose(r.Path)
		if err != nil {
			return fault.Wrap(fault.Precondition, pathless(err), "has a path for the root "+r.Name+" that the server cannot examine, so where it leads cannot be known")
		}
		resolved[i] = real
		for j := range i {
			hit, err := overlap(resolved[j], real)
			if err != nil {
				return fault.Wrap(fault.Precondition, pathless(err), "has the roots "+roots[j].Name+" and "+r.Name+", which the server cannot examine, so whether they lie apart cannot be known")
			}
			if hit {
				return errRootsNest(roots[j].Name, r.Name)
			}
		}
	}
	return nil
}

// errRootsNest is the refusal of two roots that are one directory, or one
// inside the other, once their symbolic links are followed.
func errRootsNest(a, b string) error {
	return fault.New(fault.Precondition, "has the roots "+a+" and "+b+" in one directory or one inside the other once their symbolic links are followed on this server, which would let one tenant's directory hold another's")
}

// resolveRoot returns the directory the root's path stands for, every
// symbolic link in it followed, and refuses, as a precondition, a root whose
// path is not absolute on this server or not an existing directory, and one
// that is one directory with another root, or lies inside or around it,
// once both are resolved: the settings were checked when written, and a
// link can have changed since.
func resolveRoot(roots settings.DirectoryRoots, root settings.DirectoryRoot) (string, error) {
	if !filepath.IsAbs(root.Path) {
		return "", fault.New(fault.Precondition, "storage.directory.root: the root "+root.Name+" is not an absolute path on this server")
	}
	real, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		return "", fault.Wrap(fault.Precondition, pathless(err), "storage.directory.root: the directory of the root "+root.Name+" is not there on this server")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fault.New(fault.Precondition, "storage.directory.root: the root "+root.Name+" is not a directory on this server")
	}
	for _, o := range roots {
		if o.Name == root.Name || !filepath.IsAbs(o.Path) {
			// A root whose path is not absolute here can never be opened
			// here (above), so it holds nothing to share.
			continue
		}
		other, err := resolveLoose(o.Path)
		var hit bool
		if err == nil {
			hit, err = overlap(real, other)
		}
		if err != nil {
			return "", fault.Wrap(fault.Precondition, pathless(err), "storage.directory.root: the root "+o.Name+" cannot be examined on this server, so whether the root "+root.Name+" lies apart from it cannot be known; the installation must correct storage.directory-roots")
		}
		if hit {
			return "", fault.New(fault.Precondition, "storage.directory.root: the roots "+root.Name+" and "+o.Name+" are one directory or one inside the other once their symbolic links are followed on this server; the installation must correct storage.directory-roots")
		}
	}
	return real, nil
}

// resolveLoose returns where an absolute path leads, every symbolic link in
// it followed, and, for a path whose end does not exist, its longest
// existing part resolved with the rest added: a part that does not exist is
// not a link. A link whose target does not exist is followed to the target
// and resolved the same way.
func resolveLoose(p string) (string, error) {
	return resolveLooseDepth(filepath.Clean(p), 0)
}

func resolveLooseDepth(p string, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", errors.New("too many levels of symbolic links")
	}
	var tail []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		info, lerr := os.Lstat(p)
		switch {
		case lerr == nil && info.Mode()&fs.ModeSymlink != 0:
			// A link whose target is not there: where it points is where
			// the path leads once the target is made.
			target, err := os.Readlink(p)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			return resolveLooseDepth(filepath.Join(append([]string{target}, tail...)...), depth+1)
		case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
			return "", lerr
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}

// segments returns the segments of a clean absolute path on this system,
// its volume first ("" on Unix, "C:" or a share's name on Windows), so the
// path of a file system's root has its volume alone and contains every
// other path on that volume.
func segments(p string) []string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	out := []string{vol}
	for _, s := range strings.Split(p[len(vol):], string(filepath.Separator)) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pathless returns err with no path in its text, for a cause that goes to
// the log: a root's path is the installation's and a log line is read more
// widely than the settings. errors.Is still sees its cause.
func pathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fault.Wrap(fault.Internal, pe.Err, pe.Op)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fault.Wrap(fault.Internal, le.Err, le.Op)
	}
	return err
}
