package servecmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hashTree is the outside check of a storage directory: Python's hashlib
// reads every file under the root given and prints, as JSON, each file's
// path below the root and its SHA-256, without any of NoPACs' code.
const hashTree = `
import hashlib, json, os, sys
root = sys.argv[1]
out = {}
for d, _, files in os.walk(root):
    for f in files:
        p = os.path.join(d, f)
        with open(p, "rb") as fh:
            out[os.path.relpath(p, root).replace(os.sep, "/")] = hashlib.sha256(fh.read()).hexdigest()
print(json.dumps(out))
`

// outsideTree returns every file under root and its SHA-256, as Python's
// hashlib reads them, or skips the test when no Python is there.
func outsideTree(t *testing.T, root string) map[string]string {
	t.Helper()
	py := os.Getenv("NOPACS_PYTHON")
	if py == "" {
		var err error
		if py, err = exec.LookPath("python3"); err != nil {
			t.Skip("no Python: set NOPACS_PYTHON or put python3 on the PATH for the outside check of a purge")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "-I", "-c", hashTree, root).Output() //nolint:gosec // G204: a test's own interpreter and script.
	if err != nil {
		t.Fatalf("hashlib: %v", err)
	}
	got := map[string]string{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("hashlib answered %q: %v", out, err)
	}
	return got
}

// found lists the files under root whose path holds part, with find(1).
func found(t *testing.T, root, part string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "find", root, "-type", "f", "-path", "*"+part+"*").Output() //nolint:gosec // G204: the system's find over the test's own directory.
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// cliJSON runs a command with --json against the server in dataDir and
// decodes its document, failing the test unless it exits 0.
func cliJSON(t *testing.T, dataDir string, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := nopacs(t.Context(), dataDir, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("nopacs %v: exit %d\n%s\n%s", args, code, out, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("nopacs %v answered %q: %v", args, out, err)
	}
	return doc
}

// field walks a decoded document by keys.
func field(doc map[string]any, keys ...string) any {
	var v any = doc
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// str is field as a string.
func str(doc map[string]any, keys ...string) string {
	s, _ := field(doc, keys...).(string)
	return s
}

// sendFiles sends files with dcmtk's storescu to the receiver on port.
func sendFiles(t *testing.T, scu string, port int, files ...string) {
	t.Helper()
	args := []string{"-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port)}
	args = append(args, files...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, scu, append([]string{"-v"}, args...)...).CombinedOutput() //nolint:gosec // G204: a test's own tool and files.
	if err != nil || bytes.Count(out, []byte("Received Store Response (Success)")) != len(files) {
		t.Fatalf("storescu: %v\n%s", err, out)
	}
}

// subjectCounts returns each subject of the tenant and its instances, as
// ListSubjects answers.
func subjectCounts(t *testing.T, hc *http.Client, tenantName string) map[string]int {
	t.Helper()
	var res struct {
		Subjects []struct {
			Label         string `json:"label"`
			InstanceCount string `json:"instanceCount"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.CatalogService/ListSubjects", `{"tenant":"`+tenantName+`"}`), &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, s := range res.Subjects {
		n, _ := strconv.Atoi(s.InstanceCount)
		out[s.Label] = n
	}
	return out
}

// heldFiles returns the ids of the tenant's held quarantined files of
// reason.
func heldFiles(t *testing.T, hc *http.Client, tenantName, reason string) []string {
	t.Helper()
	var res struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.QuarantineService/ListQuarantinedFiles",
		`{"tenant":"`+tenantName+`","reason":"`+reason+`","state":"QUARANTINE_STATE_HELD"}`), &res); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range res.Files {
		ids = append(ids, f.ID)
	}
	return ids
}

// TestCorrectionsThroughServe guards corrections as serve wires them, in a
// real server process, through the command line: the CT study and an MR
// file archived by dcmtk's storescu; the CT subject deleted after its
// preview (hidden from the catalog, its files still on the storage), undone
// (shown again), deleted again, and purged once its grace period (3s,
// delete.grace on the tenant) has passed, after which find(1) finds none of
// its files and Python's hashlib finds every other file of the storage
// unchanged; a corrected MR file sent again and quarantined as a different
// file replaces the archived instance as revision 2, the bytes hashlib
// reads being those sent; the MR subject relabeled, its files under the new
// label with the same bytes; and the deleted study sent again quarantined
// as deleted, keeping none of its bytes, until every value the delete holds
// is allowed, after which the study sent once more is archived again.
func TestCorrectionsThroughServe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and sends studies over the network; left out under -short")
	}
	scu := storescu(t)
	dataDir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	child := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir)
	waitServing(t, dataDir, child)
	hc := socketClient(dataDir)
	defer hc.CloseIdleConnections()

	const tn = "fix1"
	roots := filepath.Join(filepath.Dir(dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	port := freePort(t)
	for name, value := range map[string]string{"receiver.ports": strconv.Itoa(port), "receiver.allow-as-received": "true", "delete.grace": "3s"} {
		post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"`+name+`","scope":{"tenant":"`+tn+`"},"value":"`+value+`"}}`)
	}
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tn+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	waitWithin(t, "the receiver to be up", 10*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})
	testdata := filepath.Join("..", "receive", "testdata")
	var ct []string
	for _, f := range ctStudy {
		ct = append(ct, filepath.Join(testdata, f))
	}
	mrFile := filepath.Join(testdata, "MR_small.dcm")
	sendFiles(t, scu, port, append(ct, mrFile)...)
	var ctSubject, mrSubject string
	waitBody(t, "both subjects to be archived", func() bool {
		counts := subjectCounts(t, hc, tn)
		ctSubject, mrSubject = "", ""
		for s, n := range counts {
			switch n {
			case len(ctStudy):
				ctSubject = s
			case 1:
				mrSubject = s
			}
		}
		return len(counts) == 2 && ctSubject != "" && mrSubject != ""
	})
	// Every reception ended, so nothing moves under the preview's token.
	waitBody(t, "the receptions to end", func() bool {
		return !bytes.Contains(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`","inProgress":true}`), []byte(`"id"`))
	})
	root := filepath.Join(roots, tn, "main")
	before := outsideTree(t, root)
	if n := len(found(t, root, "data/"+ctSubject+"/")); n != len(ctStudy) {
		t.Fatalf("find finds %d files of the CT subject, want %d", n, len(ctStudy))
	}

	// The preview, then the delete with its token, then the undo.
	prev := cliJSON(t, dataDir, "correction", "delete", tn, ctSubject, "--reason", "consent withdrawn", "--preview")
	if str(prev, "preview", "counts", "instances") != strconv.Itoa(len(ctStudy)) || str(prev, "preview", "confirm") == "" {
		t.Fatalf("the preview is %v", prev)
	}
	del := cliJSON(t, dataDir, "correction", "delete", tn, ctSubject, "--reason", "consent withdrawn", "--yes")
	id := str(del, "correction", "id")
	if str(del, "correction", "state") != "CORRECTION_STATE_COMMITTED" || id == "" {
		t.Fatalf("the delete answered %v", del)
	}
	if _, ok := subjectCounts(t, hc, tn)[ctSubject]; ok {
		t.Fatal("the deleted subject is still in the catalog")
	}
	if n := len(found(t, root, "data/"+ctSubject+"/")); n != len(ctStudy) {
		t.Fatalf("find finds %d files of the deleted subject before its purge, want %d", n, len(ctStudy))
	}
	if und := cliJSON(t, dataDir, "correction", "undo", tn, id, "--reason", "deleted the wrong subject"); str(und, "correction", "state") != "CORRECTION_STATE_UNDONE" {
		t.Fatalf("the undo answered %v", und)
	}
	if n := subjectCounts(t, hc, tn)[ctSubject]; n != len(ctStudy) {
		t.Fatalf("the undone subject holds %d instances, want %d", n, len(ctStudy))
	}

	// Deleted again, then purged once its grace has passed.
	del = cliJSON(t, dataDir, "correction", "delete", tn, ctSubject, "--reason", "consent withdrawn", "--yes")
	id = str(del, "correction", "id")
	if p, _ := time.Parse(time.RFC3339Nano, str(del, "correction", "purgeTime")); time.Until(p) > 4*time.Second {
		t.Fatalf("the purge time %v is not 3s away", p)
	}
	time.Sleep(4 * time.Second)
	pur := cliJSON(t, dataDir, "correction", "purge", tn, id, "--yes", "--wait")
	disp, _ := field(pur, "correction", "dispositions").([]any)
	if str(pur, "correction", "state") != "CORRECTION_STATE_PURGED" || len(disp) != 1 {
		t.Fatalf("the purge answered %v", pur)
	}
	// Every object the delete owned removed: each instance, and each of the
	// study's two scans' manifest and metadata.
	if d := disp[0].(map[string]any); d["storage"] != "main" || fmt.Sprint(d["objectsRemoved"]) != strconv.Itoa(len(ctStudy)+4) ||
		fmt.Sprint(d["objectsHidden"]) != "0" || fmt.Sprint(d["objectsUnknown"]) != "0" || fmt.Sprint(d["objectsRetained"]) != "0" {
		t.Fatalf("the purge says of the storage %v", d)
	}
	if left := found(t, root, "/"+ctSubject+"/"); len(left) != 0 {
		t.Fatalf("find finds the purged subject's files: %v", left)
	}
	after := outsideTree(t, root)
	for path, sum := range before {
		if strings.Contains(path, "/"+ctSubject+"/") {
			if _, ok := after[path]; ok {
				t.Errorf("hashlib reads the purged %s", path)
			}
			continue
		}
		if after[path] != sum {
			t.Errorf("hashlib reads %s as %q, before the purge %q", path, after[path], sum)
		}
	}
	if _, ok := after["meta/.corrections/"+id+".json"]; !ok {
		t.Error("the purged delete's record is not on the storage")
	}

	// A corrected MR file replaces the archived instance.
	mr, err := os.ReadFile(mrFile) //nolint:gosec // G304: a test file of the repository.
	if err != nil {
		t.Fatal(err)
	}
	// A pixel changed, the UIDs kept: the middle of the file is in its pixel
	// data, which storescu sends as it is (it drops the trailing padding).
	mr[len(mr)/2] ^= 0x5a
	corrected := filepath.Join(t.TempDir(), "MR_corrected.dcm")
	if err := os.WriteFile(corrected, mr, 0o600); err != nil { //nolint:gosec // G703: a path under the test's own temporary directory.
		t.Fatal(err)
	}
	sendFiles(t, scu, port, corrected)
	var held []string
	waitBody(t, "the corrected file to be held", func() bool {
		held = heldFiles(t, hc, tn, "QUARANTINE_REASON_DIFFERENT_FILE")
		return len(held) == 1
	})
	rep := cliJSON(t, dataDir, "correction", "replace", tn, held[0], "--reason", "corrected file sent again")
	if str(rep, "correction", "state") != "CORRECTION_STATE_COMMITTED" || fmt.Sprint(field(rep, "correction", "newRevision")) != "2" {
		t.Fatalf("the replace answered %v", rep)
	}
	sum := str(rep, "correction", "newSha256")
	revs := found(t, root, ".r2.dcm")
	if len(revs) != 1 || outsideTree(t, filepath.Dir(revs[0]))[filepath.Base(revs[0])] != sum {
		t.Fatalf("find finds revision 2 at %v, hashlib reads another checksum than %s", revs, sum)
	}
	cliJSON(t, dataDir, "correction", "purge", tn, str(rep, "correction", "id"), "--yes", "--wait")

	// The MR subject relabeled: the same bytes under the new label.
	mrBefore := map[string]string{}
	for path, s := range outsideTree(t, root) {
		if strings.HasPrefix(path, "data/"+mrSubject+"/") {
			mrBefore[strings.TrimPrefix(path, "data/"+mrSubject+"/")] = s
		}
	}
	rel := cliJSON(t, dataDir, "correction", "relabel", tn, mrSubject, "--to", "sub-renamed", "--reason", "wrong subject id at the scanner", "--wait")
	if str(rel, "correction", "state") != "CORRECTION_STATE_COMMITTED" {
		t.Fatalf("the relabel answered %v", rel)
	}
	if counts := subjectCounts(t, hc, tn); counts["sub-renamed"] != 1 || counts[mrSubject] != 0 {
		t.Fatalf("after the relabel the catalog holds %v", counts)
	}
	renamed := outsideTree(t, filepath.Join(root, "data", "sub-renamed"))
	for path, s := range mrBefore {
		if renamed[path] != s {
			t.Errorf("hashlib reads %s under the new label as %q, under the old %q", path, renamed[path], s)
		}
	}

	// The deleted study sent again is held as deleted, until each value the
	// delete holds is allowed.
	sendFiles(t, scu, port, ct...)
	waitBody(t, "the re-sent study to be held as deleted", func() bool {
		return len(heldFiles(t, hc, tn, "QUARANTINE_REASON_WAS_DELETED")) == len(ctStudy)
	})
	if _, ok := subjectCounts(t, hc, tn)[ctSubject]; ok {
		t.Fatal("the re-sent deleted study was archived")
	}
	show := cliJSON(t, dataDir, "correction", "show", tn, id)
	holds, _ := field(show, "correction", "holds").([]any)
	if len(holds) < 3 {
		t.Fatalf("the purged delete holds %v", holds)
	}
	flags := map[string]string{"HELD_VALUE_KIND_SUBJECT": "--subject", "HELD_VALUE_KIND_STUDY": "--study", "HELD_VALUE_KIND_SERIES": "--series", "HELD_VALUE_KIND_INSTANCE": "--instance"}
	var last map[string]any
	for _, h := range holds {
		m := h.(map[string]any)
		last = cliJSON(t, dataDir, "correction", "allow", tn, id, flags[m["kind"].(string)], m["value"].(string), "--reason", "consent given again")
	}
	if field(last, "correction", "arrivalsAllowed") != true {
		t.Fatalf("after every value the delete is %v", last)
	}
	// A file held as deleted kept none of its bytes (a withdrawal keeps
	// nothing of what arrives again), so the sender sends the study again.
	sendFiles(t, scu, port, ct...)
	waitBody(t, "the allowed study to be archived again", func() bool {
		return subjectCounts(t, hc, tn)[ctSubject] == len(ctStudy)
	})
}
