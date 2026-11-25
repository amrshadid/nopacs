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

// jsonType is the content type of a Connect JSON request.
const jsonType = "application/json"

// dataReads returns the reads of a tenant's data a key is held to, each a
// procedure and its JSON body, for the instance inst (instanceUnder): the
// catalog's lists and gets and the metadata.
func dataReads(tn string, inst []string) [][2]string {
	sub, ses, scan, uid := inst[0], inst[1], inst[2], inst[3]
	scope := `"tenant":"` + tn + `","subjectLabel":"` + sub + `"`
	return [][2]string{
		{"/nopacs.v1.CatalogService/ListSubjects", `{"tenant":"` + tn + `"}`},
		{"/nopacs.v1.CatalogService/GetSubject", `{` + scope + `}`},
		{"/nopacs.v1.CatalogService/ListSessions", `{` + scope + `}`},
		{"/nopacs.v1.CatalogService/GetSession", `{` + scope + `,"sessionLabel":"` + ses + `"}`},
		{"/nopacs.v1.CatalogService/ListScans", `{` + scope + `,"sessionLabel":"` + ses + `"}`},
		{"/nopacs.v1.CatalogService/GetScan", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `"}`},
		{"/nopacs.v1.MetadataService/GetScanMetadata", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `"}`},
		{"/nopacs.v1.MetadataService/GetInstanceMetadata", `{` + scope + `,"sessionLabel":"` + ses + `","scanLabel":"` + scan + `","instanceId":"` + uid + `"}`},
	}
}

// readFiles calls ReadFiles with the key for the subject sub and returns
// how many files it streamed, or the error that ended it.
func readFiles(t *testing.T, k readOnlyKey, tn, sub string) (int, error) {
	t.Helper()
	data := nopacsv1connect.NewDataServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: keyed{k.secret}}, "http://"+k.listen)))
	st, err := data.ReadFiles(t.Context(), &v1.ReadFilesRequest{Tenant: tn, SubjectLabel: sub})
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()
	var files int
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		if m.GetFile() != nil {
			files++
		}
	}
}

// checkRuleAndChange holds a read-only tenant's boundary over the network:
// its network rule still refuses a request from outside it, as a key that
// is no good, and a change by its admin's key is refused as
// FAILED_PRECONDITION naming reason, never INTERNAL.
func checkRuleAndChange(t *testing.T, k readOnlyKey, tn, reason string) {
	t.Helper()
	if code, body := k.do(t, http.MethodPost, "/nopacs.v1.CatalogService/ListSubjects", jsonType, `{"tenant":"`+tn+`"}`, "192.0.2.7"); code != http.StatusUnauthorized {
		t.Errorf("a read of the %s tenant from outside its networks: HTTP %d %.300s, want 401", reason, code, body)
	}
	if code, body := k.do(t, http.MethodPost, "/nopacs.v1.SettingsService/SetSetting", jsonType,
		`{"setting":{"name":"delete.grace","scope":{"tenant":"`+tn+`"},"value":"3s"}}`, ""); code != http.StatusBadRequest ||
		!strings.Contains(body, `"failed_precondition"`) || !strings.Contains(body, reason) {
		t.Errorf("a change of the %s tenant by its admin's key: HTTP %d %.300s, want failed_precondition naming %s", reason, code, body, reason)
	}
}

