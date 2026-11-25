package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestPutSuccessNeedsConfirmation guards that Put reports success only when
// the server said something about these bytes: the echo of the SHA-256 on
// an object and on every part (required, not only compared when present),
// and for a completed upload a well formed answer naming the object plus
// either the right composite checksum or, when the server gives none, a HEAD
// that shows the object with the size and ETag put. A bare 200, an error
// body under a 200, a wrong key and a wrong checksum are each a failure, with
// the upload aborted. Written.Confirmed is set only when the server answered
// for the whole object's checksum, never on the HEAD alone.
func TestPutSuccessNeedsConfirmation(t *testing.T) {
	const part = MinPartSize
	ctx := context.Background()
	small := []byte("hello")
	big := blobtest.PatternBytes(1, 2*part+9)

	type tc struct {
		name  string
		hook  func(w http.ResponseWriter, r *http.Request, n int) bool
		comp  int32
		data  []byte
		fails bool
		heads int    // HEAD requests the Put makes, when it succeeds
		body  string // The fake's answer to completing, when not its usual one.
	}
	isPart := func(r *http.Request) bool { return r.Method == http.MethodPut && r.URL.Query().Has("partNumber") }
	isObject := func(r *http.Request) bool { return r.Method == http.MethodPut && !r.URL.Query().Has("partNumber") }
	for _, c := range []tc{
		{name: "object confirmed", data: small},
		{name: "object, bare 200", data: small, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if isObject(r) {
				w.WriteHeader(200)
				return true
			}
			return false
		}},
		{name: "object, 200 with an error body", data: small, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if isObject(r) {
				w.WriteHeader(200)
				fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>x</Message></Error>`)
				return true
			}
			return false
		}},
		{name: "object, another checksum", data: small, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if isObject(r) {
				w.Header().Set("x-amz-checksum-sha256", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				return true
			}
			return false
		}},
		{name: "part, no echo", data: big, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if isPart(r) {
				w.Header().Set("ETag", `"p"`)
				return true
			}
			return false
		}},
		{name: "composite checksum right", data: big, comp: 1},
		{name: "composite checksum wrong", data: big, comp: 2, fails: true},
		{name: "no composite checksum, confirmed by HEAD", data: big, heads: 1},
		// The next four are answered after the fake has stored the object, so a
		// HEAD would find it: the answer itself must be refused.
		{name: "completion, bare 200", data: big, fails: true, body: "-"},
		{name: "completion, 200 with an error body", data: big, fails: true, body: `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<Error><Code>InternalError</Code><Message>x</Message></Error>`},
		{name: "completion names another key", data: big, fails: true, body: `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>other</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`},
		{name: "completion without an ETag", data: big, fails: true, body: `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key></CompleteMultipartUploadResult>`},
		{name: "HEAD shows another size", data: big, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodHead && r.URL.Path == "/"+fakeBucket+"/k" {
				w.Header().Set("Content-Length", "7")
				return true
			}
			return false
		}},
		{name: "HEAD shows another object", data: big, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodHead && r.URL.Path == "/"+fakeBucket+"/k" {
				w.Header().Set("Content-Length", fmt.Sprint(len(big)))
				w.Header().Set("ETag", `"someone-else"`)
				return true
			}
			return false
		}},
		{name: "HEAD finds nothing", data: big, fails: true, hook: func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodHead && r.URL.Path == "/"+fakeBucket+"/k" {
				w.WriteHeader(404)
				return true
			}
			return false
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openFake(t, f, func(cfg *Config) { cfg.PartSize = part })
			f.hook = c.hook
			f.composite.Store(c.comp)
			f.completeBody.Store(c.body)
			headsBefore := f.count(http.MethodHead, "")
			w, err := s.Put(ctx, "k", bytes.NewReader(c.data))
			if c.fails {
				if err == nil {
					t.Fatalf("Put = %+v with no error", w)
				}
				if fault.KindOf(err) != fault.Unavailable {
					t.Errorf("kind %v, want Unavailable", fault.KindOf(err))
				}
				noLeak(t, c.name, err)
				if len(c.data) > part {
					// The upload is not left: it is aborted unless it was completed.
					if _, unfinished, _ := f.state(); unfinished != 0 {
						t.Errorf("%d uploads left unfinished", unfinished)
					}
				}
				return
			}
			if err != nil || w.Size != int64(len(c.data)) {
				t.Fatalf("Put = %+v, %v", w, err)
			}
			if got := f.count(http.MethodHead, "") - headsBefore; got != c.heads {
				t.Errorf("the Put made %d HEADs, want %d", got, c.heads)
			}
			// Written.Confirmed only when the server answered for the whole
			// object's checksum, not when a HEAD showed its size and ETag.
			if w.Confirmed != (c.heads == 0) {
				t.Errorf("Written.Confirmed = %v, want %v", w.Confirmed, c.heads == 0)
			}
		})
	}
}

// TestOpenChecksTheServerConfirmsUploads guards the check at Open: a server
// that stores an object but does not echo its checksum is refused as Invalid
// with a clear message, since no Put could be confirmed on it; the probe
// object is removed; credentials that may not write still open the storage,
// with a note; and a probe object that cannot be removed is noted.
func TestOpenChecksTheServerConfirmsUploads(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	if _, _, objects := f.state(); objects != 0 {
		t.Errorf("Open left %d objects: the probe's object must be removed", objects)
	}
	if n := s.Notes(); n.WriteUnchecked || n.ProbeLeft {
		t.Errorf("notes of a good server = %+v", n)
	}
	for _, r := range f.requests() {
		if r.method == http.MethodPut && !strings.Contains(r.path, ".blob-open-probe-") {
			t.Errorf("Open wrote %s, which is not a name no key can have", r.path)
		}
	}

	g := newFakeS3(t)
	g.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodPut {
			w.Header().Set("ETag", `"x"`)
			return true
		}
		return false
	}
	_, err := Open(ctx, g.config())
	if fault.KindOf(err) != fault.Invalid || !errors.Is(err, errNotConfirmed) {
		t.Fatalf("Open on a server that does not echo = %v, want Invalid", err)
	}
	if !strings.Contains(fault.Message(err), "checksum") {
		t.Errorf("the message does not say why: %q", fault.Message(err))
	}
	noLeak(t, "no echo", err)
	if _, _, objects := g.state(); objects != 0 {
		t.Errorf("the probe object was left: %d objects", objects)
	}

	h := newFakeS3(t)
	h.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodPut {
			xmlError(w, 403, "AccessDenied", "no")
			return true
		}
		return false
	}
	s2, err := Open(ctx, h.config())
	if err != nil || !s2.Notes().WriteUnchecked {
		t.Errorf("Open with credentials that may not write = %v, notes %+v; want success and the note", err, func() Notes {
			if s2 == nil {
				return Notes{}
			}
			return s2.Notes()
		}())
	}

	k := newFakeS3(t)
	k.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method == http.MethodDelete {
			xmlError(w, 403, "AccessDenied", "no")
			return true
		}
		return false
	}
	s3, err := Open(ctx, k.config())
	if err != nil || !s3.Notes().ProbeLeft {
		t.Errorf("Open when the probe cannot be removed = %v; want success and the note", err)
	}
}

// TestPutConfirmedOnServers guards, on a real server, that the confirmation
// of a completed upload works as the server answers it: whichever of the
// composite checksum or the HEAD the server's answer calls for, a Put of
// several parts succeeds, and the test says which was used.
func TestPutConfirmedOnServers(t *testing.T) {
	cfg := serverConfig(t)
	p := newProxy(t, cfg)
	s := openThrough(t, p, cfg, nil)
	p.reset()
	data := blobtest.PatternBytes(2, 3*MinPartSize+11)
	if _, err := s.Put(context.Background(), "multi", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	t.Logf("completion confirmed by HEAD: %v (otherwise by the composite checksum)", p.count("HEAD", "") == 1)
	if p.count("HEAD", "") > 1 {
		t.Errorf("%d HEADs", p.count("HEAD", ""))
	}
}
