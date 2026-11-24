package place

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestOnlyTheFirstOpenOfAProcessRemovesWhatACrashLeft guards Cleanups: the
// first open to write of a storage removes a file a crash left in its
// temporary directory; a later open sharing the same Cleanups leaves a new
// one there, since it may be a Put in progress of the store already open; an
// opener without Cleanups removes it at every open; and a first open that
// failed leaves the removal to the next.
func TestOnlyTheFirstOpenOfAProcessRemovesWhatACrashLeft(t *testing.T) {
	ctx := context.Background()
	root := realTemp(t)
	dir := filepath.Join(root, "lab", "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, blob.ReservedPrefix+"tmp")
	s := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}}
	lab := Tenant{ID: "t1", Name: "lab"}
	values := fixedValues{roots: settings.DirectoryRoots{{Name: "fast", Path: root}}}
	shared := Opener{Values: values, Cleanups: &Cleanups{}}

	// A temporary directory that is a file fails the first open after its
	// claim; the removal is still to do.
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Open(ctx, lab, s); err == nil {
		t.Fatal("an open of a storage whose temporary directory is a file succeeded")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}

	plant := func(name string) string {
		t.Helper()
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}
	openClose := func(o Opener) {
		t.Helper()
		bs, err := o.Open(ctx, lab, s)
		if err != nil {
			t.Fatal(err)
		}
		_ = bs.Close()
	}

	crash := plant("crash")
	openClose(shared)
	if exists(crash) {
		t.Fatal("the first open of the process left what a crash left")
	}
	inProgress := plant("in-progress")
	openClose(shared)
	if !exists(inProgress) {
		t.Fatal("a later open of the same process removed a file that may be a Put in progress")
	}
	openClose(Opener{Values: values})
	if exists(inProgress) {
		t.Fatal("an opener without Cleanups did not remove what was left")
	}
}

// TestAClaimWaitsForTheOpenThatCleans guards Cleanups.claim: an open that
// comes while the first open of the place removes what a crash left waits
// for it, so its first Put is not removed by that removal, then opens
// beside it; a claim whose context ends while it waits fails.
func TestAClaimWaitsForTheOpenThatCleans(t *testing.T) {
	c := &Cleanups{}
	p, q := placeID{s3: "p"}, placeID{s3: "q"}
	e, first, err := c.claim(context.Background(), p)
	if err != nil || !first {
		t.Fatalf("the first claim = %v, %v; want true", first, err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.claim(ended, p); err == nil {
		t.Fatal("a claim with an ended context did not fail while another cleans")
	}
	got := make(chan bool, 1)
	go func() {
		_, f, err := c.claim(context.Background(), p)
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	select {
	case <-got:
		t.Fatal("a claim did not wait for the open that cleans")
	case <-time.After(50 * time.Millisecond):
	}
	c.finish(e, true)
	if f := <-got; f {
		t.Fatal("a claim after a successful clean was told to clean again")
	}
	eq, f, _ := c.claim(context.Background(), q)
	if !f {
		t.Fatal("another place was not cleaned at its first open")
	}
	c.finish(eq, false)
	if _, f, _ := c.claim(context.Background(), q); !f {
		t.Fatal("a place whose clean was not whole was not cleaned again at the next open")
	}
}

// TestADirectorySpelledOtherwiseIsTheSamePlace guards that Cleanups knows
// a directory by its identity, not its spelling: an open of the storage
// through a root spelled otherwise (through a link to the root's parent,
// and in another case where the file system ignores case), while a Put of
// the first store is in flight, opens beside it and leaves the Put's
// temporary file. Keyed by the spelled path, the second open counted as a
// first and removed it, and the Put failed at its rename.
func TestADirectorySpelledOtherwiseIsTheSamePlace(t *testing.T) {
	base := realTemp(t)
	root := filepath.Join(base, "Roots", "fast")
	if err := os.MkdirAll(filepath.Join(root, "lab", "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	spellings := map[string]string{}
	link := filepath.Join(realTemp(t), "linked")
	if err := os.Symlink(filepath.Join(base, "Roots"), link); err == nil {
		spellings["a link to the root's parent"] = filepath.Join(link, "fast")
	} else {
		t.Logf("no symbolic links here: %v", err)
	}
	other := filepath.Join(base, "roots", "FAST")
	if info, err := os.Stat(other); err == nil && info.IsDir() {
		spellings["another case"] = other
	} else {
		t.Log("the file system is case-sensitive: the case spelling is not tried")
	}
	if len(spellings) == 0 {
		t.Skip("no other spelling of a directory here")
	}
	s := tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "main"}}
	lab := Tenant{ID: "t1", Name: "lab"}
	for name, spelled := range spellings {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			shared := &Cleanups{}
			first, err := Opener{Values: fixedValues{roots: settings.DirectoryRoots{{Name: "fast", Path: root}}}, Cleanups: shared}.Open(ctx, lab, s)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = first.Close() }()
			pr, pw := io.Pipe()
			put := make(chan error, 1)
			go func() {
				_, err := first.Put(ctx, "s/held", pr)
				_ = pr.CloseWithError(io.ErrClosedPipe) // A Put that ended early does not leave the writes below blocked.
				put <- err
			}()
			if _, err := pw.Write([]byte("first part ")); err != nil {
				t.Fatal(err)
			}
			second, err := Opener{Values: fixedValues{roots: settings.DirectoryRoots{{Name: "fast", Path: spelled}}}, Cleanups: shared}.Open(ctx, lab, s)
			if err != nil {
				t.Fatalf("the open through the other spelling: %v", err)
			}
			defer func() { _ = second.Close() }()
			if _, err := pw.Write([]byte("last part")); err != nil {
				t.Fatal(err)
			}
			_ = pw.Close()
			if err := <-put; err != nil {
				t.Fatalf("the Put in flight while the storage was opened through another spelling: %v", err)
			}
		})
	}
}
