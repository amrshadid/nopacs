package dir

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Put writes r to key, whole or not at all; see blob.Store. The steps, and
// why each sync is there, are in the package's documentation.
//
// A failed Put removes its temporary file and nothing else. Directories it
// made for the key stay: an empty directory holds no object, so it is never
// listed, and removing it could race another Put that is about to rename
// an object into it, or, since a remove by name cannot tell a directory
// from what replaced it, remove an object that took its name.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if err := s.beginWrite(ctx); err != nil {
		return blob.Written{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Written{}, err
	}
	dir := parent(key)
	if err := s.prepareDirs(key, dir); err != nil {
		return blob.Written{}, err
	}
	f, tmp, err := s.createTemp()
	if err != nil {
		return blob.Written{}, err
	}
	done := false
	defer func() {
		if !done {
			_ = f.Close()          // Already closed on some paths; the Put's own error is returned.
			_ = s.root.Remove(tmp) // A temporary file that cannot be removed now is removed at the next Open.
		}
	}()
	w, err := s.copyHashed(ctx, f, r)
	if err != nil {
		return blob.Written{}, err
	}
	if err := s.syncFile(f); err != nil {
		return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage could not sync the object's bytes")
	}
	if err := f.Close(); err != nil {
		return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage could not close the object's file")
	}
	if err := s.moveIntoPlace(tmp, key, dir); err != nil {
		return blob.Written{}, err
	}
	done = true
	s.listing.invalidate(dir)
	// From here the new object is in place. A failure to sync a directory
	// is still reported, so the caller writes it again, since the name
	// might not survive a crash of the machine.
	if err := s.syncDir(s.root, native(dir)); err != nil && !notFound(err) {
		return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage wrote the object but could not sync its directory")
	}
	if err := s.syncDir(s.root, tempDir); err != nil {
		return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage wrote the object but could not sync its temporary directory")
	}
	return w, nil
}

// prepareDirs makes the directories of key durable (see ensureDirs), and
// refuses, as Conflict, a key whose place is a directory or lies under a
// file. It runs before any byte is read, so a Put that cannot succeed
// consumes nothing.
func (s *Store) prepareDirs(key, dir string) error {
	if err := s.ensureDirs(dir); err != nil {
		return err
	}
	info, err := s.root.Lstat(native(key))
	switch {
	case err == nil && info.IsDir():
		return conflictUnder()
	case err != nil && !notFound(err):
		return s.wrap(fault.Unavailable, err, "the storage could not examine the object's place")
	}
	return nil
}

// createTemp creates an empty temporary file in the temporary directory and
// returns it open for writing, with its name relative to the root.
func (s *Store) createTemp() (*os.File, string, error) {
	name, err := tempName()
	if err != nil {
		return nil, "", err
	}
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return nil, "", s.wrap(fault.Unavailable, err, "the storage could not create a file in its temporary directory")
	}
	return f, name, nil
}

// moveIntoPlace renames the temporary file tmp to key.
//
// Nothing in this process removes a directory, so a key's directory found
// missing at the rename was removed by someone else. The record of durable
// directories then no longer holds for it: it is forgotten, the directories
// made durable again, and the rename tried again, a few times. A temporary
// file found missing was removed by someone else too: a process removes what
// a crash left only at its first open of a storage (place.Cleanups), so most
// likely a second process that opened this storage and cleaned its temporary
// directory; that is said, since only one process may use a storage.
func (s *Store) moveIntoPlace(tmp, key, dir string) error {
	for range 4 {
		err := s.rename(s.root, tmp, native(key))
		switch {
		case err == nil:
			return nil
		case isCrossDevice(err):
			return s.wrap(fault.Unavailable, err, "the storage's directory spans more than one file system (a volume is mounted below it); a storage's directory must be one file system")
		case goneMidway(err):
			if _, lerr := s.root.Lstat(tmp); notFound(lerr) {
				return s.wrap(fault.Unavailable, err, "the put's temporary file was removed before it was put in place; only one process may use a storage, and another process may have opened this one")
			}
			s.durable.forgetPath(dir)
			if err := s.ensureDirs(dir); err != nil {
				return err
			}
			continue
		}
		if info, lerr := s.root.Lstat(native(key)); lerr == nil && info.IsDir() {
			return conflictUnder()
		}
		return s.wrap(fault.Unavailable, err, "the storage could not put the object in place")
	}
	return fault.New(fault.Unavailable, "the object's directory kept being removed by something outside this process while it was put")
}

// checkDirInfo says what is wrong with info, Lstat's answer for a name that
// must be a directory of a key.
func checkDirInfo(info fs.FileInfo) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return linkError()
	case !info.IsDir():
		return fault.New(fault.Conflict, "the storage holds an object where the key needs a directory; a key cannot hold an object while other keys lie under it")
	}
	return nil
}

func conflictUnder() error {
	return fault.New(fault.Conflict, "the storage holds a directory at this key, made for other keys under it; a key cannot hold an object where a directory is")
}

func linkError() error {
	return fault.New(fault.Internal, "the storage holds a symbolic link where a directory of the key belongs; the driver follows none")
}

// checkDirs examines each directory of dir with Lstat, so that a symbolic
// link planted where a directory belongs is refused instead of followed. It
// returns an error that notFound recognizes when one is missing or is a
// file.
func (s *Store) checkDirs(dir string) error {
	if dir == "." {
		return nil
	}
	segs := strings.Split(dir, "/")
	for i := range segs {
		info, err := s.root.Lstat(native(strings.Join(segs[:i+1], "/")))
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return linkError()
		}
		if !info.IsDir() {
			return syscall.ENOTDIR
		}
	}
	return nil
}

// copyHashed copies r to f, counting and hashing the bytes as they pass,
// and stops when ctx ends. The copy goes through a buffer of its own rather
// than io.Copy, so neither side's ReadFrom or WriteTo can move bytes past
// the hash or past the check of ctx.
func (s *Store) copyHashed(ctx context.Context, f *os.File, r io.Reader) (blob.Written, error) {
	bp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bp)
	buf := *bp
	h := sha256.New()
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return blob.Written{}, fault.Wrap(fault.Unavailable, err, "the put was canceled")
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				if isFull(err) {
					return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage is full")
				}
				return blob.Written{}, s.wrap(fault.Unavailable, err, "the storage could not write the object")
			}
			h.Write(buf[:n]) // A hash never returns an error.
			size += int64(n)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return blob.Written{}, fmt.Errorf("storage put: reading the object: %w", rerr)
		}
	}
	w := blob.Written{Size: size}
	h.Sum(w.SHA256[:0])
	return w, nil
}
