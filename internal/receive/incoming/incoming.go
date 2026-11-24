package incoming

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// IDLength is the length of a file's id: 128 random bits in lowercase hex.
const IDLength = 32

// The suffixes of a file's name in its first two states.
const (
	tempSuffix   = ".tmp"
	landedSuffix = ".new"
)

// copyBuffer is the size of the buffer a landing copies through.
const copyBuffer = 256 << 10

// State is how far a file in the area got, as its name says.
type State int

// The states.
const (
	// Temp is a file being written, never acknowledged.
	Temp State = iota + 1
	// Landed is a file synced and acknowledged, not yet recorded.
	Landed
	// Recorded is a file a row of the tenant's database names.
	Recorded
	// Made is a file made from a recorded one, such as its de-identified
	// copy, or one being made (Derive): never owed, made again whenever
	// it is needed.
	Made
)

// String returns the state's name.
func (s State) String() string {
	switch s {
	case Temp:
		return "temp"
	case Landed:
		return "landed"
	case Recorded:
		return "recorded"
	case Made:
		return "made"
	}
	return "State(" + strconv.Itoa(int(s)) + ")"
}

// Area is one tenant's incoming area. It is safe for concurrent use.
type Area struct {
	root *os.Root

	mu      sync.Mutex
	durable map[string]bool // directories known durable in the root

	// hook, when not nil, runs at each durable step of a landing, for a
	// test that stops the process there (export_test.go).
	hook func(point string)
	// steps are the calls that make a landing durable, replaced by a test
	// that records their order (export_test.go).
	steps steps
}

// steps are the durable calls of a landing.
type steps struct {
	syncFile func(f *os.File) error
	rename   func(root *os.Root, from, to string) error
	syncDir  func(root *os.Root, dir string) error
}

// realSteps are the calls a server makes.
func realSteps() steps {
	return steps{
		syncFile: func(f *os.File) error { return f.Sync() },
		rename:   func(root *os.Root, from, to string) error { return root.Rename(from, to) },
		syncDir:  syncDirectory,
	}
}

// File is a file that landed: whole, synced, its name durable.
type File struct {
	// Receiver names the receiver the file came to.
	Receiver string
	// ID is the file's id.
	ID string
	// Path is where it is, relative to the area, in its recorded form
	// ("<receiver>/ab/<id>"), the form the database keeps.
	Path string
	// Size is its size in bytes.
	Size int64
	// SHA256 is the SHA-256 of its bytes, computed as they passed.
	SHA256 [sha256.Size]byte
}

// ErrTooLarge is in the chain of a landing refused because the file is
// larger than its bound.
var ErrTooLarge = errors.New("the file is larger than the receiver's largest file")

// TooLargeError is a landing refused for its size: Read bytes were read
// before the bound was passed, and nothing was kept.
type TooLargeError struct {
	// Read is the bytes read before the landing stopped.
	Read int64
	// Limit is the bound.
	Limit int64
}

// Error returns the bound.
func (e *TooLargeError) Error() string {
	return "the file is larger than " + strconv.FormatInt(e.Limit, 10) + " bytes"
}

// Unwrap returns ErrTooLarge.
func (e *TooLargeError) Unwrap() error { return ErrTooLarge }

// Open opens the incoming area of the tenant named tenantName under root,
// making the tenant's directory (mode 0700) when it is missing and making
// its entry durable. The tenant's name is checked by the master's rule, so it
// is one path segment that can be a directory's name on every system.
func Open(root, tenantName string) (*Area, error) {
	if err := master.ValidateTenantName(tenantName); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, fault.New(fault.Invalid, "the incoming area's root is not set")
	}
	dir := filepath.Join(root, tenantName)
	if err := os.MkdirAll(dir, 0o700); err != nil { // #nosec G301 G703 -- the operator's own data directory and a validated tenant name.
		return nil, unavailable("the tenant's incoming directory cannot be made", err)
	}
	if err := syncPath(root); err != nil {
		return nil, unavailable("the incoming area's root cannot be synced", err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, unavailable("the tenant's incoming directory cannot be opened", err)
	}
	return &Area{root: r, durable: map[string]bool{}, steps: realSteps()}, nil
}

