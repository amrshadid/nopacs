package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// stowSink stands for the tenant's pipeline: it keeps each file whole.
type stowSink struct {
	mu  sync.Mutex
	got [][]byte
}

// Accept implements receive.Sink.
func (s *stowSink) Accept(_ context.Context, a receive.Arrival) receive.Outcome {
	b, err := io.ReadAll(a.Body)
	if err != nil {
		return receive.Outcome{Status: receive.Refused, Reason: "not kept"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, b)
	return receive.Outcome{Status: receive.Received}
}

// stowServer is the STOW-RS route's stand-in over an installation with one
// running stow receiver of lab1.
type stowServer struct {
	e    *env
	url  string
	sink *stowSink
	seq  atomic.Int64
}

// serveSTOW starts the stand-in, the stow receiver taking requests from the
// keys listed.
func serveSTOW(t *testing.T, e *env, keys ...string) *stowServer {
	t.Helper()
	dir := stow.NewDirectory()
	sink := &stowSink{}
	ep, err := stow.New(stow.Config{Tenant: "lab1", Sink: sink, Directory: dir, Receiver: tenant.Receiver{
		Name: "web", Type: tenant.ReceiverSTOW, MaxFileBytes: 1 << 20,
		STOW: tenant.STOWReceiver{Keys: keys, MaxRequestBytes: 1 << 24},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Stop(context.Background()) })
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.faulty, NewID: e.newID, Settings: e.known, Keys: e.keys, Now: e.now, STOW: dir})
	if err != nil {
		t.Fatal(err)
	}
	s := &stowServer{e: e, sink: sink}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.standIn(h, w, r) }))
	t.Cleanup(hs.Close)
	s.url = hs.URL
	return s
}

// standIn is the server's pattern route for STOW-RS as far as the handler
// sees it, as downloadStandIn is the download's: the path's values set,
// api.STOWNeed checked by the real access.Check with the request's key,
// the Decision handed on, an error written with the status of its code.
func (s *stowServer) standIn(h api.Handlers, w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("stow-%06d", s.seq.Add(1))
	w.Header().Set(api.RequestIDHeader, id)
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/tenants/")
	name, rest, ok2 := strings.Cut(rest, "/dicom-web/")
	if r.Method != http.MethodPost || !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("rest", rest)
	if master.ValidateTenantName(name) == nil {
		r.SetPathValue("tenant", name)
	}
	need, facts := api.STOWNeed(r)
	facts.Method, facts.RequestID, facts.RemoteAddress = api.STOWPattern, id, "127.0.0.1:1"
	cred := access.APIKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	d, err := s.e.access.Check(r.Context(), cred, need, facts)
	if err == nil {
		err = h.STOW(api.WithDecision(r.Context(), d), w, r)
	}
	var ce *connect.Error
	if err != nil && !errors.As(err, &ce) {
		err = codes.ToConnect(err)
	}
	if err != nil {
		w.WriteHeader(httpStatusOf(err))
		_, _ = io.WriteString(w, connect.CodeOf(err).String()+": "+errMessage(err)) // #nosec G705 -- a test's stand-in, plain text.
	}
}

// errMessage returns a Connect error's message.
func errMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// post stores the corpus files named into path (after dicom-web/) as c.
func (s *stowServer) post(t *testing.T, c caller, path string, files ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/dicom"}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = pw.Write(b)
	}
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, s.url+"/tenants/lab1/dicom-web/"+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+mw.Boundary())
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// TestSTOWTakesOnlyTheKeysItLists guards the route's rule, closed by
// default: a key listed by the tenant's stow receiver with writer on the
// tenant stores, and the request is audited in the tenant's log with what
// it stored; a key the receiver does not list (an admin of the tenant
// among them), a key it lists that holds only reader, and no key at all
// are refused before any part reaches the pipeline; a path that is not a
// STOW-RS resource is 404.
func TestSTOWTakesOnlyTheKeysItLists(t *testing.T) {
	e := newEnv(t)
	e.tenant("lab1")
	writer, writerID := e.key(e.onTenant("lab1", master.RoleWriter))
	unlisted, _ := e.key(e.onTenant("lab1", master.RoleAdmin))
	reader, readerID := e.key(e.onTenant("lab1", master.RoleReader))
	s := serveSTOW(t, e, writerID, readerID)

	status, body := s.post(t, writer, "studies/1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.1", "ct5n-2062.dcm", "ct5n-2392.dcm")
	if status != http.StatusOK || !strings.Contains(body, `"00081199"`) {
		t.Fatalf("a listed writer: %d %s", status, body)
	}
	if n := len(s.sink.got); n != 2 {
		t.Errorf("the pipeline got %d files, want 2", n)
	}
	var entry *master.AuditEntry
	for _, en := range e.tenantLog("lab1") {
		if en.Method == api.STOWPattern && en.KeyID == writerID {
			entry = &en
		}
	}
	if entry == nil || entry.Outcome != master.OutcomeDone || entry.Details["stored"] != "2" || entry.Details["refused"] != "0" ||
		entry.Details["receiver"] != "web" || entry.Details["resource"] == "" {
		t.Errorf("the request's audit entry: %+v", entry)
	}

	for _, c := range []struct {
		name   string
		who    caller
		path   string
		status int
		text   string
	}{
		{"a writer the receiver does not list", unlisted, "studies", http.StatusForbidden, "no stow receiver of the tenant takes requests from this key"},
		{"a listed key with reader only", reader, "studies", http.StatusForbidden, ""},
		{"no key", nobody, "studies", http.StatusUnauthorized, ""},
		{"not a STOW-RS resource", writer, "studies/1.2.3/series", http.StatusNotFound, ""},
		{"a study that is no UID", writer, "studies/abc", http.StatusNotFound, ""},
	} {
		status, body := s.post(t, c.who, c.path, "ct5n-2062.dcm")
		if status != c.status || !strings.Contains(body, c.text) {
			t.Errorf("%s: %d %q, want %d", c.name, status, body, c.status)
		}
	}
	if n := len(s.sink.got); n != 2 {
		t.Errorf("a refused request reached the pipeline: %d files", n)
	}
}
