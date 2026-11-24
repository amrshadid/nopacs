package dir

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
)

// repeat yields n bytes by cycling through buf, so a benchmark measures the
// store and not the making of its input.
type repeat struct {
	buf  []byte
	off  int
	left int64
}

func (r *repeat) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n := copy(p, r.buf[r.off:])
	r.off = (r.off + n) % len(r.buf)
	r.left -= int64(n)
	return n, nil
}

var benchSizes = []struct {
	name string
	size int64
}{
	{"256KB", 256 << 10},
	{"256MB", 256 << 20},
}

// BenchmarkPut measures a Put that replaces one key, in MB/s, from the
// first byte to a synced file renamed into place and both directories of
// the rename synced: what a reception pays per file archived. The 256 MB
// case is skipped under -short.
func BenchmarkPut(b *testing.B) {
	buf := blobtest.PatternBytes(1, 1<<20)
	for _, c := range benchSizes {
		b.Run(c.name, func(b *testing.B) {
			if testing.Short() && c.size > 1<<20 {
				b.Skip("large objects are skipped under -short")
			}
			s := openTemp(b)
			ctx := context.Background()
			b.SetBytes(c.size)
			b.ResetTimer()
			for b.Loop() {
				if _, err := s.Put(ctx, "bench/object.dcm", &repeat{buf: buf, left: c.size}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRead measures reading one whole object, in MB/s: what a
// verification or a download pays. The object is in the system's cache
// after the first round, so this is the store's own cost, not the disk's.
// The 256 MB case is skipped under -short.
func BenchmarkRead(b *testing.B) {
	buf := blobtest.PatternBytes(2, 1<<20)
	for _, c := range benchSizes {
		b.Run(c.name, func(b *testing.B) {
			if testing.Short() && c.size > 1<<20 {
				b.Skip("large objects are skipped under -short")
			}
			s := openTemp(b)
			ctx := context.Background()
			if _, err := s.Put(ctx, "bench/object.dcm", &repeat{buf: buf, left: c.size}); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(c.size)
			b.ResetTimer()
			for b.Loop() {
				r, err := s.Read(ctx, "bench/object.dcm", 0, -1)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, r); err != nil {
					b.Fatal(err)
				}
				if err := r.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPutFirstIntoDirectory measures a 256 KB Put into a new scan's
// directory, three levels below directories that exist: it pays the syncs
// that make the new directories durable, which the Puts that follow into
// the same directory (BenchmarkPut) do not.
func BenchmarkPutFirstIntoDirectory(b *testing.B) {
	buf := blobtest.PatternBytes(1, 1<<20)
	const size = 256 << 10
	s := openTemp(b)
	ctx := context.Background()
	b.SetBytes(size)
	i := 0
	for b.Loop() {
		i++
		key := fmt.Sprintf("data/subject/session%d/scan/object.dcm", i)
		if _, err := s.Put(ctx, key, &repeat{buf: buf, left: size}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListDirectory measures paging through one directory of 20,000,
// 100,000 and 200,000 files, in pages of 1,000, as a rebuild of the
// catalog does; it reports the time of the whole listing per operation.
// The files are made behind the store's back before the timer starts. The
// two larger cases are skipped under -short.
func BenchmarkListDirectory(b *testing.B) {
	for _, n := range []int{20000, 100000, 200000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			if testing.Short() && n > 20000 {
				b.Skip("large directories are skipped under -short")
			}
			s := openTemp(b)
			dir := filepath.Join(s.root.Name(), "big")
			if err := os.Mkdir(dir, 0o700); err != nil {
				b.Fatal(err)
			}
			for i := range n {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("k%07d", i)), nil, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			ctx := context.Background()
			for b.Loop() {
				s.listing.invalidate("big") // each listing starts from the disk
				got := 0
				opts := blob.ListOptions{Prefix: "big/"}
				for {
					p, err := s.List(ctx, opts)
					if err != nil {
						b.Fatal(err)
					}
					got += len(p.Keys)
					if p.Next == "" {
						break
					}
					opts.Token = p.Next
				}
				if got != n {
					b.Fatalf("listed %d of %d", got, n)
				}
			}
		})
	}
}

// BenchmarkPackedScan measures reading a scan of 500 small instances kept
// one object per instance against one packed object read by ranges
// (blobtest.BenchPackedScan), so the layout of a scan on this storage is
// chosen by its numbers.
func BenchmarkPackedScan(b *testing.B) {
	blobtest.BenchPackedScan(b, openTemp(b))
}

// BenchmarkReadSmallDeep measures opening and reading 8 KiB of an object
// at the depth of an archived instance's key, the read a frame, a bulk
// data value or a metadata file's line costs: most of it is the checks of
// each directory of the key and of the object, not the bytes.
func BenchmarkReadSmallDeep(b *testing.B) {
	s := openTemp(b)
	ctx := context.Background()
	const key = "data/sub-tsf7ij4iejwvlbvq/ses-6jsgvunfrtp5bsyj/scan-vgb3rmyj3qagwfly/1.2.840.99.1.dcm"
	if _, err := s.Put(ctx, key, bytes.NewReader(blobtest.PatternBytes(3, 64<<10))); err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 8<<10)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r, err := s.Read(ctx, key, 1000, int64(len(buf)))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			b.Fatal(err)
		}
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
