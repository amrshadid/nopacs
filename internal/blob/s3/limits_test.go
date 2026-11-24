package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// peakHeap runs fn and returns the most the heap in use grew above what it
// was before, sampled while fn ran.
func peakHeap(fn func()) uint64 {
	// A collector that waits for the heap to double would count garbage as
	// use; this one keeps the sample close to what is live.
	defer debug.SetGCPercent(debug.SetGCPercent(20))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	fn()
	close(done)
	wg.Wait()
	return max(peak.Load(), base.HeapAlloc) - base.HeapAlloc
}

// writeHuge answers with an XML document whose one element holds size
// bytes, written a megabyte at a time so that the server holds none of it.
func writeHuge(w http.ResponseWriter, status int, open, close string, size int) {
	if status != 0 {
		w.WriteHeader(status)
	}
	_, _ = io.WriteString(w, open)
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for range size >> 20 {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
	_, _ = io.WriteString(w, close)
}

// TestAnswersAreBounded guards the size of every answer that is not an
// object's bytes: a listing whose key is 256 MiB, an error whose message is
// 256 MiB, and an upload's answer of that size, are each refused as a malformed
// answer, with the heap growing by a few megabytes and not by the answer, and
// each made once (an answer too large is not retried). A listing page of
// 1,000 keys of the longest length still passes.
func TestAnswersAreBounded(t *testing.T) {
	const huge = 256 << 20
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	ctx := context.Background()
	var mode atomic.Value
	mode.Store("")
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		switch mode.Load().(string) {
		case "listing":
			if r.Method == http.MethodGet {
				writeHuge(w, 0, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>`, `</Key></Contents></ListBucketResult>`, huge)
				return true
			}
		case "error":
			if r.Method == http.MethodGet {
				writeHuge(w, 500, `<Error><Code>InternalError</Code><Message>`, `</Message></Error>`, huge)
				return true
			}
		case "put":
			if r.Method == http.MethodPut {
				writeHuge(w, 0, `<Error><Message>`, `</Message></Error>`, huge)
				return true
			}
		case "headers":
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Length", fmt.Sprint(huge))
				w.WriteHeader(500)
				return true
			}
		}
		return false
	}
	for _, m := range []string{"listing", "error", "put", "headers"} {
		mode.Store(m)
		before := f.hits.Load()
		var err error
		grew := peakHeap(func() {
			if m == "put" {
				_, err = s.Put(ctx, "k", strings.NewReader("x"))
			} else {
				_, err = s.List(ctx, blob.ListOptions{})
			}
		})
		if fault.KindOf(err) != fault.Unavailable {
			t.Errorf("%s: %v, want Unavailable", m, err)
		}
		noLeak(t, m, err)
		if grew > 48<<20 {
			t.Errorf("%s: the heap grew by %d MiB for an answer of %d MiB", m, grew>>20, huge>>20)
		}
		if n := f.hits.Load() - before; n != 1 {
			t.Errorf("%s: %d requests, want 1: an answer that is too large is not retried", m, n)
		}
		t.Logf("%s: heap grew by %d MiB", m, grew>>20)
	}
	// The largest honest answer: 1,000 keys of the port's longest length, under
	// the longest prefix, so each is 1,024 bytes in the bucket.
	mode.Store("")
	pfx := longestPrefix()
	big := openFake(t, f, func(c *Config) { c.Prefix = pfx })
	tail := keyOfLength(blob.MaxKeyLength-4, 'k')
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			return false
		}
		var b strings.Builder
		b.WriteString(`<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for i := range 1000 {
			fmt.Fprintf(&b, `<Contents><Key>%s%s%04d</Key><LastModified>2026-01-01T00:00:00.000Z</LastModified><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag><Size>12345678</Size><StorageClass>STANDARD</StorageClass><Owner><ID>0123456789abcdef</ID><DisplayName>someone</DisplayName></Owner></Contents>`, pfx, tail, i)
		}
		b.WriteString(`</ListBucketResult>`)
		if b.Len() > maxAnswer {
			t.Errorf("the largest honest page is %d bytes, over the %d-byte bound", b.Len(), maxAnswer)
		}
		_, _ = io.WriteString(w, b.String())
		return true
	}
	p, err := big.List(ctx, blob.ListOptions{Limit: 1000})
	if err != nil || len(p.Keys) != 1000 || len(p.Keys[0]) != blob.MaxKeyLength {
		t.Errorf("a page of 1,000 longest keys = %d keys, %v", len(p.Keys), err)
	}
}

// ranged answers a ranged GET with the headers and body a test gives.
func ranged(f *fakeS3, status int, header map[string]string, body []byte, hang bool) {
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet || r.URL.Query().Get("x-id") != "GetObject" {
			return false
		}
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
		if hang {
			w.(http.Flusher).Flush()
		}
		return true
	}
}

// TestReadGivesExactlyTheBytesAsked guards that a Read returns exactly the
// bytes asked for, min(length, size-offset) of them, or an error: a server
// that answers a range with another start, another end, more bytes or fewer
// than it said, a length that disagrees with the range, no length, or a
// range where none was asked, never gives the caller a short or a long
// success.
func TestReadGivesExactlyTheBytesAsked(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	ctx := context.Background()
	obj := blobtest.PatternBytes(1, 100)
	type answer struct {
		name   string
		off    int64
		length int64
		status int
		header map[string]string
		body   []byte
		want   []byte // nil when the Read must fail
	}
	h := func(kv ...string) map[string]string {
		m := map[string]string{}
		for pair := range slices.Chunk(kv, 2) {
			m[pair[0]] = pair[1]
		}
		return m
	}
	for _, a := range []answer{
		{"exact", 10, 5, 206, h("Content-Range", "bytes 10-14/100"), obj[10:15], obj[10:15]},
		{"cut at the end", 90, 50, 206, h("Content-Range", "bytes 90-99/100"), obj[90:], obj[90:]},
		{"to the end", 95, -1, 206, h("Content-Range", "bytes 95-99/100"), obj[95:], obj[95:]},
		{"whole", 0, -1, 200, h(), obj, obj},
		{"more than asked", 10, 5, 206, h("Content-Range", "bytes 10-99/100"), obj[10:], nil},
		{"fewer than asked", 10, 5, 206, h("Content-Range", "bytes 10-11/100"), obj[10:12], nil},
		{"another start", 10, 5, 206, h("Content-Range", "bytes 11-15/100"), obj[11:16], nil},
		{"end past the size", 10, 5, 206, h("Content-Range", "bytes 10-14/12"), obj[10:15], nil},
		{"no end", 10, 5, 206, h("Content-Range", "bytes 10-/100"), obj[10:15], nil},
		{"length disagrees", 10, 5, 206, h("Content-Range", "bytes 10-14/100", "Content-Length", "4"), obj[10:14], nil},
		{"body longer than the range, unframed", 10, 5, 206, h("Content-Range", "bytes 10-14/100"), append(bytes.Clone(obj[10:15]), obj[:20]...), nil},
		{"range where none was asked", 0, -1, 200, h("Content-Range", "bytes 0-99/100"), obj, nil},
		{"200 for a range", 10, 5, 200, h(), obj, nil},
	} {
		t.Run(a.name, func(t *testing.T) {
			if a.status == 206 || a.status == 200 {
				ranged(f, a.status, a.header, a.body, strings.Contains(a.name, "unframed"))
			}
			r, err := s.Read(ctx, "k", a.off, a.length)
			var got []byte
			if err == nil {
				got, err = io.ReadAll(r)
				_ = r.Close()
			}
			if a.want != nil {
				if err != nil || !bytes.Equal(got, a.want) {
					t.Fatalf("Read(%d, %d) = %d bytes, %v; want %d bytes", a.off, a.length, len(got), err, len(a.want))
				}
				return
			}
			if err == nil {
				t.Fatalf("Read(%d, %d) gave %d bytes and no error", a.off, a.length, len(got))
			}
			if int64(len(got)) > max(a.length, 0) && a.length >= 0 {
				t.Errorf("Read(%d, %d) delivered %d bytes before failing, more than asked", a.off, a.length, len(got))
			}
			noLeak(t, a.name, err)
		})
	}
	// A body that stops short of what its headers say, and a whole object of
	// no stated length, are errors too.
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet {
			return false
		}
		w.Header().Set("Content-Range", "bytes 10-14/100")
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(206)
		_, _ = w.Write(obj[10:13])
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
		return true
	}
	if r, err := s.Read(ctx, "k", 10, 5); err == nil {
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err == nil {
			t.Errorf("a body cut after 3 of 5 bytes read as %d bytes with no error", len(b))
		}
	}
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet {
			return false
		}
		w.WriteHeader(200)
		_, _ = w.Write(obj[:10])
		w.(http.Flusher).Flush() // Chunked: no length.
		_, _ = w.Write(obj[10:])
		return true
	}
	if r, err := s.Read(ctx, "k", 0, -1); err == nil {
		_ = r.Close()
		t.Error("a whole object of no stated length was accepted")
	}
}

