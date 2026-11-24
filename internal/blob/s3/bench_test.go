package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob/blobtest"
)

// repeat is a reader of n bytes made of one block over and over, so a
// benchmark measures the storage and not the generation of its input.
type repeat struct {
	block []byte
	left  int64
	pos   int
}

func (r *repeat) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.left)
	done := 0
	for int64(done) < n {
		c := copy(p[done:n], r.block[r.pos:])
		done += c
		r.pos = (r.pos + c) % len(r.block)
	}
	r.left -= n
	return int(n), nil
}

var benchSizes = []struct {
	name string
	n    int64
}{{"256KB", 256 << 10}, {"256MB", 256 << 20}}

// BenchmarkPut measures Put, in MB/s, against the server the environment
// names, for a small object (one request) and a large one (multipart).
func BenchmarkPut(b *testing.B) {
	s := openTest(b)
	block := blobtest.PatternBytes(1, 1<<20)
	for _, sz := range benchSizes {
		b.Run(sz.name, func(b *testing.B) {
			b.SetBytes(sz.n)
			for i := 0; i < b.N; i++ {
				if _, err := s.Put(context.Background(), fmt.Sprintf("bench/put-%d", i%4), &repeat{block: block, left: sz.n}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRead measures reading a whole object, in MB/s, and a ranged read
// of 64 KiB from the middle of the large one, which is what the archive does
// most.
func BenchmarkRead(b *testing.B) {
	s := openTest(b)
	block := blobtest.PatternBytes(2, 1<<20)
	for _, sz := range benchSizes {
		key := "bench/read-" + sz.name
		if _, err := s.Put(context.Background(), key, &repeat{block: block, left: sz.n}); err != nil {
			b.Fatal(err)
		}
		b.Run(sz.name, func(b *testing.B) {
			b.SetBytes(sz.n)
			for i := 0; i < b.N; i++ {
				r, err := s.Read(context.Background(), key, 0, -1)
				if err != nil {
					b.Fatal(err)
				}
				n, err := io.Copy(io.Discard, r)
				_ = r.Close()
				if err != nil || n != sz.n {
					b.Fatalf("read %d bytes, %v", n, err)
				}
			}
		})
	}
	b.Run("range-64KB-of-256MB", func(b *testing.B) {
		b.SetBytes(64 << 10)
		for i := 0; i < b.N; i++ {
			r, err := s.Read(context.Background(), "bench/read-256MB", 100<<20, 64<<10)
			if err != nil {
				b.Fatal(err)
			}
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, r); err != nil || buf.Len() != 64<<10 {
				b.Fatalf("read %d bytes, %v", buf.Len(), err)
			}
			_ = r.Close()
		}
	})
}

// BenchmarkPackedScan measures reading a scan of 500 small instances kept
// one object per instance against one packed object read by ranges
// (blobtest.BenchPackedScan), so the layout of a scan on this storage is
// chosen by its numbers.
func BenchmarkPackedScan(b *testing.B) {
	blobtest.BenchPackedScan(b, openTest(b))
}
