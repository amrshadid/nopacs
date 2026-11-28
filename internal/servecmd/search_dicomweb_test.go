package servecmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// pythonSearchWebEnv names the Python, with dicomweb-client, that runs the
// QIDO-RS comparison; it is listed in docs/develop/testing.md.
const pythonSearchWebEnv = "NOPACS_CHECK_SEARCH_DICOMWEB_PYTHON"

// qidoCases are the searches made twice: as QIDO-RS by dicomweb-client, and
// as a condition by "nopacs search". kind says the shape of the QIDO-RS
// search (studies, series in each study, series in all studies); level and
// condition say the same search in the archive's own language.
var qidoCases = []struct {
	name, kind string
	filters    map[string]string
	level      string
	condition  string
}{
	{"every study", "studies", nil, "session", ""},
	{"StudyDate range", "studies", map[string]string{"StudyDate": "20240101-20241231"}, "session", `session.date between 2024-01-01 and 2024-12-31`},
	{"StudyDate up to", "studies", map[string]string{"StudyDate": "-20231231"}, "session", `session.date <= 2023-12-31`},
	{"StudyDate from", "studies", map[string]string{"StudyDate": "20250101-"}, "session", `session.date >= 2025-01-01`},
	{"ModalitiesInStudy", "studies", map[string]string{"ModalitiesInStudy": "CT"}, "session", `has scan (Modality == "CT")`},
	{"Modality in each study", "series-per-study", map[string]string{"Modality": "MR"}, "scan", `Modality == "MR"`},
	{"SeriesNumber in each study", "series-per-study", map[string]string{"SeriesNumber": "3"}, "scan", `SeriesNumber == 3`},
	{"SeriesDescription pattern in each study", "series-per-study", map[string]string{"SeriesDescription": "T1*"}, "scan", `SeriesDescription like "T1*"`},
	{"SeriesDescription in each study", "series-per-study", map[string]string{"SeriesDescription": "Localizer"}, "scan", `SeriesDescription == "Localizer"`},
	{"Modality in all studies", "series", map[string]string{"Modality": "CT"}, "scan", `Modality == "CT"`},
	{"Modality PT in all studies", "series", map[string]string{"Modality": "PT"}, "scan", `Modality == "PT"`},
	{"Manufacturer in each study", "series-per-study", map[string]string{"Manufacturer": "SIEMENS"}, "scan", `Manufacturer == "SIEMENS"`},
	{"ProtocolName pattern in each study", "series-per-study", map[string]string{"ProtocolName": "T1W*"}, "scan", `ProtocolName like "T1W*"`},
	{"SliceThickness in each study", "series-per-study", map[string]string{"SliceThickness": "1"}, "scan", `SliceThickness == 1`},
	{"BodyPartExamined in each study", "series-per-study", map[string]string{"BodyPartExamined": "HEAD"}, "scan", `BodyPartExamined == "HEAD"`},
	{"two keys in each study", "series-per-study", map[string]string{"Modality": "MR", "SliceThickness": "1"}, "scan", `Modality == "MR" and SliceThickness == 1`},
	{"SeriesDescription contains in each study", "series-per-study", map[string]string{"SeriesDescription": "*MPRAGE*"}, "scan", `SeriesDescription like "*MPRAGE*"`},
	{"MagneticFieldStrength in all studies", "series", map[string]string{"MagneticFieldStrength": "3"}, "scan", `MagneticFieldStrength == 3`},
}

// TestSearchWithDicomwebClient guards that a tenant's QIDO-RS searches, as
// dicomweb-client (a client that is not NoPACs') makes them against a real
// server process, find the same studies and series as "nopacs search" does
// for the same conditions. A tenant of synthetic files made with go-dicom
// is sent by dcmtk's storescu to a DIMSE receiver of the server, and
// archived; each case is searched through the command line against the
// server and, by scripts/check/search-dicomweb-client.py, through the
// DICOMweb route with a reader's key. It runs on SQLite, and on Postgres
// when its address is set.
func TestSearchWithDicomwebClient(t *testing.T) {
	python, asked := os.LookupEnv(pythonSearchWebEnv)
	if !asked {
		t.Skip(pythonSearchWebEnv + " is not set; `make check-search-dicomweb` runs this comparison with dicomweb-client")
	}
	if python == "" {
		t.Fatal(pythonSearchWebEnv + " is empty; name an interpreter that has dicomweb-client")
	}
	if testing.Short() {
		t.Skip("runs a server process and sends files over the network; left out under -short")
	}
	scu := storescu(t)
	cases := map[string]string{"sqlite": `"databaseType":"DATABASE_TYPE_SQLITE"`}
	if a, ok := testkit.PostgresSchema(t); ok {
		raw, _ := json.Marshal(a)
		cases["postgres"] = `"databaseType":"DATABASE_TYPE_POSTGRES","databaseAddress":` + string(raw)
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { checkSearchDicomweb(t, python, scu, body) })
	}
}