// TestReadWaitsBoundedForTheFirstByte guards that a read whose server never
// answers fails at the call's deadline like every other call, not after the
// header timeout times the attempts, and that it was one bounded wait.
func TestReadWaitsBoundedForTheFirstByte(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	s.set.callTimeout = 300 * time.Millisecond
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodGet {
			<-stall
			return true
		}
		return false
	}
	start := time.Now()
	_, err := s.Read(ctx, "k", 0, 5)
	if fault.KindOf(err) != fault.Unavailable || time.Since(start) > 20*time.Second {
		t.Errorf("Read against a server that never answers = %v after %v, want Unavailable after about 300ms, and never the minutes of the header timeout", err, time.Since(start))
	}
	noLeak(t, "hung read", err)
	// The caller's deadline still comes first, and is what the error matches.
	s.set.callTimeout = time.Minute
	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := s.Read(dctx, "k", 0, 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Read with the caller's deadline = %v, want its DeadlineExceeded", err)
	}
}

// budgetFull fails the test if the store's budget is not whole: a Put that
// ended, in any way, gave back every byte it took.
func budgetFull(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.budget.mu.Lock()
		avail, waiting := s.budget.avail, len(s.budget.waiting)
		s.budget.mu.Unlock()
		if avail == s.set.memory && waiting == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the budget holds %d of %d bytes, %d waiting, after every Put ended", avail, s.set.memory, waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// TestPutsShareABoundedMemory guards that the memory of part buffers is
// bounded for the whole store and not per Put: 16 and then 64 Puts at once,
// each of four parts, against a slow server, hold about the store's limit
// and not 16 or 64 Puts' worth; a Put alone still sends its parts in
// parallel; Puts that wait for memory end with their context, never deadlock
// even with room for one part only, and give every byte back however they end.
func TestPutsShareABoundedMemory(t *testing.T) {
	const part = MinPartSize
	f := newFakeS3(t)
	f.discard.Store(true)
	f.composite.Store(1) // The fake keeps no part, so it confirms by the composite checksum.
	f.partDelay.Store(int64(40 * time.Millisecond))
	limit := int64(8 * (part + 1))
	s := openFake(t, f, func(c *Config) { c.PartSize = part; c.Concurrency = 4; c.MemoryLimit = limit })
	ctx := context.Background()
	for _, n := range []int{16, 64} {
		var wg sync.WaitGroup
		errs := make(chan error, n)
		grew := peakHeap(func() {
			for i := range n {
				wg.Go(func() {
					_, err := s.Put(ctx, fmt.Sprintf("k%d", i), io.LimitReader(zeroes{}, part+1))
					errs <- err
				})
			}
			wg.Wait()
		})
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("%d Puts: %v", n, err)
			}
		}
		t.Logf("%d concurrent Puts of 10 MiB: the heap grew by %d MiB (limit %d MiB)", n, grew>>20, limit>>20)
		if grew > uint64(limit)+48<<20 {
			t.Errorf("%d concurrent Puts grew the heap by %d MiB, with a limit of %d MiB", n, grew>>20, limit>>20)
		}
		budgetFull(t, s)
	}

	// Alone, a Put keeps the parallelism it is configured for.
	// The fake holds the first four parts until all four are in flight, so
	// the count does not depend on how fast anything runs.
	f.partDelay.Store(0)
	f.holdParts(4)
	if _, err := s.Put(ctx, "alone", io.LimitReader(zeroes{}, 6*part)); err != nil {
		t.Fatal(err)
	}
	if f.gateTimedOut.Load() {
		t.Error("a Put alone never had 4 parts in flight together, its Concurrency")
	}
	if got := f.maxInflight.Load(); got != 4 {
		t.Errorf("a Put alone had %d parts in flight at most, want exactly its Concurrency of 4", got)
	}
	budgetFull(t, s)

	// Room for one part only: every Put still ends.
	one := openFake(t, f, func(c *Config) { c.PartSize = part; c.Concurrency = 4; c.MemoryLimit = part + 1 })
	f.partDelay.Store(int64(5 * time.Millisecond))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := one.Put(ctx, fmt.Sprintf("one%d", i), io.LimitReader(zeroes{}, 3*part+7))
			errs <- err
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Puts with room for one part at a time deadlocked")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a Put with room for one part: %v", err)
		}
	}
	budgetFull(t, one)

	// A Put that waits for memory ends with its context, and holds nothing.
	hold := make(chan struct{})
	started := make(chan struct{})
	var holding sync.WaitGroup
	holding.Go(func() {
		_, _ = one.Put(ctx, "holder", &stallReader{started: started, hold: hold})
	})
	<-started
	wctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err := one.Put(wctx, "waiter", strings.NewReader("x"))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) > 20*time.Second {
		t.Errorf("a Put waiting for memory = %v after %v, want its context's deadline", err, time.Since(begin))
	}
	close(hold)
	holding.Wait()
	budgetFull(t, one)
	// And the ways a Put can fail give the memory back.
	boom := errors.New("broken")
	if _, err := one.Put(ctx, "bad", io.MultiReader(io.LimitReader(zeroes{}, 2*part), errReader{boom})); !errors.Is(err, boom) {
		t.Errorf("failing Put = %v", err)
	}
	cctx, cancelNow := context.WithCancel(ctx)
	if _, err := one.Put(cctx, "gone", io.MultiReader(io.LimitReader(zeroes{}, part), cancelReader{cancelNow}, io.LimitReader(zeroes{}, 2*part))); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Put = %v", err)
	}
	budgetFull(t, one)
}

