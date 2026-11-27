package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// readProxy stands between the command and the fake, as the real server's
// bound of open reads per caller does: it refuses a read of file bytes (a
// ReadFiles stream or a plain GET of an instance) while limit are open, with
// 429 and never queueing it, and it slows every read of the answer by delay,
// so streams overlap and a test can stop a download midway.
type readProxy struct {
	mu                  sync.Mutex
	limit               int
	delay               time.Duration
	open, peak, refused int
	URL                 string
}

// newReadProxy serves a proxy in front of the fake and returns it.
func newReadProxy(t *testing.T, fake *fakeserver.Server, limit int, delay time.Duration) *readProxy {
	t.Helper()
	up, err := url.Parse(fake.ServeHTTP(t))
	if err != nil {
		t.Fatal(err)
	}
	p := &readProxy{limit: limit, delay: delay}
	rp := httputil.NewSingleHostReverseProxy(up)
	rp.FlushInterval = -1
	// A download that ends or is stopped cancels requests in flight; the
	// proxy's log of those is noise.
	rp.ErrorLog = log.New(io.Discard, "", 0)
	rp.ModifyResponse = func(res *http.Response) error {
		res.Body = &slowBody{r: res.Body, delay: p.delay}
		return nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ReadFiles") && !strings.Contains(r.URL.Path, "/instances/") {
			rp.ServeHTTP(w, r)
			return
		}
		if !p.take() {
			refuse(w, r)
			return
		}
		defer p.done()
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// refuse answers a read the bound refuses. A stream is answered as the
// server does when it refuses one after accepting the call: status 200 and
// the error in the stream's end message (the Connect protocol's
// end-of-stream envelope, flag 2, a 4-byte length, a JSON document). A plain
// GET is answered 429 with Retry-After.
func refuse(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/instances/") {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many reads", http.StatusTooManyRequests)
		return
	}
	body := []byte(`{"error":{"code":"resource_exhausted","message":"the caller holds as many streams and downloads open as the server allows; end one first"}}`)
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	head := make([]byte, 5)
	head[0] = 2
	binary.BigEndian.PutUint32(head[1:], uint32(len(body))) //nolint:gosec // G115: a fixed short document.
	_, _ = w.Write(append(head, body...))
}

// take counts one more open read, or says the bound is reached.
func (p *readProxy) take() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.limit > 0 && p.open >= p.limit {
		p.refused++
		return false
	}
	p.open++
	p.peak = max(p.peak, p.open)
	return true
}

// done ends a read.
func (p *readProxy) done() {
	p.mu.Lock()
	p.open--
	p.mu.Unlock()
}

// stats returns the most reads open at once and how many were refused.
func (p *readProxy) stats() (peak, refused int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak, p.refused
}

// slowBody waits before each read of the body it wraps.
type slowBody struct {
	r     io.ReadCloser
	delay time.Duration
}

// Read waits, then reads.
func (b *slowBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay)
	return b.r.Read(p)
}

// Close closes the body.
func (b *slowBody) Close() error { return b.r.Close() }

// env points the commands at the proxy as a person on another machine would
// be: a server address and a key.
func (p *readProxy) env() map[string]string {
	return map[string]string{client.EnvServer: p.URL, client.EnvKey: testKey}
}

// runCtx is runWith with a context the test can end.
func runCtx(ctx context.Context, env map[string]string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, args, cli.Env{
		Stdout:     &stdout,
		Stderr:     &stderr,
		Getenv:     func(k string) string { return env[k] },
		Now:        func() time.Time { return fixedNow },
		Location:   zone,
		WatchPause: time.Millisecond,
	})
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// seedMany puts n scans in lab1, sub-p/ses-1/scan-1 to scan-n, each with per
// instances of size bytes, and returns every instance's bytes by the path a
// download writes it to.
func seedMany(r *rig, n, per, size int) map[string][]byte {
	want := map[string][]byte{}
	for s := 1; s <= n; s++ {
		var insts []*fakeserver.Instance
		for i := 1; i <= per; i++ {
			in := inst(fmt.Sprintf("1.7.%d.%d", s, i), size+i)
			insts = append(insts, in)
			want[fmt.Sprintf("sub-p/ses-1/scan-%d/%s.dcm", s, in.ID)] = in.Data
		}
		r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-p", SessionLabel: "ses-1", Label: fmt.Sprintf("scan-%d", s), InstanceCount: int64(per), Modality: "MR", ArchiveTime: timestamppb.New(fixedNow)}, insts...)
	}
	return want
}

// checkTree compares every file under dir with the bytes it must hold, by
// SHA-256 computed here and again by sha256sum-independent code (the standard
// library), and fails on a missing, extra or different file. extra names
// files that may be there besides.
func checkTree(t *testing.T, dir string, want map[string][]byte, extra ...string) {
	t.Helper()
	got := readTree(t, dir)
	for _, e := range extra {
		delete(got, e)
	}
	for path, data := range want {
		g, ok := got[path]
		if !ok {
			t.Errorf("%s is missing", path)
			continue
		}
		a, b := sha256.Sum256(g), sha256.Sum256(data)
		if a != b {
			t.Errorf("%s has SHA-256 %s, want %s", path, hex.EncodeToString(a[:]), hex.EncodeToString(b[:]))
		}
		delete(got, path)
	}
	if len(got) != 0 {
		t.Errorf("files that should not be there: %v", keysOf(got))
	}
}

// stdoutDoc reads the document a --json command wrote to standard output,
// whatever it wrote to standard error.
func stdoutDoc(t *testing.T, res result) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("exit %d, standard output is not JSON: %v\n%s\nstandard error: %s", res.code, err, res.stdout, res.stderr)
	}
	return doc
}

// jsonLinesOf parses standard error, one JSON document a line.
func jsonLinesOf(t *testing.T, stderr string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a line of progress is not JSON: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

// countDCM counts the finished files under dir.
func countDCM(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".dcm") {
			n++
		}
		return nil
	})
	return n
}
