package servecmd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// dcmtkEnv names a directory holding dcmtk's programs, looked in before the
// PATH, where pynetdicom's programs of the same names may come first;
// requireDcmtkEnv, set to anything, makes a missing storescu a failure, as
// in the CI jobs that run this test for Postgres and S3. The Postgres and S3
// cases take their services from testkit.
const (
	dcmtkEnv        = "NOPACS_DCMTK"
	requireDcmtkEnv = "NOPACS_REQUIRE_DCMTK"
)

// ctStudy is the public CT study of the pipeline's test files: one study,
// two series, seven instances.
var ctStudy = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}

// storescu returns dcmtk's storescu, or skips the test; it is called before
// anything is started.
func storescu(t *testing.T) string {
	t.Helper()
	var candidates []string
	if d := os.Getenv(dcmtkEnv); d != "" {
		candidates = append(candidates, filepath.Join(d, "storescu"))
	}
	if p, err := exec.LookPath("storescu"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _ := exec.CommandContext(ctx, p, "--version").CombinedOutput() //nolint:gosec // G204: a test's own tool.
		cancel()
		if bytes.Contains(out, []byte("dcmtk")) {
			return p
		}
	}
	if os.Getenv(requireDcmtkEnv) != "" {
		t.Fatalf("dcmtk's storescu is not installed, and %s is set", requireDcmtkEnv)
	}
	t.Skip("dcmtk's storescu is not installed; put it on the PATH or set " + dcmtkEnv)
	return ""
}

// receptionCase is where the end-to-end test keeps a tenant and its storage.
type receptionCase struct {
	name string
	// tenant is the CreateTenant body's database fields.
	tenant string
	// s3 is the scratch S3 server's address, empty for a directory storage.
	s3 string
}

// TestAStudyIsArchivedThroughServe guards the reception as serve wires it,
// in a real server process: a tenant made by CreateTenant, a storage and a
// receiver added by their services, the CT study sent by dcmtk's storescu,
// and the reception followed by ListReceptions to done. The tenant's
// naming key names the labels (hashed, so no UID of the study is in a
// path), the storage is opened by the same rules as the handlers', every
// instance is on the storage listed by its scan's manifest with the
// checksum of its bytes, and nothing is left in the incoming area. It runs
// on SQLite with a directory storage, and also on Postgres and on S3 when
// their addresses are set.
func TestAStudyIsArchivedThroughServe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and sends a study over the network; left out under -short")
	}
	scu := storescu(t)
	cases := []receptionCase{{name: "sqlite-directory", tenant: `"databaseType":"DATABASE_TYPE_SQLITE"`}}
	if a, ok := testkit.PostgresSchema(t); ok {
		raw, _ := json.Marshal(a)
		cases = append(cases, receptionCase{name: "postgres-directory", tenant: `"databaseType":"DATABASE_TYPE_POSTGRES","databaseAddress":` + string(raw)})
	}
	if a, ok := testkit.S3Address(t); ok {
		cases = append(cases, receptionCase{name: "sqlite-s3", tenant: `"databaseType":"DATABASE_TYPE_SQLITE"`, s3: a})
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) { archiveThroughServe(t, scu, c, i) })
	}
}

// freePort returns a TCP port on loopback no one listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// post sends a Connect JSON request on the socket as the local operator and
// returns the answer's body, failing the test unless it is 200.
func post(t *testing.T, hc *http.Client, procedure, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://local"+procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", procedure, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", procedure, resp.StatusCode, out)
	}
	return out
}

