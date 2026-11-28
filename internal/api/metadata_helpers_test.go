package api_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// corpusFile reads a public test file of the reception tests' corpus.
func corpusFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", name)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reUID returns the file with the SOP Instance UID uid replaced, in the
// file meta information and in the data set alike, by a UID of the same
// length that ends in the digit last, so a copy of a corpus file is another
// instance of the same series.
func reUID(t testing.TB, file []byte, last byte) []byte {
	t.Helper()
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := h.UID(header.SOPInstanceUID)
	if old == "" {
		t.Fatal("the file has no SOP Instance UID")
	}
	changed := old[:len(old)-1] + string(last)
	if changed == old {
		t.Fatalf("the UID already ends in %c", last)
	}
	return bytes.ReplaceAll(file, []byte(old), []byte(changed))
}

// explicitLong is an element in Explicit VR Little Endian with a 32-bit
// length (PS3.5 7.1.2), as OB, OW, SQ and UT are written.
func explicitLong(group, element uint16, vr string, value []byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, group)
	b = binary.LittleEndian.AppendUint16(b, element)
	b = append(b, vr...)
	b = append(b, 0, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(value))) // #nosec G115 -- a test's small value.
	return append(b, value...)
}

// explicitShort is an element in Explicit VR Little Endian with a 16-bit
// length, as LO is written.
func explicitShort(group, element uint16, vr string, value []byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, group)
	b = binary.LittleEndian.AppendUint16(b, element)
	b = append(b, vr...)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(value))) // #nosec G115 -- a test's small value.
	return append(b, value...)
}

// pixelTagBytes is the start of a Pixel Data element in Explicit VR Little
// Endian with the VR OW.
var pixelTagBytes = []byte{0xE0, 0x7F, 0x10, 0x00, 'O', 'W', 0, 0}

// insertBeforePixelData returns the explicit-VR file with extra, elements of
// a private group that sorts before Pixel Data, written just before it.
func insertBeforePixelData(t testing.TB, file, extra []byte) []byte {
	t.Helper()
	i := bytes.LastIndex(file, pixelTagBytes)
	if i < 0 {
		t.Fatal("the file has no Pixel Data element of the VR OW in explicit VR")
	}
	out := append([]byte{}, file[:i]...)
	out = append(out, extra...)
	return append(out, file[i:]...)
}

// privateBulk returns the elements of a private group (7FDF) a test adds
// before pixel data: a binary value of 3000 bytes, which is bulk data over
// 1 KiB, and a text value of 70000 bytes, which is bulk data over 64 KiB.
// It returns the elements and the two values.
func privateBulk() (elements, binary3000, text70000 []byte) {
	binary3000 = bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 750)
	text70000 = bytes.Repeat([]byte("0123456789abcdef"), 4375)
	var b []byte
	b = append(b, explicitShort(0x7FDF, 0x0010, "LO", []byte("NOPACS TEST "))...)
	b = append(b, explicitLong(0x7FDF, 0x1001, "OB", binary3000)...)
	b = append(b, explicitLong(0x7FDF, 0x1002, "UT", text70000)...)
	return b, binary3000, text70000
}

// deflatedFile returns the file as Deflated Explicit VR Little Endian
// (PS3.5 A.5): its file meta information is rewritten to state that syntax,
// and its data set, which must be explicit VR little endian, follows
// compressed with raw DEFLATE.
func deflatedFile(t testing.TB, file []byte, last byte) []byte {
	t.Helper()
	file = reUID(t, file, last)
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	class, _ := h.UID(header.SOPClassUID)
	sop, _ := h.UID(header.SOPInstanceUID)
	// The data set starts after the meta group: the first element past
	// group 0002 (the (0008,0005) or (0008,0008) that opens a data set).
	i := 132
	for binary.LittleEndian.Uint16(file[i:]) == 2 {
		n := int(binary.LittleEndian.Uint16(file[i+6:]))
		if string(file[i+4:i+6]) == "OB" || string(file[i+4:i+6]) == "UN" {
			n = int(binary.LittleEndian.Uint32(file[i+8:])) + 4
		}
		i += 8 + n
	}
	pad := func(u string) []byte {
		if len(u)%2 == 1 {
			return append([]byte(u), 0)
		}
		return []byte(u)
	}
	var meta []byte
	meta = append(meta, explicitLong(2, 1, "OB", []byte{0, 1})...)
	meta = append(meta, explicitShort(2, 2, "UI", pad(class))...)
	meta = append(meta, explicitShort(2, 3, "UI", pad(sop))...)
	meta = append(meta, explicitShort(2, 0x10, "UI", pad("1.2.840.10008.1.2.1.99"))...)
	meta = append(meta, explicitShort(2, 0x12, "UI", pad("1.2.3.4"))...)
	out := append(make([]byte, 128), "DICM"...)
	out = append(out, explicitShort(2, 0, "UL", binary.LittleEndian.AppendUint32(nil, uint32(len(meta))))...) // #nosec G115 -- a test's small value.
	out = append(out, meta...)
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(file[i:]); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return append(out, z.Bytes()...)
}

// fakeClock is a clock a test moves by hand: its waits are real and short.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After waits a thousandth of d.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	return time.After(max(d/1000, time.Millisecond))
}