// checkMovedServes holds a moved tenant whose storages are still this
// installation's to its contract over the network, with a key: every read
// of its data is served (the catalog's lists and gets, the metadata, the
// plain download, DICOMweb's search and ReadFiles), and its rule and the
// refusal of a change hold (checkRuleAndChange). inst is an instance's
// labels and UID (instanceUnder).
func checkMovedServes(t *testing.T, k readOnlyKey, tn string, inst []string) {
	t.Helper()
	const reason = "TENANT_MOVED"
	sub, uid := inst[0], inst[3]
	for _, r := range dataReads(tn, inst) {
		if code, body := k.do(t, http.MethodPost, r[0], jsonType, r[1], ""); code != http.StatusOK || (!strings.Contains(body, sub) && !strings.Contains(body, uid)) {
			t.Errorf("%s of the moved tenant: HTTP %d %.300s", r[0], code, body)
		}
	}
	if code, body := k.do(t, http.MethodGet, "/tenants/"+tn+"/instances/"+strings.Join(inst, "/"), "", "", ""); code != http.StatusOK || len(body) < 132 || body[128:132] != "DICM" {
		t.Errorf("the download of an instance of the moved tenant: HTTP %d, %d bytes", code, len(body))
	}
	if code, body := k.do(t, http.MethodGet, "/tenants/"+tn+"/dicom-web/studies", "", "", ""); code != http.StatusOK || !strings.Contains(body, "0020000D") {
		t.Errorf("a DICOMweb search of the moved tenant: HTTP %d %.300s", code, body)
	}
	if files, err := readFiles(t, k, tn, sub); err != nil || files == 0 {
		t.Errorf("ReadFiles of the moved tenant: %d files, %v", files, err)
	}
	checkRuleAndChange(t, k, tn, reason)
}

// checkFenced holds a fenced tenant to its contract over the network, with
// a key: it serves no data, so every read of it (the catalog's lists and
// gets, the metadata, the plain download, DICOMweb's search and retrieval,
// ReadFiles) is FAILED_PRECONDITION with a message that begins
// TENANT_FENCED and names owner, the installation the storage's owner
// record names; its state and reason stay visible (GetTenant, GetStatus);
// and its rule and the refusal of a change hold (checkRuleAndChange).
func checkFenced(t *testing.T, k readOnlyKey, tn string, inst []string, owner string) {
	t.Helper()
	const reason = "TENANT_FENCED"
	refused := func(what string, code int, body string) {
		t.Helper()
		if code != http.StatusBadRequest || !strings.Contains(body, `"failed_precondition"`) ||
			!strings.Contains(body, `"`+reason+`: the tenant `+tn+` serves no data`) || !strings.Contains(body, owner) ||
			strings.Contains(body, inst[0]) || strings.Contains(body, "DICM") {
			t.Errorf("%s of the fenced tenant: HTTP %d %.300s, want failed_precondition %s naming %s", what, code, body, reason, owner)
		}
	}
	for _, r := range dataReads(tn, inst) {
		code, body := k.do(t, http.MethodPost, r[0], jsonType, r[1], "")
		refused(r[0], code, body)
	}
	code, body := k.do(t, http.MethodGet, "/tenants/"+tn+"/instances/"+strings.Join(inst, "/"), "", "", "")
	refused("the download of an instance", code, body)
	code, body = k.do(t, http.MethodGet, "/tenants/"+tn+"/dicom-web/studies", "", "", "")
	refused("a DICOMweb search", code, body)
	code, body = k.do(t, http.MethodGet, "/tenants/"+tn+"/dicom-web/studies/"+inst[3]+"/metadata", "", "", "")
	refused("a DICOMweb retrieval", code, body)
	files, err := readFiles(t, k, tn, inst[0])
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition || files != 0 ||
		!strings.HasPrefix(ce.Message(), reason+": the tenant "+tn+" serves no data") || !strings.Contains(ce.Message(), owner) {
		t.Errorf("ReadFiles of the fenced tenant: %d files, %v; want failed_precondition %s naming %s", files, err, reason, owner)
	}
	// What stays visible: the state, with the reason naming the owner.
	code, body = k.do(t, http.MethodPost, "/nopacs.v1.TenantService/GetTenant", jsonType, `{"name":"`+tn+`"}`, "")
	if code != http.StatusOK || !strings.Contains(body, `"TENANT_STATE_FENCED"`) || !strings.Contains(body, owner) {
		t.Errorf("GetTenant of the fenced tenant: HTTP %d %.300s, want its state and reason", code, body)
	}
	code, body = k.do(t, http.MethodPost, "/nopacs.v1.MonitorService/GetStatus", jsonType, `{"scope":{"tenant":"`+tn+`"}}`, "")
	if code != http.StatusOK || !strings.Contains(body, `"TENANT_STATE_FENCED"`) || strings.Contains(body, `"counts"`) {
		t.Errorf("GetStatus of the fenced tenant: HTTP %d %.300s, want its state alone", code, body)
	}
	checkRuleAndChange(t, k, tn, reason)
}
