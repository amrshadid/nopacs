package receive

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// scaleEnv, when set, runs the scale test of many files, which takes minutes
// where every sync reaches the drive (F_FULLFSYNC on macOS).
const scaleEnv = "NOPACS_TEST_SCALE"

// TestScaleLargeFileInBoundedMemory guards that memory does not grow with a
// file's size: a generated file of 512 MiB is landed, archived and read back
// while the live heap stays under 64 MiB.
func TestScaleLargeFileInBoundedMemory(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("writes a 512 MiB file twice; left out under -short and the race detector")
	}
	g := newGenerator(t)
	e := newEnv(t, envOptions{})
	runtime.GC()
	stop := testkit.HeapPeak()
	r, size := g.File(0, 512<<20)
	start := time.Now()
	e.sendReader(r)
	rs := e.settle(1)
	peak := stop()
	if !done(rs[0]) || rs[0].Counts.BytesArchived != size {
		t.Fatalf("reception %+v", rs[0])
	}
	t.Logf("%d MiB from land to verified in %v, live heap peak %.1f MiB", size>>20, time.Since(start).Round(time.Millisecond), float64(peak)/(1<<20))
	if peak > 64<<20 {
		t.Fatalf("the live heap reached %.1f MiB for one file of %d MiB", float64(peak)/(1<<20), size>>20)
	}
}

// TestScaleManyFilesKeepTheHeapFlat guards that memory does not grow with
// the number of files in a reception beyond their rows: one reception of
// 5,000 small files, one scan, peaks within 32 MiB of a reception of 500.
func TestScaleManyFilesKeepTheHeapFlat(t *testing.T) {
	if testing.Short() || raceEnabled || os.Getenv(scaleEnv) == "" {
		t.Skip("a reception of 5,000 files, minutes on a disk whose sync is slow; run by make test-scale (" + scaleEnv + "=1)")
	}
	g := newGenerator(t)
	peakOf := func(n int) uint64 {
		e := newEnv(t, envOptions{workers: 4})
		e.deadline = 20 * time.Minute
		runtime.GC()
		stop := testkit.HeapPeak()
		begin := time.Now()
		for i := range n {
			r, _ := g.File(i, 2048)
			e.sendReader(r)
		}
		sent := time.Since(begin)
		rs := e.settle(1)
		peak := stop()
		t.Logf("%d files: sent in %v, done in %v", n, sent.Round(time.Millisecond), time.Since(begin).Round(time.Millisecond))
		if !done(rs[0]) || rs[0].Counts.FilesArchived != int64(n) {
			t.Fatalf("reception %+v", rs[0])
		}
		if left := e.incomingFiles(); left != 0 {
			t.Fatalf("%d files left in the incoming area", left)
		}
		_ = e.p.Stop(e.ctx)
		return peak
	}
	small := peakOf(500)
	large := peakOf(5000)
	t.Logf("live heap peak: %.1f MiB for 500 files, %.1f MiB for 5,000", float64(small)/(1<<20), float64(large)/(1<<20))
	if large > small+32<<20 {
		t.Fatalf("the heap grew with the number of files: %.1f MiB for 500, %.1f MiB for 5,000", float64(small)/(1<<20), float64(large)/(1<<20))
	}
}