// Close releases the area.
func (a *Area) Close() error { return a.root.Close() }

// NewID returns a new random id.
func NewID() string {
	var b [IDLength / 2]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return hex.EncodeToString(b[:])
}

// PathOf returns the recorded path of the file with id that came to the
// receiver named.
func PathOf(receiver, id string) string { return receiver + "/" + id[:2] + "/" + id }

// ParsePath returns the receiver and id of a recorded path, or an error
// unless path is exactly the form PathOf makes. Every path that reaches the
// file system is checked by it.
func ParsePath(path string) (receiver, id string, err error) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 || master.ValidateTenantName(parts[0]) != nil || !validID(parts[2]) || parts[1] != parts[2][:2] {
		return "", "", fault.New(fault.Invalid, "a place in the incoming area is the form this server makes")
	}
	return parts[0], parts[2], nil
}

// validID reports whether id is IDLength lowercase hex digits.
func validID(id string) bool {
	if len(id) != IDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Land streams r into the area as a new file, at most limit bytes, and
// returns it only once its bytes and its name are durable, in the order the
// package documentation gives. A file over limit is a *TooLargeError and is
// not kept. ctx ending stops the copy.
func (a *Area) Land(ctx context.Context, receiver string, r io.Reader, limit int64) (File, error) {
	if err := master.ValidateTenantName(receiver); err != nil {
		return File{}, fault.Wrap(fault.Invalid, err, "a receiver's name is the form a tenant's is")
	}
	id := NewID()
	dir := receiver + "/" + id[:2]
	if err := a.ensureDir(receiver, "."); err != nil {
		return File{}, err
	}
	if err := a.ensureDir(dir, receiver); err != nil {
		return File{}, err
	}
	temp := dir + "/" + id + tempSuffix
	f, err := a.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, unavailable("a file cannot be made in the incoming area", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = a.root.Remove(temp)
		}
	}()
	h := sha256.New()
	n, err := copyLimited(ctx, io.MultiWriter(f, h), r, limit+1)
	if err != nil {
		return File{}, err
	}
	if n > limit {
		return File{}, &TooLargeError{Read: n, Limit: limit}
	}
	a.at("land.before-sync")
	if err := a.steps.syncFile(f); err != nil {
		return File{}, unavailable("a received file cannot be synced", err)
	}
	if err := f.Close(); err != nil {
		return File{}, unavailable("a received file cannot be closed", err)
	}
	keep = true
	a.at("land.before-rename")
	landed := dir + "/" + id + landedSuffix
	if err := a.steps.rename(a.root, temp, landed); err != nil {
		_ = a.root.Remove(temp)
		return File{}, unavailable("a received file cannot be named", err)
	}
	a.at("land.before-dirsync")
	if err := a.steps.syncDir(a.root, dir); err != nil {
		// The name may not be durable: the file is not acknowledged, and
		// a sender that is told so sends it again. What is on the disk is
		// found at start, as a landed file with no row.
		return File{}, unavailable("the incoming directory cannot be synced", err)
	}
	a.at("land.done")
	out := File{Receiver: receiver, ID: id, Path: dir + "/" + id, Size: n}
	copy(out.SHA256[:], h.Sum(nil))
	return out, nil
}

// copyLimited copies at most max bytes from r to w, checking ctx between
// reads, and returns how many it copied.
func copyLimited(ctx context.Context, w io.Writer, r io.Reader, max int64) (int64, error) {
	buf := make([]byte, copyBuffer)
	var n int64
	for n < max {
		if err := ctx.Err(); err != nil {
			return n, fault.Wrap(fault.Unavailable, err, "the landing was stopped")
		}
		chunk := buf
		if left := max - n; left < int64(len(chunk)) {
			chunk = chunk[:left]
		}
		m, rerr := r.Read(chunk)
		if m > 0 {
			if _, werr := w.Write(chunk[:m]); werr != nil {
				return n, unavailable("a received file cannot be written", werr)
			}
			n += int64(m)
		}
		if errors.Is(rerr, io.EOF) {
			return n, nil
		}
		if rerr != nil {
			return n, fault.Wrap(fault.Unavailable, rerr, "the sender's bytes could not be read")
		}
	}
	return n, nil
}

