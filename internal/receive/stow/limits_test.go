package stow

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// emptyParts returns a multipart body of n parts with no header and no
// bytes, the least a client can send per part, and its Content-Type.
func emptyParts(n int) ([]byte, string) {
	var b strings.Builder
	for range n {
		b.WriteString("--B\r\n\r\n\r\n")
	}
	b.WriteString("--B--\r\n")
	return []byte(b.String()), `multipart/related; type="application/dicom"; boundary=B`
}

// TestServeBoundsPartsPerRequest guards the bound on parts: a request of
// MaxParts parts is answered part by part, and one of a part more is
// refused 413 at that part with the parts before it counted.
func TestServeBoundsPartsPerRequest(t *testing.T) {
	r := newRig(t, 1<<20, 1<<30, nil)
	body, ct := emptyParts(MaxParts)
	if w, res := r.serve("", body, ct); w.Code != http.StatusConflict || res.Refused != MaxParts {
		t.Errorf("%d parts: %d %+v", MaxParts, w.Code, res)
	}
	body, ct = emptyParts(MaxParts + 1)
	w, res := r.serve("", body, ct)
	if w.Code != http.StatusRequestEntityTooLarge || res.Refused != MaxParts || res.Reason != phraseTooMany {
		t.Errorf("%d parts: %d %+v", MaxParts+1, w.Code, res)
	}
}

// TestServeMemoryPerPartIsSmall guards what a request of many parts
// allocates: the bytes a part's identity is read into are one buffer for
// the request, not one per part, so MaxParts empty parts allocate a few
// MiB, not MaxParts times 64 KiB.
func TestServeMemoryPerPartIsSmall(t *testing.T) {
	r := newRig(t, 1<<20, 1<<30, nil)
	body, ct := emptyParts(MaxParts)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w, res := r.serve("", body, ct)
	runtime.ReadMemStats(&after)
	if w.Code != http.StatusConflict || res.Refused != MaxParts {
		t.Fatalf("answer %d %+v", w.Code, res)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<20 {
		t.Errorf("%d empty parts allocated %d MiB", MaxParts, got>>20)
	}
}

// TestServeBoundsAPartHeader guards the bound on a part's header: a part
// whose header is past MaxPartHeader is refused 400 and nothing of it is
// kept, whether far past (9 MiB, under the multipart reader's own bound of
// 10 MiB) or just past; a header just under the bound is read and its part
// stored.
func TestServeBoundsAPartHeader(t *testing.T) {
	r := newRig(t, 1<<20, 1<<30, nil)
	file := corpus(t, "ct5n-2062.dcm")
	for _, c := range []struct {
		name  string
		extra int
		want  int
	}{
		{"9 MiB", 9 << 20, http.StatusBadRequest},
		{"just past", MaxPartHeader + 100, http.StatusBadRequest},
		{"just under", MaxPartHeader - 200, http.StatusOK},
	} {
		body := append([]byte("--B\r\nX-Padding: "+strings.Repeat("a", c.extra)+"\r\nContent-Type: application/dicom\r\n\r\n"), file...)
		body = append(body, "\r\n--B--\r\n"...)
		before := len(r.sink.stored())
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		w, res := r.serve("", body, `multipart/related; type="application/dicom"; boundary=B`)
		runtime.ReadMemStats(&m1)
		if w.Code != c.want {
			t.Errorf("%s: %d %+v", c.name, w.Code, res)
		}
		// A header is refused once it has taken its bound, not after it
		// was read whole.
		if got := m1.TotalAlloc - m0.TotalAlloc; got > 2<<20 {
			t.Errorf("%s: the request allocated %d KiB", c.name, got>>10)
		}
		kept := len(r.sink.stored()) - before
		if (c.want == http.StatusOK) != (kept == 1) {
			t.Errorf("%s: %d files kept", c.name, kept)
		}
	}
}

// TestServeKeepsNothingOfACutPart guards a body that ends inside a part,
// with no closing boundary: the part is not taken for whole, so it is not
// kept and the request is answered 400; a part whole before it is kept and
// counted.
func TestServeKeepsNothingOfACutPart(t *testing.T) {
	f := corpus(t, ctFiles[0])
	const ct = `multipart/related; type="application/dicom"; boundary=B`
	start := []byte("--B\r\nContent-Type: application/dicom\r\n\r\n")
	for _, cut := range []int{len(f) / 2, len(f)} {
		r := newRig(t, 1<<20, 1<<24, nil)
		body := append(append([]byte{}, start...), f[:cut]...)
		w, res := r.serve("", body, ct)
		if w.Code != http.StatusBadRequest || res.Stored != 0 || res.Reason != phraseCutPart || len(r.sink.stored()) != 0 {
			t.Errorf("a part cut at %d of %d bytes: %d %+v, %d kept", cut, len(f), w.Code, res, len(r.sink.stored()))
		}
	}
	r := newRig(t, 1<<20, 1<<24, nil)
	body := append(append([]byte{}, start...), f...)
	body = append(body, "\r\n--B\r\n\r\n"...)
	body = append(body, f[:100]...)
	w, res := r.serve("", body, ct)
	if w.Code != http.StatusBadRequest || res.Stored != 1 || len(r.sink.stored()) != 1 || !bytes.Equal(r.sink.stored()[0], f) {
		t.Errorf("a whole part, then one cut: %d %+v, %d kept", w.Code, res, len(r.sink.stored()))
	}
}

// TestACutBodyReadsNothingMore guards the reads of a body once Stop has
// cut its request, whatever the client still sends: each fails at once,
// neither moving the read deadline (which would undo the cut's) nor
// reading the connection; and a read after the request's context ended
// fails the same way.
func TestACutBodyReadsNothingMore(t *testing.T) {
	var cut atomic.Bool
	extended, src := 0, strings.NewReader(strings.Repeat("x", 1<<20))
	s := &stallReader{r: src, ctx: context.Background(), cut: &cut, extend: func() { extended++ }}
	p := make([]byte, 1024)
	if n, err := s.Read(p); n != len(p) || err != nil || extended != 1 {
		t.Fatalf("a read before the cut: %d %v, %d extensions", n, err, extended)
	}
	cut.Store(true)
	for range 3 {
		if n, err := s.Read(p); n != 0 || !errors.Is(err, errCut) {
			t.Fatalf("a read after the cut: %d %v", n, err)
		}
	}
	if extended != 1 || src.Len() != 1<<20-len(p) {
		t.Errorf("after the cut: %d extensions, %d bytes read", extended, 1<<20-src.Len())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = &stallReader{r: src, ctx: ctx, cut: new(atomic.Bool), extend: func() { extended++ }}
	if n, err := s.Read(p); n != 0 || err == nil || extended != 1 {
		t.Errorf("a read after the context ended: %d %v, %d extensions", n, err, extended)
	}
}

// streamingClient sends a STOW-RS request over a raw connection, chunked:
// a first part's header of the given media type, then data until stop is
// closed, every pause. It returns the connection, whose answer the test
// reads.
func streamingClient(t *testing.T, addr, mediaType string, pause time.Duration, stop <-chan struct{}) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	head := "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/related; type=\"application/dicom\"; boundary=B\r\nTransfer-Encoding: chunked\r\n\r\n"
	first := "--B\r\nContent-Type: " + mediaType + "\r\n\r\n"
	if _, err := fmt.Fprintf(conn, "%s%x\r\n%s\r\n", head, len(first), first); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 32<<10)
	frame := fmt.Sprintf("%x\r\n%s\r\n", len(chunk), chunk)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(pause):
			}
			if _, err := io.WriteString(conn, frame); err != nil {
				return
			}
		}
	}()
	return conn
}

