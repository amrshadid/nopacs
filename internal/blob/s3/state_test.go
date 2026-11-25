package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// headAs is a hook that answers a HEAD of key with the headers given, as a
// server with cold classes would, and lets every other request through.
func headAs(key string, headers map[string]string) func(http.ResponseWriter, *http.Request, int) bool {
	return func(w http.ResponseWriter, r *http.Request, _ int) bool {
		if r.Method != http.MethodHead || r.URL.Path != "/"+fakeBucket+"/"+key {
			return false
		}
		w.Header().Set("Content-Length", "5")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		return true
	}
}

// TestStatOnlineState guards that Stat reads an object's state from the
// HEAD's headers as S3 documents them: the cold classes archived, a restore
// running restoring, a restored copy online until its expiry, an
// Intelligent-Tiering archive tier archived, the instant class online, and
// a restore header that cannot be read ignored for the class to decide.
// Reading any of these as missing or as online would send a read to fail,
// or a scrub to call a held object lost.
func TestStatOnlineState(t *testing.T) {
	ctx := context.Background()
	until := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		headers map[string]string
		state   blob.OnlineState
		until   time.Time
	}{
		{"standard", map[string]string{"x-amz-storage-class": "STANDARD"}, blob.Online, time.Time{}},
		{"no class", nil, blob.Online, time.Time{}},
		{"glacier", map[string]string{"x-amz-storage-class": "GLACIER"}, blob.Archived, time.Time{}},
		{"deep archive", map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE"}, blob.Archived, time.Time{}},
		{"instant retrieval", map[string]string{"x-amz-storage-class": "GLACIER_IR"}, blob.Online, time.Time{}},
		{"tiering archive", map[string]string{"x-amz-storage-class": "INTELLIGENT_TIERING", "x-amz-archive-status": "ARCHIVE_ACCESS"}, blob.Archived, time.Time{}},
		{"restoring", map[string]string{"x-amz-storage-class": "GLACIER", "x-amz-restore": `ongoing-request="true"`}, blob.Restoring, time.Time{}},
		{"restored", map[string]string{"x-amz-storage-class": "GLACIER", "x-amz-restore": `ongoing-request="false", expiry-date="Mon, 21 Dec 2026 00:00:00 GMT"`}, blob.Online, until},
		{"restored, no date", map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE", "x-amz-restore": `ongoing-request="false"`}, blob.Online, time.Time{}},
		{"restore unreadable", map[string]string{"x-amz-storage-class": "GLACIER", "x-amz-restore": `whatever`}, blob.Archived, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openFake(t, f, nil)
			f.hook = headAs("k", c.headers)
			info, err := s.Stat(ctx, "k")
			if err != nil {
				t.Fatal(err)
			}
			if info.Size != 5 || info.State != c.state || !info.OnlineUntil.Equal(c.until) {
				t.Fatalf("Stat = %+v, want size 5, %v until %v", info, c.state, c.until)
			}
		})
	}
}

// TestStatStoreChecksum guards the server's checksum in Stat: asked with
// the checksum mode, a whole object's SHA-256 is given; a multipart object's
// composite checksum, by its type or its "-<parts>" form, and a value that
// is not 32 bytes in base64 are not, so a copy is never confirmed against a
// checksum of something else.
func TestStatStoreChecksum(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3(t)
	s := openFake(t, f, nil)
	data := []byte("hello")
	if _, err := s.Put(ctx, "k", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "k")
	if err != nil || !info.HasSHA256 || info.SHA256 != sha256.Sum256(data) {
		t.Fatalf("Stat = %+v, %v; want the object's SHA-256", info, err)
	}
	reqs := f.requests()
	if last := reqs[len(reqs)-1]; last.method != http.MethodHead || last.header.Get("x-amz-checksum-mode") != "ENABLED" {
		t.Fatalf("Stat sent %s with checksum mode %q, want a HEAD with ENABLED", last.method, last.header.Get("x-amz-checksum-mode"))
	}
	whole := sha256b64(data)
	for name, h := range map[string]map[string]string{
		"composite by type":  {"x-amz-checksum-sha256": whole, "x-amz-checksum-type": "COMPOSITE"},
		"composite by parts": {"x-amz-checksum-sha256": whole + "-3"},
		"short":              {"x-amz-checksum-sha256": "AAAA"},
		"not base64":         {"x-amz-checksum-sha256": "%%%"},
	} {
		f.hook = headAs("k", h)
		info, err := s.Stat(ctx, "k")
		if err != nil || info.HasSHA256 || info.SHA256 != ([sha256.Size]byte{}) {
			t.Errorf("%s: Stat = %+v, %v; want no checksum", name, info, err)
		}
	}
	f.hook = headAs("k", map[string]string{"x-amz-checksum-sha256": whole, "x-amz-checksum-type": "FULL_OBJECT"})
	if info, err := s.Stat(ctx, "k"); err != nil || !info.HasSHA256 {
		t.Errorf("a full-object checksum: Stat = %+v, %v; want it", info, err)
	}
}