// ensureDir makes the directory dir durable in the root: made if missing,
// and its parent synced so its entry survives a crash, whether this call
// made it or found it, since one found may have been made by a call whose
// sync never happened.
func (a *Area) ensureDir(dir, parent string) error {
	a.mu.Lock()
	known := a.durable[dir]
	a.mu.Unlock()
	if known {
		return nil
	}
	if err := a.root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return unavailable("a directory of the incoming area cannot be made", err)
	}
	if err := syncDirectory(a.root, parent); err != nil {
		return unavailable("the incoming area cannot be synced", err)
	}
	a.mu.Lock()
	a.durable[dir] = true
	a.mu.Unlock()
	return nil
}

// Commit renames a landed file to its recorded name, once the row that names
// it has committed. A file already recorded is left as it is. The rename is
// not synced: a crash that undoes it leaves a landed name with a row, which
// a restart commits again.
func (a *Area) Commit(path string) error {
	_, id, err := ParsePath(path)
	if err != nil {
		return err
	}
	for range renameLooks {
		err = a.root.Rename(path+landedSuffix, path)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			return unavailable("an incoming file cannot be renamed", err)
		}
		if _, serr := a.root.Lstat(path); serr == nil {
			return nil
		}
		// Under neither name: an Unrecord renamed it between the two
		// looks, so it is landed again; look again.
	}
	return fault.New(fault.NotFound, "the incoming file "+id+" is not there")
}

// renameLooks is how many times Commit and Unrecord look for a file under
// its two names before they call it missing. Each rename is atomic, so a
// file is always under one of them, and a look that finds neither saw the
// other rename happen between its two reads (the batch committing a name
// while a release takes it back); the next look finds it.
const renameLooks = 3

// Unrecord renames a recorded file back to its landed name, so a restart that
// finds it with no row checks and records it again instead of removing it.
// A file already landed is left as it is.
func (a *Area) Unrecord(path string) error {
	_, id, err := ParsePath(path)
	if err != nil {
		return err
	}
	for range renameLooks {
		err = a.root.Rename(path, path+landedSuffix)
		switch {
		case err == nil:
			return syncDirectory(a.root, path[:strings.LastIndexByte(path, '/')])
		case !errors.Is(err, fs.ErrNotExist):
			return unavailable("an incoming file cannot be renamed", err)
		}
		if _, serr := a.root.Lstat(path + landedSuffix); serr == nil {
			return nil
		}
		// Under neither name: a Commit renamed it between the two looks,
		// so it is recorded now; look again.
	}
	return fault.New(fault.NotFound, "the incoming file "+id+" is not there")
}

// Open opens the file at a recorded path for reading, under its recorded
// name or, when its rename has not happened yet, its landed one, and returns
// it with its size. A file under neither is NotFound. Only a regular file is
// opened.
func (a *Area) Open(path string) (*os.File, int64, error) {
	if _, _, err := ParsePath(path); err != nil {
		return nil, 0, err
	}
	for _, name := range []string{path, path + landedSuffix} {
		f, err := a.root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, unavailable("an incoming file cannot be opened", err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, 0, fault.New(fault.Internal, "an incoming file is not a regular file")
		}
		return f, info.Size(), nil
	}
	return nil, 0, fault.New(fault.NotFound, "the incoming file is not there")
}

// Exists reports whether a file is at the recorded path under either of its
// names.
func (a *Area) Exists(path string) (bool, error) {
	if _, _, err := ParsePath(path); err != nil {
		return false, err
	}
	for _, name := range []string{path, path + landedSuffix} {
		_, err := a.root.Lstat(name)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, unavailable("an incoming file cannot be looked at", err)
		}
	}
	return false, nil
}

// Remove removes the file at a recorded path under both of its names, and
// the file made from it (Derive); one left half written is found and
// removed at the next start. A file that is not there is not an error. The removal is not synced: a file that
// comes back after a crash has no row, and is removed again at start.
func (a *Area) Remove(path string) error {
	if _, _, err := ParsePath(path); err != nil {
		return err
	}
	for _, name := range []string{path, path + landedSuffix, path + derivedSuffix} {
		if err := a.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return unavailable("an incoming file cannot be removed", err)
		}
	}
	return nil
}

