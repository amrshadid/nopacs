package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// noLeak fails the test when text, which an error shows to a log or to a
// caller, holds anything an error must not: the server's words, the
// endpoint's address, the bucket, the access key or the secret.
func noLeak(t *testing.T, what string, err error, extra ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want an error", what)
		return
	}
	forbidden := append([]string{"secret-server-words", "REQ123", fakeBucket, fakeKey, fakeSecret, "127.0.0.1", "localhost"}, extra...)
	for _, text := range []string{err.Error(), fault.Message(err)} {
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s: the error shows %q: %q", what, f, text)
			}
		}
	}
}

// TestConfigRefused guards that a configuration that cannot work is refused
// as Invalid at Open, before any connection, and that the message names the
// field and never a value, since a value may be a secret.
func TestConfigRefused(t *testing.T) {
	good := Config{Endpoint: "https://s3.example.com", Region: "r", Bucket: "bucket-1", AccessKeyID: NewSecret("k"), SecretAccessKey: NewSecret("very-secret"), AllowAddress: allowAll}
	for name, mod := range map[string]func(*Config){
		"no endpoint scheme":  func(c *Config) { c.Endpoint = "s3.example.com" },
		"ftp":                 func(c *Config) { c.Endpoint = "ftp://s3.example.com" },
		"plain http":          func(c *Config) { c.Endpoint = "http://s3.example.com" },
		"credentials in url":  func(c *Config) { c.Endpoint = "https://k:very-secret@s3.example.com" },
		"path in endpoint":    func(c *Config) { c.Endpoint = "https://s3.example.com/bucket" },
		"empty host":          func(c *Config) { c.Endpoint = "https://" },
		"no region":           func(c *Config) { c.Region = "" },
		"bad bucket":          func(c *Config) { c.Bucket = "Bad_Bucket" },
		"no bucket":           func(c *Config) { c.Bucket = "" },
		"no key":              func(c *Config) { c.AccessKeyID = Secret{} },
		"no secret":           func(c *Config) { c.SecretAccessKey = Secret{} },
		"no address check":    func(c *Config) { c.AllowAddress = nil },
		"bad prefix":          func(c *Config) { c.Prefix = "a b/c" },
		"dotdot prefix":       func(c *Config) { c.Prefix = "../x" },
		"small part":          func(c *Config) { c.PartSize = 1 << 20 },
		"huge part":           func(c *Config) { c.PartSize = 2 << 30 },
		"negative concurrent": func(c *Config) { c.Concurrency = -1 },
		"many concurrent":     func(c *Config) { c.Concurrency = 100 },
		"junk CA bundle":      func(c *Config) { c.CABundle = []byte("not pem") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mod(&cfg)
			_, err := Open(context.Background(), cfg)
			if fault.KindOf(err) != fault.Invalid {
				t.Fatalf("Open = %v, want Invalid", err)
			}
			if strings.Contains(err.Error(), "very-secret") {
				t.Errorf("the error shows the secret: %v", err)
			}
		})
	}
	if got := fmt.Sprintf("%v %+v %#v", good, good, good); strings.Contains(got, "very-secret") {
		t.Errorf("printing a Config shows the secret: %s", got)
	}
}

// TestAllowPublic guards the ready-made address check: it passes a public
// address and refuses loopback, private, shared, link-local (cloud metadata),
// multicast and unspecified ones, also when mapped into IPv6.
func TestAllowPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true, "93.184.216.34": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"100.64.0.1": false, "169.254.169.254": false, "fe80::1": false, "fd00::1": false,
		"224.0.0.1": false, "ff02::1": false, "0.0.0.0": false, "::": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true,
	} {
		if got := AllowPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AllowPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

// TestAddressCheck guards that a server on loopback is refused when the
// caller's check refuses loopback, with no connection made to it, and
// served when the check allows it.
func TestAddressCheck(t *testing.T) {
	f := newFakeS3(t)
	var seen []netip.Addr
	cfg := f.config()
	cfg.AllowAddress = func(a netip.Addr) bool { seen = append(seen, a); return false }
	start := time.Now()
	_, err := Open(context.Background(), cfg)
	if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("Open with loopback refused = %v, want Unavailable and the refusal", err)
	}
	noLeak(t, "refused address", err)
	if n := f.hits.Load(); n != 0 {
		t.Errorf("the server got %d requests, want none: a refused address must not be connected to", n)
	}
	if len(seen) != 1 || !seen[0].IsLoopback() {
		t.Errorf("the check was asked about %v, want exactly one loopback address (a refused address is not retried)", seen)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("Open took %v to refuse an address", time.Since(start))
	}
	if AllowPublic(seen[0]) {
		t.Errorf("AllowPublic passed %v", seen[0])
	}
	openFake(t, f, nil) // The same server, allowed.
}