// readAnswerHead reads the status and Retry-After of the answer on conn.
func readAnswerHead(t *testing.T, conn net.Conn) (int, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// TestStopCutsAClientStillSending guards Stop against a client that keeps
// sending a part that is being refused (not DICOM, or past the largest
// file) or read: it returns within the drain bound and a little more,
// however long the client would go on, and the request is answered 503
// with Retry-After, nothing of the part kept; and a client that then stops
// sending but keeps its connection open does not hold the connection: what
// net/http reads of the body after the answer ends at once.
func TestStopCutsAClientStillSending(t *testing.T) {
	for _, mediaType := range []string{"text/plain", "application/dicom"} {
		t.Run(strings.ReplaceAll(mediaType, "/", "-"), func(t *testing.T) {
			const drain = 100 * time.Millisecond
			r := newRig(t, 1024, 1<<40, func(c *Config) { c.Drain = drain; c.ReadStall = 2 * time.Second })
			results := make(chan Result, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				results <- r.ep.Serve(req.Context(), w, req, Request{Base: "http://x"})
			}))
			stop := make(chan struct{})
			var stopOnce sync.Once
			stopClient := func() { stopOnce.Do(func() { close(stop) }) }
			defer stopClient()
			conn := streamingClient(t, srv.Listener.Addr().String(), mediaType, 200*time.Microsecond, stop)
			waitInFlight(t, r.ep)
			start := time.Now()
			stopped := make(chan struct{})
			go func() { _ = r.ep.Stop(context.Background()); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(drain + 2*time.Second):
				t.Fatal("Stop still waits while the client sends")
			}
			if d := time.Since(start); d < drain {
				t.Errorf("Stop returned after %v, before the drain bound", d)
			}
			// The client goes quiet with its connection open, before what
			// is left of its body could be read.
			stopClient()
			res := within(t, results)
			status, retry := readAnswerHead(t, conn)
			if res.Status != http.StatusServiceUnavailable || status != http.StatusServiceUnavailable || retry == "" || res.Stored != 0 {
				t.Errorf("the request cut: %+v, answered %d with Retry-After %q", res, status, retry)
			}
			if n := len(r.sink.stored()); n != 0 {
				t.Errorf("%d files kept", n)
			}
			// The server's connection must still end, or httptest's Close
			// waits on it.
			closed := make(chan struct{})
			go func() { srv.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("a quiet client that keeps its connection open holds the cut request's connection")
			}
		})
	}
}

