package dir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// openTemp opens a store on a new temporary directory, closed when tb ends.
func openTemp(tb testing.TB) *Store {
	tb.Helper()
	s, err := Open(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	return s
}

// TestSuite guards that the directory driver keeps the storage contract,
// declaring what a file system cannot do: one name cannot be an object and
// a prefix at once.
func TestSuite(t *testing.T) {
	blobtest.Run(t, blobtest.Driver{
		Open:         func(tb testing.TB) blob.Store { return openTemp(tb) },
		UsageKnown:   usageKnown,
		KeyAndPrefix: false,
	})
}

// TestOpenRefuses guards that Open refuses, as Invalid, a root that does not
// exist (an unmounted volume must not be written to its mount point), one
// that is a file, and, on Unix, one this process cannot write; and that the
// message names no path.
func TestOpenRefuses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"missing": filepath.Join(dir, "missing"), "file": file}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(dir, "readonly")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		cases["read-only"] = ro
	}
	for name, p := range cases {
		s, err := Open(p)
		if err == nil {
			_ = s.Close()
		}
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("Open(%s) = %v, want Invalid", name, err)
		}
		if err != nil && strings.Contains(fault.Message(err), dir) {
			t.Errorf("Open(%s): the message names the path: %q", name, fault.Message(err))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open made the missing directory: %v", err)
	}
}

// plant writes a file behind the store's back, making its directories.
func plant(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, name string) bool {
	t.Helper()
	_, err := os.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// tempFiles returns the names in the store's temporary directory.
func tempFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, tempDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestOpenCleans guards that Open makes the temporary directory when it is
// missing, removes every file a crash left in it, and leaves every object
// alone.
func TestOpenCleans(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(root, tempDir)); err != nil || !info.IsDir() {
		t.Fatalf("Open did not make the temporary directory: %v, %v", info, err)
	}
	if _, err := s.Put(context.Background(), "a/keep", strings.NewReader("object")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 600 { // more than one chunk of a directory read
		plant(t, filepath.Join(root, tempDir, fmt.Sprintf("left%03d", i)), []byte("partial"))
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if left := tempFiles(t, root); len(left) != 0 {
		t.Errorf("%d temporary files are still there after Open", len(left))
	}
	if b, err := os.ReadFile(filepath.Join(root, "a", "keep")); err != nil || string(b) != "object" {
		t.Fatalf("Open changed an object: %q, %v", b, err)
	}
}

// TestOpenCostIsFlat guards that Open reads only the temporary directory,
// whatever the number of objects: on an archive of hundreds of millions of
// files, a walk of the tree at every start would take hours. It counts the
// directories Open reads with an empty storage and with thousands of
// objects in hundreds of directories, which must be the same.
func TestOpenCostIsFlat(t *testing.T) {
	reads := func(root string) []string {
		var opened []string
		st := realSteps()
		st.open = func(r *os.Root, dir string) (*os.File, error) {
			opened = append(opened, filepath.ToSlash(dir))
			return r.OpenFile(dir, os.O_RDONLY|openNonblock, 0)
		}
		s, err := open(root, st)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return opened
	}
	empty := t.TempDir()
	full := t.TempDir()
	for i := range 3000 {
		plant(t, filepath.Join(full, "data", fmt.Sprintf("s%02d", i%30), fmt.Sprintf("x%d", i%300), fmt.Sprintf("%d.dcm", i)), nil)
	}
	if err := os.Mkdir(filepath.Join(full, tempDir), 0o700); err != nil {
		t.Fatal(err)
	}
	plant(t, filepath.Join(full, tempDir, "left"), nil)
	a, b := reads(empty), reads(full)
	if want := []string{tempDir}; !slices.Equal(a, want) {
		t.Errorf("Open of an empty storage read %q, want only %q", a, want)
	}
	// One more reading of the temporary directory, after a removal, is the
	// only difference allowed.
	if want := []string{tempDir, tempDir}; !slices.Equal(b, want) {
		t.Errorf("Open of a storage of 3000 objects read %q, want %q", b, want)
	}
}

// TestOpenRefusesTempDir guards that Open refuses a temporary directory it
// cannot trust: a file, a symbolic link (to a directory inside or outside
// the root), or one the system says is on another file system than the
// root, since a rename from it would fail or, through a link, write
// elsewhere.
func TestOpenRefusesTempDir(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		root := t.TempDir()
		plant(t, filepath.Join(root, tempDir), nil)
		if _, err := Open(root); fault.KindOf(err) != fault.Invalid {
			t.Fatalf("Open = %v, want Invalid", err)
		}
	})
	t.Run("link", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(root, tempDir)); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		if _, err := Open(root); fault.KindOf(err) != fault.Invalid {
			t.Fatalf("Open = %v, want Invalid", err)
		}
	})
	t.Run("other file system", func(t *testing.T) {
		st := realSteps()
		st.sameFS = func(fs.FileInfo, fs.FileInfo) (bool, bool) { return false, true }
		_, err := open(t.TempDir(), st)
		if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "must be one file system") {
			t.Fatalf("Open = %v, want Invalid saying a storage is one file system", err)
		}
	})
	t.Run("same file system", func(t *testing.T) {
		s := openTemp(t)
		a, err := os.Lstat(s.root.Name())
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Lstat(filepath.Join(s.root.Name(), tempDir))
		if err != nil {
			t.Fatal(err)
		}
		same, known := sameFileSystem(a, b)
		if known && !same {
			t.Fatal("the root and its own temporary directory are reported on two file systems")
		}
		if runtime.GOOS != "windows" && !known {
			t.Fatal("a Unix system did not tell the device of a file")
		}
	})
}

