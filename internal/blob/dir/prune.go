package dir

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"syscall"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// RemoveEmptyDirs implements blob.DirPruner: the directory of dir and every
// directory beneath it are removed bottom up, then each directory above it
// up to (not including) the top level, each only when it holds nothing.
// "Only when empty" is the file system's own rule for removing a
// directory, checked in the same call that removes it, so an object a Put
// renamed in meanwhile keeps its directory; a Put that made a directory
// this then removed finds it gone at its rename and makes it again (see
// moveIntoPlace). A symbolic link is never followed.
func (s *Store) RemoveEmptyDirs(ctx context.Context, dir string) error {
	if err := s.beginWrite(ctx); err != nil {
		return err
	}
	if err := blob.CheckKey(dir); err != nil {
		return err
	}
	if strings.HasPrefix(dir, blob.ReservedPrefix) {
		return fault.New(fault.Invalid, "the storage's own directories are not pruned")
	}
	if _, err := s.pruneBelow(ctx, dir); err != nil {
		return err
	}
	for p := parent(dir); strings.Contains(p, "/"); p = parent(p) {
		removed, err := s.removeIfEmpty(p)
		if err != nil || !removed {
			return err
		}
	}
	return nil
}

// pruneBelow removes the empty directories under d, then d itself when it
// is empty, and reports whether d is gone.
func (s *Store) pruneBelow(ctx context.Context, d string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fault.Wrap(fault.Unavailable, err, "the storage call was canceled")
	}
	info, err := s.root.Lstat(native(d))
	if notFound(err) {
		return true, nil
	}
	if err != nil {
		return false, s.wrap(fault.Unavailable, err, "the storage could not look at a directory")
	}
	if !info.IsDir() {
		return false, nil
	}
	f, err := s.open(s.root, native(d))
	if notFound(err) {
		return true, nil
	}
	if err != nil {
		return false, s.wrap(fault.Unavailable, err, "the storage could not open a directory")
	}
	entries, err := s.readDir(f, -1)
	_ = f.Close() // Read whole; a close error changes nothing read.
	if err != nil {
		return false, s.wrap(fault.Unavailable, err, "the storage could not list a directory")
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		if _, err := s.pruneBelow(ctx, d+"/"+e.Name()); err != nil {
			return false, err
		}
	}
	return s.removeIfEmpty(d)
}

// removeIfEmpty removes the directory d when it holds nothing, and reports
// whether it is gone; its parent is synced, so the removal survives a
// crash, and what this process knows of d is forgotten.
func (s *Store) removeIfEmpty(d string) (bool, error) {
	info, err := s.root.Lstat(native(d))
	if notFound(err) {
		return true, nil
	}
	if err != nil {
		return false, s.wrap(fault.Unavailable, err, "the storage could not look at a directory")
	}
	if !info.IsDir() {
		return false, nil
	}
	if err := removeRetrying(s.root, native(d)); err != nil {
		switch {
		case notFound(err):
			return true, nil
		case notEmpty(err):
			return false, nil
		}
		return false, s.wrap(fault.Unavailable, err, "the storage could not remove an empty directory")
	}
	s.durable.forgetPath(d)
	s.listing.invalidate(d)
	s.listing.invalidate(parent(d))
	if err := s.syncDir(s.root, native(parent(d))); err != nil && !notFound(err) {
		return true, s.wrap(fault.Unavailable, err, "the storage removed a directory but could not sync its parent")
	}
	return true, nil
}

// notEmpty reports whether err is a directory's removal refused because it
// holds something.
func notEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, fs.ErrExist)
}