// stallReader tells a test it was reached and gives nothing until released.
type stallReader struct {
	started chan struct{}
	hold    chan struct{}
	once    sync.Once
}

func (s *stallReader) Read([]byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.hold
	return 0, io.EOF
}

// TestListingMalformedAnswersFail guards that an answer that names something
// other than what was asked is an error and never a shorter listing: a name
// outside the prefix (on a truncated page or not), names out of order or
// repeated, a truncated answer that names nothing or that does not move the
// listing forward.
func TestListingMalformedAnswersFail(t *testing.T) {
	for name, c := range map[string]struct {
		prefix string
		answer func(start string) string
	}{
		"last name outside the prefix": {"p", func(start string) string {
			if start == "" {
				return `<ListBucketResult><IsTruncated>true</IsTruncated><Contents><Key>p/a</Key></Contents><Contents><Key>q-other</Key></Contents></ListBucketResult>`
			}
			return `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>p/b</Key></Contents></ListBucketResult>`
		}},
		"name outside the prefix": {"p", func(string) string { return `<ListBucketResult><Contents><Key>q/x</Key></Contents></ListBucketResult>` }},
		"prefix outside": {"p", func(string) string {
			return `<ListBucketResult><CommonPrefixes><Prefix>q/</Prefix></CommonPrefixes></ListBucketResult>`
		}},
		"out of order": {"p", func(string) string {
			return `<ListBucketResult><Contents><Key>p/b</Key></Contents><Contents><Key>p/a</Key></Contents></ListBucketResult>`
		}},
		"repeated": {"p", func(string) string {
			return `<ListBucketResult><Contents><Key>p/a</Key></Contents><Contents><Key>p/a</Key></Contents></ListBucketResult>`
		}},
		"truncated, names nothing": {"p", func(string) string { return `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>` }},
		"does not advance": {"p", func(start string) string {
			return `<ListBucketResult><IsTruncated>true</IsTruncated><Contents><Key>p/a</Key></Contents></ListBucketResult>`
		}},
		"not after the start": {"p", func(start string) string {
			if start == "" {
				return `<ListBucketResult><IsTruncated>true</IsTruncated><Contents><Key>p/m</Key></Contents></ListBucketResult>`
			}
			return `<ListBucketResult><Contents><Key>p/a</Key></Contents></ListBucketResult>`
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openFake(t, f, func(cfg *Config) { cfg.Prefix = "" })
			f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
					return false
				}
				_, _ = io.WriteString(w, c.answer(r.URL.Query().Get("start-after")))
				return true
			}
			// The names above are as the server sees them: under "p/" the
			// storage's prefix is "p".
			s.set.prefix = "p/"
			p, err := s.List(context.Background(), blob.ListOptions{Limit: 10})
			if err == nil {
				t.Fatalf("List = %+v with no error", p)
			}
			if fault.KindOf(err) != fault.Unavailable {
				t.Errorf("kind %v, want Unavailable", fault.KindOf(err))
			}
			noLeak(t, name, err)
		})
	}
}