// TestChecksumsTrustedOnlyWhenChecked guards that the server's word on a
// checksum is taken only from a server Open saw refuse a wrong one: on a
// server that verifies, a put is confirmed and Stat gives the checksum; on
// one that echoes whatever it was sent, or answers the wrong checksum with
// anything but a mismatch, or when opened for reading (no test is made),
// Written.Confirmed and Info.HasSHA256 are false and Notes says so, and the
// test's object is not left behind. A copy confirmed by a server that never
// computed a checksum would pass damaged bytes as verified.
func TestChecksumsTrustedOnlyWhenChecked(t *testing.T) {
	ctx := context.Background()
	data := []byte("hello")
	check := func(t *testing.T, s *Store, f *fakeS3, trusted bool) {
		t.Helper()
		if s.Notes().ChecksumUnchecked == trusted {
			t.Fatalf("Notes().ChecksumUnchecked = %v, want %v", s.Notes().ChecksumUnchecked, !trusted)
		}
		if _, _, objects := f.state(); objects != 0 {
			t.Fatalf("Open left %d objects", objects)
		}
		if !s.set.forReading {
			w, err := s.Put(ctx, "k", bytes.NewReader(data))
			if err != nil || w.Confirmed != trusted {
				t.Fatalf("Put = %+v, %v; want Confirmed %v", w, err, trusted)
			}
		} else {
			f.mu.Lock()
			f.objects["k"] = data
			f.mu.Unlock()
		}
		info, err := s.Stat(ctx, "k")
		if err != nil || info.HasSHA256 != trusted || (!trusted && info.SHA256 != [sha256.Size]byte{}) {
			t.Fatalf("Stat = %+v, %v; want HasSHA256 %v", info, err, trusted)
		}
	}
	t.Run("verifies", func(t *testing.T) {
		f := newFakeS3(t)
		check(t, openFake(t, f, nil), f, true)
		var mismatches int
		for _, r := range f.requests() {
			if r.method == http.MethodPut && r.header.Get("x-amz-checksum-sha256") != sha256b64(r.body) {
				mismatches++
			}
		}
		if mismatches != 1 {
			t.Fatalf("Open sent %d puts with a wrong checksum, want 1", mismatches)
		}
	})
	t.Run("echoes only", func(t *testing.T) {
		f := newFakeS3(t)
		f.noVerify.Store(true)
		check(t, openFake(t, f, nil), f, false)
	})
	t.Run("another refusal", func(t *testing.T) {
		f := newFakeS3(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
			if r.Method == http.MethodPut && r.Header.Get("x-amz-checksum-sha256") != sha256b64(mustRead(r)) {
				xmlError(w, http.StatusBadRequest, "InvalidRequest", "secret-server-words")
				return true
			}
			return false
		}
		s := openFake(t, f, nil)
		f.hook = nil
		check(t, s, f, false)
	})
	t.Run("for reading", func(t *testing.T) {
		f := newFakeS3(t)
		check(t, openFake(t, f, func(c *Config) { c.ForReading = true }), f, false)
	})
}

// mustRead returns the body of r and puts it back for whoever reads next.
func mustRead(r *http.Request) []byte {
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
	return b.Bytes()
}

