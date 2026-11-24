package blobtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
)

// PackedScanInstances is how many instances the packed scan benchmark's
// scan holds: a series of a few hundred slices, the common case of MR and
// CT, where the cost of a request per object adds up.
const PackedScanInstances = 500

// packedScanReaders is how many reads at once the concurrent cases make,
// as a server reading a scan for one client would.
const packedScanReaders = 8

// BenchPackedScan measures reading a whole scan of PackedScanInstances
// small instances from s, the same scan kept two ways: one object per
// instance, read one object each, and one packed object holding them all
// end to end, read by one ranged Read per instance (what a client asking
// for each instance in turn would cost) or by one Read of the whole object.
// Each way is read one request at a time and packedScanReaders at once,
// for instances of 16 KiB and of 128 KiB (a small MR slice, a CT slice).
// It reports the time of one scan and its MB/s, and fails if a byte read
// differs from what was put, so a fast wrong answer cannot pass. A
// driver's benchmark calls it with a fresh store.
func BenchPackedScan(b *testing.B, s blob.Store) {
	ctx := context.Background()
	var seed uint64 // each instance's bytes differ, so a read of the wrong one fails
	for _, size := range []int{16 << 10, 128 << 10} {
		objects := make([]string, PackedScanInstances)
		var packed bytes.Buffer
		for i := range objects {
			objects[i] = fmt.Sprintf("bench/scan-%d/one/%04d.dcm", size, i)
			seed++
			data := PatternBytes(seed, size)
			if _, err := s.Put(ctx, objects[i], bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
			packed.Write(data)
		}
		packedKey := fmt.Sprintf("bench/scan-%d/packed.bin", size)
		want := packed.Bytes()
		if _, err := s.Put(ctx, packedKey, bytes.NewReader(want)); err != nil {
			b.Fatal(err)
		}
		// read returns the bytes of instance i, kept the way the case says.
		type way struct {
			name string
			read func(i int) (io.ReadCloser, error)
		}
		ways := []way{
			{"objects", func(i int) (io.ReadCloser, error) { return s.Read(ctx, objects[i], 0, -1) }},
			{"packed-ranges", func(i int) (io.ReadCloser, error) {
				return s.Read(ctx, packedKey, int64(i*size), int64(size))
			}},
		}
		for _, w := range ways {
			for _, readers := range []int{1, packedScanReaders} {
				b.Run(fmt.Sprintf("%dKB/%s/readers-%d", size>>10, w.name, readers), func(b *testing.B) {
					b.SetBytes(int64(len(want)))
					for b.Loop() {
						readScan(b, w.read, want, size, readers)
					}
					b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/scan")
				})
			}
		}
		b.Run(fmt.Sprintf("%dKB/packed-whole", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(want)))
			for b.Loop() {
				r, err := s.Read(ctx, packedKey, 0, -1)
				if err != nil {
					b.Fatal(err)
				}
				got, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil || !bytes.Equal(got, want) {
					b.Fatalf("the packed object read back %d bytes, %v; want its %d bytes as put", len(got), err, len(want))
				}
			}
			b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/scan")
		})
	}
}

// readScan reads every instance of the scan by read, readers at once, and
// fails b unless each is the bytes put.
func readScan(b *testing.B, read func(i int) (io.ReadCloser, error), want []byte, size, readers int) {
	next := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for range readers {
		wg.Go(func() {
			buf := make([]byte, 0, size)
			for i := range next {
				r, err := read(i)
				if err == nil {
					buf, err = readAllInto(buf[:0], r)
					_ = r.Close()
				}
				if err == nil && !bytes.Equal(buf, want[i*size:(i+1)*size]) {
					err = fmt.Errorf("instance %d read back other bytes than were put", i)
				}
				if err != nil {
					mu.Lock()
					failed = err
					mu.Unlock()
				}
			}
		})
	}
	for i := range len(want) / size {
		next <- i
	}
	close(next)
	wg.Wait()
	if failed != nil {
		b.Fatal(failed)
	}
}

// readAllInto appends what r yields to buf.
func readAllInto(buf []byte, r io.Reader) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}
