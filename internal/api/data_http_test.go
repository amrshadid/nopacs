package api_test

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// downloadFixture is lab1 after reception of the data tests' files,
// served through the download's stand-in, and its instance of 2.5 MiB.
type downloadFixture struct {
	f    *placeFixture
	a    *archive
	s    *dataServer
	path label.Path
	in   tenant.ScanInstance
	file []byte
}

// newDownloadFixture receives the data tests' files and finds the large
// instance.
func newDownloadFixture(t *testing.T) *downloadFixture {
	t.Helper()
	f := newPlaceFixture(t)
	big := largeFile(t, 5<<19, '5')
	a := receiveArchive(t, f, [][]byte{big, corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct2n-6293.dcm")})
	d := &downloadFixture{f: f, a: a, s: serveData(t, f.env), file: big}
	for _, p := range a.scans(t) {
		for _, h := range a.held(t, p) {
			if h.Size == int64(len(big)) {
				d.path, d.in = p, h
			}
		}
	}
	if d.in.Key == "" {
		t.Fatal("the large instance is not held")
	}
	return d
}

// url is the large instance's download path, with a query when given.
func (d *downloadFixture) url(query string) string {
	u := downloadPath(d.path, d.in.SOPInstanceUID)
	if query != "" {
		u += "?" + query
	}
	return u
}

// TestDownloadServesTheArchivedBytes guards the plain HTTP download of an
// archive made by reception, compared with the bytes received: the whole
// instance with its headers (DICOM content type, exact length, byte
// ranges accepted, the strong ETag of its SHA-256); each form of Range
// answered 206 with its Content-Range; a range past the end and a suffix
// of zero answered 416 with the length; a Range of two ranges, or of
// another unit, ignored; If-Range with another or a weak tag answered
// whole; If-None-Match answered 304; HEAD answering GET's status and
// headers without a body; a window of the query and a Range within it; and
// a download resumed in two ranges put together equal to the file.
func TestDownloadServesTheArchivedBytes(t *testing.T) {
	d := newDownloadFixture(t)
	s, c, file := d.s, d.f.lab1Reader, d.file
	size := int64(len(file))
	tag := tagOf(file)

	whole := s.get(t, http.MethodGet, d.url(""), c)
	h := whole.header
	if whole.status != http.StatusOK || whole.readErr != nil || !bytes.Equal(whole.body, file) {
		t.Fatalf("the whole instance: %d, %d bytes, %v", whole.status, len(whole.body), whole.readErr)
	}
	if h.Get("Content-Type") != "application/dicom" || h.Get("Content-Length") != strconv.FormatInt(size, 10) ||
		h.Get("Accept-Ranges") != "bytes" || h.Get("ETag") != tag || h.Get(api.RequestIDHeader) == "" {
		t.Errorf("the headers of the whole instance: %v", h)
	}

	for _, r := range []struct {
		header      string
		first, last int64
	}{
		{"bytes=0-99", 0, 99},
		{"bytes=1000-", 1000, size - 1},
		{"bytes=-500", size - 500, size - 1},
		{"bytes=5-99999999999", 5, size - 1},
		{"bytes=-99999999999", 0, size - 1},
		{"BYTES=7-7", 7, 7},
		{"bytes=" + strconv.FormatInt(size-1, 10) + "-", size - 1, size - 1},
	} {
		got := s.get(t, http.MethodGet, d.url(""), c, "Range", r.header)
		want := "bytes " + strconv.FormatInt(r.first, 10) + "-" + strconv.FormatInt(r.last, 10) + "/" + strconv.FormatInt(size, 10)
		if got.status != http.StatusPartialContent || got.header.Get("Content-Range") != want || !bytes.Equal(got.body, file[r.first:r.last+1]) ||
			got.header.Get("ETag") != tag || got.header.Get("Content-Length") != strconv.FormatInt(r.last-r.first+1, 10) {
			t.Errorf("%s: %d %q, %d bytes", r.header, got.status, got.header.Get("Content-Range"), len(got.body))
		}
	}
	for _, r := range []string{"bytes=" + strconv.FormatInt(size, 10) + "-", "bytes=-0", "bytes=99999999999999999999-"} {
		got := s.get(t, http.MethodGet, d.url(""), c, "Range", r)
		if got.status != http.StatusRequestedRangeNotSatisfiable || got.header.Get("Content-Range") != "bytes */"+strconv.FormatInt(size, 10) || len(got.body) != 0 {
			t.Errorf("%s: %d %q, want 416 with the length", r, got.status, got.header.Get("Content-Range"))
		}
	}
	for _, r := range []string{"bytes=0-1,5-6", "items=0-10", "bytes=10-5", "bytes=x-", "bytes=-"} {
		got := s.get(t, http.MethodGet, d.url(""), c, "Range", r)
		if got.status != http.StatusOK || !bytes.Equal(got.body, file) {
			t.Errorf("%s: %d, %d bytes, want the whole instance", r, got.status, len(got.body))
		}
	}

	// If-Range: the range only while the instance is the one the tag names.
	for _, c2 := range []struct {
		ifRange string
		status  int
	}{
		{tag, http.StatusPartialContent},
		{`"` + strings.Repeat("0", 64) + `"`, http.StatusOK},
		{"W/" + tag, http.StatusOK},
		{"Fri, 20 Nov 2026 09:00:00 GMT", http.StatusOK},
	} {
		got := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes=100-", "If-Range", c2.ifRange)
		want := file
		if c2.status == http.StatusPartialContent {
			want = file[100:]
		}
		if got.status != c2.status || !bytes.Equal(got.body, want) {
			t.Errorf("If-Range %s: %d, %d bytes, want %d", c2.ifRange, got.status, len(got.body), c2.status)
		}
	}
	for _, v := range []string{tag, "W/" + tag, `"x", ` + tag, "*"} {
		got := s.get(t, http.MethodGet, d.url(""), c, "If-None-Match", v)
		if got.status != http.StatusNotModified || got.header.Get("ETag") != tag || len(got.body) != 0 {
			t.Errorf("If-None-Match %s: %d", v, got.status)
		}
	}
	if got := s.get(t, http.MethodGet, d.url(""), c, "If-None-Match", `"other"`); got.status != http.StatusOK {
		t.Errorf("If-None-Match of another tag: %d, want 200", got.status)
	}

	head := s.get(t, http.MethodHead, d.url(""), c)
	if head.status != http.StatusOK || len(head.body) != 0 || head.header.Get("Content-Length") != strconv.FormatInt(size, 10) || head.header.Get("ETag") != tag {
		t.Errorf("HEAD: %d %v, %d bytes", head.status, head.header, len(head.body))
	}
	head = s.get(t, http.MethodHead, d.url(""), c, "Range", "bytes=10-19")
	if head.status != http.StatusPartialContent || head.header.Get("Content-Range") != "bytes 10-19/"+strconv.FormatInt(size, 10) || len(head.body) != 0 {
		t.Errorf("HEAD with a range: %d %v", head.status, head.header)
	}

	// A window of the query is the body; a Range is counted within it.
	win := s.get(t, http.MethodGet, d.url("offset=132&length=1000"), c)
	if win.status != http.StatusOK || !bytes.Equal(win.body, file[132:1132]) || win.header.Get("Content-Type") != "application/octet-stream" || win.header.Get("ETag") != tag {
		t.Errorf("a window: %d %v, %d bytes", win.status, win.header, len(win.body))
	}
	win = s.get(t, http.MethodGet, d.url("offset=132&length=1000"), c, "Range", "bytes=10-19")
	if win.status != http.StatusPartialContent || !bytes.Equal(win.body, file[142:152]) || win.header.Get("Content-Range") != "bytes 10-19/1000" {
		t.Errorf("a range in a window: %d %q", win.status, win.header.Get("Content-Range"))
	}
	end := strconv.FormatInt(size, 10)
	if got := s.get(t, http.MethodGet, d.url("offset="+end+"&length=1"), c); got.status != http.StatusRequestedRangeNotSatisfiable || got.header.Get("Content-Range") != "bytes */"+end {
		t.Errorf("a window past the end: %d %q", got.status, got.header.Get("Content-Range"))
	}
	if got := s.get(t, http.MethodGet, d.url("offset=0&length="+end), c); got.status != http.StatusOK || !bytes.Equal(got.body, file) {
		t.Errorf("a window of the whole instance: %d", got.status)
	}
	for _, q := range []string{"offset=01&length=5", "offset=0", "length=5", "offset=0&length=0", "offset=0&length=5&x=1",
		"offset=0&length=5&offset=0", "offset=-1&length=5", "offset=0&length=%35", "offset=9223372036854775807&length=1"} {
		if got := s.get(t, http.MethodGet, d.url(q), c); got.status != http.StatusBadRequest {
			t.Errorf("query %q: %d, want 400", q, got.status)
		}
	}

	// A download resumed where it stopped, with the tag it began with.
	cut := size / 3
	part1 := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes=0-"+strconv.FormatInt(cut-1, 10))
	part2 := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes="+strconv.FormatInt(cut, 10)+"-", "If-Range", part1.header.Get("ETag"))
	joined := append(slices.Clone(part1.body), part2.body...)
	if part1.status != http.StatusPartialContent || part2.status != http.StatusPartialContent || tagOf(joined) != tag || !bytes.Equal(joined, file) {
		t.Errorf("a resumed download: %d and %d, %d bytes", part1.status, part2.status, len(joined))
	}

	// Every instance of the archive, whole, is the file received.
	for _, p := range d.a.scans(t) {
		for _, in := range d.a.held(t, p) {
			got := s.get(t, http.MethodGet, downloadPath(p, in.SOPInstanceUID), c)
			if got.status != http.StatusOK || !bytes.Equal(got.body, d.a.sent[in.SOPInstanceUID]) {
				t.Errorf("%v: %d, %d bytes", p, got.status, len(got.body))
			}
		}
	}
}

// TestDownloadRefusesAndAudits guards what a download does not answer, as
// on every route: no key 401, no grant or a grant on another tenant 403, a
// tenant that does not exist 403 (404 to a caller who may list every
// tenant), and after the check a path that names nothing the tenant holds
// 404, never a redirect; and that each
// download is audited once in the tenant's log with the route as its
// method, the scan's path as its object, whether a range and a window were
// asked for and the bytes sent, and never the instance id.
func TestDownloadRefusesAndAudits(t *testing.T) {
	d := newDownloadFixture(t)
	s, f := d.s, d.f
	id := d.in.SOPInstanceUID
	other := strings.Replace(d.url(""), "/lab1/", "/lab2/", 1)
	nolab := strings.Replace(d.url(""), "/lab1/", "/nolab/", 1)
	for _, c := range []struct {
		name   string
		as     caller
		path   string
		status int
	}{
		{"no key", nobody, d.url(""), http.StatusUnauthorized},
		{"no grant", f.noGrant, d.url(""), http.StatusForbidden},
		{"a grant on another tenant", f.lab2Admin, d.url(""), http.StatusForbidden},
		{"a reader of lab1 into lab2", f.lab1Reader, other, http.StatusForbidden},
		{"a tenant that does not exist", f.lab1Reader, nolab, http.StatusForbidden},
		{"a tenant that does not exist, to an installation admin", f.instAdmin, nolab, http.StatusNotFound},
		{"an instance not held", f.lab1Reader, downloadPath(d.path, "1.2.3.4.5"), http.StatusNotFound},
		{"an instance of lab1 asked of lab2 by its admin", f.lab2Admin, other, http.StatusNotFound},
		{"a scan not held", f.lab1Reader, downloadPath(label.Path{Subject: d.path.Subject, Session: d.path.Session, Scan: "nothere"}, id), http.StatusNotFound},
		{"a label in another case", f.lab1Reader, downloadPath(label.Path{Subject: strings.ToUpper(d.path.Subject), Session: d.path.Session, Scan: d.path.Scan}, id), http.StatusNotFound},
		{"an id that is not a UID", f.lab1Reader, downloadPath(d.path, "abc"), http.StatusNotFound},
	} {
		got := s.get(t, http.MethodGet, c.path, c.as)
		if got.status != c.status || bytes.Contains(got.body, d.file[:200]) {
			t.Errorf("%s: %d, want %d", c.name, got.status, c.status)
		}
		if got.header.Get("Location") != "" {
			t.Errorf("%s: redirected to %s", c.name, got.header.Get("Location"))
		}
	}

	got := s.get(t, http.MethodGet, d.url("offset=132&length=1000"), f.lab1Reader, "Range", "bytes=0-9")
	if got.status != http.StatusPartialContent {
		t.Fatalf("a range in a window: %d", got.status)
	}
	whole := s.get(t, http.MethodGet, d.url(""), f.lab1Reader)
	log := f.tenantLog("lab1")
	object := d.path.Subject + "/" + d.path.Session + "/" + d.path.Scan
	for _, c := range []struct {
		requestID   string
		ranged, win string
		bytes       int
	}{
		{got.header.Get(api.RequestIDHeader), "true", "true", 10},
		{whole.header.Get(api.RequestIDHeader), "false", "false", len(d.file)},
	} {
		en := entriesOf(log, c.requestID)
		if len(en) != 1 {
			t.Errorf("%d entries with request id %s, want 1", len(en), c.requestID)
			continue
		}
		e := en[0]
		if e.Method != api.DownloadPattern || e.Object != object || e.Outcome != master.OutcomeDone || e.TenantName != "lab1" ||
			e.Details["range"] != c.ranged || e.Details["window"] != c.win || e.Details["instance"] != "present" || e.Details["bytes"] != strconv.Itoa(c.bytes) {
			t.Errorf("the download's entry is %+v", e)
		}
	}
	for _, e := range append(log, f.masterLog()...) {
		text := e.Method + e.Object + e.Reason
		for _, v := range e.Details {
			text += v
		}
		if strings.Contains(text, id) {
			t.Errorf("an entry holds the instance id: %+v", e)
		}
	}
}

// TestDownloadDataLoss guards that a download never passes on as whole an
// instance its storage does not hold as archived: a stored file whose
// last byte was changed is cut before its last byte, so the body is short
// of its Content-Length, and a small one is answered 500 before any byte;
// one missing from its storage is answered 500, on GET and on HEAD; a file
// cut short is cut in a range too; and each is audited as failed.
func TestDownloadDataLoss(t *testing.T) {
	d := newDownloadFixture(t)
	s, c := d.s, d.f.lab1Reader
	ctx := context.Background()
	changed := slices.Clone(d.file)
	changed[len(changed)-1] ^= 0xFF
	if _, err := d.a.store.Put(ctx, d.in.Key, bytes.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	got := s.get(t, http.MethodGet, d.url(""), c)
	if got.status != http.StatusOK || got.readErr == nil || len(got.body) >= len(d.file) {
		t.Fatalf("a changed file: %d, %d of %d bytes, read error %v; want a body cut short", got.status, len(got.body), len(d.file), got.readErr)
	}
	if !bytes.Equal(got.body, d.file[:len(got.body)]) {
		t.Error("a changed file: the bytes before the cut are not the archived ones")
	}
	// A range is not checked against the SHA-256: the client checks what it
	// assembles against the ETag, which is the archived bytes'.
	tail := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes=-1")
	if tail.status != http.StatusPartialContent || tail.header.Get("ETag") != tagOf(d.file) || !bytes.Equal(tail.body, changed[len(changed)-1:]) {
		t.Errorf("a range of a changed file: %d", tail.status)
	}

	// An instance that fits one piece is checked whole before its status.
	mr := corpusFile(t, "MR_small.dcm")
	for _, h := range d.a.held(t, d.path) {
		if h.Size != int64(len(mr)) {
			continue
		}
		bad := slices.Clone(mr)
		bad[len(bad)-1] ^= 0xFF
		if _, err := d.a.store.Put(ctx, h.Key, bytes.NewReader(bad)); err != nil {
			t.Fatal(err)
		}
		if got := s.get(t, http.MethodGet, downloadPath(d.path, h.SOPInstanceUID), c); got.status != http.StatusInternalServerError || bytes.Contains(got.body, mr[:200]) {
			t.Errorf("a small changed file: %d, want 500", got.status)
		}
	}

	// Cut short: a range whose first piece is short is answered 500; one
	// that reaches past the bytes left after its first piece is cut.
	if _, err := d.a.store.Put(ctx, d.in.Key, bytes.NewReader(d.file[:400<<10])); err != nil {
		t.Fatal(err)
	}
	if small := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes=0-999999"); small.status != http.StatusPartialContent ||
		small.readErr == nil || len(small.body) >= 400<<10 || !bytes.Equal(small.body, d.file[:len(small.body)]) {
		t.Errorf("a long range of a file cut short: %d, %d bytes, %v", small.status, len(small.body), small.readErr)
	}
	if _, err := d.a.store.Put(ctx, d.in.Key, bytes.NewReader(d.file[:1000])); err != nil {
		t.Fatal(err)
	}
	if short := s.get(t, http.MethodGet, d.url(""), c, "Range", "bytes=500-1999"); short.status != http.StatusInternalServerError {
		t.Errorf("a short range of a file cut short: %d, want 500", short.status)
	}
	if head := s.get(t, http.MethodHead, d.url(""), c); head.status != http.StatusInternalServerError {
		t.Errorf("HEAD of a file cut short: %d, want 500", head.status)
	}

	if err := d.a.store.Delete(ctx, d.in.Key); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		gone := s.get(t, m, d.url(""), c)
		if gone.status != http.StatusInternalServerError || bytes.Contains(gone.body, d.file[:200]) {
			t.Errorf("%s of a missing file: %d, want 500", m, gone.status)
		}
		en := entriesOf(d.f.tenantLog("lab1"), gone.header.Get(api.RequestIDHeader))
		if len(en) != 1 || en[0].Outcome != master.OutcomeFailed || !strings.Contains(en[0].Reason, "missing") {
			t.Errorf("%s of a missing file is audited as %+v", m, en)
		}
	}
	en := entriesOf(d.f.tenantLog("lab1"), got.header.Get(api.RequestIDHeader))
	if len(en) != 1 || en[0].Outcome != master.OutcomeFailed || !strings.Contains(en[0].Reason, "does not match") {
		t.Errorf("the cut download is audited as %+v", en)
	}
}
