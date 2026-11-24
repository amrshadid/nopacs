package readat

import (
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
)

// MaxChunk is the most one fetch asks the storage for, and MinChunk the
// least.
const (
	MaxChunk = 256 << 10
	MinChunk = 512
)

// Reader reads one stored object at offsets. It is not safe for concurrent
// use: it holds one chunk, which each read may replace.
type Reader struct {
	ctx   context.Context
	store blob.Store
	key   string
	size  int64
	// chunk holds the bytes from chunkAt on.
	chunk   []byte
	chunkAt int64
	// next is the length of the next read-ahead.
	next int64
	// fetches counts the reads asked of the storage.
	fetches int
}

// New returns a reader of the object at key on store, size bytes long (the
// size the catalog holds for it: a read past it is io.EOF, whatever the
// storage holds). Every fetch is made with ctx.
func New(ctx context.Context, store blob.Store, key string, size int64) *Reader {
	return &Reader{ctx: ctx, store: store, key: key, size: size}
}

// Fetches returns how many ranged reads the reader has asked of the
// storage.
func (b *Reader) Fetches() int { return b.fetches }

// ReadAt implements io.ReaderAt.
func (b *Reader) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= b.size {
			return n, io.EOF
		}
		if pos < b.chunkAt || pos >= b.chunkAt+int64(len(b.chunk)) {
			if err := b.fetch(pos, int64(len(p)-n)); err != nil {
				return n, err
			}
		}
		n += copy(p[n:], b.chunk[pos-b.chunkAt:])
	}
	return n, nil
}

// fetch holds the bytes from pos on: want of them at least, rounded up, and
// more when pos follows the bytes held.
func (b *Reader) fetch(pos, want int64) error {
	if b.chunk == nil || pos != b.chunkAt+int64(len(b.chunk)) {
		b.next = MinChunk
	}
	length := min(max(b.next, (want+MinChunk-1)/MinChunk*MinChunk), MaxChunk, b.size-pos)
	b.next = min(b.next*2, MaxChunk)
	if want > length && want <= b.size-pos {
		length = want
	}
	b.fetches++
	rc, err := b.store.Read(b.ctx, b.key, pos, length)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, length)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return err
	}
	b.chunk, b.chunkAt = buf, pos
	return nil
}
