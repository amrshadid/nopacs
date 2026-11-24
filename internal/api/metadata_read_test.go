package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readCounter wraps a store and counts the bytes read from each key.
type readCounter struct {
	blob.Store
	read map[string]*atomic.Int64
}

func newReadCounter(s blob.Store) *readCounter {
	return &readCounter{Store: s, read: map[string]*atomic.Int64{}}
}

// bytesOf returns how many bytes were read from key.
func (c *readCounter) bytesOf(key string) int64 {
	if n := c.read[key]; n != nil {
		return n.Load()
	}
	return 0
}

type countedBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countedBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *readCounter) Read(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	rc, err := c.Store.Read(ctx, key, off, length)
	if err != nil {
		return rc, err
	}
	n := c.read[key]
	if n == nil {
		n = &atomic.Int64{}
		c.read[key] = n
	}
	return countedBody{rc, n}, nil
}

// encapsulatedFile returns a file in JPEG 2000 Lossless (an encapsulated
// transfer syntax) of frames fragments of fragment bytes each, which a
// reader skips item by item.
func encapsulatedFile(t *testing.T, frames, fragment int) []byte {
	t.Helper()
	pad := func(u string) []byte {
		if len(u)%2 == 1 {
			return append([]byte(u), 0)
		}
		return []byte(u)
	}
	us := func(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
	const ts = "1.2.840.10008.1.2.4.90"
	sop := "1.2.826.0.1.3680043.8.498.77"
	var meta []byte
	meta = append(meta, explicitLong(2, 1, "OB", []byte{0, 1})...)
	meta = append(meta, explicitShort(2, 2, "UI", pad("1.2.840.10008.5.1.4.1.1.4"))...)
	meta = append(meta, explicitShort(2, 3, "UI", pad(sop))...)
	meta = append(meta, explicitShort(2, 0x10, "UI", pad(ts))...)
	meta = append(meta, explicitShort(2, 0x12, "UI", pad("1.2.3.4"))...)
	out := append(make([]byte, 128), "DICM"...)
	out = append(out, explicitShort(2, 0, "UL", binary.LittleEndian.AppendUint32(nil, uint32(len(meta))))...) // #nosec G115 -- a test's small value.
	out = append(out, meta...)
	out = append(out, explicitShort(8, 0x16, "UI", pad("1.2.840.10008.5.1.4.1.1.4"))...)
	out = append(out, explicitShort(8, 0x18, "UI", pad(sop))...)
	out = append(out, explicitShort(0x10, 0x20, "LO", pad("P1"))...)
	out = append(out, explicitShort(0x20, 0x0D, "UI", pad("1.2.826.0.1.3680043.8.498.78"))...)
	out = append(out, explicitShort(0x20, 0x0E, "UI", pad("1.2.826.0.1.3680043.8.498.79"))...)
	out = append(out, explicitShort(0x28, 2, "US", us(1))...)
	out = append(out, explicitShort(0x28, 4, "CS", pad("MONOCHROME2"))...)
	out = append(out, explicitShort(0x28, 8, "IS", pad(strconv.Itoa(frames)))...)
	out = append(out, explicitShort(0x28, 0x10, "US", us(64))...)
	out = append(out, explicitShort(0x28, 0x11, "US", us(64))...)
	out = append(out, explicitShort(0x28, 0x100, "US", us(8))...)
	out = append(out, explicitShort(0x28, 0x101, "US", us(8))...)
	out = append(out, explicitShort(0x28, 0x102, "US", us(7))...)
	out = append(out, explicitShort(0x28, 0x103, "US", us(0))...)
	out = append(out, 0xE0, 0x7F, 0x10, 0x00, 'O', 'B', 0, 0, 0xFF, 0xFF, 0xFF, 0xFF)
	item := func(n int) []byte {
		b := []byte{0xFE, 0xFF, 0x00, 0xE0}
		return binary.LittleEndian.AppendUint32(b, uint32(n)) // #nosec G115 -- a test's small value.
	}
	out = append(out, item(0)...)
	for range frames {
		out = append(out, item(fragment)...)
		out = append(out, bytes.Repeat([]byte{0x5A}, fragment)...)
	}
	out = append(out, 0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0)
	return out
}

// TestHeaderOfAnEncapsulatedFileDoesNotReadItsFrames guards the cost of
// answering the header of a file of many large frames: the reader skips each
// frame by reading its item header, and each such jump must fetch a few
// kilobytes, not a read-ahead of a quarter of a megabyte, so the bytes read
// from the storage stay a small fraction of the file.
func TestHeaderOfAnEncapsulatedFileDoesNotReadItsFrames(t *testing.T) {
	file := encapsulatedFile(t, 120, 100_000)
	store, path, held, line := storedInstance(t, file)
	cs := newReadCounter(store)
	m, err := api.MetaInstance(context.Background(), cs, "lab1", path, 64<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetTransferSyntaxUid() != "1.2.840.10008.1.2.4.90" || !strings.Contains(m.GetDicomJson(), "BulkDataURI") {
		t.Fatalf("transfer syntax %q, JSON %.200s", m.GetTransferSyntaxUid(), m.GetDicomJson())
	}
	got, limit := cs.bytesOf(held.Key), int64(len(file))/100
	if got > limit {
		t.Errorf("%d bytes read from a file of %d to answer its header, want at most %d", got, len(file), limit)
	}
}

// pageLine returns the sidecar line and the catalog row of an instance with
// a header of plain values only, so a page of it is answered from the line
// alone.
func pageLine(t *testing.T, path label.Path, uid string, ts string) (sidecar.Instance, tenant.ScanInstance) {
	t.Helper()
	key, err := layout.Key{Kind: layout.Instance, Path: path, SOPInstanceUID: uid}.String()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("ab", 32)
	set := `{"00080018":{"vr":"UI","Value":["` + uid + `"]},"00100020":{"vr":"LO","Value":["` + strings.Repeat("P", 600) + `"]}}`
	in := sidecar.Instance{SOPInstanceUID: uid, Key: key, Size: 1000, SHA256: sha, TransferSyntax: ts, Dataset: []byte(set)}
	return in, tenant.ScanInstance{SOPInstanceUID: uid, Key: key, Size: 1000}
}

// TestPageFromTheLineOpensNoInstanceFile guards that a line that names its
// instance's transfer syntax and holds its header answers it without any
// read of the instance's file, and that a line from before the field
// existed costs one small read of the start of the file.
func TestPageFromTheLineOpensNoInstanceFile(t *testing.T) {
	file := corpusFile(t, "MR_small.dcm")
	store, path, held, line := storedInstance(t, file)
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	js, err := h.DICOMJSON()
	if err != nil {
		t.Fatal(err)
	}
	line.DatasetOmitted, line.Dataset = false, js
	line.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
	line.TransferSyntax = h.TransferSyntaxUID
	cs := newReadCounter(store)
	m, err := api.MetaInstance(context.Background(), cs, "lab1", path, 1<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetTransferSyntaxUid() != h.TransferSyntaxUID || m.GetDicomJson() == "" {
		t.Fatalf("transfer syntax %q, JSON %.100s", m.GetTransferSyntaxUid(), m.GetDicomJson())
	}
	if n := cs.bytesOf(held.Key); n != 0 {
		t.Errorf("%d bytes of the instance's file were read for a line that holds what the answer needs", n)
	}
	line.TransferSyntax = ""
	cs = newReadCounter(store)
	if _, err := api.MetaInstance(context.Background(), cs, "lab1", path, 1<<20, held, line); err != nil {
		t.Fatal(err)
	}
	if n := cs.bytesOf(held.Key); n == 0 || n > 8<<10 {
		t.Errorf("%d bytes of the file were read for the transfer syntax of an older line, want 1 to 8192", n)
	}
}

// TestPageOfALargeScanStartsNearItsInstances guards the index a large
// metadata file ends with: a page near the end of a scan of a thousand
// instances reads a small part of the file, and answers the right instances,
// where a read from the first line would read nearly all of it.
func TestPageOfALargeScanStartsNearItsInstances(t *testing.T) {
	path := label.Path{Subject: "S1", Session: "E1", Scan: "T1"}
	const n = 1000
	uidOf := func(i int) string { return "1.2." + strconv.Itoa(1000000+i) }
	var buf bytes.Buffer
	w := sidecar.NewWriter(&buf)
	var helds []tenant.ScanInstance
	for i := range n {
		in, held := pageLine(t, path, uidOf(i), "1.2.840.10008.1.2.1")
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
		helds = append(helds, held)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key, err := layout.MetadataKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), key, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 300<<10 {
		t.Fatalf("the metadata file is %d bytes, too small to use its index", buf.Len())
	}
	cs := newReadCounter(store)
	got, err := api.MetaPage(context.Background(), cs, "lab1", path, helds[900:903])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].GetInstanceId() != uidOf(900) || got[2].GetInstanceId() != uidOf(902) {
		t.Fatalf("answered %d instances, first %q", len(got), got[0].GetInstanceId())
	}
	if read, limit := cs.bytesOf(key), int64(buf.Len())/4; read > limit {
		t.Errorf("%d bytes read of a metadata file of %d for a page near its end, want at most %d", read, buf.Len(), limit)
	}
}

// TestEmptyPixelDataHasAReferenceOnBothPaths guards that Pixel Data of no
// bytes is answered with a bulk data reference, whatever its length, both
// from a line of the metadata file and from the instance's file, which must
// agree.
func TestEmptyPixelDataHasAReferenceOnBothPaths(t *testing.T) {
	src := corpusFile(t, "MR_small.dcm")
	h, err := header.Read(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	file := append(append([]byte{}, src[:h.PixelData.Offset]...), explicitLong(0x7FE0, 0x10, "OW", nil)...)
	file = append(file, src[h.PixelData.Offset+h.PixelData.Length:]...)
	store, path, held, line := storedInstance(t, file)
	h, err = header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	js, err := h.DICOMJSON()
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := api.MetaInstance(context.Background(), store, "lab1", path, 1<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	line.DatasetOmitted, line.Dataset = false, js
	line.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
	fromLine, err := api.MetaInstance(context.Background(), store, "lab1", path, 1<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]string{"file": fromFile.GetDicomJson(), "line": fromLine.GetDicomJson()} {
		if !strings.Contains(m, "BulkDataURI") || !strings.Contains(m, "length=0") {
			t.Errorf("%s: empty pixel data has no reference with length 0: %.300s", name, m)
		}
	}
	if fromFile.GetDicomJson() != fromLine.GetDicomJson() {
		t.Errorf("the two paths differ:\n%s\n%s", fromFile.GetDicomJson(), fromLine.GetDicomJson())
	}
}
