package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// longHeaders returns n copies of a corpus file, each its own instance of
// one series, with private text elements of about 840 KB before its pixel
// data, so each instance's line holds a data set near the longest a line
// keeps.
func longHeaders(t *testing.T, n int) [][]byte {
	t.Helper()
	extra := explicitShort(0x7FDF, 0x0010, "LO", []byte("NOPACS TEST "))
	for e := range 14 {
		extra = append(extra, explicitLong(0x7FDF, uint16(0x1001+e), "UT", bytes.Repeat([]byte("abcdefgh"), 7500))...) //nolint:gosec // G115: e < 14.
	}
	base := insertBeforePixelData(t, corpusFile(t, "MR_small.dcm"), extra)
	sop := uidsOfFile(t, base).sop
	out := make([][]byte, n)
	for i := range n {
		out[i] = bytes.ReplaceAll(base, []byte(sop), []byte(sop[:len(sop)-4]+fmt.Sprintf("%04d", 1000+i)))
	}
	return out
}

// peakHeap runs f with the collector run often, so the heap in use follows
// what is live, and returns how far the heap in use rose above where it
// was before f, at the highest of the samples taken while f ran.
func peakHeap(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// TestDICOMwebSearchStreamsItsAnswer guards the memory of a search of
// instances with includefield=all, whose results each hold a data set of
// up to a megabyte: the answer is written one result at a time, as a JSON
// array that is whole and valid, so the heap does not grow with the
// answer; 24 results of about 840 KB each never raise the live heap by
// more than a few of them.
func TestDICOMwebSearchStreamsItsAnswer(t *testing.T) {
	const n = 24
	f := newPlaceFixture(t)
	receiveArchive(t, f, longHeaders(t, n))
	s := serveWeb(t, f.env)
	path := webPath("instances?includefield=all&limit=1000")
	a := s.get(t, http.MethodGet, path, f.lab1Reader)
	if a.status != http.StatusOK || a.readErr != nil || a.header.Get("Content-Length") != "" {
		t.Fatalf("status %d, Content-Length %q, read %v", a.status, a.header.Get("Content-Length"), a.readErr)
	}
	if got := results(t, a); len(got) != n || len(a.body) < n*800_000 {
		t.Fatalf("%d results in %d bytes, want %d of about 840 KB", len(got), len(a.body), n)
	}
	// The same search again, its body counted and let go as it arrives, so
	// what the heap holds is the server's.
	var read int64
	rise := peakHeap(func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.lab1Reader.key)
		res, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		read, err = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("status %d, read %v", res.StatusCode, err)
		}
	})
	if read != int64(len(a.body)) {
		t.Fatalf("the second answer held %d bytes, the first %d", read, len(a.body))
	}
	t.Logf("the heap rose by %.1f MiB during a search of %d MiB of results", float64(rise)/(1<<20), read>>20)
	if rise > 16<<20 {
		t.Errorf("the heap rose by %d MiB during a search of %d MiB of results, want under 16 MiB", rise>>20, read>>20)
	}
}

// TestDICOMwebSearchHoldsAnOpenRead guards that a search counts as one of
// the open reads the server serves at once, as a retrieve does: with every
// one held, a search is refused with 429 and reads nothing, and once one
// is free it is answered.
func TestDICOMwebSearchHoldsAnOpenRead(t *testing.T) {
	w := newWebFixture(t)
	release := api.HoldServerReads(w.s.h)
	for _, rest := range []string{"studies", "series", "instances?PatientName=nobody"} {
		if a := w.get(t, rest); a.status != http.StatusTooManyRequests {
			t.Errorf("%s with every open read held: %d %s", rest, a.status, clipBody(a.body))
		}
	}
	release()
	if a := w.get(t, "studies"); a.status != http.StatusOK {
		t.Errorf("a search once the reads are free: %d %s", a.status, clipBody(a.body))
	}
}

