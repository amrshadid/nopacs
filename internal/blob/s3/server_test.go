package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// proxy is a reverse proxy in front of the test server that counts what
// passes and may damage it, so a test sees exactly which requests a Put made
// and what the server does with bytes that arrive wrong.
type proxy struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []string
	mutate func(*http.Request, []byte) []byte
}

func newProxy(t testing.TB, cfg Config) *proxy {
	t.Helper()
	target, err := url.Parse(cfg.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{}
	rp := &httputil.ReverseProxy{ErrorLog: log.New(io.Discard, "", 0), Rewrite: func(pr *httputil.ProxyRequest) {
		r := pr.Out
		// r.Host stays what the client signed.
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		p.mu.Lock()
		p.reqs = append(p.reqs, r.Method+" "+r.URL.RawQuery)
		mutate := p.mutate
		p.mu.Unlock()
		if mutate != nil && r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			b = mutate(r, b)
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
	}}
	p.Server = httptest.NewServer(rp)
	t.Cleanup(p.Close)
	return p
}

// reset forgets the requests so far, such as those of Open.
func (p *proxy) reset() {
	p.mu.Lock()
	p.reqs = nil
	p.mu.Unlock()
}

// count returns how many requests had this method and a query containing
// sub.
func (p *proxy) count(method, sub string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.reqs {
		if m, q, _ := strings.Cut(r, " "); m == method && strings.Contains(q, sub) {
			n++
		}
	}
	return n
}

// openThrough opens a store on the test server through p, with a part size
// of 5 MiB so that small objects make several parts.
func openThrough(t testing.TB, p *proxy, cfg Config, mod func(*Config)) *Store {
	t.Helper()
	cfg.Endpoint = p.URL
	cfg.PartSize = MinPartSize
	cfg.Concurrency = 3
	if mod != nil {
		mod(&cfg)
	}
	return openAt(t, cfg)
}

// serverConfig returns the test server's configuration with a prefix of
// its own, and its bucket made.
func serverConfig(t testing.TB) Config {
	t.Helper()
	cfg := testConfig(t)
	ensureBucket(t, cfg)
	cfg.Prefix = uniquePrefix()
	cfg.StaleUploadAge = -1 // See openTest.
	return cfg
}

// serverSum reads key from the server directly, with the SDK's own client
// and none of the driver's code, and returns its SHA-256.
func serverSum(t testing.TB, s *Store, key string) ([sha256.Size]byte, int64) {
	t.Helper()
	out, err := s.client.GetObject(context.WithValue(context.Background(), objectStream{}, struct{}{}), &awss3.GetObjectInput{Bucket: aws.String(s.set.bucket), Key: aws.String(s.set.prefix + key)})
	if err != nil {
		t.Fatalf("reading %q from the server: %v", key, err)
	}
	defer out.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, out.Body)
	if err != nil {
		t.Fatal(err)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum, n
}

// TestMultipartSizes guards the choice between one request and an upload:
// an object of one part or less is one PutObject, a larger one a multipart
// upload of exactly the parts it needs, completed once, at the sizes where an
// off-by-one would show (a part less one byte, a part, a part and one byte,
// two parts, two parts and one byte). Each object reads back whole and across
// the part boundary, and the server holds the same bytes.
func TestMultipartSizes(t *testing.T) {
	const part = MinPartSize
	for _, n := range []int{0, 1, part - 1, part, part + 1, 2 * part, 2*part + 1, 3*part + 12345} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			cfg := serverConfig(t)
			p := newProxy(t, cfg)
			s := openThrough(t, p, cfg, nil)
			p.reset()
			ctx := context.Background()
			data := blobtest.PatternBytes(1, n)
			w, err := s.Put(ctx, "obj/x", bytes.NewReader(data))
			if err != nil || w.Size != int64(n) || w.SHA256 != sha256.Sum256(data) {
				t.Fatalf("Put = %+v, %v", w, err)
			}
			wantParts := (n + part - 1) / part
			if n <= part {
				if p.count("PUT", "") != 1 || p.count("POST", "uploads") != 0 || p.count("PUT", "partNumber") != 0 {
					t.Errorf("an object of %d bytes took %v, want one PUT", n, p.reqs)
				}
			} else if p.count("POST", "uploads") != 1 || p.count("PUT", "partNumber") != wantParts || p.count("POST", "uploadId") != 1 {
				t.Errorf("an object of %d bytes took %v, want one upload of %d parts completed once", n, p.reqs, wantParts)
			}
			if got := s.mustRead(t, "obj/x", 0, -1); !bytes.Equal(got, data) {
				t.Errorf("read back %d bytes that differ", len(got))
			}
			if n > part+10 {
				if got := s.mustRead(t, "obj/x", part-3, 10); !bytes.Equal(got, data[part-3:part+7]) {
					t.Errorf("a range across the part boundary gave wrong bytes")
				}
			}
			if sum, size := serverSum(t, s, "obj/x"); sum != sha256.Sum256(data) || size != int64(n) {
				t.Errorf("the server holds %d bytes with another checksum", size)
			}
		})
	}
}