// TestListingFoldsForeignNamesCheaply guards, on a real server, that objects
// under the prefix that the port could not have written, including one that
// sorts after every name a key can have in a folded group, neither show nor
// make a page cost many requests, nor end the listing early.
func TestListingFoldsForeignNamesCheaply(t *testing.T) {
	cfg := serverConfig(t)
	p := newProxy(t, cfg)
	s := openThrough(t, p, cfg, nil)
	ctx := context.Background()
	for _, k := range []string{"d/a", "d/x/b", "d/y", "e/z"} {
		if _, err := s.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	foreign := []string{"d/a b", "d/x/~a", "d/x/é", "d/x/\U0010fffd", "d/zz~"}
	for _, k := range foreign {
		rawPut(t, s, s.set.prefix+k)
	}
	t.Cleanup(func() {
		for _, k := range foreign {
			_, _ = s.client.DeleteObject(ctx, deleteInput(s, s.set.prefix+k))
		}
	})
	p.reset()
	opts := blob.ListOptions{Prefix: "d/", Delimiter: "/", Limit: 2}
	var all []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the listing does not end")
		}
		pg, err := s.List(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, pg.Keys...)
		all = append(all, pg.Prefixes...)
		if pg.Next == "" {
			break
		}
		opts.Token = pg.Next
	}
	if want := "d/a d/x/ d/y"; strings.Join(sortedCopy(all), " ") != want {
		t.Errorf("listed %v, want %s", all, want)
	}
	if n := p.count("GET", "list-type"); n > 14 {
		t.Errorf("the listing took %d requests, want few (3 entries, pages of 2)", n)
	}
}

