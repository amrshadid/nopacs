package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	fakeBucket = "fake-bucket"
	fakeKey    = "AKIAFAKETESTKEY0000"
	fakeSecret = "fake/secret+value/for/tests/only0000000000"
)

// request is what a fake server saw of one request.
type request struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   []byte
}

// fakeS3 is a small in-memory S3 server for what the real servers cannot be
// made to do: answer wrongly, slowly or with a redirect, and show exactly
// what the driver sent. It holds objects and multipart uploads, and a hook
// that may answer a request itself first.
type fakeS3 struct {
	*httptest.Server

	mu      sync.Mutex
	seen    []request
	objects map[string][]byte
	uploads map[string]map[int][]byte
	aborted int
	nextID  int

	// hook, when set, may answer a request, returning true when it did.
	hook   func(w http.ResponseWriter, r *http.Request, n int) bool
	secure bool
	// badCRC makes every GET carry a CRC32 that is not the object's.
	badCRC atomic.Bool
	// badEcho makes every PUT answer with a checksum that is not the one sent.
	badEcho atomic.Bool
	// noVerify makes the fake keep an object whatever its checksum says,
	// and echo the checksum sent, as a server that never computes one.
	noVerify atomic.Bool
	// hits counts requests, for hooks that fail the first few.
	hits atomic.Int64

	// completeBody, when not empty, is sent as the answer to completing an
	// upload instead of the usual one, after the fake has completed it: "-" is
	// a bare 200 with no body.
	completeBody atomic.Value // string
	// composite makes the answer to completing an upload carry the object's
	// composite checksum: 0 none, 1 the right one, 2 a wrong one.
	composite atomic.Int32
	// discard makes the fake read an uploaded part and forget it, so that a
	// test of the client's memory does not measure the fake's. Every part
	// then waits partDelay before it is answered; inflight and maxInflight
	// count the parts being uploaded together.
	discard   atomic.Bool
	partDelay atomic.Int64 // nanoseconds
	// gate, set by holdParts, holds the first gateWant parts to arrive until
	// all of them are in flight together, or for a deadline, and says so in
	// gateTimedOut: a test of how many parts a Put sends at once then waits
	// for that event and not for a delay.
	gateMu       sync.Mutex
	gateWant     int64
	gateArrived  int64
	gateOpen     chan struct{}
	gateTimedOut atomic.Bool
	inflight     atomic.Int64
	maxInflight  atomic.Int64

	// stale is the list of unfinished uploads a listing answers with, in key
	// order, when set; abortedIDs are the ids aborted, and listMode says how
	// the listing matches a request: "honor" a prefix and a key-marker,
	// "exact" a prefix only as a whole key (as MinIO's listing does), "wide"
	// no prefix at all, or "paging-only" no prefix and no marker but the one
	// that continues a listing.
	// dig holds the digests of parts that were discarded.
	dig        map[string]map[int][sha256.Size]byte
	stale      []fakeUpload
	listMode   string
	abortedIDs []string
}

// fakeUpload is an unfinished upload in the fake's listing.
type fakeUpload struct {
	key, id   string
	initiated time.Time
}

func newFakeS3(tb testing.TB) *fakeS3 {
	tb.Helper()
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, dig: map[string]map[int][sha256.Size]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	tb.Cleanup(f.Close)
	return f
}

// newFakeS3TLS is a fake that speaks https with a certificate of its own,
// which config trusts through Config.CABundle.
func newFakeS3TLS(tb testing.TB) *fakeS3 {
	tb.Helper()
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, dig: map[string]map[int][sha256.Size]byte{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	f.secure = true
	tb.Cleanup(f.Close)
	return f
}

// config returns a configuration that reaches the fake.
func (f *fakeS3) config() Config {
	c := Config{
		Endpoint: f.URL, Region: "us-east-1", Bucket: fakeBucket, PathStyle: true,
		AccessKeyID: NewSecret(fakeKey), SecretAccessKey: NewSecret(fakeSecret), AllowHTTP: true, AllowAddress: allowAll,
	}
	if f.secure {
		c.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Certificate().Raw})
	}
	return c
}

func (f *fakeS3) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.seen...)
}

// count returns how many requests had this method and, when sub is not
// empty, a query that has the parameter sub.
func (f *fakeS3) count(method, sub string) int {
	n := 0
	for _, r := range f.requests() {
		if r.method != method {
			continue
		}
		if _, ok := r.query[sub]; sub == "" || ok {
			n++
		}
	}
	return n
}

