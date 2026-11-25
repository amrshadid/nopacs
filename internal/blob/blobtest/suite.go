package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Driver is what a driver gives the suite.
type Driver struct {
	// Open returns a fresh, empty store that no other test uses, and closes
	// it when tb ends (tb.Cleanup). Closing a store twice must be harmless,
	// since a test may close it itself.
	Open func(tb testing.TB) blob.Store
	// UsageKnown says whether the driver can report the space of its volume.
	// The suite holds it to what it says: a driver that says true must
	// report numbers that add up, and one that says false must report
	// Known false rather than numbers it made up.
	UsageKnown bool
	// KeyAndPrefix says whether one key can hold an object while other keys
	// lie under it ("a" and "a/b"), as a bucket allows. A storage on a file
	// system cannot, since one name cannot be a file and a directory at
	// once; it must then refuse the second Put as Conflict and keep the
	// first object. Such a storage may also refuse a key under which keys
	// once lay, after they were deleted, since the directory they were in
	// can remain. The archive's layout never makes such a pair.
	KeyAndPrefix bool
	// Versioned says the store keeps older versions of an object (a
	// versioned bucket), so a purge may answer several removals, or hidden.
	Versioned bool
	// KeepsSHA256 says Stat gives the storage's own SHA-256 of an object put
	// in one request (Info.HasSHA256). The suite holds it to what it says,
	// and the checksum to the bytes.
	KeepsSHA256 bool
	// ConfirmsPuts says the storage itself answers for every byte of a small
	// object it was put, one request whose SHA-256 it checked
	// (Written.Confirmed); the suite holds it to what it says for such an
	// object. Larger objects may be confirmed part by part or not at all.
	ConfirmsPuts bool
}

// Run runs the whole contract suite against the stores d opens. Each test
// has its own store and runs in parallel with the others.
func Run(t *testing.T, d Driver) {
	tests := []struct {
		name string
		fn   func(*env)
	}{
		{"PutRead", testPutRead},
		{"Empty", testEmpty},
		{"Ranges", testRanges},
		{"Replace", testReplace},
		{"ConcurrentPuts", testConcurrentPuts},
		{"FailedPut", testFailedPut},
		{"CanceledPut", testCanceledPut},
		{"RefusedKeys", testRefusedKeys},
		{"PrefixIsNotAnObject", testPrefixIsNotAnObject},
		{"KeyAndPrefix", testKeyAndPrefix},
		{"Delete", testDelete},
		{"Purge", testPurge},
		{"List", testList},
		{"ListDelimiter", testListDelimiter},
		{"ListPartialPrefix", testListPartialPrefix},
		{"ListDefaultLimit", testListDefaultLimit},
		{"ListStable", testListStable},
		{"ListRefused", testListRefused},
		{"Usage", testUsage},
		{"Closed", testClosed},
		{"NextOnlyWhenMore", testNextOnlyWhenMore},
		{"ReadSeesOneVersion", testReadSeesOneVersion},
		{"PutDeleteRace", testPutDeleteRace},
		{"SiblingsRace", testSiblingsRace},
		{"NewParentsAtOnce", testNewParentsAtOnce},
		{"StatState", testStatState},
		{"Recall", testRecall},
		{"PutConfirmed", testPutConfirmed},
		{"Encryption", testEncryption},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(&env{T: t, s: d.Open(t), d: d, ctx: context.Background()})
		})
	}
}