// fakeDNS answers every A query with the address it holds, with a TTL of
// 0, so a test can change what a name resolves to between calls.
type fakeDNS struct {
	addr atomic.Value // net.IP
	conn net.PacketConn
}

func newFakeDNS(t *testing.T, initial string) *fakeDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDNS{conn: conn}
	d.addr.Store(net.ParseIP(initial).To4())
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			q := buf[12:n]
			end := 0
			for end < len(q) && q[end] != 0 {
				end += int(q[end]) + 1
			}
			question := q[:end+5] // name, zero, type, class
			qtype := uint16(q[end+1])<<8 | uint16(q[end+2])
			resp := append([]byte{buf[0], buf[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}, question...)
			if qtype == 1 {
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
				resp = append(resp, d.addr.Load().(net.IP)...)
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	return d
}

func (d *fakeDNS) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var nd net.Dialer
		return nd.DialContext(ctx, "udp", d.conn.LocalAddr().String())
	}}
}

// TestAddressCheckFollowsDNS guards that the address check applies to the
// address a name resolves to at each connection: a name that resolved to an
// allowed address and later resolves to a refused one is refused, with no
// connection made to the new address.
func TestAddressCheckFollowsDNS(t *testing.T) {
	f := newFakeS3(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.URL, "http://"))
	dns := newFakeDNS(t, "127.0.0.1")
	cfg := f.config()
	cfg.Endpoint = "http://storage.test:" + port
	cfg.Resolver = dns.resolver()
	var asked []netip.Addr
	cfg.AllowAddress = func(a netip.Addr) bool { asked = append(asked, a); return a.IsLoopback() }
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open through the fake DNS: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// The fake closes every connection after one answer, so the next call
	// resolves again.
	if _, err := s.Stat(context.Background(), "k"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("Stat while the name resolves to loopback = %v, want NotFound", err)
	}
	before := f.hits.Load()
	dns.addr.Store(net.ParseIP("10.9.8.7").To4()) // The name now points somewhere refused.
	_, err = s.Stat(context.Background(), "k")
	if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("Stat after the name moved to a private address = %v, want the refusal", err)
	}
	noLeak(t, "moved name", err, "storage.test", "10.9.8.7")
	if f.hits.Load() != before {
		t.Errorf("a request reached the server after the name moved")
	}
	if last := asked[len(asked)-1]; last != netip.MustParseAddr("10.9.8.7") {
		t.Errorf("the check was last asked about %v, want 10.9.8.7", last)
	}
}

// TestRedirectNotFollowed guards that a redirect is not followed: the server
// it names gets no request, and the error is Unavailable without its address.
func TestRedirectNotFollowed(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	t.Cleanup(other.Close)
	f := newFakeS3(t)
	var redirect atomic.Bool
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if redirect.Load() {
			w.Header().Set("Location", other.URL+r.URL.Path)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return true
		}
		return false
	}
	s := openFake(t, f, nil)
	redirect.Store(true)
	ctx := context.Background()
	_, err := s.Stat(ctx, "k")
	noLeak(t, "redirected stat", err, other.Listener.Addr().String())
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a redirect = %v, want Unavailable", err)
	}
	_, err = s.Put(ctx, "k", strings.NewReader("x"))
	noLeak(t, "redirected put", err)
	_, err = s.List(ctx, blob.ListOptions{})
	noLeak(t, "redirected list", err)
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want none", n)
	}
	// A redirect during Open fails Open the same way.
	if _, err := Open(ctx, f.config()); err == nil {
		t.Fatal("Open followed or ignored a redirect")
	}
}

