//go:build unix

package api_test

import (
	"bytes"
	"fmt"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// openFiles counts the file descriptors the process holds open among the
// first 4096.
func openFiles() int {
	n := 0
	for fd := range 4096 {
		// A descriptor is open when it can be duplicated.
		if d, err := syscall.Dup(fd); err == nil {
			_ = syscall.Close(d)
			n++
		}
	}
	return n
}

// manySeries returns n copies of a corpus file, each the one instance of a
// series of its own, so each is archived as a scan of its own.
func manySeries(t *testing.T, n int) [][]byte {
	t.Helper()
	base := corpusFile(t, "MR_small.dcm")
	u := uidsOfFile(t, base)
	out := make([][]byte, n)
	for i := range n {
		suffix := fmt.Sprintf("%04d", 1000+i)
		b := bytes.ReplaceAll(base, []byte(u.series), []byte(u.series[:len(u.series)-4]+suffix))
		out[i] = bytes.ReplaceAll(b, []byte(u.sop), []byte(u.sop[:len(u.sop)-4]+suffix))
	}
	return out
}

// TestDICOMwebSearchHoldsOneScanOpen guards what a search of instances over
// many scans holds: one scan's metadata file and storage open at a time,
// the one before closed when it moves on, so a search that passes 120
// scans holds a few more files open than before it, never one per scan.
func TestDICOMwebSearchHoldsOneScanOpen(t *testing.T) {
	const n = 120
	f := newPlaceFixture(t)
	receiveArchive(t, f, manySeries(t, n))
	s := serveWeb(t, f.env)
	// A first search opens what every request opens once (connections,
	// the catalog's files), so the count before the measured one is of a
	// server at rest.
	s.get(t, http.MethodGet, webPath("instances?PatientName=nobody"), f.lab1Reader)
	before := openFiles()
	var peak atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			if v := int64(openFiles()); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	a := s.get(t, http.MethodGet, webPath("instances?PatientName=nobody"), f.lab1Reader)
	close(stop)
	<-done
	if a.status != http.StatusNoContent {
		t.Fatalf("a search that matches nothing: %d %s", a.status, clipBody(a.body))
	}
	if rise := int(peak.Load()) - before; rise > 20 {
		t.Errorf("a search over %d scans held %d more files open at its peak, want a few", n, rise)
	}
}