// advance moves the clock forward.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// archive is lab1 after a real reception of files.
type archive struct {
	f *placeFixture
	// store is lab1's storage, opened as the reception wrote to it.
	store blob.Store
	// p is lab1's store.
	p tenant.Pipeline
	// sent maps a SOP Instance UID to the bytes that were received.
	sent map[string][]byte
	// pipe is the reception pipeline that archived them, still running,
	// and wait the clock it runs by.
	pipe *receive.Pipeline
	wait *settle
}

// receiveArchive makes lab1 receive and archive files, each a whole DICOM
// file, through the reception pipeline over lab1's real database and its
// directory storage, and waits until every one is held.
func receiveArchive(t *testing.T, f *placeFixture, files [][]byte) *archive {
	t.Helper()
	return receiveArchiveVia(t, f, files, nil)
}

// receiveArchiveVia is receiveArchive with the store the pipeline is given
// made by wrap from lab1's, or lab1's own when wrap is nil; the archive
// returned holds lab1's own.
func receiveArchiveVia(t *testing.T, f *placeFixture, files [][]byte, wrap func(tenant.Pipeline) tenant.Pipeline) *archive {
	t.Helper()
	if _, err := f.pipelineOf("lab1").Receiver(context.Background(), "scanner1"); fault.KindOf(err) == fault.NotFound {
		// A test that set the receiver up itself (with a rule set) keeps it.
		f.receiving(t)
	}
	store, err := dir.Open(filepath.Join(f.root, "lab1", "mri"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := f.pipelineOf("lab1")
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	wait := newSettle()
	given := p
	if wrap != nil {
		given = wrap(p)
	}
	pipe, err := receive.New(receive.Config{
		Tenant: "lab1", Store: given, Hasher: hasher, Clock: wait.clock, Wake: wait.Wake, IncomingRoot: filepath.Join(f.dir, "incoming"),
		OpenStorage: func(context.Context, string) (blob.Store, error) { return store, nil },
		Lease:       10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipe.Stop(context.Background()) })
	a := &archive{f: f, store: store, p: p, sent: map[string][]byte{}, pipe: pipe, wait: wait}
	f.pipe.Store(pipe)
	for _, b := range files {
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := h.UID(header.SOPInstanceUID)
		a.sent[uid] = b
		out := pipe.Accept(context.Background(), receive.Arrival{Receiver: "scanner1", Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"}})
		if out.Status != receive.Received {
			t.Fatalf("a file was %v (%s), want received", out.Status, out.Reason)
		}
	}
	wait.await(t, pipe, p, len(files))
	return a
}

// scans returns the paths of lab1's scans, by label.
func (a *archive) scans(t *testing.T) []label.Path {
	t.Helper()
	var out []label.Path
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		got, err := a.p.ListScans(context.Background(), tenant.ScanFilter{Order: tenant.OrderLabel}, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range got.Items {
			out = append(out, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
		}
		if got.Next == "" {
			return out
		}
		page.Cursor = got.Next
	}
}

// held returns the instances a scan holds, in order.
func (a *archive) held(t *testing.T, path label.Path) []tenant.ScanInstance {
	t.Helper()
	var out []tenant.ScanInstance
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		got, next, err := a.p.ListHeldInstances(context.Background(), path, page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// object reads a whole object of the storage.
func (a *archive) object(t testing.TB, key string) []byte {
	t.Helper()
	rc, err := a.store.Read(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// sidecarKey returns the key of a scan's metadata file.
func sidecarKey(t testing.TB, path label.Path) string {
	t.Helper()
	k, err := layout.MetadataKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// scanRequest names a scan of lab1.
func scanRequest(p label.Path) *v1.GetScanMetadataRequest {
	return &v1.GetScanMetadataRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan}
}

// instanceRequest names an instance of a scan of lab1.
func instanceRequest(p label.Path, id string) *v1.GetInstanceMetadataRequest {
	return &v1.GetInstanceMetadataRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, InstanceId: id}
}

// bulkURI is what a BulkDataURI of the contract names.
type bulkURI struct {
	path           string
	offset, length int64
}

// parseURI reads a BulkDataURI of the contract: a relative reference of the
// form /tenants/<tenant>/instances/<subject>/<session>/<scan>/<id>?offset=
// &length=.
func parseURI(t testing.TB, s string, p label.Path, id string) bulkURI {
	t.Helper()
	want := "/tenants/lab1/instances/" + p.Subject + "/" + p.Session + "/" + p.Scan + "/" + id + "?offset="
	if !strings.HasPrefix(s, want) {
		t.Fatalf("BulkDataURI %q does not start with %q", s, want)
	}
	var u bulkURI
	rest := strings.TrimPrefix(s, want)
	off, length, ok := strings.Cut(rest, "&length=")
	if !ok {
		t.Fatalf("BulkDataURI %q has no length", s)
	}
	u.path = want
	for _, n := range []struct {
		text string
		dst  *int64
	}{{off, &u.offset}, {length, &u.length}} {
		var v int64
		for _, c := range n.text {
			if c < '0' || c > '9' {
				t.Fatalf("BulkDataURI %q holds a number that is not one", s)
			}
			v = v*10 + int64(c-'0')
		}
		*n.dst = v
	}
	return u
}