func (s *Store) mustRead(t testing.TB, key string, off, length int64) []byte {
	t.Helper()
	r, err := s.Read(context.Background(), key, off, length)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMultipartFailuresOnServer guards, on a real server, that a multipart
// upload ends aborted and the key as it was when the reader fails midway,
// when the context ends midway, and when a part is damaged in transit: no
// unfinished upload is left (the cleanup after the test fails otherwise), the
// key holds nothing or the old object, and the error is the cause.
func TestMultipartFailuresOnServer(t *testing.T) {
	const part = MinPartSize
	ctx := context.Background()
	cfg := serverConfig(t)
	p := newProxy(t, cfg)
	s := openThrough(t, p, cfg, nil)
	old := blobtest.PatternBytes(1, 777)
	if _, err := s.Put(ctx, "k", bytes.NewReader(old)); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the reader broke")
	for i, n := range []int64{1, part - 1, part, part + 1, 2*part + 5, 3*part - 1} {
		_, err := s.Put(ctx, "k", io.MultiReader(blobtest.Pattern(uint64(i), n), errReader{boom}))
		if !errors.Is(err, boom) {
			t.Errorf("a reader that fails after %d bytes: %v, want its error", n, err)
		}
		cctx, cancel := context.WithCancel(ctx)
		_, err = s.Put(cctx, "k", io.MultiReader(blobtest.Pattern(uint64(i), n), cancelReader{cancel}, blobtest.Pattern(9, 3*part)))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancel after %d bytes: %v, want context.Canceled", n, err)
		}
		cancel()
		if got := s.mustRead(t, "k", 0, -1); !bytes.Equal(got, old) {
			t.Fatalf("after failures at %d bytes, the key holds %d bytes, want the old object", n, len(got))
		}
		if left := unfinished(t, s); len(left) != 0 {
			t.Fatalf("after failures at %d bytes, unfinished uploads remain: %v", n, left)
		}
	}
}

// TestDamagedBytesRefused guards that bytes damaged on their way to the
// server are never kept: the server checks the SHA-256 the request carries
// (and the signature over its payload), refuses the request, the Put fails,
// and the key holds what it held. It is run for an object and for a part.
func TestDamagedBytesRefused(t *testing.T) {
	for _, size := range []int{4096, 2*MinPartSize + 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cfg := serverConfig(t)
			p := newProxy(t, cfg)
			s := openThrough(t, p, cfg, nil)
			ctx := context.Background()
			if _, err := s.Put(ctx, "k", strings.NewReader("old")); err != nil {
				t.Fatal(err)
			}
			var damaged atomic.Int64
			p.mu.Lock()
			p.mutate = func(r *http.Request, b []byte) []byte {
				if r.Method == http.MethodPut && len(b) > 100 {
					b = slices.Clone(b)
					b[len(b)/2] ^= 0x01
					damaged.Add(1)
				}
				return b
			}
			p.mu.Unlock()
			_, err := s.Put(ctx, "k", bytes.NewReader(blobtest.PatternBytes(1, size)))
			if err == nil || damaged.Load() == 0 {
				t.Fatalf("Put of bytes damaged in transit = %v (damaged %d requests), want an error", err, damaged.Load())
			}
			t.Logf("refused: %v", err)
			if got := s.mustRead(t, "k", 0, -1); string(got) != "old" {
				t.Errorf("the key holds %d bytes after a damaged Put, want the old object", len(got))
			}
		})
	}
}

