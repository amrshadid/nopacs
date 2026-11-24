package incoming

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"sync"
	"testing"
	"time"
)

// TestDerivedWritersNeverShareAFile guards the name a made file is written
// under: two writers of the same file at once (a worker whose lease was
// taken over, still writing, and the one that took its unit) each write a
// file of their own, so each checksum is of the bytes it wrote, and what
// ends under <id>.deid is one of them whole, never a mix; a half-written
// file a writer left is found by Scan as made and removed by RemoveMade.
func TestDerivedWritersNeverShareAFile(t *testing.T) {
	a, _ := openArea(t)
	f, err := a.Land(context.Background(), "scanner", bytes.NewReader([]byte("original")), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{bytes.Repeat([]byte{'a'}, 1<<20), bytes.Repeat([]byte{'b'}, 3<<19)}
	started := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(bodies))
	sums := make([][32]byte, len(bodies))
	errs := make([]error, len(bodies))
	var wg sync.WaitGroup
	for i, body := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := a.Derive(f.Path, func(w io.WriteSeeker) error {
				// Both files are open before either is written.
				ready.Done()
				<-started
				for off := 0; off < len(body); off += 4096 {
					if _, err := w.Write(body[off : off+4096]); err != nil {
						return err
					}
				}
				return nil
			})
			sums[i], errs[i] = d.SHA256, err
		}()
	}
	opened := make(chan struct{})
	go func() { ready.Wait(); close(opened) }()
	select {
	case <-opened:
	case <-time.After(30 * time.Second):
		t.Fatal("the two writers could not both open a file to write")
	}
	close(started)
	wg.Wait()
	for i, body := range bodies {
		if errs[i] != nil || sums[i] != sha256.Sum256(body) {
			t.Errorf("writer %d: %v, its checksum is not of the bytes it wrote", i, errs[i])
		}
	}
	r, _, err := a.OpenDerived(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, bodies[0]) && !bytes.Equal(got, bodies[1]) {
		t.Errorf("the made file is %d bytes of neither writer whole", len(got))
	}

	// A writer stopped part way leaves its file half written, as a crash
	// does: Scan finds it made, beside the whole one, and RemoveMade
	// removes each by the name it was found under.
	stop, scanned, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Derive(f.Path, func(w io.WriteSeeker) error {
			_, _ = w.Write([]byte("half"))
			close(stop)
			<-scanned
			return io.ErrUnexpectedEOF
		})
	}()
	<-stop
	defer func() { close(scanned); <-done }()
	made := 0
	if _, err := a.Scan(func(e Entry) error {
		if e.State == Made {
			made++
			return a.RemoveMade(e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if made != 2 {
		t.Errorf("Scan found %d made files, want the whole copy and the half-written one", made)
	}
	if _, _, err := a.OpenDerived(f.Path); err == nil {
		t.Error("the made file is still there after RemoveMade")
	}
}