func xmlError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>REQ123</RequestId></Error>`, code, message)
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" && f.discard.Load() {
		f.hits.Add(1)
		cur := f.inflight.Add(1)
		for m := f.maxInflight.Load(); cur > m && !f.maxInflight.CompareAndSwap(m, cur); m = f.maxInflight.Load() {
		}
		h := sha256.New()
		_, _ = io.Copy(h, r.Body)
		if num, err := strconv.Atoi(r.URL.Query().Get("partNumber")); err == nil {
			var d [sha256.Size]byte
			h.Sum(d[:0])
			f.mu.Lock()
			if f.dig[r.URL.Query().Get("uploadId")] == nil {
				f.dig[r.URL.Query().Get("uploadId")] = map[int][sha256.Size]byte{}
			}
			f.dig[r.URL.Query().Get("uploadId")][num] = d
			f.mu.Unlock()
		}
		f.passGate()
		time.Sleep(time.Duration(f.partDelay.Load()))
		f.inflight.Add(-1)
		w.Header().Set("ETag", `"part"`)
		w.Header().Set("x-amz-checksum-sha256", r.Header.Get("x-amz-checksum-sha256"))
		return
	}
	body, _ := io.ReadAll(r.Body)
	n := int(f.hits.Add(1))
	f.mu.Lock()
	f.seen = append(f.seen, request{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	hook := f.hook
	f.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if hook != nil && hook(w, r, n) {
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+fakeBucket+"/")
	q := r.URL.Query()
	w.Header().Set("Connection", "close")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/"+fakeBucket+"/" || r.Method == http.MethodGet && r.URL.Path == "/"+fakeBucket:
		if _, ok := q["uploads"]; ok {
			f.listUploads(w, q)
			return
		}
		fmt.Fprint(w, `<ListBucketResult><Name>`+fakeBucket+`</Name><KeyCount>0</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case r.Method == http.MethodPut && q.Get("partNumber") != "":
		num, _ := strconv.Atoi(q.Get("partNumber"))
		f.mu.Lock()
		if u := f.uploads[q.Get("uploadId")]; u != nil {
			u[num] = body
		}
		f.mu.Unlock()
		w.Header().Set("ETag", `"part`+q.Get("partNumber")+`"`)
		w.Header().Set("x-amz-checksum-sha256", f.echo(r))
	case r.Method == http.MethodPut:
		// A server that checks checksums refuses a body whose SHA-256 is not
		// the one sent, before it keeps anything; noVerify makes the fake one
		// that only repeats what it was sent.
		if c := r.Header.Get("x-amz-checksum-sha256"); c != "" && c != sha256b64(body) && !f.noVerify.Load() {
			xmlError(w, http.StatusBadRequest, "BadDigest", "the checksum does not match")
			return
		}
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"obj"`)
		w.Header().Set("x-amz-checksum-sha256", f.echo(r))
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.nextID++
		id := "upload" + strconv.Itoa(f.nextID)
		f.uploads[id] = map[int][]byte{}
		f.mu.Unlock()
		// #nosec G705 -- a test server answering with fixed text and its own names.
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, fakeBucket, key, id)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.mu.Lock()
		u := f.uploads[q.Get("uploadId")]
		var all []byte
		digests := sha256.New()
		for i := 1; i <= len(u); i++ {
			all = append(all, u[i]...)
			d := sha256.Sum256(u[i])
			digests.Write(d[:])
		}
		count := len(u)
		if dd := f.dig[q.Get("uploadId")]; dd != nil { // Discarded parts: only their digests are known.
			digests = sha256.New()
			for i := 1; i <= len(dd); i++ {
				d := dd[i]
				digests.Write(d[:])
			}
			count = len(dd)
		}
		f.objects[key] = all
		delete(f.uploads, q.Get("uploadId"))
		f.mu.Unlock()
		extra := ""
		switch f.composite.Load() {
		case 1:
			extra = fmt.Sprintf("<ChecksumSHA256>%s-%d</ChecksumSHA256>", base64.StdEncoding.EncodeToString(digests.Sum(nil)), count)
		case 2:
			extra = "<ChecksumSHA256>AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=-1</ChecksumSHA256>"
		}
		if body, _ := f.completeBody.Load().(string); body != "" {
			if body != "-" {
				_, _ = io.WriteString(w, body)
			}
			return
		}
		// #nosec G705 -- a test server answering with fixed text and its own names.
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>x</Location><Bucket>%s</Bucket><Key>%s</Key><ETag>"done"</ETag>%s</CompleteMultipartUploadResult>`, fakeBucket, key, extra)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		f.abortedIDs = append(f.abortedIDs, q.Get("uploadId"))
		for i, u := range f.stale {
			if u.id == q.Get("uploadId") {
				f.stale = append(f.stale[:i], f.stale[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			xmlError(w, http.StatusNotFound, "NoSuchKey", "no such key")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		if r.Header.Get("X-Test-Bad-Crc") != "" || f.badCRC.Load() {
			w.Header().Set("x-amz-checksum-crc32", "AAAAAA==")
		}
		if r.Method == http.MethodHead {
			// Asked with the checksum mode, a server that keeps checksums
			// answers the object's own.
			if r.Header.Get("x-amz-checksum-mode") == "ENABLED" {
				w.Header().Set("x-amz-checksum-sha256", sha256b64(obj))
			}
			return
		}
		_, _ = w.Write(obj)
	default:
		xmlError(w, http.StatusNotImplemented, "NotImplemented", "not implemented")
	}
}

// holdParts makes the fake hold each of the next n discarded parts until n are
// in flight at once.
func (f *fakeS3) holdParts(n int) {
	f.gateMu.Lock()
	f.gateWant, f.gateArrived, f.gateOpen = int64(n), 0, make(chan struct{})
	f.gateMu.Unlock()
	f.gateTimedOut.Store(false)
	f.maxInflight.Store(0)
}

// passGate holds a part at the gate, if one is set and the part is among the
// first to arrive, until the gate opens or a generous deadline passes.
func (f *fakeS3) passGate() {
	f.gateMu.Lock()
	if f.gateWant == 0 {
		f.gateMu.Unlock()
		return
	}
	f.gateArrived++
	arrived, open := f.gateArrived, f.gateOpen
	if arrived == f.gateWant {
		close(open)
	}
	f.gateMu.Unlock()
	if arrived > f.gateWant {
		return
	}
	select {
	case <-open:
	case <-time.After(2 * time.Minute):
		f.gateTimedOut.Store(true)
	}
}

// echo is the checksum a PUT's answer carries: the one sent, or, when
// badEcho is set, another, as a server that stored other bytes would give.
func (f *fakeS3) echo(r *http.Request) string {
	if f.badEcho.Load() {
		return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	}
	return r.Header.Get("x-amz-checksum-sha256")
}

// listUploads answers a listing of the unfinished uploads in f.stale, in
// pages, as f.listMode says.
func (f *fakeS3) listUploads(w http.ResponseWriter, q url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	max := 1000
	if v, err := strconv.Atoi(q.Get("max-uploads")); err == nil && v < max {
		max = v
	}
	var matched []fakeUpload
	for _, u := range f.stale {
		switch f.listMode {
		case "wide": // Ignores the prefix, honors the marker.
			if m := q.Get("key-marker"); m != "" && u.key <= m {
				continue
			}
		case "paging-only": // Ignores the prefix and a marker that starts a listing; honors the one that pages.
			if m := q.Get("key-marker"); m != "" && q.Get("upload-id-marker") != "" && u.key <= m {
				continue
			}
		case "exact":
			if p := q.Get("prefix"); p != "" && u.key != p {
				continue
			}
			if m := q.Get("key-marker"); m != "" && u.key <= m {
				continue
			}
		default:
			if !strings.HasPrefix(u.key, q.Get("prefix")) {
				continue
			}
			if m := q.Get("key-marker"); m != "" && u.key <= m {
				continue
			}
		}
		matched = append(matched, u)
	}
	trunc := len(matched) > max
	if trunc {
		matched = matched[:max]
	}
	fmt.Fprint(w, `<ListMultipartUploadsResult><Bucket>`+fakeBucket+`</Bucket>`)
	if trunc {
		last := matched[len(matched)-1]
		fmt.Fprintf(w, `<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>%s</NextUploadIdMarker>`, last.key, last.id)
	} else {
		fmt.Fprint(w, `<IsTruncated>false</IsTruncated>`)
	}
	for _, u := range matched {
		fmt.Fprintf(w, `<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>`, u.key, u.id, u.initiated.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	fmt.Fprint(w, `</ListMultipartUploadsResult>`)
}

// state returns how many uploads were aborted, how many are unfinished and
// how many objects the fake holds.
func (f *fakeS3) state() (aborted, unfinished, objects int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted, len(f.uploads), len(f.objects)
}

// sha256b64 is a checksum as S3 writes it.
func sha256b64(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// openFake opens a store on f.
func openFake(tb testing.TB, f *fakeS3, mod func(*Config)) *Store {
	tb.Helper()
	cfg := f.config()
	if mod != nil {
		mod(&cfg)
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}