// TestStaleUploads guards the cleanup of uploads a crash left: Open aborts
// those under its prefix that are older than the age it was given, and leaves
// a younger one, one in another prefix, and one whose key this storage could
// not have written.
func TestStaleUploads(t *testing.T) {
	cfg := serverConfig(t)
	cfg.StaleUploadAge = -1 // The first store only makes the uploads.
	maker := openAt(t, cfg)
	other := cfg
	other.Prefix = uniquePrefix()
	bystander := openAt(t, other)

	begin := func(s *Store, key string) {
		t.Helper()
		out, err := s.client.CreateMultipartUpload(context.Background(), &awss3.CreateMultipartUploadInput{Bucket: aws.String(s.set.bucket), Key: aws.String(s.set.prefix + key)})
		if err != nil || out.UploadId == nil {
			t.Fatalf("beginning an upload: %v", err)
		}
	}
	begin(maker, "crash/a")
	begin(maker, "crash/b")
	begin(maker, "not a key") // Not a name the port can write.
	begin(bystander, "crash/c")
	if got := unfinished(t, maker); len(got) != 3 {
		t.Fatalf("setup: %d unfinished uploads, want 3", len(got))
	}

	// A day old is the default: nothing here is that old.
	young := cfg
	young.StaleUploadAge = 0
	s := openAt(t, young)
	if got := unfinished(t, s); len(got) != 3 {
		t.Errorf("Open aborted a young upload: %v left, want 3", got)
	}
	time.Sleep(1100 * time.Millisecond) // S3 times to the second.
	old := cfg
	old.StaleUploadAge = time.Second
	s = openAt(t, old)
	left := unfinished(t, s)
	if !slices.Equal(left, []string{"not a key"}) {
		t.Errorf("after Open with a one second age, %v are left, want only the key the port could not have written", left)
	}
	if got := unfinished(t, bystander); !slices.Equal(got, []string{"crash/c"}) {
		t.Errorf("the other prefix's uploads are %v, want crash/c untouched", got)
	}
	// Clean up what the test left on purpose.
	for _, st := range []*Store{maker, bystander} {
		for _, u := range uploadsUnder(t, st) {
			_ = st.abort(context.Background(), u.Key, u.UploadId)
		}
	}
}

// TestPrefixIsolation guards that a storage under a prefix touches nothing
// outside it: a second storage under a prefix that only begins the same
// ("a" and "ab") is neither listed, read nor deleted through the first, a key
// the same in both is two objects, and an object under the prefix that no key
// could name is not listed.
func TestPrefixIsolation(t *testing.T) {
	cfg := testConfig(t)
	ensureBucket(t, cfg)
	base := strings.TrimSuffix(uniquePrefix(), "/")
	ca, cb := cfg, cfg
	ca.Prefix, cb.Prefix = base+"/a", base+"/ab"
	a, b := openAt(t, ca), openAt(t, cb)
	ctx := context.Background()
	for _, k := range []string{"x", "d/y", "d/z/w"} {
		if _, err := a.Put(ctx, k, strings.NewReader("in a "+k)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Put(ctx, k, strings.NewReader("in b "+k)); err != nil {
			t.Fatal(err)
		}
	}
	// Objects under a's prefix that the port could not have named. (A folder
	// marker, "folder/", would also be left out of a listing of keys, but
	// shows as an empty prefix in a listing by level: S3 itself groups it so.)
	for _, junk := range []string{"has space", "d/odd name"} {
		_, err := a.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(a.set.bucket), Key: aws.String(a.set.prefix + junk), Body: strings.NewReader("junk")})
		if err != nil {
			t.Fatalf("planting %q: %v", junk, err)
		}
	}
	for _, delim := range []string{"", "/"} {
		var keys []string
		opts := blob.ListOptions{Delimiter: delim, Limit: 2}
		for {
			p, err := a.List(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, p.Keys...)
			keys = append(keys, p.Prefixes...)
			if p.Next == "" {
				break
			}
			opts.Token = p.Next
		}
		slices.Sort(keys)
		want := []string{"d/y", "d/z/w", "x"}
		if delim == "/" {
			want = []string{"d/", "x"}
		}
		if !slices.Equal(keys, want) {
			t.Errorf("List(delimiter %q) through a = %q, want %q", delim, keys, want)
		}
	}
	if got := string(a.mustRead(t, "d/y", 0, -1)); got != "in a d/y" {
		t.Errorf("a reads %q", got)
	}
	if err := a.Delete(ctx, "d/y"); err != nil {
		t.Fatal(err)
	}
	if got := string(b.mustRead(t, "d/y", 0, -1)); got != "in b d/y" {
		t.Errorf("deleting in a changed b: %q", got)
	}
	if _, err := a.Stat(ctx, "d/y"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Stat after Delete = %v", err)
	}
	// Everything of b is in the bucket under b's prefix only.
	out, err := a.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(a.set.bucket), Prefix: aws.String(base + "/")})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Contents {
		if k := aws.ToString(o.Key); !strings.HasPrefix(k, ca.Prefix+"/") && !strings.HasPrefix(k, cb.Prefix+"/") {
			t.Errorf("an object outside both prefixes: %q", k)
		}
	}
	// Clean the junk the store could not list.
	for _, junk := range []string{"has space", "d/odd name"} {
		_, _ = a.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(a.set.bucket), Key: aws.String(a.set.prefix + junk)})
	}
}

