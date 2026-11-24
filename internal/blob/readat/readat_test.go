package readat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob/dir"
)

// TestReadsAreBoundedRanges guards the reader's bound on what it fetches:
// bytes read at offsets equal the object's, a read that continues where
// the last ended fetches twice as much as before up to MaxChunk, a read
// that jumps fetches only MinChunk, and a read past the size given is
// io.EOF however long the object is.
func TestReadsAreBoundedRanges(t *testing.T) {
	store, err := dir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "data/o.dcm", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r := New(ctx, store, "data/o.dcm", int64(len(data))-10)
	p := make([]byte, 100)
	for off := int64(0); off < 8000; off += 100 {
		if _, err := r.ReadAt(p, off); err != nil || !bytes.Equal(p, data[off:off+100]) {
			t.Fatalf("at %d: %v", off, err)
		}
	}
	// 512, 1024, 2048, 4096 hold the first 7680 bytes; a fifth fetch the rest.
	if r.Fetches() != 5 {
		t.Errorf("reading 8000 bytes in order took %d fetches, want 5", r.Fetches())
	}
	before := r.Fetches()
	if _, err := r.ReadAt(p[:8], 3<<20); err != nil || !bytes.Equal(p[:8], data[3<<20:3<<20+8]) {
		t.Fatalf("a jump: %v", err)
	}
	if r.Fetches() != before+1 || len(r.chunk) != MinChunk {
		t.Errorf("a jump fetched %d bytes in %d reads, want %d in 1", len(r.chunk), r.Fetches()-before, MinChunk)
	}
	if n, err := r.ReadAt(p[:20], int64(len(data))-15); !errors.Is(err, io.EOF) || n != 5 {
		t.Errorf("a read across the size given: %d, %v, want 5 and io.EOF", n, err)
	}
}