// TestErrorsLeakNothing guards that, whatever the server answers, an error's
// text and its caller-safe message hold neither the server's words, nor the
// endpoint, the bucket, the access key or the secret, and that the kind is
// the port's.
func TestErrorsLeakNothing(t *testing.T) {
	f := newFakeS3(t)
	var mode atomic.Value
	mode.Store("ok")
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		switch mode.Load().(string) {
		case "forbidden":
			xmlError(w, 403, "AccessDenied", "secret-server-words "+fakeKey+" "+fakeBucket)
		case "unknown":
			xmlError(w, 400, "SecretServerWordsCode", "secret-server-words")
		case "busy":
			xmlError(w, 503, "SlowDown", "secret-server-words")
		case "html":
			w.WriteHeader(502)
			fmt.Fprint(w, "<html>secret-server-words</html>")
		case "reset":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		default:
			return false
		}
		return true
	}
	s := openFake(t, f, nil)
	ctx := context.Background()
	for _, m := range []string{"forbidden", "unknown", "busy", "html", "reset"} {
		mode.Store(m)
		short, cancel := context.WithTimeout(ctx, 20*time.Second)
		for op, err := range map[string]error{
			"put":    second(s.Put(short, "k", strings.NewReader("x"))),
			"stat":   second(s.Stat(short, "k")),
			"read":   second(s.Read(short, "k", 0, -1)),
			"list":   second(s.List(short, blob.ListOptions{})),
			"delete": s.Delete(short, "k"),
		} {
			noLeak(t, m+" "+op, err)
			if k := fault.KindOf(err); err != nil && k != fault.Unavailable {
				t.Errorf("%s %s: kind %v, want Unavailable", m, op, k)
			}
		}
		cancel()
	}
	mode.Store("forbidden")
	_, err := Open(ctx, f.config())
	noLeak(t, "Open with refused credentials", err)
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("Open with refused credentials = %v, want Invalid", err)
	}
}

// TestTLS guards the trust in a server's certificate: the system's roots do
// not trust a server's own certificate, so Open fails as Unavailable without
// the address or the certificate's details, and the extra CA bundle makes it
// work.
func TestTLS(t *testing.T) {
	f := newFakeS3TLS(t)
	cfg := f.config()
	cfg.CABundle = nil
	_, err := Open(context.Background(), cfg)
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Open to a server with an untrusted certificate = %v, want Unavailable", err)
	}
	noLeak(t, "untrusted certificate", err, "x509", "certificate")
	s := openFake(t, f, nil)
	if _, err := s.Put(context.Background(), "k", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	// Plain http is refused unless the caller says so.
	g := newFakeS3(t)
	cfg = g.config()
	cfg.AllowHTTP = false
	if _, err := Open(context.Background(), cfg); fault.KindOf(err) != fault.Invalid || g.hits.Load() != 0 {
		t.Errorf("Open to an http endpoint without AllowHTTP = %v with %d requests, want Invalid and none", err, g.hits.Load())
	}
}

func second[T any](_ T, err error) error { return err }

// TestRetries guards what is repeated and what is not: a request that fails
// with a busy server is tried three times in all and then fails, one that is
// refused is tried once, and the request that completes a multipart upload
// is never repeated, since a repeat after a success would report a failure
// for an object that is in place.
func TestRetries(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
	ctx := context.Background()

	var fails atomic.Int64
	fails.Store(2)
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodHead && fails.Add(-1) >= 0 {
			xmlError(w, 503, "SlowDown", "busy")
			return true
		}
		return false
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	before := f.count(http.MethodHead, "")
	if _, err := s.Stat(ctx, "k"); err != nil {
		t.Fatalf("Stat after two failures = %v, want success on the third try", err)
	}
	if got := f.count(http.MethodHead, "") - before; got != 3 {
		t.Errorf("Stat made %d requests after two failures, want 3", got)
	}

	fails.Store(1000)
	before = f.count(http.MethodHead, "")
	start := time.Now()
	_, err := s.Stat(ctx, "k")
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Stat against a busy server = %v, want Unavailable", err)
	}
	if got := f.count(http.MethodHead, "") - before; got != 3 {
		t.Errorf("Stat against a busy server made %d requests, want 3 and then to give up", got)
	}
	if time.Since(start) > 60*time.Second {
		t.Errorf("giving up took %v", time.Since(start))
	}

	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodHead {
			xmlError(w, 403, "AccessDenied", "no")
			return true
		}
		return false
	}
	before = f.count(http.MethodHead, "")
	_, _ = s.Stat(ctx, "k")
	if got := f.count(http.MethodHead, "") - before; got != 1 {
		t.Errorf("a refused Stat made %d requests, want 1", got)
	}

	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploadId") {
			xmlError(w, 503, "SlowDown", "busy")
			return true
		}
		return false
	}
	data := blobtest.PatternBytes(1, 2*MinPartSize+10)
	_, err = s.Put(ctx, "big", bytes.NewReader(data))
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Put whose completion fails = %v, want Unavailable", err)
	}
	if got := f.count(http.MethodPost, "uploadId"); got != 1 {
		t.Errorf("the completion was sent %d times, want once", got)
	}
	if a, _, _ := f.state(); a != 1 {
		t.Errorf("the failed upload was aborted %d times, want once", a)
	}
}

