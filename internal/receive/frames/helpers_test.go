package frames

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testdata reads a public corpus file of the reception pipeline's tests.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// clock is a clock that never waits, and keeps every wait asked of it.
type clock struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (c *clock) Now() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

// catalog lists the scans it holds, in the order given, two a page so a
// pass reads several pages, and fills in their values as the tenant port
// does: a value held is kept.
type catalog struct {
	mu    sync.Mutex
	scans []tenant.CatalogScan
	// fills lists each fill asked, in order.
	fills []fillAsked
}

// fillAsked is one FillScanValues asked of the catalog.
type fillAsked struct {
	series string
	values tenant.ScanValues
}

func (c *catalog) FillScanValues(_ context.Context, seriesUID string, v tenant.ScanValues) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.scans {
		sc := &c.scans[i]
		if sc.SeriesUID != seriesUID {
			continue
		}
		c.fills = append(c.fills, fillAsked{series: seriesUID, values: v})
		if sc.Values.Modality == "" {
			sc.Values.Modality = v.Modality
		}
		if sc.Values.SeriesDescription == "" {
			sc.Values.SeriesDescription = v.SeriesDescription
		}
		if !sc.Values.HasSeriesNumber {
			sc.Values.SeriesNumber, sc.Values.HasSeriesNumber = v.SeriesNumber, v.HasSeriesNumber
		}
		return nil
	}
	return fault.New(fault.NotFound, "no scan holds the series")
}

func (c *catalog) ListScans(_ context.Context, filter tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
	if filter.Order != tenant.OrderLabel {
		return tenant.CatalogPage[tenant.CatalogScan]{}, errors.New("the pass lists scans by label")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := 0
	if page.Cursor != "" {
		for i, s := range c.scans {
			if s.SubjectLabel+"/"+s.SessionLabel+"/"+s.Label == page.Cursor {
				start = i + 1
			}
		}
	}
	end := min(start+2, len(c.scans))
	out := tenant.CatalogPage[tenant.CatalogScan]{Items: c.scans[start:end]}
	if end < len(c.scans) {
		s := c.scans[end-1]
		out.Next = s.SubjectLabel + "/" + s.SessionLabel + "/" + s.Label
	}
	return out, nil
}

// feed keeps the events appended to it. Only AppendEvents is used by the
// pass; the other methods of tenant.Events are not called.
type feed struct {
	tenant.Events
	mu     sync.Mutex
	events []*tenant.FramesAdded
}

func (f *feed) AppendEvents(_ context.Context, events []tenant.EventAppend) (int, error) {
	if err := tenant.ValidateEventAppends(events); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range events {
		p, ok := tenant.DecodeEvent(e.Event.Type(), e.Event.Payload())
		fa, isFrames := p.(*tenant.FramesAdded)
		if !ok || !isFrames {
			return 0, errors.New("not a frames event")
		}
		f.events = append(f.events, fa)
	}
	return len(events), nil
}

// counted is a storage that counts the reads and puts made of it, by key,
// checks that every put of a metadata sidecar is made under the scan's
// lock, and may be made to fail.
type counted struct {
	blob.Store
	mu     sync.Mutex
	reads  map[string][]int64
	puts   map[string]int
	locked *locks
	// failRead, when set, is asked before each read and fails it when it
	// returns an error.
	failRead func(key string) error
	// failPut, when set, fails a put of a key it returns true for after
	// some of the bytes were read from the caller, as a crash midway does.
	failPut func(key string) bool
	// unlocked counts sidecar puts made without the lock.
	unlocked int
}

func (c *counted) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	c.mu.Lock()
	c.reads[key] = append(c.reads[key], length)
	f := c.failRead
	c.mu.Unlock()
	if f != nil {
		if err := f(key); err != nil {
			return nil, err
		}
	}
	return c.Store.Read(ctx, key, offset, length)
}