// TestStopCutsAnIdleClient503 guards the answer of a request Stop cuts
// while its client sends nothing: the cut ends the read with a deadline,
// and the request is answered 503 with Retry-After as the receiver
// stops, not 408 as for a client that stalled.
func TestStopCutsAnIdleClient503(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.Drain = 100 * time.Millisecond; c.ReadStall = time.Minute })
	results := make(chan Result, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		results <- r.ep.Serve(req.Context(), w, req, Request{Base: "http://x"})
	}))
	defer srv.Close()
	body, ct := multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm")))
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: " + ct + "\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	waitInFlight(t, r.ep)
	if err := r.ep.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := <-results
	status, retry := readAnswerHead(t, conn)
	if res.Status != http.StatusServiceUnavailable || status != http.StatusServiceUnavailable || retry == "" || res.Stored != 0 {
		t.Errorf("an idle request cut: %+v, answered %d with Retry-After %q", res, status, retry)
	}
}

// onRead runs fn at the first read and then reads as if empty: it marks
// the moment in a body where something happens to its request.
type onRead struct {
	fn   func()
	done bool
}

// Read implements io.Reader.
func (o *onRead) Read([]byte) (int, error) {
	if !o.done {
		o.done = true
		o.fn()
	}
	return 0, io.EOF
}

// TestARefusedCallerEndsTheRequestUnanswered guards a request whose
// caller is refused while it is open (Request.Refused): the part stored
// before is kept and counted, the request ends at its next read of the
// body with nothing of the parts after it kept, and Serve writes no
// answer, leaving the refusal to the route, rather than answering as the
// receiver stops.
func TestARefusedCallerEndsTheRequestUnanswered(t *testing.T) {
	f := corpus(t, ctFiles[0])
	const ct = `multipart/related; type="application/dicom"; boundary=B`
	head := "--B\r\nContent-Type: application/dicom\r\n\r\n"
	first := append(append([]byte(head), f...), "\r\n"+head...)
	rest := append(append(append([]byte{}, corpus(t, ctFiles[1])...), "\r\n"+head...), corpus(t, ctFiles[2])...)
	rest = append(rest, "\r\n--B--\r\n"...)

	r := newRig(t, 1<<20, 1<<24, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var refused atomic.Bool
	body := io.MultiReader(bytes.NewReader(first), &onRead{fn: func() { refused.Store(true); cancel() }}, bytes.NewReader(rest))
	req := httptest.NewRequest(http.MethodPost, "http://archive.test/tenants/lab1/dicom-web/studies", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", "application/dicom+json")
	w := httptest.NewRecorder()
	res := r.ep.Serve(ctx, w, req, Request{Base: "http://archive.test/tenants/lab1/dicom-web", Refused: refused.Load})
	if !res.Unanswered || res.Stored != 1 || res.Refused != 0 {
		t.Errorf("a caller refused after the first part: %+v, want unanswered with 1 stored", res)
	}
	if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Errorf("Serve wrote an answer for a refused caller: %d %q", w.Code, w.Body.String())
	}
	if got := r.sink.stored(); len(got) != 1 || !bytes.Equal(got[0], f) {
		t.Errorf("%d parts kept, want the first only", len(got))
	}
}

// TestARefusedBodyDoesNotHoldItsConnection guards the connection of a
// request refused part way through its body while the client, which sent
// a part header past MaxPartHeader in a chunked body, then sends nothing
// more and keeps the connection open: net/http reads what is left of a
// body after the answer, and that read must end at once, so the connection
// ends and the server can close.
func TestARefusedBodyDoesNotHoldItsConnection(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.ReadStall = time.Minute })
	results := make(chan Result, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		results <- r.ep.Serve(req.Context(), w, req, Request{Base: "http://x"})
	}))
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/related; type=\"application/dicom\"; boundary=B\r\nTransfer-Encoding: chunked\r\n\r\n"
	part := "--B\r\nContent-Type: application/dicom\r\nX-Filler: " + strings.Repeat("x", MaxPartHeader+headerSlack+1024) + "\r\n\r\n"
	if _, err := fmt.Fprintf(conn, "%s%x\r\n%s\r\n", head, len(part), part); err != nil {
		t.Fatal(err)
	}
	res := within(t, results)
	status, _ := readAnswerHead(t, conn)
	if res.Status != http.StatusBadRequest || status != http.StatusBadRequest {
		t.Errorf("a part header too large: %+v, answered %d", res, status)
	}
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("a quiet client that keeps its connection open holds the refused request's connection")
	}
}

// within receives the request's Result, failing the test when Serve has
// not returned within 5 seconds, so a hang fails at once.
func within(t *testing.T, results <-chan Result) Result {
	t.Helper()
	select {
	case res := <-results:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s")
		return Result{}
	}
}

// waitInFlight waits until the endpoint serves one request, so a Stop that
// follows meets it in flight however slow the machine: a fixed sleep let a
// loaded machine reach the Stop first, which then had nothing to wait for.
func waitInFlight(t *testing.T, e *Endpoint) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		e.mu.Lock()
		n := len(e.active)
		e.mu.Unlock()
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests in flight, want 1", n)
		}
	}
}