// restoreHook answers a HEAD of "k" with headers (with after, once a restore
// was asked, when set) and a RestoreObject of it with status and, for an
// error, code; it records the body of each restore and counts the HEADs.
type restoreHook struct {
	headers map[string]string
	after   map[string]string
	status  int
	code    string
	bodies  []string
	heads   int
}

func (h *restoreHook) serve(w http.ResponseWriter, r *http.Request, n int) bool {
	if r.URL.Path != "/"+fakeBucket+"/k" {
		return false
	}
	switch {
	case r.Method == http.MethodHead:
		h.heads++
		if h.after != nil && len(h.bodies) > 0 {
			return headAs("k", h.after)(w, r, n)
		}
		return headAs("k", h.headers)(w, r, n)
	case r.Method == http.MethodPost && r.URL.Query().Has("restore"):
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		h.bodies = append(h.bodies, b.String())
		if h.code != "" {
			xmlError(w, h.status, h.code, "secret-server-words")
			return true
		}
		w.WriteHeader(h.status)
		return true
	}
	return false
}

// TestRecall guards Recall on S3: an archived object is restored with the
// days and the tier asked (no days for an Intelligent-Tiering archive tier,
// which refuses them); the server's 202, 200 and 409 RestoreAlreadyInProgress
// answer started, online and in progress; an object being restored or never
// cold is answered without a request, so twenty waiting readers cost one
// restore at most; a hot class with an unreadable restore header asks
// nothing; a class this version does not know is asked, and an answer that
// it needs no restore is believed only when a second HEAD shows it online,
// never while the HEAD still shows it cold (a caller would recall and read
// round and round); an expedited recall of a deep class is refused before
// any request; a tier the server will not serve now fails with
// blob.ErrRecallRefused; a key that holds nothing is NotFound.
func TestRecall(t *testing.T) {
	ctx := context.Background()
	glacier := map[string]string{"x-amz-storage-class": "GLACIER"}
	deepTiering := map[string]string{"x-amz-storage-class": "INTELLIGENT_TIERING", "x-amz-archive-status": "DEEP_ARCHIVE_ACCESS"}
	for _, c := range []struct {
		name     string
		hook     restoreHook
		req      blob.RecallRequest
		want     blob.RecallState
		restores int
		heads    int // 0: not checked
		days     bool
		kind     fault.Kind // of an error, when one is wanted
		refused  bool       // the error carries blob.ErrRecallRefused
	}{
		{name: "started", hook: restoreHook{headers: glacier, status: 202}, req: blob.RecallRequest{Days: 7, Speed: blob.RecallBulk}, want: blob.RecallStarted, restores: 1, days: true},
		{name: "already back", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "GLACIER", "x-amz-restore": `ongoing-request="false", expiry-date="Mon, 21 Dec 2026 00:00:00 GMT"`}, status: 200}, req: blob.RecallRequest{Days: 3}, want: blob.RecallOnline, restores: 1, days: true},
		{name: "conflict", hook: restoreHook{headers: glacier, status: 409, code: "RestoreAlreadyInProgress"}, req: blob.RecallRequest{Days: 1}, want: blob.RecallInProgress, restores: 1, days: true},
		{name: "restoring", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "GLACIER", "x-amz-restore": `ongoing-request="true"`}, status: 202}, req: blob.RecallRequest{Days: 1}, want: blob.RecallInProgress},
		{name: "never cold", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "STANDARD"}, status: 202}, req: blob.RecallRequest{Days: 1}, want: blob.RecallOnline},
		{name: "hot, restore unreadable", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "STANDARD_IA", "x-amz-restore": "whatever"}, status: 202}, req: blob.RecallRequest{Days: 1}, want: blob.RecallOnline},
		{name: "tiering archive", hook: restoreHook{headers: deepTiering, status: 202}, req: blob.RecallRequest{Days: 9}, want: blob.RecallStarted, restores: 1},
		{name: "unknown class needs none", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "SOME_NEW_CLASS"}, status: 403, code: "InvalidObjectState"}, req: blob.RecallRequest{Days: 1}, want: blob.RecallOnline, restores: 1, heads: 2, days: true},
		{name: "cold class says none", hook: restoreHook{headers: glacier, status: 403, code: "InvalidObjectState"}, req: blob.RecallRequest{Days: 1}, restores: 1, heads: 2, days: true, kind: fault.Unavailable},
		{name: "back in its active tier", hook: restoreHook{headers: deepTiering, after: map[string]string{"x-amz-storage-class": "INTELLIGENT_TIERING"}, status: 403, code: "ObjectAlreadyInActiveTierError"}, req: blob.RecallRequest{Days: 1}, want: blob.RecallOnline, restores: 1, heads: 2},
		{name: "expedited, deep archive", hook: restoreHook{headers: map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE"}, status: 202}, req: blob.RecallRequest{Days: 1, Speed: blob.RecallExpedited}, kind: fault.Unavailable, refused: true},
		{name: "expedited, deep tiering", hook: restoreHook{headers: deepTiering, status: 202}, req: blob.RecallRequest{Days: 1, Speed: blob.RecallExpedited}, kind: fault.Unavailable, refused: true},
		{name: "expedited not now", hook: restoreHook{headers: glacier, status: 503, code: "GlacierExpeditedRetrievalNotAvailable"}, req: blob.RecallRequest{Days: 1, Speed: blob.RecallExpedited}, restores: 3, days: true, kind: fault.Unavailable, refused: true}, // A 503 is tried three times, as every request is.
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openFake(t, f, nil)
			h := c.hook
			f.hook = h.serve
			ans, err := s.Recall(ctx, "k", c.req)
			if c.kind != 0 {
				if fault.KindOf(err) != c.kind || errors.Is(err, blob.ErrRecallRefused) != c.refused {
					t.Fatalf("Recall = %+v, %v; want %v, ErrRecallRefused %v", ans, err, c.kind, c.refused)
				}
				noLeak(t, c.name, err)
			} else if err != nil || ans.State != c.want {
				t.Fatalf("Recall = %+v, %v; want %v", ans, err, c.want)
			}
			if len(h.bodies) != c.restores {
				t.Fatalf("Recall sent %d restores, want %d", len(h.bodies), c.restores)
			}
			if c.heads != 0 && h.heads != c.heads {
				t.Fatalf("Recall sent %d HEADs, want %d", h.heads, c.heads)
			}
			if c.restores == 0 {
				return
			}
			body := h.bodies[0]
			tier := map[blob.RecallSpeed]string{blob.RecallStandard: "Standard", blob.RecallBulk: "Bulk", blob.RecallExpedited: "Expedited"}[c.req.Speed]
			if !strings.Contains(body, "<Tier>"+tier+"</Tier>") {
				t.Errorf("the restore's body %q does not ask the tier %s", body, tier)
			}
			if gotDays := strings.Contains(body, fmt.Sprintf("<Days>%d</Days>", c.req.Days)); gotDays != c.days {
				t.Errorf("the restore's body %q: days asked %v, want %v", body, gotDays, c.days)
			}
		})
	}

	f := newFakeS3(t)
	s := openFake(t, f, nil)
	if _, err := s.Recall(ctx, "none", blob.RecallRequest{Days: 1}); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("Recall of a key that holds nothing = %v, want NotFound", err)
	}
	if n := f.count(http.MethodPost, "restore"); n != 0 {
		t.Fatalf("Recall of nothing sent %d restores", n)
	}
}

