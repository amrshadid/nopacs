package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestDownloadNeedReadsThePathValues guards the requirement of the plain
// HTTP download as the server computes it per request: reader on the
// tenant the path value names, reader on the installation when the server
// left that value empty (a name that is not a tenant's), the scan's path as
// the object with an invalid label written "invalid", and the Range header
// and the window recorded only as given or not, never the instance id.
func TestDownloadNeedReadsThePathValues(t *testing.T) {
	const uid = "1.2.826.0.1.3680043.8.498.1"
	req := func(tenant, subject, query, rng string) (access.Requirement, access.Request) {
		r := httptest.NewRequest("GET", "/x?"+query, nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		for k, v := range map[string]string{"tenant": tenant, "subject": subject, "session": "E01", "scan": "T1", "instance": uid} {
			r.SetPathValue(k, v)
		}
		return api.DownloadNeed(r)
	}
	need, f := req("lab1", "S01", "offset=0&length=10", "bytes=0-")
	if need != access.OnTenant("lab1", master.RoleReader) {
		t.Errorf("a download needs %+v, want reader on lab1", need)
	}
	if f.Object != "S01/E01/T1" || f.Details["range"] != "true" || f.Details["window"] != "true" || f.Details["instance"] != "present" {
		t.Errorf("facts %+v", f)
	}
	for k, v := range f.Details {
		if strings.Contains(v, uid) {
			t.Errorf("the detail %s holds the instance id", k)
		}
	}
	need, f = req("", "S%2F01", "", "")
	if need != access.OnInstallation(master.RoleReader) {
		t.Errorf("a download with no valid tenant needs %+v, want reader on the installation", need)
	}
	if f.Object != "invalid/E01/T1" || f.Details["range"] != "false" || f.Details["window"] != "false" {
		t.Errorf("facts of a hostile path %+v", f)
	}
	if !strings.HasPrefix(api.DownloadPattern, "GET /tenants/{tenant}/") {
		t.Errorf("the pattern is %q", api.DownloadPattern)
	}
}