// TestEveryPortKeyFitsUnderEveryPrefix guards the relation between the two
// limits: the port's longest key (blob.MaxKeyLength) under the longest prefix
// (MaxPrefixLength) is exactly the 1,024 bytes S3 accepts, so every key the
// port accepts is stored under every prefix, and a prefix longer than
// MaxPrefixLength is refused at Open.
func TestEveryPortKeyFitsUnderEveryPrefix(t *testing.T) {
	if blob.MaxKeyLength+MaxPrefixLength > MaxKeyBytes {
		t.Fatalf("the port's longest key (%d) and the longest prefix (%d) pass S3's %d bytes", blob.MaxKeyLength, MaxPrefixLength, MaxKeyBytes)
	}
	f := newFakeS3(t)
	pfx := longestPrefix()
	s := openFake(t, f, func(c *Config) { c.Prefix = pfx })
	ctx := context.Background()
	key := keyOfLength(blob.MaxKeyLength, 'a')
	if len(key) != blob.MaxKeyLength || blob.CheckKey(key) != nil || len(pfx)+len(key) != MaxKeyBytes {
		t.Fatalf("the test's key is %d bytes, %d with its prefix: %v", len(key), len(pfx)+len(key), blob.CheckKey(key))
	}
	if _, err := s.Put(ctx, key, strings.NewReader("x")); err != nil {
		t.Fatalf("Put of the longest key under the longest prefix: %v", err)
	}
	cfg := f.config()
	cfg.Prefix = strings.Repeat("a", MaxPrefixLength)
	if _, err := Open(ctx, cfg); fault.KindOf(err) != fault.Invalid {
		t.Errorf("Open with a prefix of %d bytes = %v, want Invalid", len(cfg.Prefix)+1, err)
	}
	cfg.Prefix = strings.Repeat("a", MaxPrefixLength-1)
	if _, err := Open(ctx, cfg); err != nil {
		t.Errorf("Open with a prefix of %d bytes: %v", MaxPrefixLength, err)
	}
}

// TestNameOverTheBucketLimitIsRefused guards the driver's own refusal of a
// name that would pass 1,024 bytes in the bucket. The port's key limit
// normally prevents one, so the test gives the store a prefix longer than
// Open allows, through the unexported settings: the Put is Invalid with no
// request, Stat and Read say NotFound, Delete succeeds, since such a name
// cannot be there.
func TestNameOverTheBucketLimitIsRefused(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	s.set.prefix = strings.Repeat("p", MaxPrefixLength+50) + "/"
	ctx := context.Background()
	key := keyOfLength(blob.MaxKeyLength, 'a')
	if s.fits(key) || !s.fits(keyOfLength(MaxKeyBytes-len(s.set.prefix), 'a')) {
		t.Fatal("fits is not the 1,024-byte rule")
	}
	before := f.hits.Load()
	if _, err := s.Put(ctx, key, strings.NewReader("x")); fault.KindOf(err) != fault.Invalid {
		t.Errorf("Put = %v, want Invalid", err)
	}
	if _, err := s.Stat(ctx, key); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Stat = %v, want NotFound", err)
	}
	if _, err := s.Read(ctx, key, 0, -1); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Read = %v, want NotFound", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Errorf("Delete = %v, want nil", err)
	}
	if n := f.hits.Load() - before; n != 0 {
		t.Errorf("%d requests were made for a name that cannot be stored", n)
	}
}

