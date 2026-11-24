package servecmd_test

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// readOnlyKey is a key over the network, for the reads and changes a
// read-only tenant is held to.
type readOnlyKey struct {
	listen, secret string
}

// newReadOnlyKey makes a key with the role admin on the tenant tn, through
// the local socket hc, and a rule that lets it be used only from loopback:
// the installation trusts the forwarded address a request from 127.0.0.1
// names (access.trusted-proxies, a single address written without /32), and the tenant's networks
// (access.tenant-networks) are 127.0.0.0/8, so a request that names
// another address is refused by the tenant's rule.
func newReadOnlyKey(t *testing.T, hc *http.Client, listen, tn string) readOnlyKey {
	t.Helper()
	var key struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.AccessService/CreateKey", `{"holder":"read-only checks"}`), &key); err != nil {
		t.Fatal(err)
	}
	post(t, hc, "/nopacs.v1.AccessService/SetGrant", `{"keyId":"`+key.Key.ID+`","scope":{"tenant":"`+tn+`"},"role":"ROLE_ADMIN"}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"access.trusted-proxies","scope":{"installation":{}},"value":"127.0.0.1"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"access.tenant-networks","scope":{"tenant":"`+tn+`"},"value":"127.0.0.0/8"}}`)
	return readOnlyKey{listen: listen, secret: key.Secret}
}

// do sends a request with the key and returns the answer's status and body;
// from, when set, is the address the request says it was forwarded for.
func (k readOnlyKey) do(t *testing.T, method, path, contentType, body, from string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, "http://"+k.listen+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+k.secret)
	if from != "" {
		req.Header.Set("X-Forwarded-For", from)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// keyed is an http.RoundTripper that sends the key with every request.
type keyed struct{ secret string }

// RoundTrip implements http.RoundTripper.
func (k keyed) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+k.secret)
	return http.DefaultTransport.RoundTrip(r)
}

// instanceUnder returns the labels and the SOP Instance UID of an instance
// file the directory storage at root holds under subject.
func instanceUnder(t *testing.T, root, subject string) []string {
	t.Helper()
	var parts []string
	err := filepath.WalkDir(filepath.Join(root, "data", subject), func(p string, d fs.DirEntry, err error) error {
		if err != nil || parts != nil || d.IsDir() || !strings.HasSuffix(p, ".dcm") {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(root, "data"), p)
		parts = strings.Split(filepath.ToSlash(strings.TrimSuffix(rel, ".dcm")), "/")
		return err
	})
	if err != nil || len(parts) != 4 {
		t.Fatalf("no instance of %s under %s: %v %v", subject, root, parts, err)
	}
	return parts
}

// checkReadOnly holds a read-only tenant to its contract over the network,
// with a key: every read is served (the catalog's lists and gets, the
// metadata, the plain download, DICOMweb's search and ReadFiles), the
// tenant's network rule still refuses a request from outside it, and a
// change is refused as FAILED_PRECONDITION naming reason, never INTERNAL.
// inst is an instance's labels and UID (instanceUnder).
func checkReadOnly(t *testing.T, k readOnlyKey, tn string, inst []string, reason string) {
	t.Helper()
	sub, ses, scan, uid := inst[0], inst[1], inst[2], inst[3]
	const js = "application/json"
	scope := `"tenant":"` + tn + `","subjectLabel":"` + sub + `"`
	for _, r := range [][2]string{
		{"/nopacs.v1.CatalogService/ListSubjects", `{"tenant":"` + tn + `"}`},
		{"/nopacs.v1.CatalogService/GetSubject", `{` + scope + `}`},
		{"/nopacs.v1.CatalogService/ListSessions", `{` + scope + `}`},
		{"/nopacs.v1.CatalogService/GetSession", `{` + scope + `,"sessionLabel":"` + ses + `"}`},
		{"/nopacs.v1.CatalogService/ListScans", `{` + scope + `,"sessionLabel":"` + ses + `"}`},
		{"/nopacs.v1.CatalogService/GetScan", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `"}`},
		{"/nopacs.v1.MetadataService/GetScanMetadata", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `"}`},
		{"/nopacs.v1.MetadataService/GetInstanceMetadata", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `","instanceId":"` + uid + `"}`},
	} {
		if code, body := k.do(t, http.MethodPost, r[0], js, r[1], ""); code != http.StatusOK || (!strings.Contains(body, sub) && !strings.Contains(body, uid)) {
			t.Errorf("%s of the %s tenant: HTTP %d %.300s", r[0], reason, code, body)
		}
	}
	if code, body := k.do(t, http.MethodGet, "/tenants/"+tn+"/instances/"+strings.Join(inst, "/"), "", "", ""); code != http.StatusOK || len(body) < 132 || body[128:132] != "DICM" {
		t.Errorf("the download of an instance of the %s tenant: HTTP %d, %d bytes", reason, code, len(body))
	}
	if code, body := k.do(t, http.MethodGet, "/tenants/"+tn+"/dicom-web/studies", "", "", ""); code != http.StatusOK || !strings.Contains(body, "0020000D") {
		t.Errorf("a DICOMweb search of the %s tenant: HTTP %d %.300s", reason, code, body)
	}
	data := nopacsv1connect.NewDataServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: keyed{k.secret}}, "http://"+k.listen)))
	st, err := data.ReadFiles(t.Context(), &v1.ReadFilesRequest{Tenant: tn, SubjectLabel: sub})
	if err != nil {
		t.Fatalf("ReadFiles of the %s tenant: %v", reason, err)
	}
	var files int
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFiles of the %s tenant: %v", reason, err)
		}
		if m.GetFile() != nil {
			files++
		}
	}
	_ = st.Close()
	if files == 0 {
		t.Errorf("ReadFiles of the %s tenant streamed no instance", reason)
	}
	// The tenant's rule still applies: an address outside its networks is
	// refused as a key that is no good, before any read.
	if code, body := k.do(t, http.MethodPost, "/nopacs.v1.CatalogService/ListSubjects", js, `{"tenant":"`+tn+`"}`, "192.0.2.7"); code != http.StatusUnauthorized {
		t.Errorf("a read of the %s tenant from outside its networks: HTTP %d %.300s, want 401", reason, code, body)
	}
	// A change, the key an admin of the tenant: refused with the state.
	if code, body := k.do(t, http.MethodPost, "/nopacs.v1.SettingsService/SetSetting", js,
		`{"setting":{"name":"delete.grace","scope":{"tenant":"`+tn+`"},"value":"3s"}}`, ""); code != http.StatusBadRequest ||
		!strings.Contains(body, `"failed_precondition"`) || !strings.Contains(body, reason) {
		t.Errorf("a change of the %s tenant by its admin's key: HTTP %d %.300s, want failed_precondition naming %s", reason, code, body, reason)
	}
}
