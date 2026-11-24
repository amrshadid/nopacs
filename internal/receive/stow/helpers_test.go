package stow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ctStudy is the Study Instance UID of the corpus's CT files.
const ctStudy = "1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.1"

// ctFiles are the corpus's files of ctStudy.
var ctFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}

// corpus reads a public test file of the receive package's testdata.
func corpus(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeSink stands for the pipeline: it lands what it is handed as the
// pipeline does, reading to the receiver's largest file and one byte
// more, and keeps each file's bytes.
type fakeSink struct {
	mu     sync.Mutex
	limit  int64
	refuse bool
	got    [][]byte
	told   []receive.Arrival
}

// Accept implements receive.Sink.
func (s *fakeSink) Accept(_ context.Context, a receive.Arrival) receive.Outcome {
	if s.refuse {
		return receive.Outcome{Status: receive.Refused, Reason: "refused"}
	}
	b, err := io.ReadAll(io.LimitReader(a.Body, s.limit+1))
	switch {
	case err != nil:
		return receive.Outcome{Status: receive.Refused, Reason: "not kept"}
	case int64(len(b)) > s.limit:
		return receive.Outcome{Status: receive.Failed, Reason: "too large"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, b)
	a.Body = nil
	s.told = append(s.told, a)
	return receive.Outcome{Status: receive.Received}
}

// stored returns the files kept.
func (s *fakeSink) stored() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.got...)
}

// rig is an endpoint over a fake sink, started in its directory.
type rig struct {
	t    *testing.T
	sink *fakeSink
	dir  *Directory
	ep   *Endpoint
	cfg  Config
}

// newRig starts an endpoint of a stow receiver that takes key-a, with a
// largest file of maxFile and a largest request of maxRequest bytes; change
// edits its config first.
func newRig(t *testing.T, maxFile, maxRequest int64, change func(*Config)) *rig {
	t.Helper()
	sink := &fakeSink{limit: maxFile}
	dir := NewDirectory()
	cfg := Config{
		Tenant: "lab1", Sink: sink, Directory: dir,
		Receiver: tenant.Receiver{Name: "web", Type: tenant.ReceiverSTOW, MaxFileBytes: maxFile,
			STOW: tenant.STOWReceiver{Keys: []string{"key-a"}, MaxRequestBytes: maxRequest}},
	}
	if change != nil {
		change(&cfg)
	}
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Stop(context.Background()) })
	return &rig{t: t, sink: sink, dir: dir, ep: ep, cfg: cfg}
}

// part is one part of a request: its bytes and its Content-Type ("" for
// none).
type part struct {
	contentType string
	body        []byte
}

// dicomParts makes an application/dicom part of each body.
func dicomParts(bodies ...[]byte) []part {
	out := make([]part, len(bodies))
	for i, b := range bodies {
		out[i] = part{contentType: "application/dicom", body: b}
	}
	return out
}

// multipartBody writes parts as a multipart/related body and returns it
// with its Content-Type.
func multipartBody(t *testing.T, parts []part) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(p.body)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), `multipart/related; type="application/dicom"; boundary=` + mw.Boundary()
}

// serve sends a STOW-RS request into study ("" for the root) through Serve
// and returns the recorded answer and the result.
func (r *rig) serve(study string, body []byte, contentType string) (*httptest.ResponseRecorder, Result) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://archive.test/tenants/lab1/dicom-web/studies", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/dicom+json")
	w := httptest.NewRecorder()
	res := r.ep.Serve(context.Background(), w, req, Request{
		Study: study, Base: "http://archive.test/tenants/lab1/dicom-web",
		Caller: tenant.Caller{Address: "192.0.2.7:51234"}, Association: "req-1",
	})
	return w, res
}

// stowAnswer is the answer's DICOM JSON as a client reads it.
type stowAnswer struct {
	studyURL string
	stored   []storedItem
	failed   []failedItem
}

// storedItem and failedItem are items of the answer's two sequences.
type storedItem struct{ class, instance, url string }
type failedItem struct {
	class, instance string
	reason          int
}

// readAnswer reads an answer's DICOM JSON (PS3.18 F.2).
func readAnswer(t *testing.T, body []byte) stowAnswer {
	t.Helper()
	type element struct {
		VR    string            `json:"vr"`
		Value []json.RawMessage `json:"Value"`
	}
	var top map[string]element
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	str := func(e element) string {
		if len(e.Value) == 0 {
			return ""
		}
		var s string
		_ = json.Unmarshal(e.Value[0], &s)
		return s
	}
	items := func(e element) []map[string]element {
		var out []map[string]element
		for _, raw := range e.Value {
			var m map[string]element
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}
	var a stowAnswer
	a.studyURL = str(top["00081190"])
	for _, it := range items(top["00081199"]) {
		a.stored = append(a.stored, storedItem{class: str(it["00081150"]), instance: str(it["00081155"]), url: str(it["00081190"])})
	}
	for _, it := range items(top["00081198"]) {
		var n int
		if v := it["00081197"].Value; len(v) > 0 {
			_ = json.Unmarshal(v[0], &n)
		}
		a.failed = append(a.failed, failedItem{class: str(it["00081150"]), instance: str(it["00081155"]), reason: n})
	}
	return a
}