func (c *counted) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	c.mu.Lock()
	c.puts[key]++
	fail := c.failPut != nil && c.failPut(key)
	if strings.HasPrefix(key, layout.MetaRoot) && c.locked != nil && !c.locked.heldAny() {
		c.unlocked++
	}
	c.mu.Unlock()
	if fail {
		return c.Store.Put(ctx, key, io.MultiReader(io.LimitReader(r, 100), failing{}))
	}
	return c.Store.Put(ctx, key, r)
}

// failing is a reader that fails, as a process cut off midway.
type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("the process stopped") }

// locks is the scans' lock the pass is given, which says whether it is
// held.
type locks struct {
	mu   sync.Mutex
	held map[string]bool
}

func (l *locks) lock(storage string, path label.Path) func() {
	k := storage + "\x00" + path.Subject + "/" + path.Session + "/" + path.Scan
	l.mu.Lock()
	l.held[k] = true
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.held, k)
		l.mu.Unlock()
	}
}

func (l *locks) heldAny() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held) > 0
}

// archive is a tenant's archive on a directory storage, as the pipeline
// leaves it: instance files, and each scan's sidecar.
type archive struct {
	t     *testing.T
	ctx   context.Context
	store *counted
	cat   *catalog
	feed  *feed
	locks *locks
	clock *clock
	busy  bool
}

func newArchive(t *testing.T) *archive {
	t.Helper()
	d, err := dir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	l := &locks{held: map[string]bool{}}
	return &archive{t: t, ctx: context.Background(), cat: &catalog{}, feed: &feed{}, locks: l, clock: &clock{},
		store: &counted{Store: d, reads: map[string][]int64{}, puts: map[string]int{}, locked: l}}
}

// file is one instance of a scan: the corpus file whose bytes it holds,
// under the SOP Instance UID uid, or with missing set an instance whose
// file is not on the storage, or with changed set one whose file on the
// storage has its last byte changed, its size the same, so it is not the
// file its line's checksum names.
type file struct {
	name, uid string
	missing   bool
	changed   bool
}

// scan puts a scan's instance files and its sidecar on the storage and
// lists it in the catalog: a sidecar as a version that kept no frame table
// wrote it when old, and as this version writes it otherwise.
func (a *archive) scan(path label.Path, old bool, files ...file) {
	a.t.Helper()
	var lines []sidecar.Instance
	for _, f := range files {
		data := testdata(a.t, f.name)
		key, err := layout.InstanceKey(path, f.uid)
		if err != nil {
			a.t.Fatal(err)
		}
		if !f.missing {
			stored := data
			if f.changed {
				stored = bytes.Clone(data)
				stored[len(stored)-1] ^= 0xFF
			}
			if _, err := a.store.Store.Put(a.ctx, key, bytes.NewReader(stored)); err != nil {
				a.t.Fatal(err)
			}
		}
		h, err := header.Read(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			a.t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		in := sidecar.Instance{SOPInstanceUID: f.uid, Key: key, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
			PixelData: &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}, DatasetOmitted: true}
		if !old {
			if in.Frames, err = Table(h, bytes.NewReader(data)); err != nil {
				a.t.Fatal(err)
			}
			in.FrameSHA256 = frameSums(data, in.Frames)
		}
		lines = append(lines, in)
	}
	key, err := layout.MetadataKey(path)
	if err != nil {
		a.t.Fatal(err)
	}
	var b bytes.Buffer
	if old {
		b.WriteString(`{"version":1,"instances":[` + "\n")
		for i, in := range lines {
			line, err := json.Marshal(in)
			if err != nil {
				a.t.Fatal(err)
			}
			if i > 0 {
				b.WriteString(",\n")
			}
			b.Write(line)
		}
		b.WriteString("\n]}\n")
	} else {
		if _, err := sidecar.Merge(&b, nil, sidecar.List(lines), nil); err != nil {
			a.t.Fatal(err)
		}
	}
	if _, err := a.store.Store.Put(a.ctx, key, bytes.NewReader(b.Bytes())); err != nil {
		a.t.Fatal(err)
	}
	// Listed with a modality, as every scan archived since the catalog
	// kept one is; unvalued lists one without.
	a.cat.scans = append(a.cat.scans, tenant.CatalogScan{SubjectLabel: path.Subject, SessionLabel: path.Session, Label: path.Scan,
		Storage: "main", SeriesUID: seriesUIDOf(len(a.cat.scans)), Values: tenant.ScanValues{Modality: "OT"}})
}

