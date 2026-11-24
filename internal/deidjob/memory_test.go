package deidjob

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// multiFrame writes a synthetic MR image of frames 16-bit frames of rows by
// cols, native, with a few attributes the basic profile changes: no
// patient's data, a pattern of samples.
func multiFrame(t testing.TB, rows, cols, frames int) []byte {
	t.Helper()
	ds := dataset.NewDataset()
	ds.SetTransferSyntaxUID("1.2.840.10008.1.2.1")
	us := func(v int) []byte { return []byte{byte(v & 0xFF), byte(v >> 8 & 0xFF)} }
	px := make([]byte, rows*cols*frames*2)
	for i := range px {
		px[i] = byte(1 + (i/7)%250)
	}
	for _, e := range []struct {
		t  uint32
		vr dataelem.VR
		v  any
	}{
		{0x00080016, dataelem.UI, "1.2.840.10008.5.1.4.1.1.4"}, {0x00080018, dataelem.UI, "2.25.4811"},
		{0x00080020, dataelem.DA, "20240102"}, {0x00080060, dataelem.CS, "MR"}, {0x00100010, dataelem.PN, "SYNTHETIC^ONE"},
		{0x00100020, dataelem.LO, "SYNTHETIC-1"}, {0x0020000D, dataelem.UI, "2.25.4812"}, {0x0020000E, dataelem.UI, "2.25.4813"},
		{0x00280002, dataelem.US, us(1)}, {0x00280004, dataelem.CS, "MONOCHROME2"},
		{0x00280008, dataelem.IS, strconv.Itoa(frames)},
		{0x00280010, dataelem.US, us(rows)}, {0x00280011, dataelem.US, us(cols)},
		{0x00280100, dataelem.US, us(16)}, {0x00280101, dataelem.US, us(16)}, {0x00280102, dataelem.US, us(15)},
		{0x00280103, dataelem.US, us(0)}, {0x7FE00010, dataelem.OW, px},
	} {
		v := e.v
		if s, ok := v.(string); ok {
			v = []byte(s)
		}
		if err := ds.Add(dataelem.NewDataElement(tag.Tag(e.t), e.vr, v)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "mf.dcm")
	f, err := os.Create(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	fw := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	fw.SetFileMetaInfo(&filewriter.FileMetaInfo{MediaStorageSOPClassUID: "1.2.840.10008.5.1.4.1.1.4",
		MediaStorageSOPInstanceUID: "2.25.4811", TransferSyntaxUID: "1.2.840.10008.1.2.1"})
	if err := fw.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// peakOf returns the most heap fn holds at once beyond what was held
// before it, sampled every 50 microseconds.
func peakOf(fn func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	done := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > base && m.HeapAlloc-base > peak.Load() {
				peak.Store(m.HeapAlloc - base)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	fn()
	close(done)
	return peak.Load()
}

// TestDryRunMemoryOfAMultiFrameFile measures what a dry run holds for one
// file of 40 frames of 512 by 512 (20 MiB), passed by the basic profile,
// beyond the file itself: at most 2.5 times the file (the written copy,
// given its room once, and the check's reading of it), the factor doc.go
// states; and never more without the copies of values than with them
// (bulk values stay in the file, so those copies are small here).
func TestDryRunMemoryOfAMultiFrameFile(t *testing.T) {
	if testing.Short() || raceOn {
		t.Skip("measures the heap of a 20 MiB file: not with -short, and not under the race detector")
	}
	file := multiFrame(t, 512, 512, 40)
	v, err := profile.New("research", 1, basicProfile(), profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := throwawayKeys()
	en, err := engine.New(v.Expanded(), keys, engine.Options{Method: "research@1"})
	if err != nil {
		t.Fatal(err)
	}
	try := func(noValues bool) (receive.Trial, uint64) {
		var tr receive.Trial
		peak := peakOf(func() {
			tr, err = receive.TryFile(context.Background(), en, file, engine.Reception{}, receive.TrialOptions{Keys: keys,
				HeaderLimit: header.MaxHeader, PixelLimit: MaxPixelBytes, NoValues: noValues,
				Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }})
		})
		if err != nil || tr.Hold != nil {
			t.Fatalf("the file is not passed: %v, %+v", err, tr.Hold)
		}
		return tr, peak
	}
	_, without := try(true)
	_, with := try(false)
	size := uint64(len(file))
	t.Logf("a %d-byte file: peak %.2fx without values, %.2fx with them", size, float64(without)/float64(size), float64(with)/float64(size))
	if without > 5*size/2 || without > with+size/100 {
		t.Errorf("a dry run holds %d bytes over a %d-byte file (with values %d)", without, size, with)
	}
}
