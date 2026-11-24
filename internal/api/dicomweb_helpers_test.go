package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// serveWeb starts, over the installation e, the stand-ins of the server's
// pattern routes for the download and for DICOMweb on one server, so a
// URI one answers names a resource of the other, and stops it when the
// test ends.
func serveWeb(t testing.TB, e *env) *dataServer {
	t.Helper()
	return serveWebWith(t, e, nil)
}

// serveWebWith is serveWeb with the server's count of open reads given, as
// the server shares it with its senders; nil gives the handlers their own.
func serveWebWith(t testing.TB, e *env, reads *checkedread.Slots) *dataServer {
	t.Helper()
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.faulty, NewID: e.newID, Settings: e.known, Keys: e.keys, Now: e.now,
		Reads: reads})
	if err != nil {
		t.Fatal(err)
	}
	s := &dataServer{e: e, h: h}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		segs := strings.Split(r.URL.EscapedPath(), "/")
		if len(segs) > 3 && segs[3] == "dicom-web" {
			s.webStandIn(h, w, r)
			return
		}
		s.downloadStandIn(h, w, r)
	}))
	t.Cleanup(hs.Close)
	s.url, s.client = hs.URL, hs.Client()
	return s
}

// webStandIn is the server's pattern route for DICOMweb, as far as the
// handler sees it (internal/server, servePattern): "GET
// /tenants/{tenant}/dicom-web/{rest...}" with HEAD served as GET, the
// tenant's value set only when it is a tenant's name and "rest" the
// segments after the root joined as sent, the requirement and facts from
// api.DICOMwebNeed checked by the real access.Check with the credential
// of the Authorization header, the Decision handed on, and an error
// before the answer began written with the status of its code. What the
// server itself answers (percent-encoding, dot segments) is the server's
// tests'.
func (s *dataServer) webStandIn(h api.Handlers, w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("web-%06d", s.seq.Add(1))
	w.Header().Set(api.RequestIDHeader, id)
	segs := strings.Split(r.URL.EscapedPath(), "/")
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || len(segs) < 5 || segs[1] != "tenants" || segs[3] != "dicom-web" {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("rest", strings.Join(segs[4:], "/"))
	if master.ValidateTenantName(segs[2]) == nil {
		r.SetPathValue("tenant", segs[2])
	}
	need, facts := api.DICOMwebNeed(r)
	facts.Method, facts.RequestID, facts.RemoteAddress = api.DICOMwebPattern, id, "127.0.0.1:1"
	cred := access.APIKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	d, err := s.e.access.Check(r.Context(), cred, need, facts)
	if err == nil {
		err = h.DICOMweb(api.WithDecision(r.Context(), d), w, r)
	}
	var ce *connect.Error
	if err != nil && !errors.As(err, &ce) {
		err = codes.ToConnect(err)
	}
	if err != nil {
		w.WriteHeader(httpStatusOf(err))
	}
}

// webPath is the path of a resource of lab1's DICOMweb root.
func webPath(rest string) string { return "/tenants/lab1/dicom-web/" + rest }

// part is one part of a multipart body.
type part struct {
	contentType string
	body        []byte
}

// partsOf reads a multipart/related answer, checking its type parameter.
func partsOf(t testing.TB, a httpAnswer, wantType string) []part {
	t.Helper()
	media, params, err := mime.ParseMediaType(a.header.Get("Content-Type"))
	if err != nil || media != "multipart/related" || params["type"] != wantType {
		t.Fatalf("Content-Type %q, want multipart/related; type=%q (status %d, body %q)", a.header.Get("Content-Type"), wantType, a.status, clipBody(a.body))
	}
	rd := multipart.NewReader(bytes.NewReader(a.body), params["boundary"])
	var out []part
	for {
		p, err := rd.NextPart()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("the multipart body is not whole: %v", err)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, part{contentType: p.Header.Get("Content-Type"), body: b})
	}
}

// clipBody returns the start of a body, for a message.
func clipBody(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

// results decodes a search's or a metadata answer's DICOM JSON array.
func results(t testing.TB, a httpAnswer) []map[string]map[string]any {
	t.Helper()
	if a.status == http.StatusNoContent {
		return nil
	}
	if a.status != http.StatusOK || a.header.Get("Content-Type") != "application/dicom+json" {
		t.Fatalf("status %d, Content-Type %q, body %q", a.status, a.header.Get("Content-Type"), clipBody(a.body))
	}
	var out []map[string]map[string]any
	if err := json.Unmarshal(a.body, &out); err != nil {
		t.Fatalf("not a DICOM JSON array: %v", err)
	}
	return out
}

// value returns the first value of an attribute of a result as text, or
// "" when it has none.
func value(r map[string]map[string]any, key string) string {
	vs, _ := r[key]["Value"].([]any)
	if len(vs) == 0 {
		return ""
	}
	switch v := vs[0].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	case map[string]any:
		s, _ := v["Alphabetic"].(string)
		return s
	}
	return ""
}

// uids are the UIDs of a file of the corpus.
type uids struct{ study, series, sop string }

// uidsOfFile reads the UIDs of a file.
func uidsOfFile(t testing.TB, b []byte) uids {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := h.UID(header.StudyUID)
	se, _ := h.UID(header.SeriesUID)
	sop, _ := h.UID(header.SOPInstanceUID)
	return uids{st, se, sop}
}