// TestMemoryBounded guards that a Put's memory does not grow with the object:
// 160 MiB goes through parts of 5 MiB in at most 3 buffers, and the heap in
// use stays far below the object's size.
func TestMemoryBounded(t *testing.T) {
	cfg := serverConfig(t)
	s := openThrough(t, newProxy(t, cfg), cfg, nil)
	const size = 160 << 20
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
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	w, err := s.Put(context.Background(), "big", blobtest.Pattern(5, size))
	close(done)
	wg.Wait()
	if err != nil || w.Size != size || w.SHA256 != blobtest.PatternSum(5, size) {
		t.Fatalf("Put = %+v, %v", w, err)
	}
	grew := max(peak.Load(), base.HeapInuse) - base.HeapInuse
	t.Logf("a %d MiB Put grew the heap by %d MiB at most (3 buffers of 5 MiB)", size>>20, grew>>20)
	if grew > 64<<20 {
		t.Errorf("the heap grew by %d MiB during a %d MiB Put, want far less than the object", grew>>20, size>>20)
	}
}

// TestPutStreamsWholeOrNothing guards, on a real server, that while a large
// Put is in progress no reader sees any of its bytes: the key shows nothing
// until Put returns, then all of them.
func TestPutStreamsWholeOrNothing(t *testing.T) {
	cfg := serverConfig(t)
	px := newProxy(t, cfg)
	s := openThrough(t, px, cfg, nil)
	ctx := context.Background()
	gate := make(chan struct{})
	started := make(chan struct{})
	src := io.MultiReader(blobtest.Pattern(1, 2*MinPartSize), &gateReader{started: started, gate: gate}, blobtest.Pattern(2, MinPartSize))
	var wg sync.WaitGroup
	var perr error
	wg.Go(func() { _, perr = s.Put(ctx, "k", src) })
	<-started
	// Wait until the first part has reached the server, which is the event the
	// checks below are about. (Only the first can be sent: the reader is stalled
	// while the second is being filled.)
	for deadline := time.Now().Add(2 * time.Minute); px.count("PUT", "partNumber") < 1; {
		if time.Now().After(deadline) {
			t.Fatal("no part reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Stat(ctx, "k"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Stat during a Put = %v, want NotFound: nothing is visible until the Put returns", err)
	}
	if got, _ := s.List(ctx, blob.ListOptions{}); len(got.Keys) != 0 {
		t.Errorf("List during a Put shows %q", got.Keys)
	}
	close(gate)
	wg.Wait()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := s.Stat(ctx, "k"); err != nil {
		t.Errorf("Stat after the Put = %v", err)
	}
}

// gateReader tells a test it was reached, and yields nothing until the gate
// opens.
type gateReader struct {
	started chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (g *gateReader) Read([]byte) (int, error) {
	g.once.Do(func() { close(g.started) })
	<-g.gate
	return 0, io.EOF
}

// TestOpenFailures guards Open against a real server that refuses it: a bucket
// that does not exist, a key it does not know and a wrong secret are Invalid
// (trying again changes nothing), a port nothing listens on is Unavailable,
// and no message holds the bucket, the key, the secret or the address.
func TestOpenFailures(t *testing.T) {
	good := testConfig(t)
	ensureBucket(t, good)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(good.Endpoint, "http://"), "https://"))
	for name, c := range map[string]struct {
		mod  func(*Config)
		kind fault.Kind
	}{
		"no such bucket": {func(c *Config) { c.Bucket = "no-such-bucket-for-nopacs" }, fault.Invalid},
		"unknown key":    {func(c *Config) { c.AccessKeyID = NewSecret("unknownkeyunknownkey") }, fault.Invalid},
		"wrong secret":   {func(c *Config) { c.SecretAccessKey = NewSecret("wrong-secret-wrong-secret") }, fault.Invalid},
		"nothing there":  {func(c *Config) { c.Endpoint = "http://127.0.0.1:1" }, fault.Unavailable},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			c.mod(&cfg)
			start := time.Now()
			s, err := Open(context.Background(), cfg)
			if err == nil {
				_ = s.Close()
			}
			if fault.KindOf(err) != c.kind {
				t.Fatalf("Open = %v, want %v", err, c.kind)
			}
			if time.Since(start) > 20*time.Second {
				t.Errorf("Open took %v to fail", time.Since(start))
			}
			forbidden := []string{good.Bucket, cfg.Bucket, good.AccessKeyID.Reveal(), cfg.AccessKeyID.Reveal(), good.SecretAccessKey.Reveal(), cfg.SecretAccessKey.Reveal(), "127.0.0.1", port}
			for _, text := range []string{err.Error(), fault.Message(err)} {
				for _, f := range forbidden {
					if strings.Contains(text, f) {
						t.Errorf("the error shows %q: %q", f, text)
					}
				}
			}
		})
	}
}

