package leaktest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// driveAPI calls every call of the table as every key, the keys of lowest
// grant first, and as a bad token and no key.
func driveAPI(t *testing.T, s *site, w *world, known map[string]method, rec *recorder) {
	t.Helper()
	calls := apiCalls(w)
	covered := map[string]bool{}
	for _, c := range calls {
		if m, ok := known[c.method]; !ok {
			t.Errorf("%s: the contract has no such method", c.label())
		} else {
			covered[c.method] = true
			if err := requestParses(m, c.body); err != nil {
				t.Errorf("%s: %v", c.label(), err)
			}
		}
	}
	for n := range known {
		if !covered[n] {
			t.Errorf("no call of %s: add one to apiCalls", n)
		}
	}
	callers := []caller{{name: "no-key"}, {name: "bad-token", secret: "nopacs_111111111111_1111111111111111111111111111111111111111111"}}
	callers = append(callers, w.keys...)
	for _, k := range callers {
		a := newAPI(s.listen, k.secret, k.name, rec)
		for _, last := range []bool{false, true} {
			for _, c := range calls {
				if c.last == last {
					if m, ok := known[c.method]; ok {
						a.call(m, c)
					}
				}
			}
		}
	}
}

// driveHTTP calls the routes that are not methods, as every key: DICOMweb
// search and metadata, the download of an instance, the metrics, and requests
// the server cannot read.
func driveHTTP(t *testing.T, s *site, w *world, known map[string]method, rec *recorder) {
	t.Helper()
	callers := []caller{{name: "no-key"}, {name: "bad-token", secret: "nopacs_111111111111_1111111111111111111111111111111111111111111"}}
	callers = append(callers, w.keys...)
	T := "/tenants/" + w.tenant
	for _, k := range callers {
		a := newAPI(s.listen, k.secret, k.name, rec)
		for _, x := range []scanRef{w.deid, w.raw} {
			a.get("http/download", T+"/instances/"+x.subject+"/"+x.session+"/"+x.scan+"/"+x.instanceID)
			a.get("http/download-range", T+"/instances/"+x.subject+"/"+x.session+"/"+x.scan+"/"+x.instanceID+"?x=1")
			a.get("http/dicomweb-series-search", T+"/dicom-web/series?SeriesInstanceUID="+x.seriesUID)
			a.get("http/dicomweb-instance-metadata", T+"/dicom-web/instances?SOPInstanceUID="+x.sopUID)
		}
		a.get("http/download", T+"/instances/sub-nosuch/ses-nosuch/scan-nosuch/i-nosuch")
		for _, p := range []string{"/studies", "/series", "/instances", "/studies?PatientName=*", "/studies?PatientID=*", "/studies/1.2.3/metadata",
			"/studies/1.2.3/series/1.2.4/instances/1.2.5", "/studies/1.2.3/series/1.2.4/instances/1.2.5/frames/1", "/nothing", "/studies?limit=zzz"} {
			a.get("http/dicomweb"+p, T+"/dicom-web"+p)
		}
		a.get("http/dicomweb-other-tenant", "/tenants/"+w.other+"/dicom-web/studies")
		a.get("http/dicomweb-no-tenant", "/tenants/nosuch/dicom-web/studies")
		a.get("http/metrics", "/metrics")
		a.get("http/unknown", "/no/such/route")
		a.get("http/root", "/")
		// Requests the server cannot read: bad JSON, a field the contract
		// does not have, a wrong type, a wrong content type, a method that
		// does not exist, a GET of a method.
		get := known["GetScan"]
		for _, body := range []string{"{bad json", `{"tenant":123}`, `{"tenant":"lab1","noSuchField":"x"}`, `{"tenant":"` + w.tenant + `","subjectLabel":"` + strings.Repeat("a", 5000) + `"}`, ""} {
			a.unary(context.Background(), "http/bad-request", get, body, false)
		}
		for _, p := range []struct{ label, path, ctype string }{
			{"http/no-such-method", "/nopacs.v1.ScanService/GetScan", "application/json"},
			{"http/wrong-content-type", get.procedure, "text/plain"},
			{"http/no-such-service", "/nopacs.v1.Nothing/Nothing", "application/json"},
		} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.base+p.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", p.ctype)
			a.do(p.label, req, false)
		}
		a.get("http/get-of-a-method", get.procedure)
	}
}

// drainAudit reads the audit log and the event feed through the API as keys
// that may, last, so that they hold every call of the run: every page of the
// installation's log and of lab1's, the verification and the export, and the
// event feed from its start.
func drainAudit(t *testing.T, s *site, w *world, rec *recorder) {
	t.Helper()
	known := contract()
	for _, k := range w.keys {
		if k.name != "installation-admin" && k.name != "installation-reader" && k.name != "installation-operator" && k.name != "tenant-admin" && k.name != "tenant-reader" {
			continue
		}
		a := newAPI(s.listen, k.secret, k.name, rec)
		for _, scope := range []string{`{"installation":{}}`, `{"tenant":"lab1"}`} {
			token := ""
			for page := 0; page < 400; page++ {
				body := fmt.Sprintf(`{"scope":%s,"pageSize":1000,"pageToken":%q}`, scope, token)
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.base+known["ListAuditEntries"].procedure, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				status, out := a.do("audit-api", req, false)
				var next struct {
					NextPageToken string `json:"nextPageToken"`
				}
				if status != http.StatusOK || json.Unmarshal(out, &next) != nil || next.NextPageToken == "" {
					break
				}
				token = next.NextPageToken
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			a.stream(ctx, "audit-api export", known["ExportAudit"], fmt.Sprintf(`{"scope":%s}`, scope), false)
			cancel()
			a.unary(context.Background(), "audit-api verify", known["VerifyAudit"], fmt.Sprintf(`{"scope":%s}`, scope), false)
			// The events kept so far are read from an installation's feed
			// (resume); one tenant's feed starts from now.
			feed := fmt.Sprintf(`{"scope":%s,"resume":true}`, scope)
			if scope != `{"installation":{}}` {
				feed = fmt.Sprintf(`{"scope":%s}`, scope)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 6*time.Second)
			a.stream(ctx, "events-feed", known["Watch"], feed, false)
			cancel()
		}
	}
}