// TestCrossDevice guards that a rename refused because it would cross file
// systems fails the Put with an error saying a storage is one file system,
// and leaves no object and no temporary file: the driver never falls back
// to a copy, which is not whole or nothing.
func TestCrossDevice(t *testing.T) {
	var crossErr error
	for _, e := range []error{syscall.EXDEV, syscall.Errno(17)} {
		if isCrossDevice(e) {
			crossErr = e
		}
	}
	if crossErr == nil {
		t.Skip("this system's error for a rename across file systems is not known")
	}
	s := openTemp(t)
	s.rename = func(*os.Root, string, string) error { return &os.LinkError{Op: "rename", Err: crossErr} }
	_, err := s.Put(context.Background(), "m/x", strings.NewReader("data"))
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "must be one file system") {
		t.Fatalf("Put = %v, want Unavailable saying a storage is one file system", err)
	}
	if _, err := s.Stat(context.Background(), "m/x"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("Stat = %v, want NotFound", err)
	}
	if left := tempFiles(t, s.root.Name()); len(left) != 0 {
		t.Fatalf("temporary files left: %q", left)
	}
}

// TestTempNeverListed guards that the temporary directory and the files in
// it, such as a Put in progress or a crash left, are never listed, and that
// a directory a Put made for its key is not listed as a prefix until the
// object is in it.
func TestTempNeverListed(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "d/obj", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	root := s.root.Name()
	plant(t, filepath.Join(root, tempDir, "inflight"), []byte("partial"))
	if err := os.MkdirAll(filepath.Join(root, "new", "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []blob.ListOptions{{}, {Delimiter: "/"}, {Prefix: "d/"}, {Prefix: "new", Delimiter: "/"}, {Prefix: ".blob"}, {Prefix: ".blob", Delimiter: "/"}} {
		p, err := s.List(ctx, opts)
		if err != nil {
			t.Fatalf("List(%+v): %v", opts, err)
		}
		for _, k := range append(p.Keys, p.Prefixes...) {
			if strings.Contains(k, blob.ReservedPrefix) || strings.HasPrefix(k, "new") {
				t.Errorf("List(%+v) shows %q", opts, k)
			}
		}
	}
}

// TestPutSteps guards the exact order that makes a Put whole and durable:
// each new directory of the key is made and its parent synced; the bytes go
// to a file in the temporary directory, which is synced, renamed over the
// object's name; then the object's directory and the temporary directory
// are synced. A crash cannot be made to lose an unsynced write in a test,
// so this test is what fails if a sync is dropped or moved.
func TestPutSteps(t *testing.T) {
	s := openTemp(t)
	var steps []string
	syncFile, syncDir, rename := s.syncFile, s.syncDir, s.rename
	s.syncFile = func(f *os.File) error {
		steps = append(steps, "sync file "+filepath.ToSlash(relative(t, s, f.Name())))
		return syncFile(f)
	}
	s.syncDir = func(root *os.Root, dir string) error {
		steps = append(steps, "sync dir "+filepath.ToSlash(dir))
		return syncDir(root, dir)
	}
	s.rename = func(root *os.Root, from, to string) error {
		steps = append(steps, "rename "+filepath.ToSlash(from)+" "+filepath.ToSlash(to))
		return rename(root, from, to)
	}
	data := blobtest.PatternBytes(1, 5000)
	if _, err := s.Put(context.Background(), "a/b/obj", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 6 {
		t.Fatalf("steps = %q", steps)
	}
	tmp := strings.TrimPrefix(steps[2], "sync file ")
	if !strings.HasPrefix(tmp, tempDir+"/") {
		t.Fatalf("the bytes went to %q, not the temporary directory", tmp)
	}
	want := []string{"sync dir .", "sync dir a", "sync file " + tmp, "rename " + tmp + " a/b/obj", "sync dir a/b", "sync dir " + tempDir}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps of a new key's Put = %q, want %q", steps, want)
	}

	// Replacing an existing object takes the same steps, without making
	// directories.
	steps = nil
	if _, err := s.Put(context.Background(), "a/b/obj", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("steps of a replacing Put = %q", steps)
	}
	tmp = strings.TrimPrefix(steps[0], "sync file ")
	want = []string{"sync file " + tmp, "rename " + tmp + " a/b/obj", "sync dir a/b", "sync dir " + tempDir}
	if !strings.HasPrefix(tmp, tempDir+"/") || !slices.Equal(steps, want) {
		t.Fatalf("steps of a replacing Put = %q, want %q", steps, want)
	}
}

// relative returns name relative to the store's root.
func relative(t *testing.T, s *Store, name string) string {
	t.Helper()
	if !filepath.IsAbs(name) {
		return name
	}
	r, err := filepath.Rel(s.root.Name(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPutStepFails guards that a Put whose file sync or rename fails
// returns Unavailable and leaves no object and no temporary file, so an
// unsynced object is never presented as written.
func TestPutStepFails(t *testing.T) {
	broken := errors.New("broken on purpose")
	for _, step := range []string{"sync file", "rename"} {
		t.Run(step, func(t *testing.T) {
			s := openTemp(t)
			switch step {
			case "sync file":
				s.syncFile = func(*os.File) error { return broken }
			case "rename":
				s.rename = func(*os.Root, string, string) error { return broken }
			}
			_, err := s.Put(context.Background(), "x/y", strings.NewReader("data"))
			if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, broken) {
				t.Fatalf("Put with a failing %s = %v, want Unavailable", step, err)
			}
			if _, err := s.Stat(context.Background(), "x/y"); fault.KindOf(err) != fault.NotFound {
				t.Fatalf("Stat after the failed Put = %v, want NotFound", err)
			}
			if left := tempFiles(t, s.root.Name()); len(left) != 0 {
				t.Fatalf("temporary files left: %q", left)
			}
			// The directory made for the key stays, empty, and is not
			// listed: removing it could race another Put into it.
			if entries, err := os.ReadDir(filepath.Join(s.root.Name(), "x")); err != nil || len(entries) != 0 {
				t.Fatalf("the key's directory holds %d entries, %v; want it empty", len(entries), err)
			}
			if p, err := s.List(context.Background(), blob.ListOptions{Delimiter: "/"}); err != nil || len(p.Keys)+len(p.Prefixes) != 0 {
				t.Fatalf("List after the failed Put = %+v, %v; want nothing", p, err)
			}
		})
	}
}

// TestSymlinks guards that the driver follows no symbolic link, and above
// all none out of its root: a link planted inside the root, to a directory
// or a file outside it or inside it, is never read through, written
// through, listed, or removed through, and Open does not clean through it.
func TestSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := []byte("outside the root")
	plant(t, filepath.Join(outside, "secret"), secret)
	plant(t, filepath.Join(outside, "theirs"), []byte("not ours"))
	root := t.TempDir()
	plant(t, filepath.Join(root, "real", "obj"), []byte("inside"))
	links := map[string]string{
		"out":     outside,
		"outfile": filepath.Join(outside, "secret"),
		"in":      filepath.Join(root, "real"),
		"infile":  filepath.Join(root, "real", "obj"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
	}
	// A relative link that climbs out, the form os.Root alone would also
	// refuse.
	if err := os.Symlink(filepath.Join("..", filepath.Base(outside)), filepath.Join(root, "climb")); err != nil {
		t.Fatal(err)
	}
	// Relative links that stay inside the root, which os.Root follows: only
	// the driver's own checks refuse them.
	if err := os.Symlink(filepath.Join("real", "obj"), filepath.Join(root, "relfile")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "reldir")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if !exists(t, filepath.Join(outside, "theirs")) {
		t.Fatal("Open cleaned a directory outside the root through a link")
	}

	for _, key := range []string{"out/secret", "outfile", "in/obj", "infile", "climb/secret", "relfile", "reldir/obj"} {
		if r, err := s.Read(ctx, key, 0, -1); err == nil {
			b, _ := io.ReadAll(r)
			_ = r.Close()
			t.Errorf("Read(%q) followed a link and gave %q", key, b)
		}
		if _, err := s.Stat(ctx, key); err == nil {
			t.Errorf("Stat(%q) followed a link", key)
		}
	}
	for _, key := range []string{"out/new", "in/new", "climb/new"} {
		if _, err := s.Put(ctx, key, strings.NewReader("planted")); err == nil {
			t.Errorf("Put(%q) wrote through a link", key)
		}
	}
	if exists(t, filepath.Join(outside, "new")) || exists(t, filepath.Join(root, "real", "new")) {
		t.Fatal("a Put wrote a file through a link")
	}
	for _, key := range []string{"out/secret", "outfile", "climb/secret"} {
		_ = s.Delete(ctx, key) // Whether it errs or not, the target must stay.
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || !bytes.Equal(b, secret) {
		t.Fatalf("the file outside the root was changed or removed: %q, %v", b, err)
	}

	p, err := s.List(ctx, blob.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Keys, []string{"real/obj"}) {
		t.Errorf("List = %q, want only the real object", p.Keys)
	}
	p, err = s.List(ctx, blob.ListOptions{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Keys) != 0 || !slices.Equal(p.Prefixes, []string{"real/"}) {
		t.Errorf("List(/) = %q %q, want only real/", p.Keys, p.Prefixes)
	}
	if _, err := s.List(ctx, blob.ListOptions{Prefix: "out/"}); err == nil {
		t.Error("List(out/) listed through a link")
	}

	// A Put to a link's own name replaces the link, not its target.
	if _, err := s.Put(ctx, "outfile", strings.NewReader("object")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || !bytes.Equal(b, secret) {
		t.Fatalf("a Put over a link wrote to its target: %q, %v", b, err)
	}
	if info, err := os.Lstat(filepath.Join(root, "outfile")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("after a Put, outfile is %v, %v; want a regular file", info, err)
	}
}

// TestOutsideTools guards that the file a Put wrote is, byte for byte, what
// it was given, and that the SHA-256 Put returned is the file's, by tools
// that are not NoPACs: shasum or sha256sum, and cmp. It skips when the
// tools are absent.
//
// Neither tool is given a path of this system. The checksum tool reads the
// written file on its standard input, and cmp compares its standard input,
// the bytes given to Put, with the written file named relative to its
// working directory. So the test means the same on Windows, where the
// tools that come with Git are Perl and MSYS programs that do not read a
// path with a drive letter. A tool that is present but fails shows its own
// output.
func TestOutsideTools(t *testing.T) {
	// sha256sum first: it is a program of its own wherever it exists, while
	// a shasum found on Windows may be a wrapper script that cannot find the
	// Perl program it wraps.
	sum := []string{"sha256sum"}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		sum = []string{"shasum", "-a", "256"}
	}
	if _, err := exec.LookPath(sum[0]); err != nil {
		t.Skip("neither shasum nor sha256sum is installed")
	}
	if _, err := exec.LookPath("cmp"); err != nil {
		t.Skip("cmp is not installed")
	}
	s := openTemp(t)
	data := blobtest.PatternBytes(42, 3<<20+17)
	w, err := s.Put(context.Background(), "tool/check/object.dcm", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root.Name(), "tool", "check")
	f, err := os.Open(filepath.Join(dir, "object.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var stderr bytes.Buffer
	cmd := exec.Command(sum[0], sum[1:]...) // #nosec G204 -- a fixed tool reading a test's own file.
	cmd.Stdin, cmd.Stderr = f, &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v: %s%s", sum[0], err, out, stderr.Bytes())
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != hex.EncodeToString(w.SHA256[:]) {
		t.Fatalf("%s says %q; Put returned %x", sum[0], out, w.SHA256)
	}
	cmd = exec.Command("cmp", "-", "object.dcm") // #nosec G204 -- a fixed tool on a test's own files.
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cmp: %v: %s", err, out)
	}
	if fmtSum := sha256.Sum256(data); fmtSum != w.SHA256 {
		t.Fatalf("Put returned %x for bytes whose SHA-256 is %x", w.SHA256, fmtSum)
	}
}

// TestBoundedMemory guards that a Put and a Read of a large object allocate
// a small, fixed amount, not the object's size: the archive takes objects of
// hundreds of megabytes, many at once. It does not run in parallel, since
// the allocation count is the whole process's.
func TestBoundedMemory(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	const size = 64 << 20
	const bound = 4 << 20
	allocated := func(f func()) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	if n := allocated(func() {
		if _, err := s.Put(ctx, "big/object", blobtest.Pattern(1, size)); err != nil {
			t.Fatal(err)
		}
	}); n > bound {
		t.Errorf("a Put of %d MB allocated %d bytes", size>>20, n)
	}
	if n := allocated(func() {
		r, err := s.Read(ctx, "big/object", 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.Copy(io.Discard, r)
		_ = r.Close()
		if err != nil || got != size {
			t.Fatalf("read %d bytes, %v", got, err)
		}
	}); n > bound {
		t.Errorf("a Read of %d MB allocated %d bytes", size>>20, n)
	}
}

// TestListLargeDirectory guards a directory with more entries than one page
// and than one chunk of a directory read: every key comes back once, in
// order, at several page sizes. The files are made behind the store's back,
// since how they were written is not what is tested.
func TestListLargeDirectory(t *testing.T) {
	s := openTemp(t)
	const n = 2500
	var want []string
	for i := range n {
		name := strings.Repeat("x", i%7) + hex.EncodeToString([]byte{byte(i >> 8), byte(i)})
		plant(t, filepath.Join(s.root.Name(), "big", name), nil)
		want = append(want, "big/"+name)
	}
	slices.Sort(want)
	for _, limit := range []int{97, 999, 0} {
		var got []string
		opts := blob.ListOptions{Prefix: "big/", Limit: limit}
		for {
			p, err := s.List(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, p.Keys...)
			if p.Next == "" {
				break
			}
			opts.Token = p.Next
		}
		if !slices.Equal(got, want) {
			t.Fatalf("limit %d: listed %d keys, want %d in order", limit, len(got), len(want))
		}
	}
}

// TestOpenBesideKeepsPutsInProgress guards the open of a store beside the
// one that writes: it leaves a temporary file in place (a Put in progress
// of that store), writes and reads an object of its own, and a later Open
// still removes what a crash left.
func TestOpenBesideKeepsPutsInProgress(t *testing.T) {
	ctx := context.Background()
	w := openTemp(t)
	root := w.root.Name()
	plant(t, filepath.Join(root, tempDir, "inflight"), []byte("partial"))
	b, err := OpenBeside(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := tempFiles(t, root); !slices.Equal(got, []string{"inflight"}) {
		t.Errorf("after an open beside the temporary directory holds %q, want the Put in progress kept", got)
	}
	if _, err := b.Put(ctx, "meta/rec", strings.NewReader("record")); err != nil {
		t.Fatal(err)
	}
	if got := tempFiles(t, root); !slices.Equal(got, []string{"inflight"}) {
		t.Errorf("after a Put beside the temporary directory holds %q", got)
	}
	_ = b.Close()
	if !exists(t, filepath.Join(root, "meta", "rec")) {
		t.Error("the object written beside is not there")
	}
	again, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if got := tempFiles(t, root); len(got) != 0 {
		t.Errorf("an Open after it left %q in the temporary directory", got)
	}
}

// TestOpenForReadingChangesNothing guards the open a reader makes: it
// leaves a temporary file in place (a Put in progress beside it), makes no
// temporary directory in a storage that has none, opens a directory it may
// not write, reads its objects, and refuses Put and Delete as the caller's
// mistake.
func TestOpenForReadingChangesNothing(t *testing.T) {
	ctx := context.Background()
	w := openTemp(t)
	if _, err := w.Put(ctx, "a/obj", strings.NewReader("held")); err != nil {
		t.Fatal(err)
	}
	root := w.root.Name()
	plant(t, filepath.Join(root, tempDir, "inflight"), []byte("partial"))
	r, err := OpenForReading(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := tempFiles(t, root); !slices.Equal(got, []string{"inflight"}) {
		t.Errorf("after an open for reading the temporary directory holds %q, want the Put in progress kept", got)
	}
	if _, err := r.Put(ctx, "b/obj", strings.NewReader("x")); fault.KindOf(err) != fault.Internal {
		t.Errorf("Put on a store opened for reading: %v, want Internal", err)
	}
	if err := r.Delete(ctx, "a/obj"); fault.KindOf(err) != fault.Internal || !exists(t, filepath.Join(root, "a", "obj")) {
		t.Errorf("Delete on a store opened for reading: %v, want Internal and the object kept", err)
	}
	_ = r.Close()

	bare := t.TempDir()
	plant(t, filepath.Join(bare, "c", "obj"), []byte("only read"))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(bare, 0o500); err != nil { //nolint:gosec // G302: a directory the test's user may read and enter, not write.
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(bare, 0o700) }) //nolint:gosec // G302: the directory's mode back, so the test's cleanup can remove it.
	}
	r, err = OpenForReading(bare)
	if err != nil {
		t.Fatalf("an open for reading of a directory that cannot be written: %v", err)
	}
	defer func() { _ = r.Close() }()
	rc, err := r.Read(ctx, "c/obj", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "only read" {
		t.Errorf("read %q", b)
	}
	if exists(t, filepath.Join(bare, tempDir)) {
		t.Error("an open for reading made the temporary directory")
	}
}