// seriesUIDOf returns the Series Instance UID the catalog gives its scan i.
func seriesUIDOf(i int) string { return "2.25.77" + strconv.Itoa(i) }

// unvalued lists the scan at path as one archived before the catalog kept
// its header values: with none.
func (a *archive) unvalued(path label.Path) {
	for i, sc := range a.cat.scans {
		if sc.SubjectLabel == path.Subject && sc.SessionLabel == path.Session && sc.Label == path.Scan {
			a.cat.scans[i].Values = tenant.ScanValues{}
		}
	}
}

// config returns the pass's configuration over the archive.
func (a *archive) config() Config {
	return Config{
		Catalog: a.cat,
		Events:  a.feed,
		OpenStorage: func(_ context.Context, name string) (blob.Store, error) {
			if name != "main" {
				return nil, errors.New("no such storage")
			}
			return a.store, nil
		},
		Lock:        a.locks.lock,
		HeaderLimit: func(context.Context) int64 { return header.MaxHeader },
		Busy:        func() bool { return a.busy },
		Clock:       a.clock,
	}
}

// sidecarOf reads a scan's sidecar: its bytes, its version and its lines.
func (a *archive) sidecarOf(path label.Path) ([]byte, int, []sidecar.Instance) {
	a.t.Helper()
	key, _ := layout.MetadataKey(path)
	rc, err := a.store.Store.Read(a.ctx, key, 0, -1)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		a.t.Fatal(err)
	}
	rd := sidecar.NewReader(bytes.NewReader(b))
	var lines []sidecar.Instance
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			a.t.Fatal(err)
		}
		lines = append(lines, in)
	}
	return b, rd.Version(), lines
}

// wantTables checks that every line of the scan's sidecar has the table
// read from its corpus file itself with each frame's checksum, or an empty
// one without checksums for a file not on the storage or changed there.
func (a *archive) wantTables(path label.Path, files ...file) {
	a.t.Helper()
	_, version, lines := a.sidecarOf(path)
	if version != sidecar.VersionFrames || len(lines) != len(files) {
		a.t.Fatalf("%s: version %d with %d lines, want %d and %d", path.Scan, version, len(lines), sidecar.VersionFrames, len(files))
	}
	for i, f := range files {
		want := [][]sidecar.Range{}
		if !f.missing && !f.changed {
			data := testdata(a.t, f.name)
			h, err := header.Read(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				a.t.Fatal(err)
			}
			if want, err = Table(h, bytes.NewReader(data)); err != nil {
				a.t.Fatal(err)
			}
		}
		got, _ := json.Marshal(lines[i].Frames)
		wantText, _ := json.Marshal(want)
		if string(got) != string(wantText) || lines[i].Frames == nil {
			a.t.Errorf("%s %s: table %s, want %s", path.Scan, f.name, got, wantText)
		}
		var wantSums []string
		if !f.missing && !f.changed {
			wantSums = frameSums(testdata(a.t, f.name), want)
		}
		if !slices.Equal(lines[i].FrameSHA256, wantSums) {
			a.t.Errorf("%s %s: frame checksums %v, want %v", path.Scan, f.name, lines[i].FrameSHA256, wantSums)
		}
	}
}

// frameSums returns the SHA-256 of each frame of table, its ranges of data
// joined, worked out here without Checksums: nil for an empty table.
func frameSums(data []byte, table [][]sidecar.Range) []string {
	var out []string
	for _, f := range table {
		h := sha256.New()
		for _, r := range f {
			h.Write(data[r.Offset : r.Offset+r.Length])
		}
		out = append(out, hex.EncodeToString(h.Sum(nil)))
	}
	return out
}