// BadKeys returns keys that every store must refuse as Invalid, each
// breaking one rule of blob.CheckKey: the forms a key takes when it tries to
// leave a storage's root, names nothing a disk can hold, or would mean two
// different things on two storages or two file systems.
func BadKeys() []string {
	return []string{
		"",
		"/a", "a/", "a//b", "/",
		".", "..", "../a", "a/..", "a/../b", "a/./b", "./a",
		`a\b`, `..\a`, "a:b", "c:/x", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b",
		"a\x00b", "a\nb", "a\tb", "\x1b[31m", "a\x7fb", "a\u0085b",
		"\xff", "a/\xc3",
		"a.", "a/b./c", "a ", "a/b /c", " a", "a b",
		"con", "CON", "a/nul.txt", "Aux", "prn.dcm", "com1", "LPT9.x", "com\u00b9", "con .txt",
		".blob-tmp-0123", "a/.BLOB-x", ".blob-", ".blob-tmp/obj",
		// Names a file system reads as another name: composed and
		// decomposed forms (one file on APFS), an invisible code point
		// before the reserved prefix (dropped by HFS+, so the key would
		// land in a driver's temporary directory), a Windows short name.
		"caf\u00e9", "cafe\u0301", "\u200c.blob-tmp/obj", "a\u200db", "\ufeffa", "ABCDEF~1", "PROGRA~1/x",
		"a+b", "a@b", "a#b", "a%2Fb", "a$", "a=b", "a,b", "a;b", "a'b", "a(b)", "a[b]", "a{b}", "a!b", "a&b", "a^b", "a`b",
		strings.Repeat("a", blob.MaxSegmentLength+1),
		strings.Repeat("a/", blob.MaxKeyLength/2) + "a",
		strings.Repeat("a/", blob.MaxKeySegments) + "a",
	}
}

// env is one test's store and the helpers that use it.
type env struct {
	*testing.T
	s   blob.Store
	d   Driver
	ctx context.Context
}

// put writes data to key and checks what Put says it wrote.
func (e *env) put(key string, data []byte) blob.Written {
	e.Helper()
	w, err := e.s.Put(e.ctx, key, bytes.NewReader(data))
	if err != nil {
		e.Fatalf("Put(%q, %d bytes): %v", key, len(data), err)
	}
	if w.Size != int64(len(data)) || w.SHA256 != sha256.Sum256(data) {
		e.Fatalf("Put(%q) says it wrote %d bytes, %x; want %d bytes, %x", key, w.Size, w.SHA256, len(data), sha256.Sum256(data))
	}
	return w
}

// read returns the bytes of a range of key.
func (e *env) read(key string, offset, length int64) []byte {
	e.Helper()
	r, err := e.s.Read(e.ctx, key, offset, length)
	if err != nil {
		e.Fatalf("Read(%q, %d, %d): %v", key, offset, length, err)
	}
	defer closeOrFail(e.T, r)
	b, err := io.ReadAll(r)
	if err != nil {
		e.Fatalf("reading %q: %v", key, err)
	}
	return b
}

// wantObject checks that key holds exactly data, by Read and by Stat.
func (e *env) wantObject(key string, data []byte) {
	e.Helper()
	if got := e.read(key, 0, -1); !bytes.Equal(got, data) {
		e.Fatalf("%q holds %d bytes (%x), want %d bytes (%x)", key, len(got), sha256.Sum256(got), len(data), sha256.Sum256(data))
	}
	info, err := e.s.Stat(e.ctx, key)
	if err != nil || info.Size != int64(len(data)) {
		e.Fatalf("Stat(%q) = %+v, %v; want size %d", key, info, err, len(data))
	}
}

// wantAbsent checks that key holds nothing, by Stat, Read and List.
func (e *env) wantAbsent(key string) {
	e.Helper()
	if info, err := e.s.Stat(e.ctx, key); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("Stat(%q) = %+v, %v; want NotFound", key, info, err)
	}
	if r, err := e.s.Read(e.ctx, key, 0, -1); fault.KindOf(err) != fault.NotFound {
		if err == nil {
			closeOrFail(e.T, r)
		}
		e.Fatalf("Read(%q) = %v; want NotFound", key, err)
	}
	if keys, _ := e.listAll(blob.ListOptions{Prefix: key}); slices.Contains(keys, key) {
		e.Fatalf("List shows %q, which should hold nothing", key)
	}
}