// rawPut puts an object under a name the port may not have, straight to the
// server.
func rawPut(t *testing.T, s *Store, name string) {
	t.Helper()
	_, err := s.client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(s.set.bucket), Key: aws.String(name), Body: strings.NewReader("x"),
	})
	if err != nil {
		t.Fatalf("raw put %q: %v", name, err)
	}
}

func deleteInput(s *Store, name string) *awss3.DeleteObjectInput {
	return &awss3.DeleteObjectInput{Bucket: aws.String(s.set.bucket), Key: aws.String(name)}
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestEveryPortKeyFitsUnderEveryPrefixOnServers guards, on a real server,
// the relation between the port's and S3's limits: the port's longest key
// under the longest prefix is exactly 1,024 bytes in the bucket, and it is
// stored, read, listed and deleted.
func TestEveryPortKeyFitsUnderEveryPrefixOnServers(t *testing.T) {
	cfg := serverConfig(t)
	cfg.Prefix = longestPrefix()[:MaxPrefixLength-len("t-0123456789abcdef/")] + uniquePrefix()
	s := openAt(t, cfg)
	ctx := context.Background()
	key := keyOfLength(blob.MaxKeyLength, 'c')
	if len(s.set.prefix) != MaxPrefixLength || len(key)+len(s.set.prefix) != MaxKeyBytes || blob.CheckKey(key) != nil {
		t.Fatalf("the test's name is %d bytes with a prefix of %d: %v", len(key)+len(s.set.prefix), len(s.set.prefix), blob.CheckKey(key))
	}
	if _, err := s.Put(ctx, key, strings.NewReader("edge")); err != nil {
		t.Fatalf("Put of a key of %d bytes under a prefix of %d: %v", len(key), len(s.set.prefix), err)
	}
	if got := string(s.mustRead(t, key, 0, -1)); got != "edge" {
		t.Errorf("read %q", got)
	}
	pg, err := s.List(ctx, blob.ListOptions{})
	if err != nil || len(pg.Keys) != 1 || pg.Keys[0] != key {
		t.Errorf("List = %v, %v", pg.Keys, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Error(err)
	}
}
