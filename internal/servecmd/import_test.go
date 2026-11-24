package servecmd_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// importFolder is the tree's folder, named as a site might name a folder
// after a patient, so a reader's answers are searched for it.
const importFolder = "Smith_John"

// pydicomImportScript reads every DICOM file under the two directories
// given, the tree and the archive, with pydicom, and prints one line per
// directory: how many files it read, and the sorted pairs of SOP Instance
// UID and SHA-256 of their bytes, so the archive is compared with the tree
// by a tool that is not NoPACs.
const pydicomImportScript = `
import hashlib, os, sys, pydicom
for top in sys.argv[1:]:
    pairs = []
    for d, _, names in os.walk(top):
        for n in names:
            p = os.path.join(d, n)
            if os.path.islink(p):
                continue
            try:
                ds = pydicom.dcmread(p)
            except Exception:
                continue
            pairs.append(str(ds.SOPInstanceUID) + "=" + hashlib.sha256(open(p, "rb").read()).hexdigest())
    pairs.sort()
    print(len(pairs), ",".join(pairs))
`

// TestImportThroughServe guards the import as serve wires it, in a real
// server process, through the commands: an import root allotted to the
// tenant, an import receiver added by "nopacs receiver add --type import",
// a job made by "nopacs job import" over a tree of public corpus files with a
// text file and a link out of the root, followed by "nopacs job show" until
// it settles done; "nopacs job files" lists the two skipped files by path and
// reason and "nopacs job units" the unit with its counts; every DICOM file of
// the tree is in the archive byte for byte (by pydicom and hashlib, when
// NOPACS_PYTHON names an interpreter), the link not followed, the tree
// unchanged; and a reader key's answers over the network hold no path,
// while the import's files are refused to it.
func TestImportThroughServe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and imports a tree; left out under -short")
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
	roots, tree, outside := filepath.Join(base, "roots"), filepath.Join(base, "tree"), filepath.Join(base, "outside.dcm")
	files := map[string]string{} // path under the tree: corpus file
	for _, f := range ctStudy {
		files[importFolder+"/sessA/ct/"+f] = f
	}
	files[importFolder+"/sessB/MR_small.dcm"] = "MR_small.dcm"
	for p, f := range files {
		full := filepath.Join(tree, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, testdata(t, f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, importFolder, "sessA", "notes.txt"), []byte("notes about the visit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, testdata(t, "liver_1frame.dcm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, importFolder, "sessB", "link.dcm")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	before := treeSums(t, tree)

	const tn = "imp1"
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"import.roots","scope":{"installation":{}},"value":`+quoted("old="+tree)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"import.allowed-roots","scope":{"tenant":"`+tn+`"},"value":"old"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+tn+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	// A root inside the data directory is refused.
	if code, _ := call(hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"import.roots","scope":{"installation":{}},"value":`+
		quoted("old="+tree+"\ndata="+filepath.Join(dataDir, "tenants"))+`}}`, ""); code != http.StatusBadRequest {
		t.Errorf("a root inside the data directory: HTTP %d, want 400 (failed precondition)", code)
	}

	cli := func(args ...string) string {
		t.Helper()
		code, out, errOut := nopacs(t.Context(), dataDir, args...)
		if code != 0 {
			t.Fatalf("nopacs %v: exit %d\n%s\n%s", args, code, out, errOut)
		}
		return out
	}
	cli("receiver", "add", tn, "old-tree", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain",
		"--quiet-period", "5s", "--as-received", "--enabled")
	if show := cli("receiver", "show", tn, "old-tree"); !strings.Contains(show, "idle") {
		t.Errorf("receiver show:\n%s", show)
	}
	var made struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(cli("job", "import", tn, "old-tree", importFolder, "--json")), &made); err != nil || made.Job.ID == "" {
		t.Fatalf("job import: %v", err)
	}
	type counts struct {
		Found, HandedIn, Archived, Processing string
		Skipped                               []struct {
			Code  string
			Count string
		}
	}
	var got struct {
		Job struct {
			State     string `json:"state"`
			EndTime   string `json:"endTime"`
			ImportJob struct {
				Path   string `json:"path"`
				Counts counts `json:"counts"`
			} `json:"importJob"`
		} `json:"job"`
	}
	waitWithin(t, "the import to settle", 2*time.Minute, func() bool {
		if err := json.Unmarshal([]byte(cli("job", "show", tn, made.Job.ID, "--json")), &got); err != nil {
			t.Fatal(err)
		}
		return got.Job.EndTime != "" && (got.Job.ImportJob.Counts.Processing == "" || got.Job.ImportJob.Counts.Processing == "0")
	})
	c := got.Job.ImportJob.Counts
	if got.Job.State != "JOB_STATE_DONE" || c.Found != "10" || c.HandedIn != "8" || c.Archived != "8" || len(c.Skipped) != 2 || got.Job.ImportJob.Path != importFolder {
		t.Fatalf("the job ended %s with %+v (path %q), want done, 10 found, 8 archived, 2 skipped\n%s", got.Job.State, c, got.Job.ImportJob.Path, child.stderr.String())
	}
	filesOut := cli("job", "files", tn, made.Job.ID)
	for _, want := range []string{importFolder + "/sessA/notes.txt", "skipped: not_part10", importFolder + "/sessB/link.dcm", "skipped: not_regular"} {
		if !strings.Contains(filesOut, want) {
			t.Errorf("job files holds no %q:\n%s", want, filesOut)
		}
	}
	if units := cli("job", "units", tn, made.Job.ID); !strings.Contains(units, importFolder) || !strings.Contains(units, "done") {
		t.Errorf("job units:\n%s", units)
	}
	if after := treeSums(t, tree); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the tree changed:\n%v\nwas\n%v", after, before)
	}

	// The archive against the tree, by pydicom and hashlib.
	if python := os.Getenv(headerPython); python != "" {
		script := filepath.Join(t.TempDir(), "check.py")
		if err := os.WriteFile(script, []byte(pydicomImportScript), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, python, "-I", script, tree, filepath.Join(roots, tn, "main", "data")).CombinedOutput() //nolint:gosec // G204: the operator's interpreter.
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err != nil || len(lines) != 2 || !strings.HasPrefix(lines[0], "8 ") || lines[0] != lines[1] {
			t.Fatalf("pydicom: the archive is not the tree's DICOM files byte for byte: %v\n%s", err, out)
		}
		t.Logf("pydicom: the tree and the archive hold the same %s", strings.SplitN(lines[0], " ", 2)[0]+" instances, byte for byte")
	}

	// A reader's answers over the network hold no path; the files are refused.
	var key struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(post(t, hc, "/nopacs.v1.AccessService/CreateKey", `{"holder":"import reader"}`), &key); err != nil {
		t.Fatal(err)
	}
	post(t, hc, "/nopacs.v1.AccessService/SetGrant", `{"keyId":"`+key.Key.ID+`","scope":{"tenant":"`+tn+`"},"role":"ROLE_READER"}`)
	asReader := func(procedure, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+listen+procedure, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, p := range [][2]string{
		{"/nopacs.v1.JobService/GetJob", `{"tenant":"` + tn + `","id":"` + made.Job.ID + `"}`},
		{"/nopacs.v1.JobService/ListJobs", `{"tenant":"` + tn + `"}`},
		{"/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"` + tn + `"}`},
		{"/nopacs.v1.QuarantineService/ListQuarantinedFiles", `{"tenant":"` + tn + `"}`},
	} {
		code, body := asReader(p[0], p[1])
		if code != http.StatusOK || strings.Contains(body, importFolder) {
			t.Errorf("a reader's %s: HTTP %d, holds the folder's name: %v\n%s", p[0], code, strings.Contains(body, importFolder), body)
		}
	}
	if code, _ := asReader("/nopacs.v1.JobService/ListImportFiles", `{"tenant":"`+tn+`","id":"`+made.Job.ID+`"}`); code != http.StatusForbidden {
		t.Errorf("a reader's ListImportFiles: HTTP %d, want 403", code)
	}
}

// treeSums returns each file of the tree with its size and modification
// time, and each link with its target, sorted, so a change of the tree
// shows.
func treeSums(t *testing.T, tree string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(tree, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		line := p + " " + info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			line += " -> " + target
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