// TestExpressBucketRefused guards that a bucket of the S3 Express form is
// refused at Open before any request, and that the client could not open a
// session for one in any case: with the check bypassed, a request to such a
// bucket makes no session request.
func TestExpressBucketRefused(t *testing.T) {
	f := newFakeS3(t)
	cfg := f.config()
	cfg.Bucket = "zz--use1-az4--x-s3"
	if _, err := Open(context.Background(), cfg); fault.KindOf(err) != fault.Invalid || f.hits.Load() != 0 {
		t.Errorf("Open of an Express bucket = %v after %d requests, want Invalid and none", err, f.hits.Load())
	}
	set, err := f.config().check()
	if err != nil {
		t.Fatal(err)
	}
	set.bucket = "zz--use1-az4--x-s3"
	s := newStore(set)
	t.Cleanup(func() { _ = s.Close() })
	_, _ = s.Stat(context.Background(), "k")
	_, _ = s.Put(context.Background(), "k", strings.NewReader("x"))
	for _, r := range f.requests() {
		if r.query.Has("session") || r.header.Get("X-Amz-S3session-Token") != "" {
			t.Errorf("a session request or token: %s %s %v", r.method, r.path, r.query)
		}
	}
}

// listFromKeys answers a ListObjectsV2 request as S3 does, from a fixed set
// of keys: the prefix, the delimiter, start-after and max-keys, with a
// folded group shown whenever some key in it comes after start-after.
func listFromKeys(keys []string, q url.Values) string {
	prefix, delim, after := q.Get("prefix"), q.Get("delimiter"), q.Get("start-after")
	max, _ := strconv.Atoi(q.Get("max-keys"))
	if max <= 0 || max > 1000 {
		max = 1000
	}
	type entry struct {
		name   string
		prefix bool
	}
	var all []entry
	seen := map[string]bool{}
	for _, k := range slices.Sorted(slices.Values(keys)) {
		if !strings.HasPrefix(k, prefix) || k <= after {
			continue
		}
		if delim != "" {
			if i := strings.Index(k[len(prefix):], delim); i >= 0 {
				p := k[:len(prefix)+i+len(delim)]
				if !seen[p] {
					seen[p] = true
					all = append(all, entry{p, true})
				}
				continue
			}
		}
		all = append(all, entry{k, false})
	}
	trunc := len(all) > max
	if trunc {
		all = all[:max]
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<ListBucketResult><IsTruncated>%v</IsTruncated>`, trunc)
	for _, e := range all {
		if e.prefix {
			fmt.Fprintf(&b, `<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, e.name)
		} else {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key></Contents>`, e.name)
		}
	}
	b.WriteString(`</ListBucketResult>`)
	return b.String()
}

// TestListingSkipsAFoldedGroupWithForeignNames guards, against a server that
// lists as S3 does, that a group of keys folded by the delimiter is skipped
// whole after it is shown, though it also holds names that sort after any
// name the port allows ("d/x/~a"): the next page asks for what follows the
// group, and does not meet the group again, which would end the listing in an
// error or take a request for every character.
func TestListingSkipsAFoldedGroupWithForeignNames(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	keys := []string{"d/a", "d/a b", "d/x/b", "d/x/~a", "d/x/\u00e9", "d/y", "d/zz~", "e/z"}
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			return false
		}
		_, _ = io.WriteString(w, listFromKeys(keys, r.URL.Query()))
		return true
	}
	before := f.hits.Load()
	opts := blob.ListOptions{Prefix: "d/", Delimiter: "/", Limit: 2}
	var all []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the listing does not end")
		}
		pg, err := s.List(context.Background(), opts)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		all = append(all, pg.Keys...)
		all = append(all, pg.Prefixes...)
		if pg.Next == "" {
			break
		}
		opts.Token = pg.Next
	}
	if want := "d/a d/x/ d/y"; strings.Join(sortedCopy(all), " ") != want {
		t.Errorf("listed %v, want %s", all, want)
	}
	if n := f.hits.Load() - before; n > 8 {
		t.Errorf("%d requests for 3 entries in pages of 2", n)
	}
}
