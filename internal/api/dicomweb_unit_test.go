package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	blobdir "github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// rewrittenAfterTail is a storage on which the scan's metadata file is
// replaced by next right after the first ranged read of its tail, which is
// where the file's index is read: the index read is then the old file's,
// and every read after it sees the new one, as when the frame pass or an
// archive into the scan rewrites the file between the two reads.
type rewrittenAfterTail struct {
	blob.Store
	key     string
	next    []byte
	swapped atomic.Bool
}

func (s *rewrittenAfterTail) Read(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	rc, err := s.Store.Read(ctx, key, off, n)
	if err != nil || key != s.key || n <= 0 || off <= 0 || s.swapped.Swap(true) {
		return rc, err
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	if _, err := s.Put(ctx, key, bytes.NewReader(s.next)); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// indexedSidecar returns a metadata file of 600 instances of the scan p,
// each line's data set padded with pad characters, so the file is long
// enough to be read from its index; and the catalog's rows of them.
func indexedSidecar(t *testing.T, p label.Path, pad int) ([]byte, []tenant.ScanInstance) {
	t.Helper()
	var buf bytes.Buffer
	w := sidecar.NewWriter(&buf)
	var held []tenant.ScanInstance
	for i := range 600 {
		u := fmt.Sprintf("1.2.3.%d", 1000+i)
		k, err := layout.InstanceKey(p, u)
		if err != nil {
			t.Fatal(err)
		}
		ds, _ := json.Marshal(map[string]any{"00100010": map[string]any{"vr": "PN", "Value": []any{map[string]string{"Alphabetic": strings.Repeat("x", pad)}}}})
		if err := w.Add(sidecar.Instance{SOPInstanceUID: u, Key: k, Size: 10, SHA256: strings.Repeat("a", 64), Dataset: ds}); err != nil {
			t.Fatal(err)
		}
		held = append(held, tenant.ScanInstance{SOPInstanceUID: u, Key: k, Length: 10, Size: 10})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), held
}

// dirSource returns a metadata source over a directory storage that holds
// the scan p's metadata file meta, with the storage wrap makes of it.
func dirSource(t *testing.T, p label.Path, meta []byte, wrap func(blob.Store, string) blob.Store) *metaSource {
	t.Helper()
	st, err := blobdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, err := layout.MetadataKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), key, bytes.NewReader(meta)); err != nil {
		t.Fatal(err)
	}
	var store blob.Store = st
	if wrap != nil {
		store = wrap(st, key)
	}
	return &metaSource{store: store, path: p, key: key, limit: 1 << 20}
}

// TestLineOfReadsAgainAfterARewrite guards a read of a line that races a
// rewrite of the scan's metadata file: the index read from the old file
// names a place in the middle of a line of the new one, and the read is
// made once more with the index read again, so it answers the line, never
// data loss; and a file whose index names a wrong place on both reads is
// data loss.
func TestLineOfReadsAgainAfterARewrite(t *testing.T) {
	p := label.Path{Subject: "sub1", Session: "ses1", Scan: "scan1"}
	old, held := indexedSidecar(t, p, 500)
	next, _ := indexedSidecar(t, p, 700)
	var swap *rewrittenAfterTail
	src := dirSource(t, p, old, func(st blob.Store, key string) blob.Store {
		swap = &rewrittenAfterTail{Store: st, key: key, next: next}
		return swap
	})
	target := held[450]
	if start := sidecar.Locate(mustIndex(t, old), target.SOPInstanceUID); start == 0 {
		t.Fatal("the file is read from its first line; the test needs a read from its index")
	}
	_, rc, in, err := src.lineOf(context.Background(), target.SOPInstanceUID, nil)
	if err != nil {
		t.Fatalf("a read across a rewrite: %v", err)
	}
	_ = rc.Close()
	if !swap.swapped.Load() || in.SOPInstanceUID != target.SOPInstanceUID || !strings.Contains(string(in.Dataset), strings.Repeat("x", 700)) {
		t.Errorf("a read across a rewrite answered %s (rewritten: %v), want the new file's line of %s", in.SOPInstanceUID, swap.swapped.Load(), target.SOPInstanceUID)
	}

	// An index whose every offset is a few bytes past its line.
	bad := shiftIndex(t, old, 5)
	src = dirSource(t, p, bad, nil)
	if _, _, _, err := src.lineOf(context.Background(), target.SOPInstanceUID, nil); fault.KindOf(err) != fault.DataLoss {
		t.Errorf("an index that names the wrong place twice: %v, want data loss", err)
	}
}