// TestEnvironmentIgnored guards that a storage uses only what it was given:
// with credentials, an endpoint, a profile, a credentials file, instance
// metadata and a proxy all set in the environment, the requests still go
// to the configured endpoint, directly, signed with the configured key, and
// nothing else is contacted.
func TestEnvironmentIgnored(t *testing.T) {
	var metadata, proxy atomic.Int64
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { metadata.Add(1) }))
	t.Cleanup(meta.Close)
	prox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxy.Add(1) }))
	t.Cleanup(prox.Close)
	creds := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(creds, []byte("[default]\naws_access_key_id = FILEKEYFILEKEYFILEKEY\naws_secret_access_key = filesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := newFakeS3(t)
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "ENVKEYENVKEYENVKEYEN", "AWS_SECRET_ACCESS_KEY": "envsecret", "AWS_SESSION_TOKEN": "envtoken",
		"AWS_REGION": "eu-west-3", "AWS_DEFAULT_REGION": "eu-west-3",
		"AWS_ENDPOINT_URL": other.URL, "AWS_ENDPOINT_URL_S3": other.URL,
		"AWS_PROFILE": "other", "AWS_SHARED_CREDENTIALS_FILE": creds, "AWS_CONFIG_FILE": creds,
		"AWS_EC2_METADATA_SERVICE_ENDPOINT": meta.URL, "AWS_CONTAINER_CREDENTIALS_FULL_URI": meta.URL,
		"AWS_WEB_IDENTITY_TOKEN_FILE": creds, "AWS_ROLE_ARN": "arn:aws:iam::1:role/x",
		"HTTP_PROXY": prox.URL, "HTTPS_PROXY": prox.URL, "ALL_PROXY": prox.URL, "http_proxy": prox.URL, "https_proxy": prox.URL,
		"NO_PROXY": "",
	} {
		t.Setenv(k, v)
	}
	f := newFakeS3(t)
	// The endpoint is a name, because Go never sends a request to a loopback
	// address through a proxy, whatever the environment says.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.URL, "http://"))
	dns := newFakeDNS(t, "127.0.0.1")
	s := openFake(t, f, func(c *Config) {
		c.Endpoint = "http://storage.test:" + port
		c.Resolver = dns.resolver()
	})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) < 3 {
		t.Fatalf("the configured server saw %d requests", len(reqs))
	}
	for _, r := range reqs {
		auth := r.header.Get("Authorization")
		if !strings.Contains(auth, "Credential="+fakeKey+"/") || !strings.Contains(auth, "/us-east-1/s3/") {
			t.Errorf("a request was signed %q, want the configured key and region", auth)
		}
		if r.header.Get("X-Amz-Security-Token") != "" {
			t.Errorf("a request carried a session token from the environment")
		}
	}
	if n := metadata.Load() + proxy.Load() + int64(len(other.requests())); n != 0 {
		t.Errorf("%d requests went to the metadata endpoint, the proxy or the environment's endpoint", n)
	}
}

// TestChecksumHeaders guards what is sent for integrity on a server other
// than Amazon's: the SHA-256 of each request's bytes in x-amz-checksum-sha256,
// for an object and for every part, and none of the SDK's default CRC32
// trailer or aws-chunked encoding, which several servers reject.
func TestChecksumHeaders(t *testing.T) {
	// Over http the SDK signs the payload and puts a checksum it computes in a
	// header; over https it signs nothing of the payload and would send the
	// checksum as a trailer of an aws-chunked body, unless it is given the
	// value, which is what the driver does.
	t.Run("http", func(t *testing.T) { checksumHeaders(t, newFakeS3(t)) })
	t.Run("https", func(t *testing.T) { checksumHeaders(t, newFakeS3TLS(t)) })
}

