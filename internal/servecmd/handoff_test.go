package servecmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/server"
)

// corpusFile returns a file of the public corpus internal/receive/testdata
// holds (pydicom's test files, copied unchanged).
func corpusFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "receive", "testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// postCode sends a Connect JSON request on the socket as the local operator
// and returns the answer's status and body.
func postCode(t *testing.T, hc *http.Client, procedure, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://local"+procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", procedure, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// stopServer stops a server process with SIGTERM and waits for it to exit 0.
func stopServer(t *testing.T, c *child) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := childExit(t, c); code != 0 {
		t.Fatalf("serve exited %d\n%s", code, c.stderr.String())
	}
}

// tenantOf returns the tenant's state and reason as GetTenant answers them.
func tenantOf(t *testing.T, hc *http.Client, name string) (string, string) {
	t.Helper()
	var res struct {
		Tenant struct {
			State       string `json:"state"`
			StateReason string `json:"stateReason"`
		} `json:"tenant"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.TenantService/GetTenant", `{"name":"`+name+`"}`), &res); err != nil {
		t.Fatal(err)
	}
	return res.Tenant.State, res.Tenant.StateReason
}

// sendRefused sends a file with storescu to the receiver on port and fails
// the test unless no store succeeds.
func sendRefused(t *testing.T, scu string, port int, file string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, scu, "-v", "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port), file).CombinedOutput() //nolint:gosec // G204: a test's own tool and file.
	if err == nil || bytes.Contains(out, []byte("Received Store Response (Success)")) {
		t.Fatalf("storescu was not refused: %v\n%s", err, out)
	}
}

// TestHandoffThroughServe guards one writer per storage across two real
// server processes and the command line: tenant lab1 of installation A,
// holding a CT and an MR file archived by dcmtk's storescu and a delete of
// the MR subject, is exported (nopacs tenant export): A shows it moved, a
// storescu C-STORE to its receiver is refused, its catalog is still read, a
// change is refused naming TENANT_MOVED; over the network, while the owner
// record still names A, an admin key of the tenant reads everything
// (checkMovedServes), is refused from outside the tenant's networks, and
// its change is FAILED_PRECONDITION naming TENANT_MOVED, never INTERNAL.
// With A stopped, the export is imported into installation B over the same
// directory storage (nopacs tenant import), and the storage's owner record
// names B, one generation past the one the export took it to. A started
// again with its old database fences lab1, naming B: a C-STORE is refused,
// and so is a purge, naming TENANT_FENCED, and it serves no data
// (checkFenced): every read is FAILED_PRECONDITION TENANT_FENCED naming B,
// its state still shown. B, started, receives a new study into lab1, still
// hides the deleted subject, and deletes the CT subject; A, started again,
// serves none of that subject (whose file is still on the storage until
// B's purge) to its old key or its local operator. Reclaim on A is refused
// because the owner record names B.
func TestHandoffThroughServe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs two server processes and sends studies over the network; left out under -short")
	}
	scu := storescu(t)
	// rt is another series of the CT's study: it adds an instance to the CT
	// subject wherever it is archived.
	ct, mr, rt := corpusFile(t, "ct2n-6293.dcm"), corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct5n-2062.dcm")
	dirA, dirB := shortDir(t), shortDir(t)
	for _, d := range []string{dirA, dirB} {
		if _, err := run(t, nil, "init", "--data-dir", d); err != nil {
			t.Fatal(err)
		}
	}
	roots := filepath.Join(filepath.Dir(dirA), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	const tn = "lab1"
	port := freePort(t)
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))

	// Installation A: the tenant, its storage and receiver, two files and a
	// delete.
	a := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dirA, "--listen", listen)
	waitServing(t, dirA, a)
	hcA := socketClient(dirA)
	post(t, hcA, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hcA, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	for name, value := range map[string]string{"receiver.ports": strconv.Itoa(port), "receiver.allow-as-received": "true"} {
		post(t, hcA, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"`+name+`","scope":{"tenant":"`+tn+`"},"value":"`+value+`"}}`)
	}
	post(t, hcA, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	post(t, hcA, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tn+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	receiverUp := func(hc *http.Client) func() bool {
		return func() bool {
			return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`), []byte(`"RECEIVER_STATE_UP"`))
		}
	}
	waitWithin(t, "A's receiver to be up", 10*time.Second, receiverUp(hcA))
	key := newReadOnlyKey(t, hcA, listen, tn)
	// The CT first, so its subject is the one archived alone.
	sendFiles(t, scu, port, ct)
	var ctSubject, mrSubject string
	waitBody(t, "the CT file to be archived", func() bool {
		for s := range subjectCounts(t, hcA, tn) {
			ctSubject = s
		}
		return ctSubject != ""
	})
	sendFiles(t, scu, port, mr)
	waitBody(t, "the MR file to be archived", func() bool {
		for s := range subjectCounts(t, hcA, tn) {
			if s != ctSubject {
				mrSubject = s
			}
		}
		return mrSubject != ""
	})
	waitBody(t, "the receptions to end", func() bool {
		return !bytes.Contains(post(t, hcA, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`","inProgress":true}`), []byte(`"id"`))
	})
	del := cliJSON(t, dirA, "correction", "delete", tn, mrSubject, "--reason", "consent withdrawn", "--yes")
	delID := str(del, "correction", "id")
	if delID == "" {
		t.Fatalf("the delete answered %v", del)
	}

	// The export: A's tenant moved.
	exportFile, keyFile := filepath.Join(t.TempDir(), "lab1.export"), filepath.Join(t.TempDir(), "lab1.key")
	code, out, errOut := nopacs(t.Context(), dirA, "tenant", "export", tn, "--out", exportFile, "--key-out", keyFile)
	if code != 0 || !strings.Contains(out, "moved") {
		t.Fatalf("tenant export: exit %d\n%s\n%s", code, out, errOut)
	}
	if st, _ := tenantOf(t, hcA, tn); st != "TENANT_STATE_MOVED" {
		t.Fatalf("after the export A's tenant is %s", st)
	}
	waitWithin(t, "A's receiver to refuse", 10*time.Second, func() bool {
		return !bytes.Contains(post(t, hcA, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})
	sendRefused(t, scu, port, rt)
	if counts := subjectCounts(t, hcA, tn); len(counts) != 1 || counts[ctSubject] != 1 {
		t.Fatalf("the moved tenant's catalog read answers %v", counts)
	}
	if status, body := postCode(t, hcA, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"delete.grace","scope":{"tenant":"`+tn+`"},"value":"3s"}}`); status != http.StatusBadRequest || !bytes.Contains(body, []byte("TENANT_MOVED")) {
		t.Fatalf("a change into the moved tenant answered %d %s", status, body)
	}
	// The export took the owner record one generation further, still
	// naming A: until an import takes the storage over, A serves reads.
	inst := instanceUnder(t, filepath.Join(roots, tn, "main"), ctSubject)
	checkMovedServes(t, key, tn, inst)
	hcA.CloseIdleConnections()
	stopServer(t, a)

	// The import into B, stopped, over the same storage, which B lists
	// under the same root name.
	b := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dirB)
	waitServing(t, dirB, b)
	hcB := socketClient(dirB)
	post(t, hcB, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	hcB.CloseIdleConnections()
	stopServer(t, b)
	out, err := run(t, nil, "tenant", "import", exportFile, "--transfer-key", keyFile, "--data-dir", dirB)
	// The export took the owner record to generation 1; the import one
	// further.
	if err != nil || !strings.Contains(out, "storage main taken over at owner generation 2") {
		t.Fatalf("tenant import: %v\n%s", err, out)
	}
	idB, err := server.ReadInstallationID(dirB)
	if err != nil {
		t.Fatal(err)
	}
	ownerFile := filepath.Join(roots, tn, "main", "meta", ".owner.json")
	var owner struct {
		Installation string `json:"installation"`
		Generation   int64  `json:"generation"`
		MAC          string `json:"mac"`
	}
	raw, err := os.ReadFile(ownerFile) //nolint:gosec // G304: the test's own storage.
	if err != nil || json.Unmarshal(raw, &owner) != nil || owner.Installation != idB || owner.Generation != 2 || len(owner.MAC) != 64 {
		t.Fatalf("the owner record after the import is %s (%v)", raw, err)
	}

	// A again, with its old database: fenced.
	a = startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dirA, "--listen", listen)
	waitServing(t, dirA, a)
	hcA = socketClient(dirA)
	st, reason := tenantOf(t, hcA, tn)
	if st != "TENANT_STATE_FENCED" || !strings.Contains(reason, idB) {
		t.Fatalf("A's tenant after the import is %s (%q), want fenced naming %s", st, reason, idB)
	}
	sendRefused(t, scu, port, rt)
	if status, body := postCode(t, hcA, "/nopacs.v1.CorrectionService/PurgeCorrection",
		`{"tenant":"`+tn+`","id":"`+delID+`"}`); status != http.StatusBadRequest || !bytes.Contains(body, []byte("TENANT_FENCED")) {
		t.Fatalf("a purge in the fenced tenant answered %d %s", status, body)
	}
	checkFenced(t, key, tn, inst, idB)
	hcA.CloseIdleConnections()
	stopServer(t, a)

	// B serves the tenant and receives a new study into it.
	b = startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dirB)
	waitServing(t, dirB, b)
	hcB = socketClient(dirB)
	defer hcB.CloseIdleConnections()
	if st, _ := tenantOf(t, hcB, tn); st != "TENANT_STATE_READY" {
		t.Fatalf("B's tenant is %s", st)
	}
	if counts := subjectCounts(t, hcB, tn); len(counts) != 1 || counts[ctSubject] != 1 {
		t.Fatalf("B's catalog after the import is %v, want the CT subject alone (the MR one deleted)", counts)
	}
	waitWithin(t, "B's receiver to be up", 10*time.Second, receiverUp(hcB))
	sendFiles(t, scu, port, rt)
	waitBody(t, "B to archive the new series", func() bool { return subjectCounts(t, hcB, tn)[ctSubject] == 2 })
	// The deleted MR subject's delete came over with the tenant: the file
	// sent again to B is held as deleted, not archived.
	sendFiles(t, scu, port, mr)
	waitBody(t, "B to hold the deleted subject's file", func() bool { return len(heldFiles(t, hcB, tn, "QUARANTINE_REASON_WAS_DELETED")) == 1 })

	// B, the owner now, deletes the CT subject: hidden at once, its files
	// kept on the shared storage until B's purge, after its grace.
	if del := cliJSON(t, dirB, "correction", "delete", tn, ctSubject, "--reason", "consent withdrawn", "--yes"); str(del, "correction", "id") == "" {
		t.Fatalf("B's delete answered %v", del)
	}
	if counts := subjectCounts(t, hcB, tn); len(counts) != 0 {
		t.Fatalf("B's catalog after its delete is %v", counts)
	}
	if _, err := os.Stat(filepath.Join(append([]string{roots, tn, "main", "data"}, inst...)...) + ".dcm"); err != nil {
		t.Fatalf("the withdrawn instance's file is not on the storage until the purge: %v", err)
	}

	// A once more, B still running: fenced, A serves none of what B
	// withdrew, to its old keys or to its local operator, and shows its
	// state; reclaim refused, the record naming B.
	a = startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dirA, "--listen", listen)
	waitServing(t, dirA, a)
	hcA = socketClient(dirA)
	if st, reason := tenantOf(t, hcA, tn); st != "TENANT_STATE_FENCED" || !strings.Contains(reason, idB) {
		t.Fatalf("A's tenant after B's delete is %s (%q), want fenced naming %s", st, reason, idB)
	}
	if status, body := postCode(t, hcA, "/nopacs.v1.CatalogService/ListSubjects", `{"tenant":"`+tn+`"}`); status != http.StatusBadRequest ||
		!bytes.Contains(body, []byte("TENANT_FENCED")) || bytes.Contains(body, []byte(ctSubject)) {
		t.Fatalf("A's catalog of the fenced tenant, to its local operator: %d %s", status, body)
	}
	checkFenced(t, key, tn, inst, idB)
	hcA.CloseIdleConnections()
	code, out, errOut = nopacs(t.Context(), dirA, "tenant", "reclaim", tn)
	if code != 5 || !strings.Contains(out+errOut, idB) {
		t.Fatalf("tenant reclaim on A: exit %d\n%s\n%s", code, out, errOut)
	}
	stopServer(t, a)
	stopServer(t, b)
}