// mustIndex returns the index a metadata file ends with.
func mustIndex(t *testing.T, meta []byte) []sidecar.IndexEntry {
	t.Helper()
	entries, complete := sidecar.ParseIndex(meta, true)
	if !complete || entries == nil {
		t.Fatal("the metadata file has no index")
	}
	return entries
}

// shiftIndex returns the metadata file with each offset of its index moved
// by d bytes.
func shiftIndex(t *testing.T, meta []byte, d int64) []byte {
	t.Helper()
	body := meta[:len(meta)-1]
	i := bytes.LastIndexByte(body, '\n')
	entries := mustIndex(t, meta)
	for k := range entries {
		entries[k].Offset += d
	}
	line, err := json.Marshal(map[string]any{"index": entries})
	if err != nil {
		t.Fatal(err)
	}
	return append(append(bytes.Clone(body[:i+1]), line...), '\n')
}

// TestSearchReadsOfMetadataAreBounded guards the bound on what a request
// reads of the scans' metadata files: the bytes read count toward it as
// they come from the storage, a read once it is met stops, and the
// request knows it met the bound, which a search answers with 413.
func TestSearchReadsOfMetadataAreBounded(t *testing.T) {
	p := label.Path{Subject: "sub1", Session: "ses1", Scan: "scan1"}
	meta, held := indexedSidecar(t, p, 500)
	src := dirSource(t, p, meta, nil)
	l := &lines{max: 32 << 10}
	sl := &scanLines{src: src, close: func() {}, wrap: l.count}
	// The first line comes with the first buffer read, which meets the
	// bound; the next one, 300 lines on, needs reads past it.
	if _, err := sl.line(context.Background(), held[260].SOPInstanceUID); err != nil {
		t.Fatal(err)
	}
	_, err := sl.line(context.Background(), held[590].SOPInstanceUID)
	if err == nil || !l.exceeded() || l.read > 128<<10 {
		t.Fatalf("a read past a bound of 32 KiB: %v, bound met %v after %d bytes", err, l.exceeded(), l.read)
	}
	l = &lines{max: 4 << 20}
	sl = &scanLines{src: src, close: func() {}, wrap: l.count}
	if _, err := sl.line(context.Background(), held[590].SOPInstanceUID); err != nil || l.exceeded() || l.read == 0 {
		t.Errorf("a read within the bound: %v, %d bytes counted", err, l.read)
	}
}

// TestAnswerFits guards the cut of a search's answer at its bound of
// bytes: as many results as fit, the first always, so an answer of one
// large result is answered.
func TestAnswerFits(t *testing.T) {
	for _, c := range []struct {
		costs []int64
		max   int64
		want  int
	}{
		{[]int64{10, 10, 10}, 25, 2},
		{[]int64{10, 10, 10}, 30, 3},
		{[]int64{100, 1}, 10, 1},
		{nil, 10, 0},
	} {
		if got := answerFits(c.costs, c.max); got != c.want {
			t.Errorf("answerFits(%v, %d) = %d, want %d", c.costs, c.max, got, c.want)
		}
	}
}

// countedStore is a storage that counts the reads made of each key.
type countedStore struct {
	blob.Store
	mu    sync.Mutex
	reads map[string]int
}

func (s *countedStore) Read(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.reads[key]++
	s.mu.Unlock()
	return s.Store.Read(ctx, key, off, n)
}

// TestFramesArePlannedFromOneTable guards the plan of a request of several
// frames of an instance whose line holds no table: the instance's header
// and pixel data items are read once for the request, so three frames cost
// the reads one does, never a header read a frame.
func TestFramesArePlannedFromOneTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "jpeg_3frame_nobot.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	sop, _ := h.UID(header.SOPInstanceUID)
	p := label.Path{Subject: "sub1", Session: "ses1", Scan: "scan1"}
	k, err := layout.InstanceKey(p, sop)
	if err != nil {
		t.Fatal(err)
	}
	st, err := blobdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Put(context.Background(), k, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	line := sidecar.Instance{SOPInstanceUID: sop, Key: k, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]),
		PixelData: &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}}
	held := tenant.ScanInstance{SOPInstanceUID: sop, Key: k, Size: int64(len(b)), Length: int64(len(b))}
	reads := func(numbers []int) int {
		cs := &countedStore{Store: st, reads: map[string]int{}}
		src := &metaSource{store: cs, path: p, limit: 1 << 20}
		_, plan, err := src.planFrames(context.Background(), held, line, numbers)
		if err != nil || len(plan) != len(numbers) {
			t.Fatalf("frames %v: %d planned, %v", numbers, len(plan), err)
		}
		return cs.reads[k]
	}
	one, three := reads([]int{2}), reads([]int{3, 1, 2})
	if one == 0 || three != one {
		t.Errorf("one frame took %d reads of the file, three took %d, want the same", one, three)
	}
}