func checksumHeaders(t *testing.T, f *fakeS3) {
	s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
	ctx := context.Background()
	small := blobtest.PatternBytes(1, 1000)
	big := blobtest.PatternBytes(2, 2*MinPartSize+5)
	for k, d := range map[string][]byte{"small": small, "big": big} {
		w, err := s.Put(ctx, k, bytes.NewReader(d))
		if err != nil || w.SHA256 != sha256.Sum256(d) || w.Size != int64(len(d)) {
			t.Fatalf("Put(%s) = %+v, %v", k, w, err)
		}
	}
	puts := 0
	for _, r := range f.requests() {
		if r.method != http.MethodPut {
			if r.method == http.MethodPost && r.query.Has("uploads") {
				if got := r.header.Get("x-amz-checksum-algorithm"); got != "SHA256" {
					t.Errorf("CreateMultipartUpload says checksum algorithm %q, want SHA256", got)
				}
			}
			continue
		}
		puts++
		if got, want := r.header.Get("x-amz-checksum-sha256"), sha256b64(r.body); got != want {
			t.Errorf("PUT %s?%v sent checksum %q, want %q", r.path, r.query, got, want)
		}
		for _, h := range []string{"x-amz-trailer", "x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme", "x-amz-decoded-content-length"} {
			if v := r.header.Get(h); v != "" {
				t.Errorf("PUT carried %s: %q", h, v)
			}
		}
		if ce := r.header.Get("Content-Encoding"); strings.Contains(ce, "aws-chunked") {
			t.Errorf("PUT used Content-Encoding %q", ce)
		}
		if len(r.body) == 0 {
			t.Errorf("PUT sent no body")
		}
	}
	if puts != 5 {
		t.Errorf("saw %d PUTs, want the probe at Open, 1 for the small object and 3 parts for the big", puts)
	}
	for _, r := range f.requests() {
		if r.method == http.MethodPost && r.query.Has("uploadId") && !bytes.Contains(r.body, []byte("<ChecksumSHA256>")) {
			t.Errorf("the completion lists no part checksums: %s", r.body)
		}
	}
}

// TestSlowServers guards the deadlines: a server that never answers fails a
// call at the call's deadline, one that answers and then stalls fails the
// stream at its idle timeout, a Read's bytes that stop short are an error,
// and none leaves a goroutine waiting.
func TestSlowServers(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	s.set.callTimeout, s.set.dataTimeout, s.set.idleTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	var mode atomic.Value
	mode.Store("hang")
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		switch mode.Load().(string) {
		case "hang":
			<-stall
		case "stall":
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("01234"))
			w.(http.Flusher).Flush()
			<-stall
		case "short":
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("01234"))
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		default:
			return false
		}
		return true
	}
	for op, call := range map[string]func() error{
		"stat":   func() error { _, err := s.Stat(ctx, "k"); return err },
		"delete": func() error { return s.Delete(ctx, "k") },
		"list":   func() error { _, err := s.List(ctx, blob.ListOptions{}); return err },
		"put":    func() error { _, err := s.Put(ctx, "k", strings.NewReader("x")); return err },
	} {
		start := time.Now()
		err := call()
		// The SDK retries a timeout, so a call may take up to its attempts.
		if fault.KindOf(err) != fault.Unavailable || time.Since(start) > 20*time.Second {
			t.Errorf("%s against a server that never answers = %v after %v, want Unavailable within the call's deadline", op, err, time.Since(start))
		}
		noLeak(t, op, err)
	}
	// The caller's own deadline wins over the driver's, and is what the error matches.
	s.set.callTimeout = time.Minute
	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := s.Stat(dctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stat with the caller's deadline = %v, want its DeadlineExceeded", err)
	}
	mode.Store("stall")
	r, err := s.Read(ctx, "k", 0, -1)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	start := time.Now()
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err == nil || string(b) != "01234" || time.Since(start) > 20*time.Second {
		t.Errorf("a stalled stream gave %q, %v after %v, want the bytes sent and an error at the idle timeout", b, err, time.Since(start))
	}
	noLeak(t, "stalled stream", err)
	mode.Store("short")
	r, err = s.Read(ctx, "k", 0, -1)
	if err == nil {
		b, err = io.ReadAll(r)
		_ = r.Close()
	}
	if err == nil {
		t.Errorf("a stream that stopped at 5 of 10 bytes read as complete: %q", b)
	}
	noLeak(t, "short stream", err)
}