// Entry is one file Scan found.
type Entry struct {
	// Receiver names the receiver the file came to.
	Receiver string
	// ID is the file's id.
	ID string
	// Path is its recorded path, whatever its state.
	Path string
	// State is how far it got.
	State State
	// Size is its size in bytes.
	Size int64
	// name is the file's name as found, which a made file's removal needs
	// (RemoveMade).
	name string
}

// Scan calls fn for every file in the area, one directory at a time, so its
// memory follows the largest directory and not the area. A name that is not
// one of the forms is skipped and left alone, and counted in the
// answer: something other than this server put it there.
func (a *Area) Scan(fn func(Entry) error) (strangers int, err error) {
	receivers, err := a.readDir(".")
	if err != nil {
		return 0, err
	}
	for _, rd := range receivers {
		if !rd.IsDir() || master.ValidateTenantName(rd.Name()) != nil {
			strangers++
			continue
		}
		shards, err := a.readDir(rd.Name())
		if err != nil {
			return strangers, err
		}
		for _, d := range shards {
			if !d.IsDir() || !validShard(d.Name()) {
				strangers++
				continue
			}
			names, err := a.readDir(rd.Name() + "/" + d.Name())
			if err != nil {
				return strangers, err
			}
			for _, n := range names {
				e, ok := parseName(rd.Name(), d.Name(), n.Name())
				if !ok || !n.Type().IsRegular() {
					strangers++
					continue
				}
				info, err := n.Info()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return strangers, unavailable("an incoming file cannot be looked at", err)
				}
				e.Size = info.Size()
				if err := fn(e); err != nil {
					return strangers, err
				}
			}
		}
	}
	return strangers, nil
}

// RemoveTemp removes a file being written that a crash left behind.
func (a *Area) RemoveTemp(e Entry) error {
	if _, _, err := ParsePath(e.Path); err != nil || e.State != Temp {
		return fault.New(fault.Internal, "only a file being written is removed as one")
	}
	if err := a.root.Remove(e.Path + tempSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unavailable("a leftover incoming file cannot be removed", err)
	}
	return nil
}

// readDir lists a directory of the root.
func (a *Area) readDir(dir string) ([]fs.DirEntry, error) {
	f, err := a.root.Open(dir)
	if err != nil {
		return nil, unavailable("a directory of the incoming area cannot be opened", err)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, unavailable("a directory of the incoming area cannot be read", err)
	}
	return entries, nil
}

// validShard reports whether name is two lowercase hex digits.
func validShard(name string) bool {
	return len(name) == 2 && validID(name+strings.Repeat("0", IDLength-2))
}

// parseName returns the entry a name in a receiver's shard dir stands for.
func parseName(receiver, dir, name string) (Entry, bool) {
	state := Recorded
	id := name
	switch {
	// Before the temporary suffix, which a made file's name being written
	// also ends with.
	case strings.Contains(name, derivedSuffix):
		id, _, _ = strings.Cut(name, derivedSuffix)
		if !madeName(name, id) {
			return Entry{}, false
		}
		state = Made
	case strings.HasSuffix(name, tempSuffix):
		state, id = Temp, strings.TrimSuffix(name, tempSuffix)
	case strings.HasSuffix(name, landedSuffix):
		state, id = Landed, strings.TrimSuffix(name, landedSuffix)
	}
	if !validID(id) || id[:2] != dir {
		return Entry{}, false
	}
	return Entry{Receiver: receiver, ID: id, Path: receiver + "/" + dir + "/" + id, State: state, name: name}, true
}

// at runs the hook at a durable step.
func (a *Area) at(point string) {
	if a.hook != nil {
		a.hook(point)
	}
}

// unavailable wraps a file system failure as Unavailable with a message that
// holds no path: the area's path is the server's own, and stays in the log's
// cause only.
func unavailable(message string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return fault.Wrap(fault.Unavailable, err, message)
}

// syncPath syncs the directory at an absolute or relative path outside any
// root: the parent of the tenant's directory, once at Open.
func syncPath(dir string) error {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return syncDirectory(r, ".")
}
