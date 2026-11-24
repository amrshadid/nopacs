package stow

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// uidsOf reads a corpus file's SOP Class, SOP Instance and Series UIDs.
func uidsOf(t *testing.T, b []byte) (class, instance, series string) {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	class, _ = h.UID(header.SOPClassUID)
	instance, _ = h.UID(header.SOPInstanceUID)
	series, _ = h.UID(header.SeriesUID)
	return class, instance, series
}

// TestServeStoresEveryPartAsSent guards the plain case: a study of seven
// instances sent into its study is answered 200 in DICOM JSON, each
// instance listed with its WADO-RS Retrieve URL and the study's own, and
// each part handed to the pipeline byte for byte as sent, with what the
// part's header says it is and who sent it.
func TestServeStoresEveryPartAsSent(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, nil)
	var bodies [][]byte
	for _, n := range ctFiles {
		bodies = append(bodies, corpus(t, n))
	}
	body, ct := multipartBody(t, dicomParts(bodies...))
	w, res := r.serve(ctStudy, body, ct)
	if w.Code != http.StatusOK || res.Status != http.StatusOK || res.Stored != 7 || res.Refused != 0 {
		t.Fatalf("answer %d %+v: %s", w.Code, res, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/dicom+json" {
		t.Errorf("Content-Type %q", got)
	}
	a := readAnswer(t, w.Body.Bytes())
	if a.studyURL != "http://archive.test/tenants/lab1/dicom-web/studies/"+ctStudy || len(a.stored) != 7 || len(a.failed) != 0 {
		t.Fatalf("answer %+v", a)
	}
	got := r.sink.stored()
	for i, b := range bodies {
		class, instance, series := uidsOf(t, b)
		want := storedItem{class: class, instance: instance,
			url: "http://archive.test/tenants/lab1/dicom-web/studies/" + ctStudy + "/series/" + series + "/instances/" + instance}
		if a.stored[i] != want {
			t.Errorf("item %d: %+v, want %+v", i, a.stored[i], want)
		}
		if !bytes.Equal(got[i], b) {
			t.Errorf("part %d reached the pipeline as %d bytes, not the %d sent", i, len(got[i]), len(b))
		}
		told := r.sink.told[i]
		if told.Receiver != "web" || told.SOPInstanceUID != instance || told.SOPClassUID != class ||
			told.Caller.Address != "192.0.2.7:51234" || told.Association != "req-1" || told.TransferSyntaxUID == "" ||
			told.StudyUID != ctStudy || told.SeriesUID != series {
			t.Errorf("part %d was told as %+v", i, told)
		}
	}
}

