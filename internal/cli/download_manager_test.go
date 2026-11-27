package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestDownloadSetFromAFileAndFilters guards the ways of choosing a set: a
// file of references with comments (a file two references both name is
// fetched once), references named after the tenant together with it, the
// scans a catalog filter selects, and the refusals (a filter with
// references, a filter that matches nothing, a bad line named by number).
func TestDownloadSetFromAFileAndFilters(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	dir := t.TempDir()
	list := filepath.Join(dir, "refs.txt")
	if err := os.WriteFile(list, []byte("# two overlapping references\nsub-a/ses-1\n\nsub-a/ses-1/scan-2\nsub-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	res := r.run("download", "lab1", "sub-a/ses-2", "--refs-file", list, "--to", out, "--json")
	doc := document(t, res)
	if doc["files"] != float64(5) || doc["total"] != float64(5) || doc["skipped"] != float64(0) {
		t.Errorf("a set from references and a file: %v", doc)
	}
	tree := readTree(t, out)
	if len(tree) != 5 || tree["sub-b/ses-1/scan-1/1.2.3.4.1.dcm"] == nil || tree["sub-a/ses-2/scan-1/1.2.3.3.1.dcm"] == nil || tree["sub-a/ses-1/scan-2/1.2.3.2.1.dcm"] == nil {
		t.Errorf("the tree holds %v", keysOf(tree))
	}

	mr := filepath.Join(dir, "mr")
	doc = document(t, r.run("download", "lab1", "--modality", "MR", "--to", mr, "--json"))
	if doc["files"] != float64(3) {
		t.Errorf("a modality filter: %v", doc)
	}
	if got := readTree(t, mr); len(got) != 3 || got["sub-a/ses-1/scan-2/1.2.3.2.1.dcm"] != nil {
		t.Errorf("the MR scans are %v, the CT scan must not be there", keysOf(got))
	}
	two := filepath.Join(dir, "since")
	doc = document(t, r.run("download", "lab1", "--receiver", "scanner2", "--archived-since", "30d", "--to", two, "--json"))
	if doc["files"] != float64(1) {
		t.Errorf("a receiver filter with a time bound: %v", doc)
	}

	for _, c := range []struct {
		args []string
		code int
		say  string
	}{
		{[]string{"download", "lab1", "sub-a", "--modality", "MR", "--to", mr}, 2, "name no references"},
		{[]string{"download", "lab1", "--modality", "PET", "--to", mr}, 4, "no scan of the tenant matches"},
		{[]string{"download", "lab1", "--to", mr}, 2, "name what to download"},
		{[]string{"download", "lab1", "--refs-file", filepath.Join(dir, "missing"), "--to", mr}, 2, "cannot read"},
		{[]string{"download", "lab1", "--parallel", "0", "sub-a", "--to", mr}, 2, "--parallel 0 is not from 1 to 16"},
		{[]string{"download", "lab1", "--parallel", "17", "sub-a", "--to", mr}, 2, "--parallel 17 is not from 1 to 16"},
		{[]string{"download", "lab1", "sub-a", "--limit-rate", "fast", "--to", mr}, 2, "--limit-rate"},
		{[]string{"download", "lab1", "sub-a", "--layout", "zip", "--to", mr}, 2, "--layout"},
		{[]string{"download", "lab1", "sub-a", "--progress", "loud", "--to", mr}, 2, "--progress"},
	} {
		if got := r.run(c.args...); got.code != c.code || !strings.Contains(got.stderr, c.say) {
			t.Errorf("%v: exit %d, standard error %q, want exit %d saying %q", c.args, got.code, got.stderr, c.code, c.say)
		}
	}
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("sub-a\n../x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.run("download", "lab1", "--refs-file", bad, "--to", mr); got.code != 2 || !strings.Contains(got.stderr, "line 2") {
		t.Errorf("a bad line: exit %d, %q", got.code, got.stderr)
	}
}

// TestDownloadStaysWithinTheServersReadBound guards the answer to the
// server's refusal: with more streams asked than the server lets the caller
// hold, the download does not fail and does not exceed the bound by more than
// the refused attempts, it waits, reports that it waited as a "throttled"
// line, and every file arrives whole and equal to what the server holds.
func TestDownloadStaysWithinTheServersReadBound(t *testing.T) {
	r := newArchiveRig(t)
	want := seedMany(r, 10, 3, 3000)
	r.fake.SetChunkSize(1000)
	p := newReadProxy(t, r.fake, 2, 4*time.Millisecond)
	out := filepath.Join(t.TempDir(), "out")
	res := runCtx(context.Background(), p.env(), "download", "lab1", "sub-p", "--to", out, "--parallel", "8", "--progress", "json", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, standard error %q", res.code, res.stderr)
	}
	doc := stdoutDoc(t, res)
	if doc["files"] != float64(30) || len(doc["failed"].([]any)) != 0 {
		t.Errorf("the result: %v", doc)
	}
	checkTree(t, out, want)
	peak, refused := p.stats()
	if peak > 2 {
		t.Errorf("%d reads were open at once, the server allows 2", peak)
	}
	if refused == 0 {
		t.Error("the server never refused a stream, so the wait was not exercised")
	}
	throttled := 0
	for _, ev := range jsonLinesOf(t, res.stderr) {
		if ev["event"] == "throttled" {
			throttled++
			if ev["waitSeconds"].(float64) <= 0 || ev["streams"].(float64) < 1 || ev["streams"].(float64) > 8 {
				t.Errorf("a throttled line: %v", ev)
			}
		}
	}
	if throttled == 0 {
		t.Errorf("no throttled line on standard error:\n%s", res.stderr)
	}

	// With the bound under the streams asked, a smaller ask is never refused.
	p2 := newReadProxy(t, r.fake, 3, time.Millisecond)
	again := filepath.Join(t.TempDir(), "again")
	res = runCtx(context.Background(), p2.env(), "download", "lab1", "sub-p", "--to", again, "--parallel", "3", "--quiet", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, standard error %q", res.code, res.stderr)
	}
	if peak, refused := p2.stats(); peak > 3 || refused != 0 {
		t.Errorf("with 3 streams asked and 3 allowed: peak %d, %d refused", peak, refused)
	}
	checkTree(t, again, want)
}

// TestDownloadProgressLines guards the machine-readable progress: with
// --progress json every line of standard error is one JSON document with the
// documented fields, the last is the "done" phase with every file counted,
// and a plain --json run still writes nothing to standard error.
func TestDownloadProgressLines(t *testing.T) {
	r := newArchiveRig(t)
	want := seedMany(r, 6, 2, 2000)
	p := newReadProxy(t, r.fake, 0, 2*time.Millisecond)
	out := filepath.Join(t.TempDir(), "out")
	res := runCtx(context.Background(), p.env(), "download", "lab1", "sub-p", "--to", out, "--progress", "json", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d, standard error %q", res.code, res.stderr)
	}
	lines := jsonLinesOf(t, res.stderr)
	if len(lines) < 2 {
		t.Fatalf("%d lines of progress:\n%s", len(lines), res.stderr)
	}
	for _, ev := range lines {
		if ev["schemaVersion"] != float64(1) || ev["event"] != "progress" {
			t.Errorf("a line: %v", ev)
		}
	}
	last := lines[len(lines)-1]
	files := last["files"].(map[string]any)
	bytesDoc := last["bytes"].(map[string]any)
	if last["phase"] != "done" || files["done"] != float64(12) || files["total"] != float64(12) || files["fetched"] != float64(12) || files["failed"] != float64(0) ||
		bytesDoc["received"] != bytesDoc["total"] || bytesDoc["received"].(float64) == 0 {
		t.Errorf("the last line: %v", last)
	}
	if _, ok := last["rateBytesPerSecond"].(float64); !ok {
		t.Errorf("no rate in %v", last)
	}
	if _, ok := last["elapsedSeconds"].(float64); !ok {
		t.Errorf("no elapsed time in %v", last)
	}
	checkTree(t, out, want)

	plain := filepath.Join(t.TempDir(), "plain")
	res = runCtx(context.Background(), p.env(), "download", "lab1", "sub-p", "--to", plain, "--json")
	if res.code != 0 || res.stderr != "" {
		t.Errorf("--json alone: exit %d, standard error %q, want none", res.code, res.stderr)
	}
	text := filepath.Join(t.TempDir(), "text")
	res = runCtx(context.Background(), p.env(), "download", "lab1", "sub-p", "--to", text, "--progress", "text")
	if res.code != 0 || !strings.Contains(res.stderr, "12/12 files") {
		t.Errorf("--progress text: exit %d, standard error %q", res.code, res.stderr)
	}
}

// TestDownloadLimitRate guards the bandwidth bound across all streams: 40000
// bytes at 20000 bytes a second over four parallel streams cannot arrive in
// less than a second and a half, though the same download takes a fraction
// of that without the bound.
func TestDownloadLimitRate(t *testing.T) {
	r := newArchiveRig(t)
	want := seedMany(r, 4, 2, 4996)
	r.fake.SetChunkSize(1000)
	started := time.Now()
	out := filepath.Join(t.TempDir(), "out")
	if res := r.run("download", "lab1", "sub-p", "--to", out, "--parallel", "4", "--quiet"); res.code != 0 {
		t.Fatalf("exit %d, standard error %q", res.code, res.stderr)
	}
	free := time.Since(started)
	checkTree(t, out, want)

	slow := filepath.Join(t.TempDir(), "slow")
	started = time.Now()
	res := r.run("download", "lab1", "sub-p", "--to", slow, "--parallel", "4", "--limit-rate", "20000", "--quiet", "--json")
	took := time.Since(started)
	if res.code != 0 {
		t.Fatalf("exit %d, standard error %q", res.code, res.stderr)
	}
	checkTree(t, slow, want)
	if doc := stdoutDoc(t, res); doc["bytes"].(float64) < 39000 {
		t.Errorf("received %v bytes", doc["bytes"])
	}
	if took < 1500*time.Millisecond {
		t.Errorf("about 40000 bytes at 20000 B/s arrived in %s (without the bound: %s)", took, free)
	}
	if free > 1500*time.Millisecond {
		t.Logf("the unbounded download took %s", free)
	}
	for _, bad := range []string{"0", "-5", "10XiB"} {
		if got := r.run("download", "lab1", "sub-p", "--to", slow, "--limit-rate", bad); got.code != 2 {
			t.Errorf("--limit-rate %s: exit %d, want 2", bad, got.code)
		}
	}
	if got := r.run("download", "lab1", "sub-p", "--to", filepath.Join(t.TempDir(), "x"), "--limit-rate", "10MiB/s", "--quiet"); got.code != 0 {
		t.Errorf("--limit-rate 10MiB/s: exit %d, %q", got.code, got.stderr)
	}
}

// TestDownloadStoppedAndResumedWithASelectionFile guards the selection file
// end to end: a download of 12 scans in four parallel streams is stopped
// midway (the context ends), leaves only whole files, which the selection
// file records one for one; the same command run again skips exactly those
// without fetching them, fetches the rest, and every file ends equal to what
// the server holds. A file changed on disk afterward (same size, later time)
// is noticed and fetched again; naming only the selection file resumes the
// same set.
func TestDownloadStoppedAndResumedWithASelectionFile(t *testing.T) {
	r := newArchiveRig(t)
	want := seedMany(r, 12, 3, 4000)
	r.fake.SetChunkSize(1000)
	p := newReadProxy(t, r.fake, 0, 6*time.Millisecond)
	base := t.TempDir()
	out := filepath.Join(base, "out")
	sel := filepath.Join(base, "selection.json")
	args := []string{"download", "lab1", "sub-p", "--to", out, "--parallel", "4", "--selection", sel, "--quiet", "--json"}

	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go func() {
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Millisecond):
				if countDCM(out) >= 8 {
					return
				}
			}
		}
	}()
	res := runCtx(ctx, p.env(), args...)
	close(stop)
	cancel()
	if res.code == 0 || !strings.Contains(res.stderr, "interrupted") || !strings.Contains(res.stderr, "running the same download again continues") {
		t.Fatalf("a stopped download: exit %d, standard error %q", res.code, res.stderr)
	}
	whole := countDCM(out)
	if whole < 8 || whole >= 36 {
		t.Fatalf("%d files were whole when the download stopped, want from 8 to 35", whole)
	}
	// The proxy counts a stream until its handler returns, which is a moment
	// after the client closed it, so a lane that ended just before another
	// began may be counted with it: four asked, never more than six counted.
	if peak, _ := p.stats(); peak > 6 {
		t.Errorf("%d streams were open at once, 4 were asked", peak)
	}
	var doc struct {
		Format string `json:"format"`
		Tenant string `json:"tenant"`
		Refs   []string
		Files  []struct {
			Path   string `json:"path"`
			Bytes  int64  `json:"bytes"`
			SHA256 string `json:"sha256"`
		}
	}
	b, err := os.ReadFile(sel)
	if err != nil {
		t.Fatalf("no selection file after a stop: %v", err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the selection file is not JSON: %v", err)
	}
	if doc.Format != "nopacs-download-selection/1" || doc.Tenant != "lab1" || len(doc.Refs) != 1 || doc.Refs[0] != "sub-p" || len(doc.Files) != whole {
		t.Errorf("the selection file after a stop: %s", b)
	}
	for _, f := range doc.Files {
		data := want[f.Path]
		sum := sha256.Sum256(data)
		if data == nil || f.Bytes != int64(len(data)) || f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("the selection records %s as %d bytes %s", f.Path, f.Bytes, f.SHA256)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(base, ".selection.json-*")); len(left) != 0 {
		t.Errorf("a temporary file of the selection was left: %v", left)
	}
	var recorded []string
	for _, f := range doc.Files {
		recorded = append(recorded, f.Path)
	}
	sort.Strings(recorded)

	before := fetched(r)
	res = runCtx(context.Background(), p.env(), args...)
	if res.code != 0 {
		t.Fatalf("the resumed download: exit %d, standard error %q", res.code, res.stderr)
	}
	rd := stdoutDoc(t, res)
	if rd["skipped"] != float64(whole) || rd["files"] != float64(36-whole) || rd["total"] != float64(36) {
		t.Errorf("the resumed download did %v, want %d skipped and %d fetched", rd, whole, 36-whole)
	}
	checkTree(t, out, want)
	// The streams of the second run asked only for what was missing: no
	// stream began at a file the first run finished.
	for _, req := range r.fake.ReadFilesRequests()[before:] {
		if req.GetAnnounceOnly() {
			continue
		}
		path := fmt.Sprintf("sub-p/%s/%s/%s.dcm", req.GetSessionLabel(), req.GetScanLabel(), req.GetStartAfter().GetInstanceId())
		if req.GetStartAfter().GetInstanceId() != "" && want[path] == nil {
			t.Errorf("a stream started after %s, which is not a file of the set", path)
		}
	}

	// A rerun with everything whole fetches nothing and opens no stream of
	// bytes.
	before = fetched(r)
	rd = stdoutDoc(t, runCtx(context.Background(), p.env(), args...))
	if rd["skipped"] != float64(36) || rd["files"] != float64(0) || rd["bytes"] != float64(0) {
		t.Errorf("a rerun of a finished download: %v", rd)
	}
	for _, req := range r.fake.ReadFilesRequests()[before:] {
		if !req.GetAnnounceOnly() {
			t.Errorf("a rerun of a finished download opened a stream of bytes: %v", req)
		}
	}

	// A file whose bytes changed on disk (same size, later time) is read,
	// found different and fetched again.
	victim := filepath.Join(out, "sub-p", "ses-1", "scan-3", "1.7.3.2.dcm")
	orig := want["sub-p/ses-1/scan-3/1.7.3.2.dcm"]
	if err := os.WriteFile(victim, bytes.Repeat([]byte{0x55}, len(orig)), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(victim, later, later); err != nil {
		t.Fatal(err)
	}
	rd = stdoutDoc(t, runCtx(context.Background(), p.env(), args...))
	if rd["files"] != float64(1) || rd["skipped"] != float64(35) {
		t.Errorf("after a file changed on disk: %v", rd)
	}
	checkTree(t, out, want)

	// Naming only the selection file resumes the set it holds.
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	rd = stdoutDoc(t, runCtx(context.Background(), p.env(), "download", "lab1", "--to", out, "--selection", sel, "--quiet", "--json"))
	if rd["files"] != float64(1) || rd["skipped"] != float64(35) {
		t.Errorf("resuming from the selection file alone: %v", rd)
	}
	checkTree(t, out, want)
	if res := runCtx(context.Background(), p.env(), "download", "lab2", "--to", out, "--selection", sel); res.code != 2 || !strings.Contains(res.stderr, "made for tenant") {
		t.Errorf("a selection file of another tenant: exit %d, %q", res.code, res.stderr)
	}
}

// fetched is how many ReadFiles requests the fake has answered so far.
func fetched(r *rig) int { return len(r.fake.ReadFilesRequests()) }

// TestDownloadBagIt guards the BagIt layout: the payload under data/, the
// four files of the bag with the SHA-256 the server announced, nothing
// written when a file failed, and, when the Python bagit package is on the
// PATH, a validation by it that passes, and fails when a payload byte is
// changed afterward.
func TestDownloadBagIt(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	out := filepath.Join(t.TempDir(), "bag")
	res := r.run("download", "lab1", "sub-a", "sub-b", "--to", out, "--layout", "bagit", "--parallel", "3")
	expect(t, res, 0, "downloaded 5 files (4.1 KiB) to "+out+"; 0 already complete; 0 failed; BagIt bag written\n", "")
	tree := readTree(t, out)
	for _, name := range []string{"bagit.txt", "bag-info.txt", "manifest-sha256.txt", "tagmanifest-sha256.txt"} {
		if tree[name] == nil {
			t.Errorf("%s is missing; the bag holds %v", name, keysOf(tree))
		}
	}
	if string(tree["bagit.txt"]) != "BagIt-Version: 1.0\nTag-File-Character-Encoding: UTF-8\n" {
		t.Errorf("bagit.txt: %q", tree["bagit.txt"])
	}
	if !strings.Contains(string(tree["bag-info.txt"]), "Payload-Oxum: 4196.5\n") {
		t.Errorf("bag-info.txt: %q", tree["bag-info.txt"])
	}
	var manifest []string
	for path, data := range tree {
		if strings.HasPrefix(path, "data/") {
			sum := sha256.Sum256(data)
			manifest = append(manifest, hex.EncodeToString(sum[:])+"  "+path)
		}
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i][66:] < manifest[j][66:] })
	if string(tree["manifest-sha256.txt"]) != strings.Join(manifest, "\n")+"\n" || len(manifest) != 5 {
		t.Errorf("manifest-sha256.txt:\n%s\nwant\n%s", tree["manifest-sha256.txt"], strings.Join(manifest, "\n"))
	}
	if tree["data/sub-a/ses-1/scan-1/1.2.3.1.2.dcm"] == nil || tree["data/sub-b/ses-1/scan-1/1.2.3.4.1.dcm"] == nil {
		t.Errorf("the payload is not under data/: %v", keysOf(tree))
	}

	// Run again over the finished bag: still a whole bag.
	res = r.run("download", "lab1", "sub-a", "sub-b", "--to", out, "--layout", "bagit", "--json")
	if doc := document(t, res); doc["skipped"] != float64(5) || doc["layout"] != "bagit" {
		t.Errorf("a rerun over a bag: %v", doc)
	}

	if path, err := exec.LookPath("bagit.py"); err != nil {
		t.Log("bagit.py is not on the PATH: the outside validation of the bag was skipped")
	} else {
		if outp, err := exec.Command(path, "--validate", out).CombinedOutput(); err != nil { //nolint:gosec // G204: the outside checker, with the test's own directory.
			t.Errorf("bagit.py --validate failed on the bag: %v\n%s", err, outp)
		}
		victim := filepath.Join(out, "data", "sub-b", "ses-1", "scan-1", "1.2.3.4.1.dcm")
		data, _ := os.ReadFile(victim)
		data[0] ^= 0xff
		if err := os.WriteFile(victim, data, 0o600); err != nil { //nolint:gosec // G703: the test's own temporary directory.
			t.Fatal(err)
		}
		if outp, err := exec.Command(path, "--validate", out).CombinedOutput(); err == nil { //nolint:gosec // G204: the outside checker, with the test's own directory.
			t.Errorf("bagit.py --validate passed a bag with a changed payload byte:\n%s", outp)
		}
	}

	// A file that does not match its announcement: the command fails and no
	// bag is written, since the bag would list a file it does not hold.
	damaged := inst("1.2.3.5.1", 300)
	damaged.AnnounceSHA256 = strings.Repeat("0", 64)
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-d", SessionLabel: "ses-1", Label: "scan-1", InstanceCount: 2, ArchiveTime: timestamppb.New(fixedNow)}, damaged, inst("1.2.3.5.2", 300))
	failed := filepath.Join(t.TempDir(), "failed")
	if res := r.run("download", "lab1", "sub-d", "--to", failed, "--layout", "bagit"); res.code != 1 {
		t.Errorf("a failed file in a bag: exit %d, %q", res.code, res.stderr)
	}
	for name := range readTree(t, failed) {
		if !strings.HasPrefix(name, "data/") {
			t.Errorf("%s was written for a bag with a failed file", name)
		}
	}
}