// TestWrongAnswers guards against a server that answers wrongly: a ranged
// read it answers with the whole object, a size that is missing, malformed
// XML in a listing, and an empty success. None may be taken for right bytes
// or panic.
func TestWrongAnswers(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	// The fake ignores Range and sends the whole object with 200.
	if r, err := s.Read(ctx, "k", 3, 4); err == nil {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		t.Errorf("a ranged Read answered with the whole object gave %q, want an error", b)
	} else {
		noLeak(t, "ignored range", err)
	}
	// A download whose CRC32 header is wrong is read as it is: the driver asks
	// the SDK to check a response checksum only where the API requires one,
	// since servers other than Amazon's send values the SDK cannot check, and
	// the archive compares each file's SHA-256 itself.
	f.badCRC.Store(true)
	if got := s.mustRead(t, "k", 0, -1); string(got) != "0123456789" {
		t.Errorf("a download with a foreign CRC32 header read %q", got)
	}
	f.badCRC.Store(false)
	// A server that answers an upload with another checksum than the one sent
	// has kept other bytes: the Put fails, for an object and for a part.
	f.badEcho.Store(true)
	for _, n := range []int{10, DefaultPartSize + 1} {
		if _, err := s.Put(ctx, "echo", bytes.NewReader(blobtest.PatternBytes(1, n))); fault.KindOf(err) != fault.Unavailable {
			t.Errorf("Put of %d bytes whose checksum the server answers wrongly = %v, want Unavailable", n, err)
		}
	}
	f.badEcho.Store(false)
	var mode atomic.Value
	mode.Store("xml")
	f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		switch mode.Load().(string) {
		case "xml":
			fmt.Fprint(w, "<ListBucketResult><Contents><Key>k</Key></Contents><IsTruncated>true")
		case "empty":
			w.WriteHeader(http.StatusOK)
		case "nouploadid":
			if r.Method == http.MethodPost {
				fmt.Fprint(w, `<InitiateMultipartUploadResult></InitiateMultipartUploadResult>`)
				return true
			}
			return false
		default:
			return false
		}
		return true
	}
	if _, err := s.List(ctx, blob.ListOptions{}); err == nil {
		t.Error("List of malformed XML succeeded")
	} else {
		noLeak(t, "malformed list", err)
	}
	mode.Store("empty")
	if p, err := s.List(ctx, blob.ListOptions{}); err == nil && len(p.Keys)+len(p.Prefixes) != 0 {
		t.Errorf("List of an empty answer = %+v", p)
	}
	if info, err := s.Stat(ctx, "k"); err == nil && info.Size != 0 {
		t.Errorf("Stat of an answer with no size = %+v", info)
	}
	mode.Store("nouploadid")
	big := blobtest.PatternBytes(3, DefaultPartSize+1)
	if _, err := s.Put(ctx, "big", bytes.NewReader(big)); err == nil {
		t.Error("Put succeeded though the server began no upload")
	}
}