// TestServeAtAReceiverThatDeidentifiesAnswersNoRetrieveURL guards the
// answer of a receiver that de-identifies: the instances are archived
// under remapped UIDs, so neither an instance's Retrieve URL nor the
// study's is given (a URL by the remapped UIDs would hand the sender the
// mapping); each stored instance is still listed by the class and the SOP
// Instance UID it was sent with, and the request is answered 200.
func TestServeAtAReceiverThatDeidentifiesAnswersNoRetrieveURL(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.Receiver.Profile = tenant.ProfileRef{Name: "research", Version: 1} })
	var bodies [][]byte
	for _, n := range ctFiles[:2] {
		bodies = append(bodies, corpus(t, n))
	}
	body, ct := multipartBody(t, dicomParts(bodies...))
	w, res := r.serve(ctStudy, body, ct)
	if w.Code != http.StatusOK || res.Stored != 2 {
		t.Fatalf("answer %d %+v: %s", w.Code, res, w.Body)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"00081190"`)) || bytes.Contains(w.Body.Bytes(), []byte("/studies/")) {
		t.Fatalf("the answer holds a Retrieve URL: %s", w.Body)
	}
	a := readAnswer(t, w.Body.Bytes())
	if a.studyURL != "" || len(a.stored) != 2 {
		t.Fatalf("answer %+v", a)
	}
	for i, b := range bodies {
		class, instance, _ := uidsOf(t, b)
		if want := (storedItem{class: class, instance: instance}); a.stored[i] != want {
			t.Errorf("item %d: %+v, want %+v", i, a.stored[i], want)
		}
	}
}

// TestServeDecidesEachPart guards a request of good and bad parts into a
// study: a part of another study is refused A900H, one larger than the
// receiver's largest file A700H (handed to the pipeline, which refuses it
// for its size and records it), one that is not DICOM counted but not
// listed, one of another media type the same; the others are stored, and
// the answer is 202 with both sequences.
func TestServeDecidesEachPart(t *testing.T) {
	big := corpus(t, "liver_1frame.dcm")
	r := newRig(t, int64(len(big))-1, 1<<24, nil)
	good := corpus(t, "ct5n-2062.dcm")
	other := corpus(t, "MR_small.dcm")
	parts := append(dicomParts(good, other, big, []byte("this is not a DICOM file")), part{contentType: "text/plain", body: good})
	body, ct := multipartBody(t, parts)
	w, res := r.serve(ctStudy, body, ct)
	if w.Code != http.StatusAccepted || res.Stored != 1 || res.Refused != 4 {
		t.Fatalf("answer %d %+v: %s", w.Code, res, w.Body)
	}
	a := readAnswer(t, w.Body.Bytes())
	oc, oi, _ := uidsOf(t, other)
	bc, bi, _ := uidsOf(t, big)
	want := []failedItem{{oc, oi, ReasonDoesNotMatch}, {bc, bi, ReasonOutOfResources}}
	if len(a.stored) != 1 || len(a.failed) != 2 || a.failed[0] != want[0] || a.failed[1] != want[1] {
		t.Fatalf("answer %+v, want failed %+v", a, want)
	}
	if got := r.sink.stored(); len(got) != 1 || !bytes.Equal(got[0], good) {
		t.Errorf("the pipeline kept %d files, want the good one", len(got))
	}
}

// TestServeAllRefusedIsConflict guards 409 when no part was stored, and
// 202 against 200 when a part is refused without being listed.
func TestServeAllRefusedIsConflict(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, nil)
	body, ct := multipartBody(t, dicomParts(corpus(t, "MR_small.dcm")))
	if w, res := r.serve(ctStudy, body, ct); w.Code != http.StatusConflict || res.Stored != 0 || res.Refused != 1 {
		t.Errorf("one part of another study: %d %+v", w.Code, res)
	}
	body, ct = multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm"), []byte("junk")))
	if w, _ := r.serve("", body, ct); w.Code != http.StatusAccepted {
		t.Errorf("a good part and an unreadable one: %d, want 202", w.Code)
	}
	r.sink.refuse = true
	body, ct = multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm")))
	w, _ := r.serve("", body, ct)
	if a := readAnswer(t, w.Body.Bytes()); w.Code != http.StatusConflict || len(a.failed) != 1 || a.failed[0].reason != ReasonOutOfResources {
		t.Errorf("a part the pipeline refused: %d %+v, want 409 with A700H", w.Code, a)
	}
}

// TestServeRefusesTheWholeRequest guards the refusals of a request as a
// whole, each before any part is handed on: another media type 415,
// metadata with bulk data 415, no boundary 400, no part 400, a client
// that takes no JSON 406, and the archive unable to take files 503 with
// the caller reported to the receiver's health.
func TestServeRefusesTheWholeRequest(t *testing.T) {
	var reported atomic.Value
	refusing := atomic.Bool{}
	r := newRig(t, 1<<20, 1<<24, func(c *Config) {
		c.Refusal = func() tenant.AssociationFailure {
			if refusing.Load() {
				return tenant.FailureNearlyFull
			}
			return ""
		}
		c.Report = func(f tenant.AssociationFailure, g tenant.GroupCaller) { reported.Store(g.Address + " " + string(f)) }
	})
	body, ct := multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm")))
	empty, emptyCT := multipartBody(t, nil)
	for _, c := range []struct {
		name, contentType string
		body              []byte
		want              int
	}{
		{"application/dicom alone", "application/dicom", body, http.StatusUnsupportedMediaType},
		{"metadata and bulk data", strings.Replace(ct, "application/dicom", "application/dicom+json", 1), body, http.StatusUnsupportedMediaType},
		{"no boundary", `multipart/related; type="application/dicom"`, body, http.StatusBadRequest},
		{"no part", emptyCT, empty, http.StatusBadRequest},
		{"not multipart at all", "multipart/related; boundary=x", []byte("garbage"), http.StatusBadRequest},
	} {
		w, res := r.serve("", c.body, c.contentType)
		if w.Code != c.want || res.Reason == "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %+v, want %d", c.name, w.Code, res, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", "application/dicom+xml")
	w := httptest.NewRecorder()
	if res := r.ep.Serve(context.Background(), w, req, Request{}); res.Status != http.StatusNotAcceptable {
		t.Errorf("a client that takes no JSON: %d", res.Status)
	}
	refusing.Store(true)
	w2, _ := r.serve("", body, ct)
	if w2.Code != http.StatusServiceUnavailable || reported.Load() != "192.0.2.7 "+string(tenant.FailureNearlyFull) {
		t.Errorf("a full archive: %d, reported %v", w2.Code, reported.Load())
	}
	if n := len(r.sink.stored()); n != 0 {
		t.Errorf("%d files reached the pipeline", n)
	}
}

// TestServeRequestPastItsLimit guards 413 for a request larger than the
// receiver's largest request, and that the part sent whole before the
// limit is kept, being durable and owed.
func TestServeRequestPastItsLimit(t *testing.T) {
	a, b := corpus(t, "ct5n-2062.dcm"), corpus(t, "liver_1frame.dcm")
	r := newRig(t, 1<<20, int64(len(a)+len(b)/2), nil)
	body, ct := multipartBody(t, dicomParts(a, b))
	w, res := r.serve("", body, ct)
	if w.Code != http.StatusRequestEntityTooLarge || res.Stored != 1 {
		t.Fatalf("answer %d %+v: %s", w.Code, res, w.Body)
	}
	if got := r.sink.stored(); len(got) != 1 || !bytes.Equal(got[0], a) {
		t.Errorf("the pipeline kept %d files, want the first", len(got))
	}
}

// TestServeAPartPastThePrefixBound guards a part read up to the prefix
// bound without giving its identity: refused, and nothing of it kept.
func TestServeAPartPastThePrefixBound(t *testing.T) {
	r := newRig(t, 1<<30, 1<<30, nil)
	// A preamble and DICM prefix, then zeros past maxPrefix: no file meta
	// information or UID is ever read.
	src := corpus(t, "ct5n-2062.dcm")
	body, ct := multipartBody(t, dicomParts(append(src[:132:132], bytes.Repeat([]byte{0}, maxPrefix+10)...)))
	w, res := r.serve("", body, ct)
	if w.Code != http.StatusConflict || res.Refused != 1 || len(r.sink.stored()) != 0 {
		t.Errorf("answer %d %+v", w.Code, res)
	}
}

// TestServeCutsAStalledClient guards the read stall bound: a client that
// sends part of a body and then nothing is cut once a read waits longer
// than the bound, and the part it had not finished is not kept.
func TestServeCutsAStalledClient(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.ReadStall = 200 * time.Millisecond })
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
	select {
	case res := <-results:
		if res.Status != http.StatusRequestTimeout || res.Stored != 0 {
			t.Errorf("a stalled client: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled client was not cut")
	}
	if n := len(r.sink.stored()); n != 0 {
		t.Errorf("%d files of a stalled request were kept", n)
	}
}

// itoa writes n in decimal.
func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}