// TestOpenReadsAreSharedWithSenders guards the server's one bound on reads
// of archived bytes: with every read of the count the handlers are given
// held elsewhere (as the senders hold theirs, under their own owner names),
// a search is refused with 429, and once one is free it is answered.
func TestOpenReadsAreSharedWithSenders(t *testing.T) {
	w := newWebFixture(t)
	reads := &checkedread.Slots{}
	s := serveWebWith(t, w.f.env, reads)
	var releases []func()
	for i := range api.MaxServerReads {
		release, err := reads.Take("send:lab1/s" + strconv.Itoa(i/checkedread.MaxOpenReads) + "-" + strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if a := s.get(t, http.MethodGet, webPath("studies"), w.f.lab1Reader); a.status != http.StatusTooManyRequests {
		t.Errorf("a search with every read held by senders: %d %s", a.status, clipBody(a.body))
	}
	releases[0]()
	if a := s.get(t, http.MethodGet, webPath("studies"), w.f.lab1Reader); a.status != http.StatusOK {
		t.Errorf("a search once a read is free: %d %s", a.status, clipBody(a.body))
	}
	for _, r := range releases[1:] {
		r()
	}
}

// TestDICOMwebFrameListIsBounded guards the list of frames a request may
// name: a frame named twice is refused with 400, a list of more frames than
// the instance has is refused with 400 naming both counts, and a list of
// each frame once is answered.
func TestDICOMwebFrameListIsBounded(t *testing.T) {
	w := newWebFixture(t)
	u := uidsOfFile(t, corpusFile(t, "jpeg_3frame_nobot.dcm"))
	base := "studies/" + u.study + "/series/" + u.series + "/instances/" + u.sop + "/frames/"
	if a := w.get(t, base+"1,2,1"); a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "frame 1 is named twice") {
		t.Errorf("a frame named twice: %d %s", a.status, clipBody(a.body))
	}
	if a := w.get(t, base+"1,2,3,4"); a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "names 4 frames; the instance has 3") {
		t.Errorf("more frames than the instance has: %d %s", a.status, clipBody(a.body))
	}
	if parts := partsOf(t, w.get(t, base+"3,2,1"), "image/jpeg"); len(parts) != 3 {
		t.Errorf("each frame once: %d parts", len(parts))
	}
}

// TestDICOMwebFramesCutAChangedFrame guards the check of each frame against
// the SHA-256 its line records: a frame whose stored bytes were changed
// after it was archived (the file's size the same) is never sent as whole;
// the frames before it are sent whole and the body is cut before its
// closing boundary, as a changed instance is.
func TestDICOMwebFramesCutAChangedFrame(t *testing.T) {
	w := newWebFixture(t)
	file := corpusFile(t, "jpeg_3frame_nobot.dcm")
	u := uidsOfFile(t, file)
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	table, err := frames.Table(h, bytes.NewReader(file))
	if err != nil || len(table) != 3 {
		t.Fatalf("the file's frame table: %d frames, %v", len(table), err)
	}
	var in tenant.ScanInstance
	for _, held := range w.a.held(t, scanOf(t, w.a, u)) {
		if held.SOPInstanceUID == u.sop {
			in = held
		}
	}
	last := table[1][len(table[1])-1]
	changed := slices.Clone(w.a.sent[u.sop])
	at := last.Offset + last.Length/2
	changed[at] ^= 0xFF
	if _, err := w.a.store.Put(context.Background(), in.Key, bytes.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	a := w.get(t, "studies/"+u.study+"/series/"+u.series+"/instances/"+u.sop+"/frames/1,2")
	if a.status == http.StatusOK && a.readErr == nil && bytes.HasSuffix(bytes.TrimSpace(a.body), []byte("--")) {
		t.Error("a changed frame was sent in a body that looks whole")
	}
	var frame1 []byte
	for _, r := range table[0] {
		frame1 = append(frame1, file[r.Offset:r.Offset+r.Length]...)
	}
	if !bytes.Contains(a.body, frame1) {
		t.Error("the frame before the changed one was not sent whole")
	}
	if bytes.Contains(a.body, changed[at-16:at+1]) {
		t.Error("the changed bytes reached the client")
	}
}