// TestMultipartAbortedOnFailure guards that a multipart upload the driver
// began is aborted when a part is refused, when the reader fails midway, and
// when the context ends, with the error the caller must see, and that the
// object is never completed.
func TestMultipartAbortedOnFailure(t *testing.T) {
	ctx := context.Background()
	big := blobtest.PatternBytes(5, 3*MinPartSize+17)

	t.Run("part refused", func(t *testing.T) {
		f := newFakeS3(t)
		s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
		f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.URL.Query().Get("partNumber") == "2" {
				xmlError(w, 400, "InvalidPart", "secret-server-words")
				return true
			}
			return false
		}
		_, err := s.Put(ctx, "k", bytes.NewReader(big))
		noLeak(t, "refused part", err)
		if err == nil {
			t.Fatal("Put succeeded though a part was refused")
		}
		if a, u, o := f.state(); a != 1 || u != 0 || o != 0 || f.count(http.MethodPost, "uploadId") != 0 {
			t.Errorf("aborted %d, %d uploads left, %d objects, %d completions; want 1, 0, 0, 0", a, u, o, f.count(http.MethodPost, "uploadId"))
		}
	})
	t.Run("reader fails", func(t *testing.T) {
		f := newFakeS3(t)
		s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
		boom := errors.New("the reader broke")
		_, err := s.Put(ctx, "k", io.MultiReader(bytes.NewReader(big[:2*MinPartSize+3]), errReader{boom}))
		if !errors.Is(err, boom) {
			t.Fatalf("Put from a failing reader = %v, want its error", err)
		}
		if a, u, o := f.state(); a != 1 || u != 0 || o != 0 {
			t.Errorf("aborted %d, %d uploads left, %d objects; want 1, 0, 0", a, u, o)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		f := newFakeS3(t)
		s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		src := io.MultiReader(bytes.NewReader(big[:2*MinPartSize+3]), cancelReader{cancel}, bytes.NewReader(big[2*MinPartSize+3:]))
		_, err := s.Put(cctx, "k", src)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put canceled midway = %v, want context.Canceled", err)
		}
		if a, u, o := f.state(); a != 1 || u != 0 || o != 0 || f.count(http.MethodPost, "uploadId") != 0 {
			t.Errorf("aborted %d, %d uploads left, %d objects; want 1, 0, 0", a, u, o)
		}
	})
	t.Run("too many parts", func(t *testing.T) {
		f := newFakeS3(t)
		s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
		// The limit is lowered to 3 parts so that the test does not send 50 GB;
		// the reader is infinite, so the Put ends when the count is exceeded.
		s.set.maxParts = 3
		f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodPut && r.URL.Query().Has("partNumber") {
				w.Header().Set("ETag", `"p"`)
				w.Header().Set("x-amz-checksum-sha256", r.Header.Get("x-amz-checksum-sha256"))
				return true
			}
			return false
		}
		start := time.Now()
		_, err := s.Put(ctx, "k", blobtest.Pattern(1, 1<<40))
		if fault.KindOf(err) != fault.Invalid {
			t.Fatalf("Put of an object past its parts = %v, want Invalid", err)
		}
		if a, _, _ := f.state(); a != 1 {
			t.Errorf("aborted %d, want 1", a)
		}
		t.Logf("refused after %v", time.Since(start))
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// cancelReader cancels a context when first read, and yields nothing.
type cancelReader struct{ cancel context.CancelFunc }

func (c cancelReader) Read([]byte) (int, error) { c.cancel(); return 0, nil }

// TestAbortSurvivesCancel guards that the abort after a canceled Put is sent
// even though the caller's context is done.
func TestAbortSurvivesCancel(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, func(c *Config) { c.PartSize = MinPartSize })
	ctx, cancel := context.WithCancel(context.Background())
	data := blobtest.PatternBytes(1, 2*MinPartSize+1)
	src := io.MultiReader(bytes.NewReader(data[:MinPartSize+10]), cancelReader{cancel}, bytes.NewReader(data[MinPartSize+10:]))
	if _, err := s.Put(ctx, "k", src); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v", err)
	}
	if got := f.count(http.MethodDelete, "uploadId"); got != 1 {
		t.Errorf("the abort was sent %d times after the cancel, want 1", got)
	}
}

// TestPathsSent guards that a key is sent as one object name: its
// characters are the ones a key may have, and the path the server sees is the
// bucket, the storage's prefix and the key, nothing else.
func TestPathsSent(t *testing.T) {
	f := newFakeS3(t)
	s := openFake(t, f, func(c *Config) { c.Prefix = "tenant-1/store" })
	ctx := context.Background()
	if _, err := s.Put(ctx, "data/s1/a.b_c-d/x.dcm", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "data/s1/a.b_c-d/x.dcm"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range f.requests() {
		paths = append(paths, r.method+" "+r.path)
	}
	for _, want := range []string{"PUT /fake-bucket/tenant-1/store/data/s1/a.b_c-d/x.dcm", "HEAD /fake-bucket/tenant-1/store/data/s1/a.b_c-d/x.dcm"} {
		found := false
		for _, p := range paths {
			found = found || p == want
		}
		if !found {
			t.Errorf("no request %q among %q", want, paths)
		}
	}
}