// listAll pages through a listing with opts and returns every key and
// prefix, checking along the way that each page is within its limit, in
// order, continues after the page before, and ends with an empty Next.
func (e *env) listAll(opts blob.ListOptions) (keys, prefixes []string) {
	e.Helper()
	limit := blob.ListLimit(opts)
	last := ""
	for pages := 0; ; pages++ {
		if pages > 100000 {
			e.Fatalf("List(%+v) never ends", opts)
		}
		p, err := e.s.List(e.ctx, opts)
		if err != nil {
			e.Fatalf("List(%+v): %v", opts, err)
		}
		entries := merge(p.Keys, p.Prefixes)
		if len(entries) > limit {
			e.Fatalf("List(%+v) returned %d entries, over its limit", opts, len(entries))
		}
		if len(entries) == 0 && p.Next != "" {
			e.Fatalf("List(%+v) returned an empty page that is not the last", opts)
		}
		if len(entries) == 0 && pages > 0 {
			e.Fatalf("List(%+v) returned an empty page after a page whose Next promised more", opts)
		}
		for _, x := range entries {
			if x <= last {
				e.Fatalf("List(%+v): %q comes after %q, out of order or repeated", opts, x, last)
			}
			if !strings.HasPrefix(x, opts.Prefix) {
				e.Fatalf("List(%+v) returned %q, outside the prefix", opts, x)
			}
			last = x
		}
		if opts.Delimiter == "" && len(p.Prefixes) != 0 {
			e.Fatalf("List(%+v) returned prefixes without a delimiter: %q", opts, p.Prefixes)
		}
		keys = append(keys, p.Keys...)
		prefixes = append(prefixes, p.Prefixes...)
		if p.Next == "" {
			return keys, prefixes
		}
		opts.Token = p.Next
	}
}

// merge returns keys and prefixes together, sorted, as a page orders them.
func merge(keys, prefixes []string) []string {
	out := append(slices.Clone(keys), prefixes...)
	slices.Sort(out)
	return out
}

func closeOrFail(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// testUsage guards that Usage is what the driver declared: numbers that add
// up when it says it can know them, and Known false with zeros when it says
// it cannot.
func testUsage(e *env) {
	u, err := e.s.Usage(e.ctx)
	if err != nil {
		e.Fatal(err)
	}
	if u.Known != e.d.UsageKnown {
		e.Fatalf("Usage().Known = %v, but the driver declared %v", u.Known, e.d.UsageKnown)
	}
	if !u.Known {
		if u != (blob.Usage{}) {
			e.Fatalf("Usage says it cannot know but reports %+v", u)
		}
		return
	}
	if u.Total <= 0 || u.Free < 0 || u.Used < 0 || u.Free > u.Total || u.Used > u.Total {
		e.Fatalf("Usage = %+v, which does not add up", u)
	}
}

// testClosed guards that a closed store refuses every call as Unavailable
// and that closing it again is harmless.
func testClosed(e *env) {
	s := e.d.Open(e.T)
	if _, err := s.Put(e.ctx, "c", bytes.NewReader([]byte("c"))); err != nil {
		e.Fatal(err)
	}
	if err := s.Close(); err != nil {
		e.Fatal(err)
	}
	if err := s.Close(); err != nil {
		e.Fatalf("second Close: %v", err)
	}
	want := func(op string, err error) {
		e.Helper()
		if fault.KindOf(err) != fault.Unavailable {
			e.Errorf("%s after Close = %v, want Unavailable", op, err)
		}
	}
	_, err := s.Put(e.ctx, "c", bytes.NewReader(nil))
	want("Put", err)
	_, err = s.Read(e.ctx, "c", 0, -1)
	want("Read", err)
	_, err = s.Stat(e.ctx, "c")
	want("Stat", err)
	_, err = s.List(e.ctx, blob.ListOptions{})
	want("List", err)
	want("Delete", s.Delete(e.ctx, "c"))
	_, err = s.Usage(e.ctx)
	want("Usage", err)
}
