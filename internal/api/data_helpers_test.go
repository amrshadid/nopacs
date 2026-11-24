package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/blob"
	blobdir "github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// dataServer serves a fixture's handlers to the data tests: ReadFiles
// through the installation's own server and its stand-in for the server's
// path, and the plain HTTP download through a stand-in for the server's
// pattern route (downloadStandIn) on a server of its own.
type dataServer struct {
	e      *env
	url    string
	client *http.Client
	seq    atomic.Int64
	// h is the handlers served, set by serveWeb.
	h api.Handlers
}

// serveData starts the download's stand-in over the installation e, and
// stops it when the test ends.
func serveData(t testing.TB, e *env) *dataServer {
	t.Helper()
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.faulty, NewID: e.newID, Settings: e.known, Keys: e.keys, Now: e.now})
	if err != nil {
		t.Fatal(err)
	}
	s := &dataServer{e: e}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.downloadStandIn(h, w, r) }))
	t.Cleanup(hs.Close)
	s.url, s.client = hs.URL, hs.Client()
	return s
}

// downloadStandIn is the server's pattern route for the download, as far
// as the handler sees it (internal/server, servePattern): the path as sent
// split into its values, the tenant's set only when it is a tenant's name,
// the requirement and facts from api.DownloadNeed checked by the real
// access.Check with the credential of the Authorization header, the
// Decision handed on with api.WithDecision, and an error before the answer
// began written with the status of its code. What the server itself
// answers (percent-encoding, dot segments, the method) is the server's
// tests'.
func (s *dataServer) downloadStandIn(h api.Handlers, w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("dl-%06d", s.seq.Add(1))
	w.Header().Set(api.RequestIDHeader, id)
	segs := strings.Split(r.URL.EscapedPath(), "/")
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || len(segs) != 8 || segs[1] != "tenants" || segs[3] != "instances" {
		http.NotFound(w, r)
		return
	}
	for i, name := range map[int]string{4: "subject", 5: "session", 6: "scan", 7: "instance"} {
		r.SetPathValue(name, segs[i])
	}
	if master.ValidateTenantName(segs[2]) == nil {
		r.SetPathValue("tenant", segs[2])
	}
	need, facts := api.DownloadNeed(r)
	facts.Method, facts.RequestID, facts.RemoteAddress = api.DownloadPattern, id, "127.0.0.1:1"
	cred := access.APIKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	d, err := s.e.access.Check(r.Context(), cred, need, facts)
	if err == nil {
		err = h.Download(api.WithDecision(r.Context(), d), w, r)
	}
	var ce *connect.Error
	if err != nil && !errors.As(err, &ce) {
		err = codes.ToConnect(err)
	}
	if err != nil {
		w.WriteHeader(httpStatusOf(err))
	}
}

// httpStatusOf is the HTTP status the server answers an error's code with
// on a plain route.
func httpStatusOf(err error) int {
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// fileStream is what one ReadFiles answered: its messages in order, the error
// it ended with, and the request id.
type fileStream struct {
	msgs      []*v1.ReadFilesResponse
	err       error
	requestID string
}

// readFiles reads a whole stream of ReadFiles as c.
func (s *dataServer) readFiles(c caller, req *v1.ReadFilesRequest) fileStream {
	ctx, info := connect.NewClientContext(context.Background())
	st, err := s.e.as(c).data.ReadFiles(ctx, req)
	if err != nil {
		return fileStream{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	}
	defer func() { _ = st.Close() }()
	var out fileStream
	for {
		m, err := st.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				out.err = err
			}
			break
		}
		out.msgs = append(out.msgs, m)
	}
	out.requestID = info.ResponseHeader().Get(api.RequestIDHeader)
	return out
}

// streamed is one instance as a stream carried it: its announcement, the
// marker of its scan, and its bytes put together from its chunks.
type streamed struct {
	file   *v1.ArchivedFile
	marker *v1.ScanMarker
	bytes  []byte
	chunks int
}

