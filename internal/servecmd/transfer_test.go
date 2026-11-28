package servecmd_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pydicomTransferScript reads every DICOM file under the directory given with
// pydicom and prints, one line per file, whether it still holds a patient
// name and the SOP Instance UID it holds, so the files a transfer put in the
// target are read by a tool that is not NoPACs.
const pydicomTransferScript = `
import os, sys, pydicom
for d, _, names in os.walk(sys.argv[1]):
    for n in sorted(names):
        try:
            ds = pydicom.dcmread(os.path.join(d, n))
        except Exception:
            continue
        print(str(ds.get("PatientName", "")) + "|" + str(ds.SOPInstanceUID))
`

// TestTransferMoveThroughServe guards a move between tenants as serve wires
// it, in a real server process, through the commands: a source tenant that
// archives as received (files imported from a tree of public corpus files)
// and a target whose receiver of type job de-identifies by a profile; "nopacs
// transfer create --move" copies the session through the target's receiver and
// deletes the source once each instance is confirmed; "transfer show" ends
// done with every instance confirmed, the target answers the subject under
// its own label and the source answers none; and, when NOPACS_PYTHON names an
// interpreter, pydicom reads the files the target archived and finds no
// patient name and no UID of the source.
func TestTransferMoveThroughServe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and a transfer; left out under -short")
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

	base := filepath.Dir(dataDir)
	roots, tree := filepath.Join(base, "roots"), filepath.Join(base, "tree")
	for _, f := range ctStudy {
		full := filepath.Join(tree, "Smith_John", "sessA", "ct", f)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, testdata(t, f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"import.roots","scope":{"installation":{}},"value":`+quoted("old="+tree)+`}}`)
	const from, to = "srca", "dstb"
	for _, tn := range []string{from, to} {
		post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
		post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	}
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"import.allowed-roots","scope":{"tenant":"`+from+`"},"value":"old"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+from+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.ProfileService/CreateProfileVersion", `{"tenant":"`+to+`","name":"research","document":{"bases":["annex-e/basic:2026d"]}}`)
	cli := func(args ...string) string {
		t.Helper()
		code, out, errOut := nopacs(t.Context(), dataDir, args...)
		if code != 0 {
			t.Fatalf("nopacs %v: exit %d\n%s\n%s", args, code, out, errOut)
		}
		return out
	}
	cli("receiver", "add", from, "old-tree", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain", "--quiet-period", "5s", "--as-received", "--enabled")
	cli("receiver", "add", to, "xfer", "--type", "job", "--storage", "main", "--profile", "research@1", "--quiet-period", "5s", "--enabled")

	var made struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(cli("job", "import", from, "old-tree", "Smith_John", "--json")), &made); err != nil || made.Job.ID == "" {
		t.Fatalf("job import: %v", err)
	}
	waitWithin(t, "the import to settle", 2*time.Minute, func() bool {
		var got struct {
			Job struct {
				EndTime string `json:"endTime"`
			} `json:"job"`
		}
		if err := json.Unmarshal([]byte(cli("job", "show", from, made.Job.ID, "--json")), &got); err != nil {
			t.Fatal(err)
		}
		return got.Job.EndTime != ""
	})
	var subjects struct {
		Subjects []struct {
			Label     string `json:"label"`
			Instances string `json:"instanceCount"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal([]byte(cli("catalog", "subjects", from, "--json")), &subjects); err != nil || len(subjects.Subjects) != 1 {
		t.Fatalf("the source's subjects: %v %+v", err, subjects)
	}
	subject := subjects.Subjects[0].Label
	count := subjects.Subjects[0].Instances

	if out := cli("transfer", "create", from, subject, "--to", to, "--reason", "study belongs to the trial in dstb", "--preview"); !strings.Contains(out, "xfer") ||
		!strings.Contains(out, "de-identifies") {
		t.Errorf("the preview:\n%s", out)
	}
	cli("transfer", "create", from, subject, "--to", to, "--move", "--reason", "study belongs to the trial in dstb", "--yes")
	var tr struct {
		Transfer struct {
			Job struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"job"`
			Step   string `json:"step"`
			Items  struct{ Confirmed, Failed string }
			Delete string `json:"deleteCorrectionId"`
		} `json:"transfer"`
	}
	var id string
	var list struct {
		Transfers []struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		} `json:"transfers"`
	}
	if err := json.Unmarshal([]byte(cli("transfer", "list", from, "--json")), &list); err != nil || len(list.Transfers) != 1 {
		t.Fatalf("transfer list: %v %+v", err, list)
	}
	id = list.Transfers[0].Job.ID
	waitWithin(t, "the transfer to end", 3*time.Minute, func() bool {
		if err := json.Unmarshal([]byte(cli("transfer", "show", from, id, "--json")), &tr); err != nil {
			t.Fatal(err)
		}
		return tr.Transfer.Step == "TRANSFER_STEP_ENDED"
	})
	if tr.Transfer.Job.State != "JOB_STATE_DONE" || tr.Transfer.Items.Confirmed != count || tr.Transfer.Items.Failed != "" && tr.Transfer.Items.Failed != "0" ||
		tr.Transfer.Delete == "" {
		t.Fatalf("the transfer ended %+v, want done with %s confirmed and the source's delete recorded\n%s", tr.Transfer, count, child.stderr.String())
	}
	// The target answers the instances under its own label; the source none.
	var got struct {
		Subjects []struct {
			Label     string `json:"label"`
			Instances string `json:"instanceCount"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal([]byte(cli("catalog", "subjects", to, "--json")), &got); err != nil || len(got.Subjects) != 1 || got.Subjects[0].Instances != count {
		t.Fatalf("the target's subjects: %v %+v, want one with %s instances", err, got, count)
	}
	if got.Subjects[0].Label == subject {
		t.Errorf("the target filed the subject under the source's label %q, not under its own", subject)
	}
	if err := json.Unmarshal([]byte(cli("catalog", "subjects", from, "--json")), &got); err != nil || len(got.Subjects) != 0 {
		t.Errorf("the source still answers subjects after the move: %v %+v", err, got)
	}
	if python := os.Getenv(headerPython); python != "" {
		script := filepath.Join(t.TempDir(), "check.py")
		if err := os.WriteFile(script, []byte(pydicomTransferScript), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, python, "-I", script, filepath.Join(roots, to, "main", "data")).CombinedOutput() //nolint:gosec // G304: the operator's interpreter.
		lines := strings.Fields(string(out))
		if err != nil || len(lines) != len(ctStudy) {
			t.Fatalf("pydicom read %d files of the target (%v), want %d\n%s", len(lines), err, len(ctStudy), out)
		}
		for _, l := range lines {
			name, uid, _ := strings.Cut(l, "|")
			if name != "" {
				t.Errorf("a file the target archived holds the patient name %q", name)
			}
			for _, f := range ctStudy {
				if strings.Contains(string(testdata(t, f)), uid) {
					t.Errorf("a file the target archived holds the SOP Instance UID %s of a source file", uid)
				}
			}
		}
	}
}