// quoted returns s as a JSON string.
func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func archiveThroughServe(t *testing.T, scu string, c receptionCase, n int) {
	dataDir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	child := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir)
	waitServing(t, dataDir, child)
	hc := socketClient(dataDir)
	defer hc.CloseIdleConnections()

	tenantName := fmt.Sprintf("e2e%d", n)
	roots := filepath.Join(filepath.Dir(dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tenantName+`",`+c.tenant+`}`)
	port := freePort(t)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"receiver.ports","scope":{"tenant":"`+tenantName+`"},"value":"`+strconv.Itoa(port)+`"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+tenantName+`"},"value":"true"}}`)

	var open func() blob.Store
	if c.s3 == "" {
		post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tenantName+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
		open = func() blob.Store {
			s, err := dir.Open(filepath.Join(roots, tenantName, "main"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
	} else {
		u, err := url.Parse(c.s3)
		if err != nil || u.User == nil || len(u.Path) < 2 {
			t.Fatalf("%s is not an http URL with a key, a secret and a bucket", testkit.S3Env)
		}
		secret, _ := u.User.Password()
		region := u.Query().Get("region")
		if region == "" {
			region = "us-east-1"
		}
		endpoint := u.Scheme + "://" + u.Host
		entry := endpoint + " private-address"
		if u.Scheme == "http" {
			entry = endpoint + " plain-http private-address"
		}
		post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
			`{"setting":{"name":"storage.s3-endpoints","scope":{"installation":{}},"value":`+quoted(entry)+`}}`)
		prefix := fmt.Sprintf("serve-e2e-%d-%d/", time.Now().UnixNano(), n)
		post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tenantName+`","storage":{"name":"main","s3":{"endpoint":`+quoted(endpoint)+
			`,"region":`+quoted(region)+`,"bucket":`+quoted(u.Path[1:])+`,"prefix":`+quoted(prefix)+`,"pathStyle":true,"accessKeyId":`+
			quoted(u.User.Username())+`,"secretAccessKey":`+quoted(secret)+`}}}`)
		open = func() blob.Store {
			s, err := s3.Open(t.Context(), s3.Config{
				Endpoint: endpoint, Region: region, Bucket: u.Path[1:], Prefix: prefix, PathStyle: true,
				AccessKeyID: s3.NewSecret(u.User.Username()), SecretAccessKey: s3.NewSecret(secret),
				AllowHTTP: u.Scheme == "http", AllowAddress: func(netip.Addr) bool { return true }, StaleUploadAge: -1,
			})
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
	}
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tenantName+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	// Within seconds, well inside the supervisor's periodic read (30
	// seconds): the handler's notice of the new receiver starts it at once.
	waitWithin(t, "the receiver to be up", 10*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tenantName+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})

	args := []string{"-v", "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port)}
	for _, f := range ctStudy {
		args = append(args, filepath.Join("..", "receive", "testdata", f))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, scu, args...).CombinedOutput() //nolint:gosec // G204: a test's own tool and files.
	if err != nil || bytes.Count(out, []byte("Received Store Response (Success)")) != len(ctStudy) {
		t.Fatalf("storescu: %v\n%s", err, out)
	}

	var list struct {
		Receptions []struct {
			Step         string `json:"step"`
			SubjectLabel string `json:"subjectLabel"`
			Counts       struct {
				FilesArchived string `json:"filesArchived"`
			} `json:"counts"`
		} `json:"receptions"`
	}
	waitBody(t, "the reception to be done", func() bool {
		if err := json.Unmarshal(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tenantName+`"}`), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Receptions) == 1 && list.Receptions[0].Step == "RECEPTION_STEP_FAILED" {
			t.Fatalf("the reception failed:\n%s", child.stderr.String())
		}
		return len(list.Receptions) == 1 && list.Receptions[0].Step == "RECEPTION_STEP_DONE"
	})
	r := list.Receptions[0]
	if r.Counts.FilesArchived != strconv.Itoa(len(ctStudy)) || !strings.HasPrefix(r.SubjectLabel, "sub-") || len(r.SubjectLabel) != 4+16 {
		t.Fatalf("reception %+v, want %d archived under a hashed subject label", r, len(ctStudy))
	}

	// A removal is acted on at once too: the API answers a removed
	// receiver without asking its health, so only the handler's notice
	// stops it before the supervisor's periodic read (30 seconds).
	post(t, hc, "/nopacs.v1.ReceiverService/RemoveReceiver", `{"tenant":"`+tenantName+`","name":"scanner"}`)
	waitWithin(t, "the removed receiver's port to close", 10*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	})

	store := open()
	defer func() { _ = store.Close() }()
	checkStored(t, store, len(ctStudy))
	checkDownloads(t, hc, dataDir, tenantName, store)
	checkDICOMweb(t, hc, tenantName, store)
	checkCLIDownload(t, dataDir, tenantName, store)
	var left []string
	_ = filepath.WalkDir(filepath.Join(dataDir, "incoming"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("the incoming area holds %v", left)
	}
}

// hashed reports whether label is prefix and 16 characters of a keyed hash
// (lower case base32).
func hashed(label, prefix string) bool {
	h, ok := strings.CutPrefix(label, prefix)
	return ok && len(h) == 16 && strings.Trim(h, "abcdefghijklmnopqrstuvwxyz234567") == ""
}

// checkStored checks the storage: the manifests together list want
// instances, each at a key under data/ whose bytes have the manifest's
// size and SHA-256, and no key under data/ is listed by none; every key's
// labels are hashed, so none holds an identifier the sender gave.
func checkStored(t *testing.T, store blob.Store, want int) {
	t.Helper()
	ctx := t.Context()
	keys := func(prefix string) []string {
		var out []string
		opts := blob.ListOptions{Prefix: prefix}
		for {
			page, err := store.List(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, page.Keys...)
			if page.Next == "" {
				return out
			}
			opts.Token = page.Next
		}
	}
	data := map[string]bool{}
	for _, k := range keys("data/") {
		data[k] = true
		// data/<subject>/<session>/<scan>/<SOP Instance UID>.dcm, each label
		// the default rule's: a prefix and 16 characters of a keyed hash.
		parts := strings.Split(k, "/")
		if len(parts) != 5 || !hashed(parts[1], "sub-") || !hashed(parts[2], "ses-") || !hashed(parts[3], "scan-") {
			t.Errorf("a key's labels are not the default rule's hashed labels: %s", k)
		}
	}
	listed := 0
	for _, mk := range keys("meta/") {
		if !strings.HasSuffix(mk, "/manifest.json") {
			continue
		}
		rc, err := store.Read(ctx, mk, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Files []struct {
				Key    string `json:"key"`
				Size   int64  `json:"size"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		}
		err = json.NewDecoder(rc).Decode(&m)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", mk, err)
		}
		for _, f := range m.Files {
			rc, err := store.Read(ctx, f.Key, 0, -1)
			if err != nil {
				t.Fatalf("%s lists %s: %v", mk, f.Key, err)
			}
			h := sha256.New()
			size, err := io.Copy(h, rc)
			_ = rc.Close()
			if err != nil || size != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
				t.Fatalf("%s: %s read %d bytes (%v) with another checksum than listed", mk, f.Key, size, err)
			}
			if !data[f.Key] {
				t.Fatalf("%s lists %s, which is not under data/", mk, f.Key)
			}
			delete(data, f.Key)
			listed++
		}
	}
	if listed != want || len(data) != 0 {
		t.Fatalf("the manifests list %d instances, want %d; unlisted objects: %v", listed, want, data)
	}
}

// waitBody polls cond until it holds, failing the test with what it waited
// for after a minute.
func waitBody(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitWithin(t, what, time.Minute, cond)
}

// waitWithin polls cond until it holds, failing the test with what it
// waited for after d.
func waitWithin(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
