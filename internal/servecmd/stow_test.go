package servecmd_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
)

// ctStudyUID is the Study Instance UID of ctStudy.
const ctStudyUID = "1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.1"

// stowSite is a real server process with a tenant whose stow receiver
// takes requests from one key: the server's network address, the key, a
// key with the same role the receiver does not list, and the tenant's
// storage on disk.
type stowSite struct {
	t        *testing.T
	dataDir  string
	listen   string
	tenant   string
	key      string
	keyID    string
	unlisted string
	storage  string
	child    *child
	hc       *http.Client
	// serveArgs are flags given to serve after the data directory and the
	// address, such as a shorter default deadline.
	serveArgs []string
}

// newSTOWSite starts serve on a new installation listening on loopback,
// with tenant stow1, a directory storage, two keys with writer on the
// tenant and a stow receiver that lists the first, whose largest file is
// maxFile bytes and quiet period 5 seconds. It waits until the receiver
// is up. serveArgs are given to serve as well.
func newSTOWSite(t *testing.T, maxFile int, serveArgs ...string) *stowSite {
	t.Helper()
	s := &stowSite{t: t, dataDir: shortDir(t), tenant: "stow1", serveArgs: serveArgs}
	if _, err := run(t, nil, "init", "--data-dir", s.dataDir); err != nil {
		t.Fatal(err)
	}
	s.listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	s.start()
	roots := filepath.Join(filepath.Dir(s.dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	s.storage = filepath.Join(roots, s.tenant, "main")
	hc := s.hc
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+s.tenant+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	// The receiver archives as received, which a tenant made now allows
	// only when the installation's admin says so.
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+s.tenant+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+s.tenant+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	s.key, s.keyID = s.makeKey()
	s.unlisted, _ = s.makeKey()
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+s.tenant+`","receiver":{"name":"web","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","maxFileBytes":"`+strconv.Itoa(maxFile)+`","stow":{"keyIds":["`+s.keyID+`"]}}}`)
	waitWithin(t, "the stow receiver to be up", 10*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+s.tenant+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})
	return s
}

// start starts serve on the site's data directory and address, and waits
// until it answers.
func (s *stowSite) start() {
	s.t.Helper()
	s.child = startChildEnv(s.t, []string{childRealEnv + "=1"}, append([]string{"serve", "--data-dir", s.dataDir, "--listen", s.listen}, s.serveArgs...)...)
	waitServing(s.t, s.dataDir, s.child)
	if s.hc != nil {
		s.hc.CloseIdleConnections()
	}
	s.hc = socketClient(s.dataDir)
	if len(s.serveArgs) > 0 {
		// A server given flags of its own may answer a call that sends no
		// deadline sooner than the setup's calls take; they send one, as a
		// client with a deadline does.
		s.hc.Transport = withDeadline{s.hc.Transport}
	}
	s.t.Cleanup(s.hc.CloseIdleConnections)
}

// withDeadline sends every request with a Connect deadline of a minute.
type withDeadline struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (d withDeadline) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Connect-Timeout-Ms", "60000")
	return d.next.RoundTrip(r)
}

// makeKey makes a key with writer on the site's tenant, as the local
// operator, and returns its secret and id.
func (s *stowSite) makeKey() (secret, id string) {
	s.t.Helper()
	var made struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(post(s.t, s.hc, "/nopacs.v1.AccessService/CreateKey", `{"holder":"stow test"}`), &made); err != nil || made.Secret == "" {
		s.t.Fatalf("CreateKey: %v", err)
	}
	post(s.t, s.hc, "/nopacs.v1.AccessService/SetGrant", `{"keyId":"`+made.Key.ID+`","scope":{"tenant":"`+s.tenant+`"},"role":"ROLE_WRITER"}`)
	return made.Secret, made.Key.ID
}

// stowPart is one part of a STOW-RS request: a corpus file's name, or
// bytes that are not a file.
type stowPart struct {
	file string
	raw  []byte
}

