//go:build unix

package dir

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// hangLimit is how long one call may take before the test calls it hung. It
// is a detector of a call that never returns, not a measure of speed: every
// call here takes microseconds.
const hangLimit = 20 * time.Second

// swapper keeps swapping a FIFO and something else under one name, until
// stopped, so a call that looked at the name first and opened it second
// finds a FIFO in the open.
func swapper(t *testing.T, root, name string, swap func(fifo string)) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		fifo := filepath.Join(root, "fifo")
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = os.Remove(fifo)
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Error(err)
				return
			}
			swap(fifo)
		}
	}()
	return func() { close(done); <-finished }
}

// unblock opens the FIFO at name for writing without blocking, which
// releases an open for reading that is waiting on it, so a hung test can
// end.
func unblock(name string) {
	if w, err := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		_ = w.Close()
	}
}

// TestReadNeverBlocksOnSwappedFIFO guards that Read returns at once when a
// FIFO is swapped in where an object is, giving either the object or
// NotFound and never the FIFO: an open that decided from an earlier look at
// the name that it held a file would wait forever for a writer.
func TestReadNeverBlocksOnSwappedFIFO(t *testing.T) {
	s := openTemp(t)
	root := s.root.Name()
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "k")
	stop := swapper(t, root, "k", func(fifo string) {
		_ = os.Rename(fifo, key)
		reg := filepath.Join(root, "reg")
		_ = os.WriteFile(reg, []byte("data"), 0o600)
		_ = os.Rename(reg, key)
	})
	defer stop()
	for n := range 3000 {
		done := make(chan error, 1)
		go func() {
			r, err := s.Read(ctx, "k", 0, -1)
			if err == nil {
				b, rerr := io.ReadAll(r)
				_ = r.Close()
				if rerr == nil && string(b) != "data" {
					err = fault.New(fault.Internal, "read "+string(b))
				}
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil && fault.KindOf(err) != fault.NotFound {
				t.Fatalf("Read %d: %v, want the object or NotFound", n, err)
			}
		case <-time.After(hangLimit):
			go unblock(key)
			t.Fatalf("Read hung after %d calls: a FIFO swapped in for the object blocked its open", n)
		}
	}
}

// TestListNeverBlocksOnSwappedFIFO guards the same for List: a directory
// swapped for a FIFO between the reading of its parent and its own open,
// with a delimiter (where List looks inside to see whether the directory
// holds an object) and without (where it lists the directory), never hangs
// the listing.
func TestListNeverBlocksOnSwappedFIFO(t *testing.T) {
	s := openTemp(t)
	root := s.root.Name()
	ctx := context.Background()
	if _, err := s.Put(ctx, "d/obj", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	d, saved := filepath.Join(root, "d"), filepath.Join(root, "saved")
	stop := swapper(t, root, "d", func(fifo string) {
		if os.Rename(d, saved) != nil {
			return
		}
		_ = os.Rename(fifo, d)
		_ = os.Rename(d, fifo)
		_ = os.Rename(saved, d)
	})
	defer stop()
	for n := range 3000 {
		opts := blob.ListOptions{Delimiter: "/"}
		if n%2 == 1 {
			opts = blob.ListOptions{}
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.List(ctx, opts)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("List %d (%+v): %v", n, opts, err)
			}
		case <-time.After(hangLimit):
			go unblock(d)
			t.Fatalf("List hung after %d calls: a FIFO swapped in for a directory blocked its open", n)
		}
	}
}

// TestNoDescriptorLeaks guards that failing calls, of every kind, close
// every file they opened: a server that leaked one per refusal would run
// out after some thousands of receptions.
func TestNoDescriptorLeaks(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	count := func() int {
		e, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("cannot count descriptors: %v", err)
		}
		return len(e)
	}
	if _, err := s.Put(ctx, "o/k", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.root.Name(), "o", "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := count()
	for range 300 {
		_, _ = s.Put(ctx, "o/f", brokenReader{})
		_, _ = s.Put(ctx, "o/dir", strings.NewReader("x"))
		_, _ = s.Put(ctx, "o/k/sub", strings.NewReader("x"))
		_, _ = s.Read(ctx, "o/k", 10, -1)
		_, _ = s.Read(ctx, "o/none", 0, -1)
		_, _ = s.Read(ctx, "o/dir", 0, -1)
		_, _ = s.List(ctx, blob.ListOptions{Prefix: "o/", Delimiter: "/", Limit: 1})
		_ = s.Delete(ctx, "o/none")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, _ = s.Put(cctx, "o/c", strings.NewReader("x"))
	}
	if after := count(); after != before {
		t.Fatalf("%d descriptors before, %d after", before, after)
	}
}