// TestStateOnServer guards, on a real server, what Stat and Encryption read
// from its answers: an object put in one request has the server's own
// SHA-256, equal to the one the SDK's own read computes; one put in parts
// has only a composite checksum, which Stat does not give; both read at
// once; and the server's word on encryption is an answer, not an error.
func TestStateOnServer(t *testing.T) {
	ctx := context.Background()
	cfg := serverConfig(t)
	cfg.PartSize = MinPartSize
	s := openAt(t, cfg)
	small := []byte("one request")
	if _, err := s.Put(ctx, "small", bytes.NewReader(small)); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "small")
	if err != nil || info.State != blob.Online || !info.HasSHA256 {
		t.Fatalf("Stat of an object of one request = %+v, %v; want online with the server's SHA-256", info, err)
	}
	if sum, _ := serverSum(t, s, "small"); info.SHA256 != sum {
		t.Fatalf("the server's SHA-256 %x is not the object's %x", info.SHA256, sum)
	}
	big := bytes.Repeat([]byte("p"), MinPartSize+3)
	w, err := s.Put(ctx, "big", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	info, err = s.Stat(ctx, "big")
	if err != nil || info.State != blob.Online || info.HasSHA256 || info.Size != int64(len(big)) {
		t.Fatalf("Stat of an object of parts = %+v, %v; want online, its size, no whole checksum", info, err)
	}
	t.Logf("a put of parts confirmed by the server's composite checksum: %v", w.Confirmed)
	enc, err := s.Encryption(ctx)
	if err != nil {
		t.Fatalf("Encryption: %v", err)
	}
	t.Logf("the server says of its encryption: %v %q", enc.State, enc.Algorithm)
}

