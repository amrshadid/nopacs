package servecmd_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
)

// checkDICOMweb reads every instance a reception archived back through
// the real server's DICOMweb route on its local socket: a search of the
// tenant's studies (QIDO-RS) and a retrieve of each study as stored
// (WADO-RS), each part of which must have the SHA-256 of an instance of
// the scans' manifests on the storage, every instance once. So the route
// is registered, checked and served by the server itself, not a stand-in.
func checkDICOMweb(t *testing.T, hc *http.Client, tenantName string, store blob.Store) {
	t.Helper()
	want := map[string]bool{}
	for k, b := range storedManifests(t, store) {
		var m struct {
			Files []manifestFile `json:"files"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, f := range m.Files {
			want[f.SHA256] = true
		}
	}
	get := func(path, accept string) (int, http.Header, []byte) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://local/tenants/"+tenantName+"/dicom-web/"+path, nil)
		req.Header.Set("Accept", accept)
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("GET %s: the body: %v", path, err)
		}
		return res.StatusCode, res.Header, b
	}
	status, _, body := get("studies", "application/dicom+json")
	var studies []map[string]struct {
		Value []any `json:"Value"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &studies) != nil || len(studies) == 0 {
		t.Fatalf("the search of studies: %d %q", status, body)
	}
	seen := map[string]bool{}
	for _, s := range studies {
		uid, _ := s["0020000D"].Value[0].(string)
		status, h, body := get("studies/"+uid, `multipart/related; type="application/dicom"; transfer-syntax=*`)
		media, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if status != http.StatusOK || err != nil || media != "multipart/related" || params["type"] != "application/dicom" {
			t.Fatalf("the retrieve of a study: %d %q", status, h.Get("Content-Type"))
		}
		rd := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			p, err := rd.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("the retrieve of a study is not a whole multipart body: %v", err)
			}
			b, _ := io.ReadAll(p)
			sum := sha256.Sum256(b)
			k := hex.EncodeToString(sum[:])
			if !want[k] || seen[k] {
				t.Errorf("a part of %d bytes is not an instance of the manifests, or is sent twice", len(b))
			}
			seen[k] = true
		}
	}
	if len(seen) != len(want) {
		t.Errorf("DICOMweb retrieved %d instances, the manifests list %d", len(seen), len(want))
	}
}