// assemble checks the shape of a stream (a marker before every instance's
// announcement, each instance's chunks following on from offset 0 with no
// gap, each of 1 to 1 MiB bytes, no chunk before an announcement) and
// returns its instances in order and its markers.
func assemble(t testing.TB, msgs []*v1.ReadFilesResponse) ([]*streamed, []*v1.ScanMarker) {
	t.Helper()
	var (
		out     []*streamed
		markers []*v1.ScanMarker
		cur     *streamed
	)
	for i, m := range msgs {
		switch p := m.GetPart().(type) {
		case *v1.ReadFilesResponse_Scan:
			markers = append(markers, p.Scan)
			cur = nil
		case *v1.ReadFilesResponse_File:
			if len(markers) == 0 {
				t.Fatalf("message %d announces an instance before any scan's marker", i)
			}
			cur = &streamed{file: p.File, marker: markers[len(markers)-1]}
			out = append(out, cur)
		case *v1.ReadFilesResponse_Chunk:
			switch {
			case cur == nil:
				t.Fatalf("message %d is a chunk before any announcement", i)
			case p.Chunk.GetOffset() != int64(len(cur.bytes)):
				t.Fatalf("message %d: a chunk at offset %d follows %d bytes", i, p.Chunk.GetOffset(), len(cur.bytes))
			case len(p.Chunk.GetData()) == 0 || len(p.Chunk.GetData()) > api.ChunkSize:
				t.Fatalf("message %d: a chunk of %d bytes", i, len(p.Chunk.GetData()))
			}
			cur.bytes = append(cur.bytes, p.Chunk.GetData()...)
			cur.chunks++
		default:
			t.Fatalf("message %d carries nothing", i)
		}
	}
	return out, markers
}

// manifestOf reads and decodes a scan's manifest from the archive's
// storage, and returns it by SOP Instance UID with the SHA-256 of its
// bytes.
func manifestOf(t testing.TB, a *archive, p label.Path) (map[string]manifest.File, string) {
	t.Helper()
	key, err := layout.ManifestKey(p)
	if err != nil {
		t.Fatal(err)
	}
	raw := a.object(t, key)
	m, err := manifest.Decode(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]manifest.File{}
	for _, f := range m.Files {
		out[f.SOPInstanceUID] = f
	}
	sum := sha256.Sum256(raw)
	return out, hex.EncodeToString(sum[:])
}

// downloadPath is the plain HTTP path of an instance of lab1.
func downloadPath(p label.Path, id string) string {
	return "/tenants/lab1/instances/" + p.Subject + "/" + p.Session + "/" + p.Scan + "/" + id
}

// httpAnswer is what one plain HTTP request answered: its status, headers and
// body, and the error reading the body ended with (a body cut short of its
// length).
type httpAnswer struct {
	status  int
	header  http.Header
	body    []byte
	readErr error
}

// get makes one plain HTTP request as c, with the headers given in pairs.
func (s *dataServer) get(t testing.TB, method, path string, c caller, headers ...string) httpAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, rerr := io.ReadAll(res.Body)
	return httpAnswer{status: res.StatusCode, header: res.Header, body: body, readErr: rerr}
}

// tagOf is the strong entity tag of bytes: their SHA-256 in quotes.
func tagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// receiveInto makes lab1 of the installation e receive and archive files
// through the reception pipeline, to its directory storage at dir (which
// e's lab1 must have as its storage "main", with a receiver "scanner1"),
// and waits until every one is held. It is receiveArchive for a benchmark,
// whose fixture is the installation alone.
func receiveInto(tb testing.TB, e *env, dir string, files [][]byte) tenant.Pipeline {
	tb.Helper()
	store, err := blobdir.Open(dir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	p := e.pipelineOf("lab1")
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		tb.Fatal(err)
	}
	wait := newSettle()
	pipe, err := receive.New(receive.Config{
		Tenant: "lab1", Store: p, Hasher: hasher, Clock: wait.clock, Wake: wait.Wake, IncomingRoot: filepath.Join(e.dir, "incoming"),
		OpenStorage: func(context.Context, string) (blob.Store, error) { return store, nil },
		Lease:       10 * time.Minute,
	})
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := pipe.Start(context.Background()); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = pipe.Stop(context.Background()) })
	for _, b := range files {
		out := pipe.Accept(context.Background(), receive.Arrival{Receiver: "scanner1", Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"}})
		if out.Status != receive.Received {
			tb.Fatalf("a file was %v (%s), want received", out.Status, out.Reason)
		}
	}
	wait.await(tb, pipe, p, len(files))
	return p
}

// readFilesOnce opens a stream of ReadFiles with req and returns its first
// message or its error as an outcome, then ends the stream: what a table
// of every method needs of it.
func readFilesOnce(c clients, req *v1.ReadFilesRequest) outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	s, err := c.data.ReadFiles(ctx, req)
	if err != nil {
		return outcome{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	}
	defer func() { _ = s.Close() }()
	msg, err := s.Receive()
	o := outcome{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	if err == nil {
		o.res = msg
	}
	return o
}
