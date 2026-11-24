package incoming

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
)

// The suffix of a file made from a recorded one: the de-identified copy a
// receiver that de-identifies writes beside the file it received. While it
// is written its name is <id>.deid-<random>.tmp: each writer has its own,
// so a worker whose lease was taken over and is still writing never shares
// a file with the worker that took its unit.
const (
	derivedSuffix     = ".deid"
	derivedTempMarker = ".deid-"
)

// Derived is a file made from a recorded one and kept beside it.
type Derived struct {
	// Path is the recorded path of the file it was made from.
	Path string
	// Size is its size in bytes.
	Size int64
	// SHA256 is the SHA-256 of its bytes, computed as they were written.
	SHA256 [sha256.Size]byte
}

// Derive writes the file made from the recorded file at path beside it:
// write is given a file to write into from its start, and what it wrote is
// named <path>.deid once write returns, replacing one there. Nothing is
// synced: a derived file is made again from the file it came from, whole,
// whenever it is needed, and never read after a restart (Scan reports one
// and the caller removes it), so a crash that loses or cuts it loses
// nothing. A write that fails leaves nothing behind.
func (a *Area) Derive(path string, write func(w io.WriteSeeker) error) (Derived, error) {
	if _, _, err := ParsePath(path); err != nil {
		return Derived{}, err
	}
	temp := path + derivedTempMarker + NewID()[:16] + tempSuffix
	f, err := a.root.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Derived{}, unavailable("a file cannot be made in the incoming area", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = a.root.Remove(temp)
		}
	}()
	if err := write(f); err != nil {
		return Derived{}, err
	}
	// The checksum is of what is on the disk, read back once whole: the
	// writer seeks back to fill in lengths, so the bytes as it wrote them
	// are not the file's in order.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Derived{}, unavailable("a made file cannot be read back", err)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Derived{}, unavailable("a made file cannot be read back", err)
	}
	if err := f.Close(); err != nil {
		return Derived{}, unavailable("a made file cannot be closed", err)
	}
	keep = true
	if err := a.root.Rename(temp, path+derivedSuffix); err != nil {
		_ = a.root.Remove(temp)
		return Derived{}, unavailable("a made file cannot be named", err)
	}
	out := Derived{Path: path, Size: n}
	copy(out.SHA256[:], h.Sum(nil))
	return out, nil
}

// OpenDerived opens the file made from the recorded file at path for
// reading, and returns it with its size. One that is not there is NotFound.
func (a *Area) OpenDerived(path string) (*os.File, int64, error) {
	if _, _, err := ParsePath(path); err != nil {
		return nil, 0, err
	}
	f, err := a.root.OpenFile(path+derivedSuffix, os.O_RDONLY|openNonblock, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fault.New(fault.NotFound, "the made incoming file is not there")
	}
	if err != nil {
		return nil, 0, unavailable("an incoming file cannot be opened", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, fault.New(fault.Internal, "a made incoming file is not a regular file")
	}
	return f, info.Size(), nil
}

// RemoveDerived removes the file made from the recorded file at path. One
// that is not there is not an error. One left half written by a writer
// that stopped is found by Scan, and removed with RemoveMade.
func (a *Area) RemoveDerived(path string) error {
	if _, _, err := ParsePath(path); err != nil {
		return err
	}
	if err := a.root.Remove(path + derivedSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unavailable("an incoming file cannot be removed", err)
	}
	return nil
}

// RemoveMade removes a made file Scan found, whole or half written, by the
// name it was found under.
func (a *Area) RemoveMade(e Entry) error {
	if _, _, err := ParsePath(e.Path); err != nil || e.State != Made || !madeName(e.name, e.ID) {
		return fault.New(fault.Internal, "only a made file is removed as one")
	}
	dir := e.Path[:strings.LastIndexByte(e.Path, '/')]
	if err := a.root.Remove(dir + "/" + e.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unavailable("a made incoming file cannot be removed", err)
	}
	return nil
}

// madeName reports whether name is a made file's name for the id: <id>.deid,
// or <id>.deid-<random>.tmp while it is written.
func madeName(name, id string) bool {
	if name == id+derivedSuffix {
		return true
	}
	rest, ok := strings.CutPrefix(name, id+derivedTempMarker)
	if !ok {
		return false
	}
	random, ok := strings.CutSuffix(rest, tempSuffix)
	return ok && len(random) == 16 && validID(random+strings.Repeat("0", IDLength-16))
}
