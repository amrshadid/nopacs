package incoming

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// openArea opens an area in a temporary directory.
func openArea(t *testing.T) (*Area, string) {
	t.Helper()
	root := t.TempDir()
	a, err := Open(root, "lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, filepath.Join(root, "lab")
}

// TestLandKeepsTheBytesAndTheirChecksum guards a landing: the file is under
// its landed name, byte for byte, with the checksum and size of what passed.
func TestLandKeepsTheBytesAndTheirChecksum(t *testing.T) {
	a, dir := openArea(t)
	data := bytes.Repeat([]byte("dicom-bytes "), 50000)
	f, err := a.Land(context.Background(), "dimse", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != int64(len(data)) || f.SHA256 != sha256.Sum256(data) {
		t.Fatalf("size %d sha %x, want %d %x", f.Size, f.SHA256, len(data), sha256.Sum256(data))
	}
	if rcv, id, err := ParsePath(f.Path); err != nil || id != f.ID || rcv != "dimse" {
		t.Fatalf("path %q does not parse to its id: %v", f.Path, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)+".new"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the landed file is not the bytes given: %v", err)
	}
	if err := a.Commit(f.Path); err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(f.Path); err != nil {
		t.Fatalf("a second commit is not a no-op: %v", err)
	}
	r, size, err := a.Open(f.Path)
	if err != nil || size != f.Size {
		t.Fatalf("open: %v, size %d", err, size)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(b, data) {
		t.Fatal("the recorded file is not the bytes given")
	}
}

// TestLandRefusesAFileOverItsBound guards the size bound: a file one byte
// over is refused, nothing of it is kept, and one at the bound lands.
func TestLandRefusesAFileOverItsBound(t *testing.T) {
	a, _ := openArea(t)
	data := make([]byte, 4096)
	_, err := a.Land(context.Background(), "dimse", bytes.NewReader(data), 4095)
	var tl *TooLargeError
	if !errors.As(err, &tl) || !errors.Is(err, ErrTooLarge) || tl.Limit != 4095 || tl.Read != 4096 {
		t.Fatalf("got %v, want TooLargeError of 4096 read past 4095", err)
	}
	n := 0
	if _, err := a.Scan(func(Entry) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused file left %d entries", n)
	}
	if _, err := a.Land(context.Background(), "dimse", bytes.NewReader(data), 4096); err != nil {
		t.Fatalf("a file at its bound was refused: %v", err)
	}
}

// failingReader yields some bytes, then fails.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset")
	}
	m := min(len(p), f.n)
	f.n -= m
	return m, nil
}

// TestLandKeepsNothingOfAFailedSender guards that a sender that fails midway
// leaves no file and is answered as a temporary failure.
func TestLandKeepsNothingOfAFailedSender(t *testing.T) {
	a, _ := openArea(t)
	_, err := a.Land(context.Background(), "dimse", &failingReader{n: 100000}, 1<<20)
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Land(ctx, "dimse", bytes.NewReader(make([]byte, 10)), 100); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("an ended context gave %v", err)
	}
	n := 0
	if _, err := a.Scan(func(Entry) error { n++; return nil }); err != nil || n != 0 {
		t.Fatalf("failed landings left %d entries (%v)", n, err)
	}
}

// TestScanTellsTheStatesApart guards that Scan reports each name's state,
// and skips and counts a name this package did not make.
func TestScanTellsTheStatesApart(t *testing.T) {
	a, dir := openArea(t)
	ctx := context.Background()
	landed, _ := a.Land(ctx, "dimse", strings.NewReader("one"), 10)
	recorded, _ := a.Land(ctx, "dimse", strings.NewReader("two"), 10)
	if err := a.Commit(recorded.Path); err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if err := os.MkdirAll(filepath.Join(dir, "dimse", id[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dimse", id[:2], id+".tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := map[string]State{}
	strangers, err := a.Scan(func(e Entry) error { got[e.ID] = e.State; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got[landed.ID] != Landed || got[recorded.ID] != Recorded || got[id] != Temp || len(got) != 3 || strangers != 1 {
		t.Fatalf("scan gave %v and %d strangers", got, strangers)
	}
	if err := a.RemoveTemp(Entry{ID: id, Path: PathOf("dimse", id), State: Temp}); err != nil {
		t.Fatal(err)
	}
	if err := a.Unrecord(recorded.Path); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(landed.Path); err != nil {
		t.Fatal(err)
	}
	got = map[string]State{}
	if _, err := a.Scan(func(e Entry) error { got[e.ID] = e.State; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[recorded.ID] != Landed {
		t.Fatalf("after removals scan gave %v", got)
	}
}

// TestPathsAreOnlyTheServersOwn guards that a path in any other form than
// the one PathOf makes is refused before it reaches the file system.
func TestPathsAreOnlyTheServersOwn(t *testing.T) {
	a, _ := openArea(t)
	id := NewID()
	for _, p := range []string{"", id, id[:2] + "/" + id, "../" + id[:2] + "/" + id, "dimse/" + id[:2] + "/../" + id,
		"dimse/zz/" + id, "dimse/" + id[:2] + "/" + id + ".new", "dimse/" + id[:2] + "/" + strings.ToUpper(id),
		"/dimse/" + id[:2] + "/" + id, "dimse//" + id, "Dimse/" + id[:2] + "/" + id, "../" + id[:2] + "/" + id} {
		if _, _, err := ParsePath(p); err == nil {
			t.Errorf("%q was accepted", p)
		}
		if err := a.Remove(p); fault.KindOf(err) != fault.Invalid {
			t.Errorf("Remove(%q) gave %v", p, err)
		}
		if _, _, err := a.Open(p); fault.KindOf(err) != fault.Invalid {
			t.Errorf("Open(%q) gave %v", p, err)
		}
	}
	if _, err := Open(t.TempDir(), "../x"); err == nil {
		t.Error("a tenant name that is a path was accepted")
	}
}

// TestLandSyncsBeforeItAnswers guards the order of a landing's durable
// steps: the file is synced, then renamed, then its directory synced, all
// before Land returns. A kill cannot show a missing sync (the page cache
// survives a killed process); this test does.
func TestLandSyncsBeforeItAnswers(t *testing.T) {
	a, _ := openArea(t)
	var log []string
	RecordSteps(a, &log)
	if _, err := a.Land(context.Background(), "dimse", strings.NewReader("bytes"), 10); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "sync-file,rename,sync-dir" {
		t.Fatalf("the steps were %q, want sync-file,rename,sync-dir", got)
	}
}

// TestCommitAndUnrecordRacingNeverLoseTheFile guards a file whose name is
// committed by one goroutine (the batch, once its row is written) while
// another takes it back (a release): each rename is atomic, so the file is
// always under one of its two names, and neither call may answer that it
// is not there however their renames fall between each other's looks.
func TestCommitAndUnrecordRacingNeverLoseTheFile(t *testing.T) {
	a, _ := openArea(t)
	f, err := a.Land(context.Background(), "dimse", strings.NewReader("held bytes"), 100)
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 2000
	errs := make(chan error, 2*rounds)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range rounds {
			if err := a.Unrecord(f.Path); err != nil {
				errs <- err
			}
		}
	}()
	for range rounds {
		if err := a.Commit(f.Path); err != nil {
			errs <- err
		}
	}
	<-done
	close(errs)
	for err := range errs {
		t.Fatalf("a rename answered: %v", err)
	}
	if ok, err := a.Exists(f.Path); err != nil || !ok {
		t.Fatalf("the file is gone after the race: %v", err)
	}
}