// checkSearchDicomweb runs the comparison on a tenant made with the given
// CreateTenant fields.
func checkSearchDicomweb(t *testing.T, python, scu, tenantFields string) {
	t.Helper()
	files := testkit.SearchFiles(t, filepath.Join("..", "receive", "testdata"), 1)
	work := t.TempDir()
	var paths []string
	studies := map[string]bool{}
	for i, f := range files {
		p := filepath.Join(work, fmt.Sprintf("f%03d.dcm", i))
		if err := os.WriteFile(p, f.Bytes, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		studies[f.Study] = true
	}

	dataDir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	child := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir, "--listen", listen)
	waitServing(t, dataDir, child)
	hc := socketClient(dataDir)
	defer hc.CloseIdleConnections()
	roots := filepath.Join(filepath.Dir(dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	const tenantName = "srch1"
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tenantName+`",`+tenantFields+`}`)
	port := freePort(t)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"receiver.ports","scope":{"tenant":"`+tenantName+`"},"value":"`+strconv.Itoa(port)+`"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+tenantName+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tenantName+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	var made struct {
		Key    struct{ ID string } `json:"key"`
		Secret string              `json:"secret"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.AccessService/CreateKey", `{"holder":"search check"}`), &made); err != nil || made.Secret == "" {
		t.Fatalf("CreateKey: %v", err)
	}
	post(t, hc, "/nopacs.v1.AccessService/SetGrant", `{"keyId":"`+made.Key.ID+`","scope":{"tenant":"`+tenantName+`"},"role":"ROLE_READER"}`)
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tenantName+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	waitWithin(t, "the receiver to be up", 10*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tenantName+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})

	args := append([]string{"-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port)}, paths...)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, scu, args...).CombinedOutput(); err != nil { //nolint:gosec // G204: a test's own tool and files.
		t.Fatalf("storescu: %v\n%s", err, out)
	}
	var list struct {
		Receptions []struct {
			Step   string `json:"step"`
			Counts struct {
				FilesArchived string `json:"filesArchived"`
			} `json:"counts"`
		} `json:"receptions"`
	}
	waitBody(t, "the receptions to be done", func() bool {
		if err := json.Unmarshal(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tenantName+`"}`), &list); err != nil {
			t.Fatal(err)
		}
		archived := 0
		for _, r := range list.Receptions {
			if r.Step == "RECEPTION_STEP_FAILED" {
				t.Fatalf("a reception failed:\n%s", child.stderr.String())
			}
			if r.Step != "RECEPTION_STEP_DONE" {
				return false
			}
			n, _ := strconv.Atoi(r.Counts.FilesArchived)
			archived += n
		}
		return archived == len(files)
	})

	// The same searches through the command line, as the local operator.
	type row struct {
		SeriesInstanceUID string `json:"seriesInstanceUid"`
		StudyInstanceUID  string `json:"studyInstanceUid"`
	}
	var queries []map[string]any
	for _, c := range qidoCases {
		code, out, errOut := nopacs(t.Context(), dataDir, "search", tenantName, c.condition, "--level", c.level, "--all", "--json")
		if code != 0 {
			t.Fatalf("nopacs search %q: exit %d: %s", c.condition, code, errOut)
		}
		var res struct {
			Scans    []row `json:"scans"`
			Sessions []row `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("nopacs search %q: %v\n%s", c.condition, err, out)
		}
		expect := []string{}
		for _, r := range res.Scans {
			expect = append(expect, r.SeriesInstanceUID)
		}
		for _, r := range res.Sessions {
			expect = append(expect, r.StudyInstanceUID)
		}
		slices.Sort(expect)
		queries = append(queries, map[string]any{"name": c.name, "kind": c.kind, "filters": c.filters, "expect": expect})
	}
	// Every search of the manifest must be able to fail: a tenant that every
	// condition matches all of, or none of, would prove nothing.
	if n := len(studies); n < 5 {
		t.Fatalf("the tenant holds %d studies, want at least 5", n)
	}
	manifest := filepath.Join(work, "expected.json")
	b, err := json.Marshal(map[string]any{"queries": queries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", filepath.Join("..", "..", "scripts", "check", "search-dicomweb-client.py"), //nolint:gosec // G204: the operator's interpreter.
		"http://"+listen+"/tenants/"+tenantName+"/dicom-web", manifest)
	cmd.Env = append(os.Environ(), "NOPACS_DICOMWEB_KEY="+made.Secret)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the check with dicomweb-client failed: %v", err)
	}
	if !strings.Contains(string(out), "; 0 differ") {
		t.Fatal("the check did not report that nothing differs")
	}
}
