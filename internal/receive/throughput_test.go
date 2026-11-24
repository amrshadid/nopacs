package receive

import (
	"testing"
	"time"
)

// TestThroughputCountsTheLastTenSeconds guards the meter a status reports:
// what passed in the last ten seconds, per second; a second past the window
// no longer counts; a bucket reused for a later second starts empty.
func TestThroughputCountsTheLastTenSeconds(t *testing.T) {
	var m meter
	t0 := time.Unix(1_000_000, 0)
	m.received(t0, 100)
	m.received(t0.Add(500*time.Millisecond), 300)
	m.archived(t0.Add(3*time.Second), 2, 400)
	got := m.rates(t0.Add(5 * time.Second))
	want := Throughput{FilesReceivedPerSecond: 0.2, BytesReceivedPerSecond: 40, FilesArchivedPerSecond: 0.2, BytesArchivedPerSecond: 40}
	if got != want {
		t.Errorf("five seconds on: %+v, want %+v", got, want)
	}
	// Ten seconds after the first second, it is out of the window; the
	// archive three seconds later is still in it.
	got = m.rates(t0.Add(10 * time.Second))
	if got.FilesReceivedPerSecond != 0 || got.FilesArchivedPerSecond != 0.2 {
		t.Errorf("ten seconds on: %+v, want the receipts gone and the archive kept", got)
	}
	// The same bucket, a window later, holds the new second only.
	m.received(t0.Add(10*time.Second), 50)
	got = m.rates(t0.Add(10 * time.Second))
	if got.FilesReceivedPerSecond != 0.1 || got.BytesReceivedPerSecond != 5 {
		t.Errorf("the bucket reused: %+v, want one file of 50 bytes", got)
	}
	if got := m.rates(t0.Add(time.Hour)); got != (Throughput{}) {
		t.Errorf("an hour on: %+v, want nothing", got)
	}
}
