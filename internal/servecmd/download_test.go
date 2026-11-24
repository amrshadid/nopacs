package servecmd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/server"
)

// downloadPythonEnv names the Python that runs the download's outside
// check, listed in docs/testing.md. Set, even to nothing, it means the
// check was asked for, and a missing interpreter or curl fails it.
const downloadPythonEnv = "NOPACS_CHECK_DOWNLOAD_PYTHON"

// manifestFile is a file as a scan's manifest on the storage lists it.
type manifestFile struct {
	Key            string `json:"key"`
	Offset         int64  `json:"offset"`
	Length         int64  `json:"length"`
	SHA256         string `json:"sha256"`
	SOPInstanceUID string `json:"sop_instance_uid"`
}

// storedManifests returns every scan manifest the storage holds, by key,
// as the bytes stored.
func storedManifests(t *testing.T, store blob.Store) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	opts := blob.ListOptions{Prefix: "meta/"}
	for {
		page, err := store.List(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Keys {
			if !strings.HasSuffix(k, "/manifest.json") {
				continue
			}
			rc, err := store.Read(t.Context(), k, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			out[k] = b
		}
		if page.Next == "" {
			return out
		}
		opts.Token = page.Next
	}
}

// checkDownloads reads every instance a reception archived back through
// the real server's local socket, and holds each to its scan's manifest
// on the storage: the plain HTTP download whole (200, its ETag the
// manifest's SHA-256, a body with that SHA-256), a range of it (206, the
// same bytes) and a range past its end (416); and ReadFiles of each
// subject, whose announcements are the manifests' entries and whose bytes
// have their SHA-256. With downloadPythonEnv set it runs
// scripts/check/download-curl.py on the same socket, which downloads each
// instance with curl and checks it with Python's hashlib.
func checkDownloads(t *testing.T, hc *http.Client, dataDir, tenantName string, store blob.Store) {
	t.Helper()
	manifests := storedManifests(t, store)
	files := map[string]manifestFile{}
	subjects := map[string]bool{}
	for k, b := range manifests {
		var m struct {
			Files []manifestFile `json:"files"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, f := range m.Files {
			files[f.SOPInstanceUID] = f
			parts := strings.Split(f.Key, "/")
			if len(parts) != 5 {
				t.Fatalf("%s lists a key not of the data tree: %s", k, f.Key)
			}
			subjects[parts[1]] = true
		}
	}
	if len(files) == 0 {
		t.Fatal("the storage lists no instance")
	}
	get := func(path string, headers ...string) (int, http.Header, []byte) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://local"+path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("GET: the body: %v", err)
		}
		return res.StatusCode, res.Header, b
	}
	for _, f := range files {
		parts := strings.Split(strings.TrimSuffix(f.Key, ".dcm"), "/")
		path := "/tenants/" + tenantName + "/instances/" + strings.Join(parts[1:], "/")
		status, h, body := get(path)
		sum := sha256.Sum256(body)
		if status != http.StatusOK || int64(len(body)) != f.Length || hex.EncodeToString(sum[:]) != f.SHA256 || h.Get("ETag") != `"`+f.SHA256+`"` {
			t.Errorf("the download of an instance: %d, %d bytes with SHA-256 %x and ETag %s; the manifest has %d and %s", status, len(body), sum, h.Get("ETag"), f.Length, f.SHA256)
			continue
		}
		if status, _, part := get(path, "Range", "bytes=1-10"); status != http.StatusPartialContent || !bytes.Equal(part, body[1:11]) {
			t.Errorf("a range of an instance: %d", status)
		}
		if status, h, _ := get(path, "Range", "bytes="+strconv.FormatInt(f.Length, 10)+"-"); status != http.StatusRequestedRangeNotSatisfiable ||
			h.Get("Content-Range") != "bytes */"+strconv.FormatInt(f.Length, 10) {
			t.Errorf("a range past the end: %d %s", status, h.Get("Content-Range"))
		}
	}

	data := nopacsv1connect.NewDataServiceClient(connect.NewClient(connecthttp.NewTransport(hc, "http://local")))
	streamed := 0
	for subject := range subjects {
		st, err := data.ReadFiles(t.Context(), &v1.ReadFilesRequest{Tenant: tenantName, SubjectLabel: subject})
		if err != nil {
			t.Fatal(err)
		}
		var cur *v1.ArchivedFile
		h := sha256.New()
		var got int64
		end := func() {
			if cur == nil {
				return
			}
			f, ok := files[cur.GetInstanceId()]
			if !ok || cur.GetKey() != f.Key || cur.GetLength() != f.Length || cur.GetSha256() != f.SHA256 ||
				got != f.Length || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
				t.Errorf("a streamed instance: %+v with %d bytes, the manifest lists %+v", cur, got, f)
			}
			streamed++
		}
		for {
			m, err := st.Receive()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("ReadFiles: %v", err)
			}
			switch p := m.GetPart().(type) {
			case *v1.ReadFilesResponse_File:
				end()
				cur, got = p.File, 0
				h.Reset()
			case *v1.ReadFilesResponse_Chunk:
				_, _ = h.Write(p.Chunk.GetData())
				got += int64(len(p.Chunk.GetData()))
			}
		}
		end()
		_ = st.Close()
	}
	if streamed != len(files) {
		t.Errorf("ReadFiles of every subject carried %d instances, the manifests list %d", streamed, len(files))
	}

	python, asked := os.LookupEnv(downloadPythonEnv)
	if !asked {
		return
	}
	if python == "" {
		t.Fatal(downloadPythonEnv + " is empty; name a Python interpreter")
	}
	work := t.TempDir()
	var paths []string
	for k, b := range manifests {
		name := filepath.Join(work, strings.ReplaceAll(k, "/", "_"))
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	cfg, err := json.Marshal(map[string]any{"base": "http://local", "tenant": tenantName, "socket": server.SocketPath(dataDir), "manifests": paths})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(work, "check.json")
	if err := os.WriteFile(input, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	script := filepath.Join("..", "..", "scripts", "check", "download-curl.py")
	out, err := exec.CommandContext(ctx, python, "-I", script, input).CombinedOutput() //nolint:gosec // G204: the operator's interpreter.
	t.Logf("%s", out)
	if err != nil || !strings.Contains(string(out), " 0 differences") {
		t.Fatalf("the check with curl and hashlib failed: %v", err)
	}
}