// TestEncryption guards what Encryption reads of GetBucketEncryption: a
// rule's algorithm, with whether a key is named and never the key itself;
// an algorithm this version does not know as "other", so the server's text
// never reaches a status; no configuration as none; credentials that may
// not ask and a server without the call as unknown, not an error; wrong
// credentials and a server that fails as an error, since unknown would hide
// a storage that cannot be used.
func TestEncryption(t *testing.T) {
	ctx := context.Background()
	rule := func(alg, key string) string {
		k := ""
		if key != "" {
			k = "<KMSMasterKeyID>" + key + "</KMSMasterKeyID>"
		}
		return `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>` + alg + `</SSEAlgorithm>` + k + `</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`
	}
	for _, c := range []struct {
		name   string
		status int
		code   string
		body   string
		want   blob.Encryption
		fails  bool
	}{
		{name: "aes", status: 200, body: rule("AES256", ""), want: blob.Encryption{State: blob.EncryptionOn, Algorithm: "AES256"}},
		{name: "kms with a key", status: 200, body: rule("aws:kms", "arn:aws:kms:eu-central-1:111122223333:key/secret-key-name"), want: blob.Encryption{State: blob.EncryptionOn, Algorithm: "aws:kms", KeyNamed: true}},
		{name: "dsse", status: 200, body: rule("aws:kms:dsse", ""), want: blob.Encryption{State: blob.EncryptionOn, Algorithm: "aws:kms:dsse"}},
		{name: "unknown algorithm", status: 200, body: rule("server-words", ""), want: blob.Encryption{State: blob.EncryptionOn, Algorithm: "other"}},
		{name: "empty rule", status: 200, body: `<ServerSideEncryptionConfiguration><Rule></Rule></ServerSideEncryptionConfiguration>`, want: blob.Encryption{}},
		{name: "none", status: 404, code: "ServerSideEncryptionConfigurationNotFoundError", want: blob.Encryption{State: blob.EncryptionNone}},
		{name: "may not ask", status: 403, code: "AccessDenied", want: blob.Encryption{}},
		{name: "not implemented", status: 501, code: "NotImplemented", want: blob.Encryption{}},
		{name: "wrong key id", status: 403, code: "InvalidAccessKeyId", fails: true},
		{name: "wrong signature", status: 403, code: "SignatureDoesNotMatch", fails: true},
		{name: "server failing", status: 500, code: "InternalError", fails: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openFake(t, f, nil)
			f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.Method != http.MethodGet || !r.URL.Query().Has("encryption") {
					return false
				}
				if c.code != "" {
					xmlError(w, c.status, c.code, "secret-server-words")
					return true
				}
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
				return true
			}
			got, err := s.Encryption(ctx)
			if c.fails {
				if fault.KindOf(err) != fault.Unavailable {
					t.Fatalf("Encryption = %+v, %v; want Unavailable", got, err)
				}
				noLeak(t, c.name, err)
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Encryption = %+v, %v; want %+v", got, err, c.want)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), "secret-key-name") {
				t.Fatal("the answer holds the key's name")
			}
		})
	}
}