// testdata returns a corpus file's bytes.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// store sends one STOW-RS request into path (after dicom-web/) with key
// over the network, and returns the status and the body.
func (s *stowSite) store(key, path string, parts ...stowPart) (int, []byte) {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		b := p.raw
		if p.file != "" {
			b = testdata(s.t, p.file)
		}
		w, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/dicom"}})
		if err != nil {
			s.t.Fatal(err)
		}
		_, _ = w.Write(b)
	}
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(s.t.Context(), http.MethodPost, "http://"+s.listen+"/tenants/"+s.tenant+"/dicom-web/"+path, &buf)
	req.Header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+mw.Boundary())
	req.Header.Set("Accept", "application/dicom+json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

// incomingFiles returns the files of the site's incoming area.
func (s *stowSite) incomingFiles() []string {
	var out []string
	_ = filepath.WalkDir(filepath.Join(s.dataDir, "incoming"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// waitArchived waits until every reception of the tenant is done and
// returns how many files they archived together.
func (s *stowSite) waitArchived() int {
	s.t.Helper()
	var list struct {
		Receptions []struct {
			Step   string `json:"step"`
			Counts struct {
				FilesArchived string `json:"filesArchived"`
			} `json:"counts"`
		} `json:"receptions"`
	}
	archived := 0
	waitBody(s.t, "the receptions to be done", func() bool {
		if err := json.Unmarshal(post(s.t, s.hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+s.tenant+`"}`), &list); err != nil {
			s.t.Fatal(err)
		}
		archived = 0
		for _, r := range list.Receptions {
			if r.Step == "RECEPTION_STEP_FAILED" {
				s.t.Fatalf("a reception failed:\n%s", s.child.stderr.String())
			}
			if r.Step != "RECEPTION_STEP_DONE" {
				return false
			}
			n, _ := strconv.Atoi(r.Counts.FilesArchived)
			archived += n
		}
		return len(list.Receptions) > 0
	})
	return archived
}

// openStorage opens the tenant's storage as the archive wrote it.
func (s *stowSite) openStorage() blob.Store {
	s.t.Helper()
	st, err := dir.Open(s.storage)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = st.Close() })
	return st
}

// events reads the tenant's events from the first kept, through Watch as
// the local operator, until one holds each of want or a keep-alive follows
// the last event, and returns each event's JSON.
func (s *stowSite) events(want ...string) []string {
	s.t.Helper()
	payload := []byte(`{"scope":{"installation":{}},"resume":true}`)
	frame := append([]byte{0, 0, 0, 0, 0}, payload...)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload))) //nolint:gosec // G115: a short constant body.
	req, _ := http.NewRequestWithContext(s.t.Context(), http.MethodPost, "http://local/nopacs.v1.MonitorService/Watch", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	hc := &http.Client{Transport: s.hc.Transport, Timeout: 60 * time.Second}
	res, err := hc.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	rd := bufio.NewReader(res.Body)
	var out []string
	for keepAlives := 0; keepAlives < 2; {
		var head [5]byte
		if _, err := io.ReadFull(rd, head[:]); err != nil {
			s.t.Fatalf("Watch ended: %v", err)
		}
		msg := make([]byte, binary.BigEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(rd, msg); err != nil {
			s.t.Fatal(err)
		}
		switch {
		case head[0]&2 != 0:
			s.t.Fatalf("Watch ended: %s", msg)
		case bytes.Contains(msg, []byte(`"keepAlive"`)):
			keepAlives++
		default:
			out = append(out, string(msg))
			all := strings.Join(out, "\n")
			found := 0
			for _, w := range want {
				if strings.Contains(all, w) {
					found++
				}
			}
			if found == len(want) {
				return out
			}
		}
	}
	return out
}

// TestAStudyStoredBySTOWIsArchived guards the stow receiver in a real
// server process, over the network with a key: a request into the CT
// study holding its seven instances, an instance of another study, one
// larger than the receiver's largest file and bytes that are not DICOM is
// answered 202, the seven listed with their Retrieve URLs and the two
// refused with A900H and A700H; a key with the same role the receiver
// does not list is refused 403. The server is then killed at once, before
// the quiet period could end: the seven files acknowledged are in the
// incoming area, and a new server archives them, byte for byte as sent,
// each listed by its scan's manifest with its checksum, with the reception
// done and the part too large quarantined among the tenant's events, and
// nothing left in the incoming area.
func TestAStudyStoredBySTOWIsArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process twice and stores a study over the network; left out under -short")
	}
	s := newSTOWSite(t, 20000)
	parts := []stowPart{}
	for _, f := range ctStudy {
		parts = append(parts, stowPart{file: f})
	}
	parts = append(parts, stowPart{file: "MR_small.dcm"}, stowPart{file: "liver_1frame.dcm"}, stowPart{raw: []byte("not a DICOM file")})
	status, body := s.store(s.key, "studies/"+ctStudyUID, parts...)
	if status != http.StatusAccepted {
		t.Fatalf("the mixed request: %d %s", status, body)
	}
	var answer map[string]struct {
		Value []json.RawMessage `json:"Value"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	stored, failed := answer["00081199"].Value, answer["00081198"].Value
	if len(stored) != len(ctStudy) || len(failed) != 2 || !bytes.Contains(failed[0], []byte(`[43264]`)) || !bytes.Contains(failed[1], []byte(`[42752]`)) {
		t.Fatalf("the answer lists %d stored and %d failed: %s", len(stored), len(failed), body)
	}
	if !bytes.Contains(stored[0], []byte(`"http://`+s.listen+`/tenants/`+s.tenant+`/dicom-web/studies/`+ctStudyUID+`/series/`)) {
		t.Errorf("a Retrieve URL is not the instance's WADO-RS URL: %s", stored[0])
	}
	if status, body := s.store(s.unlisted, "studies", stowPart{file: ctStudy[0]}); status != http.StatusForbidden {
		t.Errorf("a key the receiver does not list: %d %s", status, body)
	}

	if err := s.child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-s.child.done; err == nil {
		t.Fatal("the killed server exited cleanly")
	}
	if got := s.incomingFiles(); len(got) != len(ctStudy) {
		t.Fatalf("after the crash the incoming area holds %d files, want the %d acknowledged", len(got), len(ctStudy))
	}
	s.start()
	if n := s.waitArchived(); n != len(ctStudy) {
		t.Fatalf("%d files archived, want %d", n, len(ctStudy))
	}

	store := s.openStorage()
	checkStored(t, store, len(ctStudy))
	sent := map[string]bool{}
	for _, f := range ctStudy {
		sum := sha256.Sum256(testdata(t, f))
		sent[hex.EncodeToString(sum[:])] = true
	}
	for k, b := range storedManifests(t, store) {
		var m struct {
			Files []manifestFile `json:"files"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, f := range m.Files {
			if !sent[f.SHA256] {
				t.Errorf("%s lists a file whose bytes are not those of any file sent", k)
			}
			delete(sent, f.SHA256)
		}
	}
	if len(sent) != 0 {
		t.Errorf("%d files sent are not archived as sent", len(sent))
	}
	events := strings.Join(s.events(`"RECEPTION_STEP_DONE"`, `"QUARANTINE_REASON_TOO_LARGE"`), "\n")
	if !strings.Contains(events, `"RECEPTION_STEP_DONE"`) || !strings.Contains(events, `"QUARANTINE_REASON_TOO_LARGE"`) {
		t.Errorf("the events lack the reception done or the part too large:\n%s", events)
	}
	waitWithin(t, "the incoming area to be empty", 10*time.Second, func() bool { return len(s.incomingFiles()) == 0 })
}

// stowPythonEnv names the Python, with dicomweb-client and pydicom, that
// runs the outside check of STOW-RS (docs/testing.md). Set, even to
// nothing, it means the check was asked for, and a missing interpreter
// fails it.
const stowPythonEnv = "NOPACS_CHECK_STOW_PYTHON"

// TestSTOWWithDicomwebClient guards STOW-RS against a client that is not
// NoPACs', in a real server process: scripts/check/stow-dicomweb-client.py
// stores the CT study with dicomweb-client and checks the answer (each
// instance listed with its WADO-RS Retrieve URL; an instance of another
// study refused 409 with A900H), and once the reception is done compares
// the archived files with what was sent, byte for byte with hashlib
// against each scan's manifest, and as data sets with pydicom. The key
// reaches the script in its environment. The script's output is logged.
func TestSTOWWithDicomwebClient(t *testing.T) {
	python, asked := os.LookupEnv(stowPythonEnv)
	if !asked {
		t.Skip(stowPythonEnv + " is not set; it names a Python with dicomweb-client and pydicom")
	}
	if python == "" {
		t.Fatal(stowPythonEnv + " is empty; name an interpreter that has dicomweb-client and pydicom")
	}
	s := newSTOWSite(t, 1<<20)
	script := filepath.Join("..", "..", "scripts", "check", "stow-dicomweb-client.py")
	var files []string
	for _, f := range ctStudy {
		files = append(files, filepath.Join("..", "receive", "testdata", f))
	}
	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), python, append([]string{"-I", script}, args...)...) //nolint:gosec // G204: the operator's interpreter and the repository's script.
		cmd.Env = append(os.Environ(), "NOPACS_STOW_KEY="+s.key)
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("stow-dicomweb-client.py %s: %v", args[0], err)
		}
	}
	runScript(append([]string{"store", "http://" + s.listen + "/tenants/" + s.tenant + "/dicom-web", ctStudyUID,
		filepath.Join("..", "receive", "testdata", "MR_small.dcm")}, files...)...)
	if n := s.waitArchived(); n != len(ctStudy) {
		t.Fatalf("%d files archived, want %d", n, len(ctStudy))
	}
	runScript(append([]string{"compare", s.storage}, files...)...)
}

// rotate rotates the key id with an overlap (a Duration's JSON form) and
// returns the successor's secret and id.
func (s *stowSite) rotate(id, overlap string) (secret, successor string) {
	s.t.Helper()
	var rot struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	body := post(s.t, s.hc, "/nopacs.v1.AccessService/RotateKey", `{"id":"`+id+`","overlap":"`+overlap+`"}`)
	if err := json.Unmarshal(body, &rot); err != nil || rot.Secret == "" {
		s.t.Fatalf("RotateKey: %v %s", err, body)
	}
	return rot.Secret, rot.Key.ID
}

// TestSTOWTakesAKeyRotatedFromTheListedOne guards a system that rotates
// the key a stow receiver lists, the receiver left as it is: the successor
// stores once the rotated key has ended (an overlap of zero), and so does
// the successor's own successor while its predecessor still works beside
// it; the ended key is refused 401 and a key of the same role that is not
// of the line is still refused 403.
func TestSTOWTakesAKeyRotatedFromTheListedOne(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s := newSTOWSite(t, 1<<20)
	second, secondID := s.rotate(s.keyID, "0s")
	if status, body := s.store(second, "studies", stowPart{file: ctStudy[0]}); status != http.StatusOK {
		t.Fatalf("the successor of the listed key, the listed key ended: %d %s", status, body)
	}
	if status, _ := s.store(s.key, "studies", stowPart{file: ctStudy[1]}); status != http.StatusUnauthorized {
		t.Errorf("the listed key after its rotation ended it: %d, want 401", status)
	}
	third, _ := s.rotate(secondID, "3600s")
	if status, body := s.store(third, "studies", stowPart{file: ctStudy[1]}); status != http.StatusOK {
		t.Fatalf("two rotations from the listed key: %d %s", status, body)
	}
	if status, body := s.store(second, "studies", stowPart{file: ctStudy[2]}); status != http.StatusOK {
		t.Errorf("a rotated key within its overlap: %d %s", status, body)
	}
	if status, _ := s.store(s.unlisted, "studies", stowPart{file: ctStudy[3]}); status != http.StatusForbidden {
		t.Errorf("a key of no listed line: %d, want 403", status)
	}
}

// TestSTOWHoldsAFileWhosePlacingUIDsDiffer guards a part whose first
// bytes name the study the request stores into while its whole header
// names another: a corpus file with 80 KiB of Data Set Trailing Padding
// (FFFC,FFFC) and then a second Study Instance UID appended, out of the
// order of tags and past the bytes the receiver reads to identify a part.
// The part passes the study check and is answered stored, and the
// pipeline then holds it as uid_mismatch, its bytes kept, never archiving
// it into the other study.
func TestSTOWHoldsAFileWhosePlacingUIDsDiffer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s := newSTOWSite(t, 1<<20)
	// Explicit VR little endian (PS3.5 7.1.2): (FFFC,FFFC) OB, a 4-byte
	// length; then (0020,000D) UI "1.2.3.999\0", a 2-byte length.
	padding := []byte{0xFC, 0xFF, 0xFC, 0xFF, 'O', 'B', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(padding[8:], 80<<10)
	padding = append(padding, make([]byte, 80<<10)...)
	value := "1.2.3.999\x00"
	elem := []byte{0x20, 0x00, 0x0D, 0x00, 'U', 'I', byte(len(value)), 0} // #nosec G115 -- a 10-byte value.
	moved := append(append(append(testdata(t, ctStudy[0]), padding...), elem...), value...)
	if status, body := s.store(s.key, "studies/"+ctStudyUID, stowPart{raw: moved}); status != http.StatusOK {
		t.Fatalf("the part: %d %s", status, body)
	}
	var list struct {
		Files []struct {
			Reason      string            `json:"reason"`
			Details     map[string]string `json:"details"`
			ContentKept bool              `json:"contentKept"`
		} `json:"files"`
	}
	waitWithin(t, "the file held", 20*time.Second, func() bool {
		_ = json.Unmarshal(post(t, s.hc, "/nopacs.v1.QuarantineService/ListQuarantinedFiles", `{"tenant":"`+s.tenant+`"}`), &list)
		return len(list.Files) > 0
	})
	f := list.Files[0]
	if len(list.Files) != 1 || f.Reason != "QUARANTINE_REASON_UID_MISMATCH" || f.Details["attribute"] != "StudyInstanceUID" || !f.ContentKept {
		t.Fatalf("held %+v", list.Files)
	}
}

// TestSTOWEndsWhenItsKeyIsRevokedPartWay guards a long STOW-RS request
// whose key is revoked while its body still arrives, in a real server
// process over the network: the first part, whole before the revocation,
// is kept and archived; once the server has checked the key again (it
// does every 15 seconds, as for a long read) the request ends at its next
// read of the body, the parts sent after are not kept, the client is
// answered 401 as a new request with that key would be, and the
// installation's audit log records the request refused with its cause.
func TestSTOWEndsWhenItsKeyIsRevokedPartWay(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and holds a request open past the key's recheck; left out under -short")
	}
	s := newSTOWSite(t, 1<<20)
	const boundary = "revokedPartWay"
	head := "--" + boundary + "\r\nContent-Type: application/dicom\r\n\r\n"
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(s.t.Context(), http.MethodPost, "http://"+s.listen+"/tenants/"+s.tenant+"/dicom-web/studies", pr)
	req.Header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+boundary)
	req.Header.Set("Accept", "application/dicom+json")
	req.Header.Set("Authorization", "Bearer "+s.key)
	type answer struct {
		status int
		body   []byte
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		answered <- answer{status: res.StatusCode, body: b}
	}()
	// The first part, closed by the next part's header so the server takes
	// it as whole and waits in the second part's bytes.
	first := append(append([]byte(head), testdata(t, ctStudy[0])...), "\r\n"+head...)
	if _, err := pw.Write(first); err != nil {
		t.Fatal(err)
	}
	waitWithin(t, "the first part to be kept", 10*time.Second, func() bool { return len(s.incomingFiles()) == 1 })
	post(t, s.hc, "/nopacs.v1.AccessService/RevokeKey", `{"id":"`+s.keyID+`"}`)

	var log struct {
		Entries []struct {
			KeyID   string `json:"keyId"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
			Method  string `json:"method"`
		} `json:"entries"`
	}
	refusal := ""
	waitWithin(t, "the key's recheck to refuse the request", 40*time.Second, func() bool {
		if err := json.Unmarshal(post(t, s.hc, "/nopacs.v1.AuditService/ListAuditEntries", `{"scope":{"installation":{}},"keyId":"`+s.keyID+`","outcome":"AUDIT_OUTCOME_REFUSED"}`), &log); err != nil {
			t.Fatal(err)
		}
		for _, e := range log.Entries {
			if e.KeyID == s.keyID && e.Outcome == "AUDIT_OUTCOME_REFUSED" && strings.Contains(e.Method, "dicom-web") {
				refusal = e.Reason
				return true
			}
		}
		return false
	})
	if refusal != "key revoked since the check" {
		t.Errorf("the audit entry's reason: %q", refusal)
	}

	go func() {
		rest := append(append(testdata(t, ctStudy[1]), "\r\n"+head...), testdata(t, ctStudy[2])...)
		_, _ = pw.Write(append(rest, "\r\n--"+boundary+"--\r\n"...))
		_ = pw.Close()
	}()
	var got answer
	select {
	case got = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the request was not answered after its key's refusal")
	}
	if got.err != nil {
		t.Fatalf("the request: %v", got.err)
	}
	if got.status != http.StatusUnauthorized || !strings.Contains(string(got.body), "the request carries no valid key") {
		t.Fatalf("the request after its key was revoked: %d %s, want 401", got.status, got.body)
	}
	if n := s.waitArchived(); n != 1 {
		t.Fatalf("%d files archived, want the first part only", n)
	}
	if got := s.incomingFiles(); len(got) != 0 {
		t.Errorf("the incoming area holds %d files after the reception", len(got))
	}
}
