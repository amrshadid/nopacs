package dir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

var errBrokenReader = errors.New("the reader broke on purpose")

// brokenReader fails at once.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errBrokenReader }

// gateReader says it was reached on its first Read, then waits to be
// released, then yields data, or fails when err is set.
type gateReader struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
	data    *bytes.Reader
}

func newGate(err error, data string) *gateReader {
	return &gateReader{reached: make(chan struct{}), release: make(chan struct{}), err: err, data: bytes.NewReader([]byte(data))}
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.reached); <-g.release })
	if g.err != nil {
		return 0, g.err
	}
	return g.data.Read(p)
}

// TestFailedPutRemovesNoObject guards that a Put that fails, or a Delete,
// never removes an object another call was told is written. A failed Put
// used to remove the directories it made once they were empty; a remove by
// name cannot tell a directory from an object that took its name meanwhile,
// so a Put that waited through it removed a third Put's acknowledged object
// named like the directory. Now nothing removes a directory, and whatever
// each call answered, every object a call was told is written is there at
// the end.
func TestFailedPutRemovesNoObject(t *testing.T) {
	for _, first := range []string{"fails", "succeeds", "deleter"} {
		t.Run(first, func(t *testing.T) {
			s := openTemp(t)
			ctx := context.Background()
			acked := map[string]string{}
			gate := newGate(nil, "A")
			if first == "fails" {
				gate = newGate(errBrokenReader, "")
			}
			aDone := make(chan error, 1)
			go func() { _, err := s.Put(ctx, "a/x", gate); aDone <- err }()
			<-gate.reached // A has made "a" and is copying into the temporary directory.
			if first == "deleter" {
				if _, err := s.Put(ctx, "a/z", strings.NewReader("z")); err != nil {
					t.Fatal(err)
				}
				if err := s.Delete(ctx, "a/z"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.Put(ctx, "a/y", brokenReader{}); !errors.Is(err, errBrokenReader) {
				t.Fatalf("the failing Put = %v", err)
			}
			if _, err := s.Put(ctx, "a", strings.NewReader("C")); err == nil {
				acked["a"] = "C"
			} else if fault.KindOf(err) != fault.Conflict {
				t.Fatalf("Put(a) where a directory is = %v, want Conflict", err)
			}
			close(gate.release)
			if err := <-aDone; err == nil {
				acked["a/x"] = "A"
			} else if first != "fails" {
				t.Fatalf("A failed: %v", err)
			}
			for key, want := range acked {
				r, err := s.Read(ctx, key, 0, -1)
				if err != nil {
					t.Fatalf("the acknowledged object %q is gone: %v", key, err)
				}
				b, _ := io.ReadAll(r)
				_ = r.Close()
				if string(b) != want {
					t.Fatalf("%q holds %q, want %q", key, b, want)
				}
			}
		})
	}
}

// TestSiblingFailureDoesNotFailPut guards that a Put does not fail because
// of what another Put, of another key, did meanwhile: here a failing Put
// beside it, at the moment the first has made its directories and syncs
// them.
func TestSiblingFailureDoesNotFailPut(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	real := s.syncDir
	block := make(chan struct{})
	reached := make(chan struct{})
	var first atomic.Bool
	s.syncDir = func(root *os.Root, dir string) error {
		if filepath.ToSlash(dir) == "p" && first.CompareAndSwap(false, true) {
			close(reached)
			<-block
		}
		return real(root, dir)
	}
	aDone := make(chan error, 1)
	go func() { _, err := s.Put(ctx, "p/q/r/obj", strings.NewReader("A")); aDone <- err }()
	<-reached
	if _, err := s.Put(ctx, "p/z", brokenReader{}); !errors.Is(err, errBrokenReader) {
		t.Fatalf("the failing Put = %v", err)
	}
	close(block)
	if err := <-aDone; err != nil {
		t.Fatalf("a Put failed only because another, beside it, failed: %v", err)
	}
}

// TestConcurrentPutDeleteOneTree guards, by count and not by time, that
// eight goroutines putting, deleting and failing Puts under one tree cause
// no failure of any call that should succeed, and lose no acknowledged
// object: each one reads back as written until its own goroutine deletes
// it, and each goroutine's lasting object is there at the end.
func TestConcurrentPutDeleteOneTree(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	const workers, rounds = 8, 150
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := range workers {
		wg.Go(func() {
			lasting := fmt.Sprintf("d1/d2/last%d", w)
			if _, err := s.Put(ctx, lasting, strings.NewReader(lasting)); err != nil {
				errs <- fmt.Errorf("put %s: %w", lasting, err)
			}
			for i := range rounds {
				if w%2 == 1 {
					if _, err := s.Put(ctx, fmt.Sprintf("d1/d2/d3/f%d/k%d", w, i%4), brokenReader{}); !errors.Is(err, errBrokenReader) {
						errs <- fmt.Errorf("a failing put: %w", err)
					}
					continue
				}
				key := fmt.Sprintf("d1/d2/d3/d4/k%d_%d", w, i%5)
				data := fmt.Sprintf("%s round %d", key, i)
				if _, err := s.Put(ctx, key, strings.NewReader(data)); err != nil {
					errs <- fmt.Errorf("put %s: %w", key, err)
					continue
				}
				r, err := s.Read(ctx, key, 0, -1)
				if err != nil {
					errs <- fmt.Errorf("an acknowledged object is gone: %s: %w", key, err)
					continue
				}
				b, _ := io.ReadAll(r)
				_ = r.Close()
				if string(b) != data {
					errs <- fmt.Errorf("an acknowledged object changed: %s holds %q", key, b)
				}
				if i%3 == 0 {
					if err := s.Delete(ctx, key); err != nil {
						errs <- fmt.Errorf("delete %s: %w", key, err)
					}
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n++; n <= 10 {
			t.Error(err)
		}
	}
	if n > 0 {
		t.Fatalf("%d failures in all", n)
	}
	for w := range workers {
		if _, err := s.Stat(ctx, fmt.Sprintf("d1/d2/last%d", w)); err != nil {
			t.Fatalf("a lasting object is gone: %v", err)
		}
	}
}

// TestPutWaitsForParentSync guards that a Put is acknowledged only when
// every directory on its key's path is durable: a Put that finds a
// directory another Put has just made, and whose parent that other Put has
// not yet synced, waits for that sync instead of returning before it.
func TestPutWaitsForParentSync(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	real := s.syncDir
	block := make(chan struct{})
	reached := make(chan struct{})
	var first atomic.Bool
	var rootSynced atomic.Bool
	s.syncDir = func(root *os.Root, dir string) error {
		if dir == "." {
			if first.CompareAndSwap(false, true) { // only A's sync waits
				close(reached)
				<-block
			}
			err := real(root, dir)
			rootSynced.Store(true)
			return err
		}
		return real(root, dir)
	}
	waiting := make(chan struct{}, 1)
	s.durable.waiting = func(string) { waiting <- struct{}{} }
	aDone := make(chan error, 1)
	go func() { _, err := s.Put(ctx, "d/a", strings.NewReader("A")); aDone <- err }()
	<-reached // A made "d"; its entry in the root is not yet synced.
	bDone := make(chan error, 1)
	go func() { _, err := s.Put(ctx, "d/b", strings.NewReader("B")); bDone <- err }()
	var bErr error
	bReturned := false
	select {
	case <-waiting:
		select {
		case bErr = <-bDone:
			t.Fatalf("Put(d/b) returned %v while the root was still not synced", bErr)
		default:
		}
	case bErr = <-bDone:
		// Returning without waiting is right only if B made the root's
		// entry durable itself.
		bReturned = true
		if !rootSynced.Load() {
			t.Fatalf("Put(d/b) returned %v while the entry of its directory d in the root was never synced", bErr)
		}
	}
	close(block)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if !bReturned {
		bErr = <-bDone
	}
	if bErr != nil {
		t.Fatal(bErr)
	}
}

// TestFoundDirectoriesAreSynced guards that a Put syncs the parent of every
// directory on its path that this process has not seen synced, also when it
// found the directory rather than made it: after a restart, a directory
// made by a Put whose sync never happened would otherwise stay undurable
// under every later object.
func TestFoundDirectoriesAreSynced(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var synced []string
	real := s.syncDir
	s.syncDir = func(r *os.Root, dir string) error {
		synced = append(synced, filepath.ToSlash(dir))
		return real(r, dir)
	}
	for range 2 {
		if _, err := s.Put(context.Background(), "a/b/obj", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{".", "a", "a/b", tempDir, "a/b", tempDir}
	if !slices.Equal(synced, want) {
		t.Fatalf("directories synced by two Puts into found directories = %q, want %q", synced, want)
	}
}

// TestPutRetriesVanishedDirectory guards that a Put whose key's directory
// is removed by something outside the store, between its making and the
// rename, makes it again and succeeds, rather than failing.
func TestPutRetriesVanishedDirectory(t *testing.T) {
	s := openTemp(t)
	real := s.rename
	var removed atomic.Bool
	s.rename = func(r *os.Root, from, to string) error {
		if removed.CompareAndSwap(false, true) {
			if err := os.RemoveAll(filepath.Join(s.root.Name(), "v")); err != nil {
				return err
			}
		}
		return real(r, from, to)
	}
	if _, err := s.Put(context.Background(), "v/w/obj", strings.NewReader("x")); err != nil {
		t.Fatalf("Put after its directory vanished = %v", err)
	}
	if _, err := s.Stat(context.Background(), "v/w/obj"); err != nil {
		t.Fatal(err)
	}
}

// TestPutSaysWhenTempFileVanished guards the error of a Put whose
// temporary file was removed under it, as a second process opening the
// storage would do: it says so, rather than blaming the directories.
func TestPutSaysWhenTempFileVanished(t *testing.T) {
	s := openTemp(t)
	real := s.rename
	s.rename = func(r *os.Root, from, to string) error {
		if err := r.Remove(from); err != nil {
			return err
		}
		return real(r, from, to)
	}
	_, err := s.Put(context.Background(), "t/obj", strings.NewReader("x"))
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "temporary file was removed") {
		t.Fatalf("Put = %v, want Unavailable saying the temporary file was removed", err)
	}
}

// TestMkdirLosesRace guards that a directory made by something else
// between the Put's look and its mkdir is taken as made, not as a failure.
func TestMkdirLosesRace(t *testing.T) {
	s := openTemp(t)
	real := s.mkdir
	s.mkdir = func(r *os.Root, dir string) error {
		if err := os.Mkdir(filepath.Join(s.root.Name(), dir), 0o700); err != nil {
			return err
		}
		return real(r, dir)
	}
	if _, err := s.Put(context.Background(), "m/n/obj", strings.NewReader("x")); err != nil {
		t.Fatalf("Put whose mkdir lost a race = %v", err)
	}
}

// TestPutRefusesLinkedDirectory guards that a symbolic link where a
// directory of a key belongs fails the Put, even a link to a directory
// inside the root, and nothing is written through it.
func TestPutRefusesLinkedDirectory(t *testing.T) {
	s := openTemp(t)
	root := s.root.Name()
	if err := os.MkdirAll(filepath.Join(root, "real", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Relative links that stay inside the root: os.Root alone would follow
	// them (it refuses only links that leave the root or are absolute), so
	// what refuses them here is the driver's own check.
	if err := os.Symlink("real", filepath.Join(root, "lnk")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "real", "deep"), filepath.Join(root, "a", "lnk")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lnk/x", "a/lnk/x", "lnk/deep/x"} {
		if _, err := s.Put(context.Background(), key, strings.NewReader("planted")); err == nil {
			t.Errorf("Put(%q) wrote through a link", key)
		}
	}
	for _, p := range []string{"real/x", "real/deep/x"} {
		if exists(t, filepath.Join(root, filepath.FromSlash(p))) {
			t.Errorf("%s was written through a link", p)
		}
	}
}

// TestDeleteSteps guards that a Delete syncs the directory the object was
// in, so a crash cannot bring the object back, and syncs nothing else.
func TestDeleteSteps(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Put(context.Background(), "a/b/obj", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	var synced []string
	real := s.syncDir
	s.syncDir = func(r *os.Root, dir string) error {
		synced = append(synced, filepath.ToSlash(dir))
		return real(r, dir)
	}
	if err := s.Delete(context.Background(), "a/b/obj"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b"}; !slices.Equal(synced, want) {
		t.Fatalf("a Delete synced %q, want %q", synced, want)
	}
}

// TestKeyCannotNameTempDir guards that no key and no prefix reaches the
// temporary directory, including through a code point some file systems
// ignore (HFS+ drops U+200C, so the name would be the directory's).
func TestKeyCannotNameTempDir(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for _, k := range []string{tempDir + "/x", "\u200c" + tempDir + "/x", strings.ToUpper(tempDir) + "/x"} {
		if _, err := s.Put(ctx, k, strings.NewReader("x")); fault.KindOf(err) != fault.Invalid {
			t.Errorf("Put(%q) = %v, want Invalid", k, err)
		}
		if _, err := s.List(ctx, blob.ListOptions{Prefix: k[:len(k)-1]}); fault.KindOf(err) != fault.Invalid {
			t.Errorf("List(%q) = %v, want Invalid", k[:len(k)-1], err)
		}
	}
}

// TestErrorsNameNoPath guards that no error the store returns names the
// storage's directory, in its message or in its text for the log, across
// the failures that carry a path: Open of a bad path, a failed sync, close
// or rename in a Put, and a failed read of a stream.
func TestErrorsNameNoPath(t *testing.T) {
	ctx := context.Background()
	check := func(what string, err error, paths ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		for _, p := range paths {
			if strings.Contains(fault.Message(err), p) || strings.Contains(err.Error(), p) {
				t.Errorf("%s: the error names the storage's path: %v", what, err)
			}
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(dir, "missing"))
	check("Open of a missing path", err, dir)
	_, err = Open(file)
	check("Open of a file", err, dir)

	broken := syscall.EIO
	for _, c := range []struct {
		name  string
		setup func(s *Store)
	}{
		{"sync", func(s *Store) {
			s.syncFile = func(f *os.File) error { return &fs.PathError{Op: "sync", Path: f.Name(), Err: broken} }
		}},
		{"close", func(s *Store) {
			s.syncFile = func(f *os.File) error { return f.Close() } // the Put's own Close then fails, naming the file
		}},
		{"rename", func(s *Store) {
			root := s.root.Name()
			s.rename = func(_ *os.Root, from, to string) error {
				return &os.LinkError{Op: "rename", Old: filepath.Join(root, from), New: filepath.Join(root, to), Err: broken}
			}
		}},
	} {
		s := openTemp(t)
		c.setup(s)
		_, err := s.Put(ctx, "e/obj", strings.NewReader("x"))
		abs, _ := filepath.Abs(s.root.Name())
		check("Put with a failing "+c.name, err, s.root.Name(), abs)
		if c.name == "sync" && !errors.Is(err, broken) {
			t.Errorf("the cause is lost: %v", err)
		}
	}

	s := openTemp(t)
	if _, err := s.Put(ctx, "e/obj", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(ctx, "e/obj", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.(*section).f.Close() // so the stream's read fails, naming the file
	_, err = io.ReadAll(r)
	check("a failed read of a stream", err, s.root.Name())
}

// TestOpenRefusesUnsyncableFileSystem guards that a file system that
// cannot sync a directory, as some network file systems answer, is refused
// at Open, saying why, instead of letting every Put fail after its object
// is in place.
func TestOpenRefusesUnsyncableFileSystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directories are not synced on Windows")
	}
	st := realSteps()
	st.syncDir = func(*os.Root, string) error { return &fs.PathError{Op: "sync", Path: ".", Err: syscall.EINVAL} }
	_, err := open(t.TempDir(), st)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "cannot sync a directory") {
		t.Fatalf("Open = %v, want Invalid saying the file system cannot sync a directory", err)
	}
}

// TestListPagingReadsDirectoryOnce guards that paging through one large
// directory reads it about once, not once per page: a rebuild of the
// catalog pages through directories of a hundred thousand files, and a read
// per page made that take minutes. It counts the chunks read.
func TestListPagingReadsDirectoryOnce(t *testing.T) {
	s := openTemp(t)
	const n = 5000
	for i := range n {
		plant(t, filepath.Join(s.root.Name(), "big", fmt.Sprintf("k%05d", i)), nil)
	}
	var chunks atomic.Int64
	real := s.readDir
	s.readDir = func(f *os.File, k int) ([]fs.DirEntry, error) {
		chunks.Add(1)
		return real(f, k)
	}
	for _, delim := range []string{"", "/"} {
		chunks.Store(0)
		got := 0
		opts := blob.ListOptions{Prefix: "big/", Delimiter: delim, Limit: 100}
		for {
			p, err := s.List(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			got += len(p.Keys)
			if p.Next == "" {
				break
			}
			opts.Token = p.Next
		}
		one := int64(n/readDirChunk + 2)
		if got != n || chunks.Load() > one {
			t.Errorf("delimiter %q: %d keys in 50 pages took %d chunk reads; one read of the directory is %d", delim, got, chunks.Load(), one)
		}
		s.listing.invalidate("big") // the next mode starts cold
	}
}

// TestListCacheSeesOtherChanges guards that a directory changed behind the
// store's back between two pages is read again, by its modification time:
// a file added ahead of the point reached is listed. The directory's time
// is moved explicitly, so the test does not depend on the file system's
// clock being finer than the time between two calls.
func TestListCacheSeesOtherChanges(t *testing.T) {
	s := openTemp(t)
	dir := filepath.Join(s.root.Name(), "c")
	for i := range 10 {
		plant(t, filepath.Join(dir, fmt.Sprintf("k%02d", i)), nil)
	}
	p, err := s.List(context.Background(), blob.ListOptions{Prefix: "c/", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	plant(t, filepath.Join(dir, "k07a"), nil)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(1e9)
	if err := os.Chtimes(dir, later, later); err != nil {
		t.Fatal(err)
	}
	q, err := s.List(context.Background(), blob.ListOptions{Prefix: "c/", Token: p.Next})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(q.Keys, "c/k07a") {
		t.Fatalf("a file added by another program was not listed: %q", q.Keys)
	}
}

// TestFullVolumeKeepsOldObject guards, on a small volume named by
// NOPACS_BLOB_SMALL_VOLUME (a tmpfs of a few megabytes, say), that a Put
// that fills the volume fails, keeps the old object, leaves no temporary
// file, and that a small Put works afterwards. It skips without one.
func TestFullVolumeKeepsOldObject(t *testing.T) {
	vol := os.Getenv("NOPACS_BLOB_SMALL_VOLUME")
	if vol == "" {
		t.Skip("NOPACS_BLOB_SMALL_VOLUME names no small volume")
	}
	root, err := os.MkdirTemp(vol, "full")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }() // #nosec G703 -- a directory this test made.
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.Put(ctx, "keep/old", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "keep/old", io.LimitReader(zeros{}, 1<<34))
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "full") {
		t.Fatalf("a Put larger than the volume = %v, want Unavailable saying it is full", err)
	}
	r, err := s.Read(ctx, "keep/old", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "old" {
		t.Fatalf("the old object holds %d bytes", len(b))
	}
	if left := tempFiles(t, root); len(left) != 0 {
		t.Fatalf("temporary files left: %q", left)
	}
	if _, err := s.Put(ctx, "after", strings.NewReader("small")); err != nil {
		t.Fatalf("a small Put after the full one: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestListCacheDroppedOnChange guards that a change this process makes in
// a directory drops its cached names at once, without relying on the
// directory's modification time, which a coarse clock may leave unchanged:
// after a Put and a Delete between two pages, with the time set back to
// what it was, the next page shows the new key and not the deleted one.
func TestListCacheDroppedOnChange(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for i := range 10 {
		if _, err := s.Put(ctx, fmt.Sprintf("c/k%02d", i), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.List(ctx, blob.ListOptions{Prefix: "c/", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root.Name(), "c")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "c/k07a", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "c/k08"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	q, err := s.List(ctx, blob.ListOptions{Prefix: "c/", Token: p.Next})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c/k05", "c/k06", "c/k07", "c/k07a", "c/k09"}; !slices.Equal(q.Keys, want) {
		t.Fatalf("the page after a Put and a Delete = %q, want %q", q.Keys, want)
	}
}
