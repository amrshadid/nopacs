package dimse_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestScaleLargeFileThroughTheListenerInBoundedMemory measures the live
// heap while storescu sends a 512 MiB file through the listener into the
// pipeline's incoming area, and guards that it stays under 64 MiB: the
// library hands the data set over a PDU at a time and the pipeline copies it
// through a fixed buffer.
func TestScaleLargeFileThroughTheListenerInBoundedMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("sends a 512 MiB file; left out under the race detector")
	}
	storescu := tool(t, "storescu")
	r := row()
	a := newArchive(t, r)
	s := listen(t, r, a.p, nil)
	big := writeLarge(t, 512<<20)
	runtime.GC()
	stop := testkit.HeapPeak()
	start := time.Now()
	out, err := command(t, storescu, "-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port, big).CombinedOutput() // #nosec G204
	took := time.Since(start)
	peak := stop()
	if err != nil {
		t.Fatalf("storescu: %v\n%s", err, out)
	}
	if n := a.p.Stats().Landed.Load(); n != 1 {
		t.Fatalf("%d files landed", n)
	}
	t.Logf("512 MiB received in %v (%.0f MB/s); live heap peak %.1f MiB", took.Round(time.Millisecond),
		512*1.048576/took.Seconds(), float64(peak)/(1<<20))
	if peak > 64<<20 {
		t.Fatalf("the live heap reached %.1f MiB receiving one file of 512 MiB", float64(peak)/(1<<20))
	}
}
