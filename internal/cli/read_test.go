package cli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// seedArchive puts two subjects in lab1: sub-a with a session of two scans
// (an MR with two instances, a CT with one) and a second session, and sub-b
// with one scan. Bytes are deterministic and differ per instance.
func seedArchive(r *rig) {
	at := func(h int) *timestamppb.Timestamp {
		return timestamppb.New(fixedNow.Add(-time.Duration(h) * time.Hour))
	}
	r.fake.AddSubject("lab1", &v1.Subject{Label: "sub-a", SessionCount: 2, InstanceCount: 4, SizeBytes: 4096, ArchiveTime: at(10)})
	r.fake.AddSubject("lab1", &v1.Subject{Label: "sub-b", SessionCount: 1, InstanceCount: 1, SizeBytes: 100, ArchiveTime: at(1)})
	r.fake.AddSession("lab1", &v1.Session{SubjectLabel: "sub-a", Label: "ses-1", StudyDate: "20240105", Modalities: []string{"CT", "MR"}, ScanCount: 2, InstanceCount: 3, SizeBytes: 3000, Receiver: "scanner1", ArchiveTime: at(10)})
	r.fake.AddSession("lab1", &v1.Session{SubjectLabel: "sub-a", Label: "ses-2", StudyDate: "20240301", Modalities: []string{"MR"}, ScanCount: 1, InstanceCount: 1, SizeBytes: 1096, Receiver: "scanner2", ArchiveTime: at(5)})
	r.fake.AddSession("lab1", &v1.Session{SubjectLabel: "sub-b", Label: "ses-1", StudyDate: "20240201", Modalities: []string{"CT"}, ScanCount: 1, InstanceCount: 1, SizeBytes: 100, Receiver: "scanner1", ArchiveTime: at(1)})
	seven := int32(7)
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-1", Label: "scan-1", SeriesInstanceUid: "1.2.3.1", Modality: "MR", SeriesDescription: "T1 axial", SeriesNumber: &seven, InstanceCount: 2, SizeBytes: 2000, Storage: "main", ManifestSha256: strings.Repeat("ab", 32), Receiver: "scanner1", ArchiveTime: at(10), UpdateTime: at(9)},
		inst("1.2.3.1.1", 700), inst("1.2.3.1.2", 1300))
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-1", Label: "scan-2", SeriesInstanceUid: "1.2.3.2", Modality: "CT", SeriesDescription: "Scout", InstanceCount: 1, SizeBytes: 1000, Storage: "main", Receiver: "scanner1", ArchiveTime: at(10)},
		inst("1.2.3.2.1", 1000))
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-2", Label: "scan-1", SeriesInstanceUid: "1.2.3.3", Modality: "MR", SeriesDescription: "T2 flair", InstanceCount: 1, SizeBytes: 1096, Storage: "main", Receiver: "scanner2", ArchiveTime: at(5)},
		inst("1.2.3.3.1", 1096))
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-b", SessionLabel: "ses-1", Label: "scan-1", SeriesInstanceUid: "1.2.3.4", Modality: "CT", SeriesDescription: "Head", InstanceCount: 1, SizeBytes: 100, Storage: "main", Receiver: "scanner1", ArchiveTime: at(1)},
		inst("1.2.3.4.1", 100))
}

// inst makes a fake instance of n bytes, different for each id.
func inst(id string, n int) *fakeserver.Instance {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte((i*7 + len(id) + int(id[len(id)-3])) & 0xff) //nolint:gosec // G115: masked to a byte.
	}
	return &fakeserver.Instance{ID: id, Data: data, JSON: `{"00080060":{"vr":"CS","Value":["MR"]}}`, TransferSyntax: "1.2.840.10008.1.2.1"}
}

// TestCatalogCommands guards the three catalog listings: the table of each,
// every filter that narrows it, the order, paging with a limit that says more
// exist, the total on standard error only when the server knew it, and the
// JSON documents with their total.
func TestCatalogCommands(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)

	expect(t, r.run("catalog", "subjects", "lab1"), 0,
		"SUBJECT  SESSIONS  INSTANCES  SIZE     FIRST ARCHIVED\n"+
			"sub-b    1         1          100 B    2026-11-17 14:00:00 +03:00\n"+
			"sub-a    2         4          4.0 KiB  2026-11-17 05:00:00 +03:00\n", "nopacs: 2 subjects in all\n")
	res := r.run("catalog", "subjects", "lab1", "--limit", "1")
	expect(t, res, 0, "SUBJECT  SESSIONS  INSTANCES  SIZE   FIRST ARCHIVED\nsub-b    1         1          100 B  2026-11-17 14:00:00 +03:00\n",
		"nopacs: more exist; showing the first 1; use --limit N or --all to see more\nnopacs: 2 subjects in all\n")
	doc := document(t, r.run("catalog", "subjects", "lab1", "--limit", "1", "--json"))
	if doc["more"] != true || doc["totalCount"] != float64(2) || len(doc["subjects"].([]any)) != 1 {
		t.Errorf("subjects --limit 1 --json: %v", doc)
	}
	doc = document(t, r.run("catalog", "subjects", "lab1", "--all", "--order", "label", "--label-prefix", "SUB-", "--json"))
	if _, has := doc["totalCount"]; has || doc["more"] != false || len(doc["subjects"].([]any)) != 2 {
		t.Errorf("a filtered list has no total: %v", doc)
	}
	if got := r.run("catalog", "subjects", "lab1", "--order", "alphabetical"); got.code != 2 || !strings.Contains(got.stderr, "--order \"alphabetical\" is not one of: newest, label, study-date, series-description") {
		t.Errorf("a bad order: %d %q", got.code, got.stderr)
	}
	if got := r.run("catalog", "subjects", "lab1", "--from", "2026-11-17", "--to", "2026-11-16"); got.code != 2 || !strings.Contains(got.stderr, "--to must be after --from") {
		t.Errorf("a reversed span: %d %q", got.code, got.stderr)
	}
	if got := r.run("catalog", "subjects", "lab1", "--from", "30d", "--to", "2h", "--json"); !strings.Contains(got.stdout, `"sub-a"`) || strings.Contains(got.stdout, `"sub-b"`) {
		t.Errorf("a span of time:\n%s", got.stdout)
	}

	expect(t, r.run("catalog", "sessions", "lab1", "sub-a"), 0,
		"SUBJECT  SESSION  STUDY DATE  MODALITIES  SCANS  INSTANCES  SIZE     RECEIVER  ARCHIVED\n"+
			"sub-a    ses-2    20240301    MR          1      1          1.1 KiB  scanner2  2026-11-17 10:00:00 +03:00\n"+
			"sub-a    ses-1    20240105    CT,MR       2      3          2.9 KiB  scanner1  2026-11-17 05:00:00 +03:00\n", "")
	ids := func(args ...string) string {
		t.Helper()
		d := document(t, r.run(append(args, "--json")...))
		var out []string
		for _, k := range []string{"sessions", "scans"} {
			xs, _ := d[k].([]any)
			for _, x := range xs {
				m := x.(map[string]any)
				if k == "sessions" {
					out = append(out, m["subjectLabel"].(string)+"/"+m["label"].(string))
				} else {
					out = append(out, m["subjectLabel"].(string)+"/"+m["sessionLabel"].(string)+"/"+m["label"].(string))
				}
			}
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"catalog", "sessions", "lab1", "--modality", "CT"}, "sub-b/ses-1,sub-a/ses-1"},
		{[]string{"catalog", "sessions", "lab1", "--receiver", "scanner2"}, "sub-a/ses-2"},
		{[]string{"catalog", "sessions", "lab1", "--order", "study-date", "--study-date-from", "20240110", "--study-date-to", "20240215"}, "sub-b/ses-1"},
		{[]string{"catalog", "scans", "lab1"}, "sub-b/ses-1/scan-1,sub-a/ses-2/scan-1,sub-a/ses-1/scan-1,sub-a/ses-1/scan-2"},
		{[]string{"catalog", "scans", "lab1", "sub-a", "ses-1"}, "sub-a/ses-1/scan-1,sub-a/ses-1/scan-2"},
		{[]string{"catalog", "scans", "lab1", "--modality", "MR"}, "sub-a/ses-2/scan-1,sub-a/ses-1/scan-1"},
		{[]string{"catalog", "scans", "lab1", "sub-a", "--order", "label"}, "sub-a/ses-1/scan-1,sub-a/ses-1/scan-2,sub-a/ses-2/scan-1"},
		{[]string{"catalog", "scans", "lab1", "--order", "series-description", "--description-prefix", "t"}, "sub-a/ses-1/scan-1,sub-a/ses-2/scan-1"},
	} {
		if got := ids(c.args...); got != c.want {
			t.Errorf("%v: %q, want %q", c.args, got, c.want)
		}
	}
	expect(t, r.run("catalog", "scans", "lab1", "sub-a", "ses-1"), 0,
		"SUBJECT  SESSION  SCAN    MODALITY  SERIES  DESCRIPTION  INSTANCES  SIZE     ARCHIVED\n"+
			"sub-a    ses-1    scan-1  MR        7       T1 axial     2          2.0 KiB  2026-11-17 05:00:00 +03:00\n"+
			"sub-a    ses-1    scan-2  CT        -       Scout        1          1000 B   2026-11-17 05:00:00 +03:00\n", "")
	// The real server refuses a subject alone with the newest-first order and
	// a modality with a session; the fake refuses them by the same rules.
	for _, args := range [][]string{
		{"catalog", "scans", "lab1", "sub-a", "--modality", "MR"},
		{"catalog", "scans", "lab1", "sub-a", "ses-1", "--modality", "MR"},
		{"catalog", "sessions", "lab1", "sub-a", "--modality", "CT"},
		{"catalog", "subjects", "lab1", "--label-prefix", "sub"},
	} {
		if got := r.run(args...); got.code == 0 || !strings.Contains(got.stderr, "filters:") {
			t.Errorf("%v: exit %d, standard error %q, want a refused filter combination", args, got.code, got.stderr)
		}
	}
	if got := r.run("catalog", "scans", "lab1", "a", "b", "c"); got.code != 2 {
		t.Errorf("four names: exit %d", got.code)
	}
	if got := r.run("catalog", "subjects", "nope"); got.code != 4 {
		t.Errorf("an unknown tenant: exit %d, want 4", got.code)
	}
}

// TestScanShow guards the scan's catalog row and, with --metadata, every
// instance's header in order, as text and as JSON where the header is an
// object, a header too large marked as such, and a scan that does not exist
// answered with exit 4.
func TestScanShow(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	big := inst("1.2.3.1.2", 10)
	big.TooLarge = true
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-c", SessionLabel: "ses-1", Label: "scan-1", InstanceCount: 2, ArchiveTime: timestamppb.New(fixedNow)}, inst("1.9.1", 10), big)

	expect(t, r.run("scan", "show", "lab1", "sub-a/ses-1/scan-1"), 0,
		"Subject:          sub-a\nSession:          ses-1\nScan:             scan-1\nSeries UID:       1.2.3.1\nModality:         MR\nSeries number:    7\n"+
			"Description:      T1 axial\nInstances:        2\nSize:             2.0 KiB\nStorage:          main\nManifest SHA-256: "+strings.Repeat("ab", 32)+"\n"+
			"Receiver:         scanner1\nArchived:         2026-11-17 05:00:00 +03:00\nUpdated:          2026-11-17 06:00:00 +03:00\n", "")
	res := r.run("scan", "show", "lab1", "sub-a/ses-1/scan-1", "--metadata")
	want := "\nInstance 1.2.3.1.1 (transfer syntax 1.2.840.10008.1.2.1)\n{\"00080060\":{\"vr\":\"CS\",\"Value\":[\"MR\"]}}\n" +
		"\nInstance 1.2.3.1.2 (transfer syntax 1.2.840.10008.1.2.1)\n{\"00080060\":{\"vr\":\"CS\",\"Value\":[\"MR\"]}}\n"
	if res.code != 0 || !strings.HasSuffix(res.stdout, want) {
		t.Errorf("--metadata: %d\n%s", res.code, res.stdout)
	}
	if res := r.run("scan", "show", "lab1", "sub-c/ses-1/scan-1", "--metadata"); !strings.Contains(res.stdout, "(header not shown: longer than 1 MiB)") {
		t.Errorf("a header too large:\n%s", res.stdout)
	}
	doc := document(t, r.run("scan", "show", "lab1", "sub-a/ses-1/scan-1", "--metadata", "--json"))
	ins := doc["instances"].([]any)
	first := ins[0].(map[string]any)
	if len(ins) != 2 || first["instanceId"] != "1.2.3.1.1" || first["dicomJson"].(map[string]any)["00080060"].(map[string]any)["vr"] != "CS" ||
		doc["scan"].(map[string]any)["modality"] != "MR" || doc["manifestSha256"] == "" {
		t.Errorf("--metadata --json: %v", doc)
	}
	big2 := document(t, r.run("scan", "show", "lab1", "sub-c/ses-1/scan-1", "--metadata", "--json"))["instances"].([]any)[0].(map[string]any)
	if big2["jsonTooLarge"] != true || big2["dicomJson"] != nil {
		t.Errorf("a header too large in JSON: %v", big2)
	}
	if _, has := document(t, r.run("scan", "show", "lab1", "sub-a/ses-1/scan-1", "--json"))["instances"]; has {
		t.Error("--json without --metadata carries instances")
	}
	if got := r.run("scan", "show", "lab1", "sub-a/ses-1/nope"); got.code != 4 {
		t.Errorf("a scan that does not exist: exit %d, want 4 (%q)", got.code, got.stderr)
	}
	for _, bad := range []string{"sub-a/ses-1", "sub-a//scan-1", "sub-a/ses-1/scan-1/x"} {
		if got := r.run("scan", "show", "lab1", bad); got.code != 2 {
			t.Errorf("scan show %q: exit %d, want 2", bad, got.code)
		}
	}
}

// sum is the SHA-256 of bytes in hexadecimal, the way shasum prints it.
func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// readTree reads every file under dir into a map from its slash path.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, rerr := os.ReadFile(p) //nolint:gosec // G122: the test's own temporary directory.
			if rerr != nil {
				t.Fatal(rerr)
			}
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	return out
}

// TestDownload guards the download: the layout under the directory, every
// file equal to what the server holds and its checksum, a rerun that fetches
// nothing it holds whole, and the JSON result.
func TestDownload(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	r.fake.SetChunkSize(300)
	out := filepath.Join(t.TempDir(), "out")

	res := r.run("download", "lab1", "sub-a", "--to", out)
	expect(t, res, 0, "downloaded 4 files (4.0 KiB) to "+out+"; 0 already complete; 0 failed\n", "")
	tree := readTree(t, out)
	for path, want := range map[string]*fakeserver.Instance{
		"sub-a/ses-1/scan-1/1.2.3.1.1.dcm": inst("1.2.3.1.1", 700), "sub-a/ses-1/scan-1/1.2.3.1.2.dcm": inst("1.2.3.1.2", 1300),
		"sub-a/ses-1/scan-2/1.2.3.2.1.dcm": inst("1.2.3.2.1", 1000), "sub-a/ses-2/scan-1/1.2.3.3.1.dcm": inst("1.2.3.3.1", 1096),
	} {
		if !bytes.Equal(tree[path], want.Data) {
			t.Errorf("%s: %d bytes with SHA-256 %s, want %d with %s", path, len(tree[path]), sum(tree[path]), len(want.Data), sum(want.Data))
		}
	}
	if len(tree) != 4 {
		t.Errorf("the tree holds %d files, want 4", len(tree))
	}
	// Windows keeps no mode bits: a directory made there takes the access
	// list of the directory it is made in, the one the user named.
	info, err := os.Stat(filepath.Join(out, "sub-a", "ses-1"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Errorf("the directories are not private: %v %v", info, err)
	}

	before := len(r.fake.ReadFilesRequests())
	res = r.run("download", "lab1", "sub-a", "--to", out, "--json")
	doc := document(t, res)
	if doc["files"] != float64(0) || doc["skipped"] != float64(4) || doc["bytes"] != float64(0) || len(doc["failed"].([]any)) != 0 {
		t.Errorf("a rerun: %v", doc)
	}
	reqs := r.fake.ReadFilesRequests()[before:]
	if len(reqs) != 2 || !reqs[0].GetAnnounceOnly() || reqs[1].GetStartAfter().GetInstanceId() != "1.2.3.3.1" || reqs[1].GetStartAfter().GetSessionLabel() != "ses-2" {
		t.Errorf("a rerun asked for %v, want the announcements and then a stream after the last file", reqs)
	}

	one := filepath.Join(t.TempDir(), "one")
	expect(t, r.run("download", "lab1", "sub-b/ses-1/scan-1", "--to", one, "--quiet"), 0, "downloaded 1 files (100 B) to "+one+"; 0 already complete; 0 failed\n", "")
	if got := readTree(t, one); len(got) != 1 || !bytes.Equal(got["sub-b/ses-1/scan-1/1.2.3.4.1.dcm"], inst("1.2.3.4.1", 100).Data) {
		t.Errorf("a scan: %v", got)
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"download", "lab1", "sub-a"}, 2},
		{[]string{"download", "lab1", "../x", "--to", one}, 2},
		{[]string{"download", "lab1", "sub-a//x", "--to", one}, 2},
		{[]string{"download", "lab1", "nobody", "--to", one}, 4},
	} {
		if got := r.run(c.args...); got.code != c.code {
			t.Errorf("%v: exit %d, want %d (%q)", c.args, got.code, c.code, got.stderr)
		}
	}
}

// TestDownloadResumes guards the resume: a partial file is continued with a
// range request that carries the checksum as If-Range (and the result is the
// whole file), a changed or damaged file is replaced, a stream that stops is
// reopened after the last whole instance, and a damaged leftover never
// reaches the final name.
func TestDownloadResumes(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	out := filepath.Join(t.TempDir(), "out")
	whole := inst("1.2.3.1.2", 1300)
	dir := filepath.Join(out, "sub-a", "ses-1", "scan-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.1.dcm"), inst("1.2.3.1.1", 700).Data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A partial copy of the second file: its first 500 bytes, with the
	// record of the file it is the start of.
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.2.dcm.part"), whole.Data[:500], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.2.dcm.part.sha256"), []byte(sum(whole.Data)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.run("download", "lab1", "sub-a/ses-1/scan-1", "--to", out, "--json")
	doc := document(t, res)
	if doc["files"] != float64(1) || doc["skipped"] != float64(1) || doc["resumed"] != float64(1) || doc["bytes"] != float64(800) {
		t.Errorf("a resumed download: %v", doc)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "1.2.3.1.2.dcm")); !bytes.Equal(got, whole.Data) {
		t.Errorf("the resumed file has SHA-256 %s, want %s", sum(got), sum(whole.Data))
	}
	for _, left := range []string{"1.2.3.1.2.dcm.part", "1.2.3.1.2.dcm.part.sha256"} {
		if _, err := os.Stat(filepath.Join(dir, left)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", left, err)
		}
	}
	var range1 string
	for _, s := range r.fake.SeenRequests() {
		if strings.HasPrefix(s.URL, "/tenants/lab1/instances/sub-a/ses-1/scan-1/1.2.3.1.2") {
			range1 = s.Header.Get("Range") + " " + s.Header.Get("If-Range")
		}
	}
	if range1 != "bytes=500- \""+sum(whole.Data)+"\"" {
		t.Errorf("the range request carried %q", range1)
	}

	// A partial copy of bytes that are not the file's: the checksum catches
	// it, the damaged copy is removed and the command fails naming the file.
	bad := filepath.Join(t.TempDir(), "bad")
	baddir := filepath.Join(bad, "sub-a", "ses-2", "scan-1")
	if err := os.MkdirAll(baddir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baddir, "1.2.3.3.1.dcm.part"), bytes.Repeat([]byte{0xEE}, 400), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baddir, "1.2.3.3.1.dcm.part.sha256"), []byte(sum(inst("1.2.3.3.1", 1096).Data)), 0o600); err != nil {
		t.Fatal(err)
	}
	res = r.run("download", "lab1", "sub-a/ses-2", "--to", bad)
	if res.code != 1 || !strings.Contains(res.stderr, "1.2.3.3.1.dcm") || !strings.Contains(res.stderr, "SHA-256") {
		t.Errorf("a damaged partial copy: exit %d, %q", res.code, res.stderr)
	}
	if got := readTree(t, bad); len(got) != 0 {
		t.Errorf("a damaged file was kept: %v", got)
	}

	// A stream that stops after one instance is reopened after it.
	r.fake.SetChunkSize(200)
	r.fake.DropReadFilesAfter(1)
	again := filepath.Join(t.TempDir(), "again")
	before := len(r.fake.ReadFilesRequests())
	expect(t, r.run("download", "lab1", "sub-a", "--to", again), 0, "downloaded 4 files (4.0 KiB) to "+again+"; 0 already complete; 0 failed\n", "")
	reqs := r.fake.ReadFilesRequests()[before:]
	if len(reqs) < 3 || reqs[len(reqs)-1].GetStartAfter().GetInstanceId() != "1.2.3.1.1" {
		t.Errorf("the stream was reopened with %v, want a start after the first instance", reqs)
	}
	if got := readTree(t, again); len(got) != 4 {
		t.Errorf("after the retry the tree holds %d files", len(got))
	}
}

// TestDownloadResumesTheFirstFileByRangeAlone guards a rerun whose first
// missing file is a partial copy: it is continued by range before any
// stream of file bytes opens, and the stream then starts after it, so no
// stream begins sending a file the client continues by range (what such a
// stream sends before it is closed would be fetched for nothing).
func TestDownloadResumesTheFirstFileByRangeAlone(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	out := filepath.Join(t.TempDir(), "out")
	first := inst("1.2.3.1.1", 700)
	dir := filepath.Join(out, "sub-a", "ses-1", "scan-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.1.dcm.part"), first.Data[:300], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.1.dcm.part.sha256"), []byte(sum(first.Data)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := document(t, r.run("download", "lab1", "sub-a", "--to", out, "--json"))
	if doc["files"] != float64(4) || doc["resumed"] != float64(1) || doc["bytes"] != float64(4096-300) {
		t.Errorf("a download resumed at its first file: %v", doc)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "1.2.3.1.1.dcm")); !bytes.Equal(got, first.Data) {
		t.Errorf("the resumed file has SHA-256 %s, want %s", sum(got), sum(first.Data))
	}
	for _, req := range r.fake.ReadFilesRequests() {
		if !req.GetAnnounceOnly() && req.GetStartAfter().GetInstanceId() == "" {
			t.Errorf("a stream of file bytes opened at the start, at the file continued by range: %v", req)
		}
	}
}

// TestDownloadFailsClosed guards what a hostile or damaged server cannot do:
// a file announced with a checksum its bytes do not have is removed, named,
// and fails the command only after the rest arrived; a name with "../" is
// refused and writes nothing outside the directory.
func TestDownloadFailsClosed(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	damaged := inst("1.2.3.5.1", 300)
	damaged.AnnounceSHA256 = strings.Repeat("0", 64)
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-d", SessionLabel: "ses-1", Label: "scan-1", InstanceCount: 2, ArchiveTime: timestamppb.New(fixedNow)}, damaged, inst("1.2.3.5.2", 300))
	out := filepath.Join(t.TempDir(), "out")
	res := r.run("download", "lab1", "sub-d", "--to", out)
	if res.code != 1 || !strings.Contains(res.stderr, filepath.FromSlash("sub-d/ses-1/scan-1/1.2.3.5.1.dcm")) || !strings.Contains(res.stdout, "1 failed") {
		t.Errorf("a file that does not match: exit %d\nstdout %q\nstderr %q", res.code, res.stdout, res.stderr)
	}
	if got := readTree(t, out); len(got) != 1 || got["sub-d/ses-1/scan-1/1.2.3.5.2.dcm"] == nil {
		t.Errorf("after a mismatch the tree holds %d files, want only the good one", len(got))
	}

	parent := t.TempDir()
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-e", SessionLabel: "..", Label: "scan-1", InstanceCount: 1, ArchiveTime: timestamppb.New(fixedNow)}, inst("1.2.3.6.1", 10))
	res = r.run("download", "lab1", "sub-e", "--to", filepath.Join(parent, "out"))
	if res.code == 0 || !strings.Contains(res.stderr, "not a label") {
		t.Errorf("a name with dots: exit %d, %q", res.code, res.stderr)
	}
	if got := readTree(t, parent); len(got) != 0 {
		t.Errorf("something was written: %v", got)
	}
	_ = fmt.Sprint
	_ = json.Valid
}

// TestDownloadContinuesOnlyItsOwnFile guards the record beside a .part: a
// .part whose record names another SHA-256 than the one announced (the
// file changed on the server since the .part was begun), and a .part with
// no record, are not continued by range but fetched afresh, so bytes of
// two versions of a file are never joined; the record goes with the .part
// once the file is whole.
func TestDownloadContinuesOnlyItsOwnFile(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	whole := inst("1.2.3.1.2", 1300)
	for name, record := range map[string]string{"another version's": sum([]byte("an earlier version of the file")), "no": ""} {
		out := filepath.Join(t.TempDir(), "out")
		dir := filepath.Join(out, "sub-a", "ses-1", "scan-1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.2.dcm.part"), bytes.Repeat([]byte{0x11}, 500), 0o600); err != nil {
			t.Fatal(err)
		}
		if record != "" {
			if err := os.WriteFile(filepath.Join(dir, "1.2.3.1.2.dcm.part.sha256"), []byte(record+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before := len(r.fake.SeenRequests())
		doc := document(t, r.run("download", "lab1", "sub-a/ses-1/scan-1", "--to", out, "--json"))
		if doc["files"] != float64(2) || doc["resumed"] != float64(0) || doc["failures"] != nil {
			t.Errorf("a .part with %s record: %v", name, doc)
		}
		for _, seen := range r.fake.SeenRequests()[before:] {
			if seen.Header.Get("Range") != "" {
				t.Errorf("a .part with %s record was continued by range: %s %s", name, seen.URL, seen.Header.Get("Range"))
			}
		}
		if got := readTree(t, out); len(got) != 2 || !bytes.Equal(got["sub-a/ses-1/scan-1/1.2.3.1.2.dcm"], whole.Data) {
			t.Errorf("a .part with %s record: the tree holds %d files, the second whole: %v", name, len(got), bytes.Equal(got["sub-a/ses-1/scan-1/1.2.3.1.2.dcm"], whole.Data))
		}
	}
}

// TestDownloadStopsAFileThatRunsLong guards the bound of a file's bytes: a
// server that sends more bytes than it announced, in the stream or in the
// range that continues a .part, has the file given up as soon as the
// bytes pass the announced length, nothing past that length written, the
// .part and its record removed, the failure named, and the next file
// fetched.
func TestDownloadStopsAFileThatRunsLong(t *testing.T) {
	r := newArchiveRig(t)
	seedArchive(r)
	r.fake.SetChunkSize(50)
	long := inst("1.2.3.7.1", 300)
	long.AnnounceLength = 100
	r.fake.AddScan("lab1", &v1.Scan{SubjectLabel: "sub-f", SessionLabel: "ses-1", Label: "scan-1", InstanceCount: 2, ArchiveTime: timestamppb.New(fixedNow)}, long, inst("1.2.3.7.2", 300))
	out := filepath.Join(t.TempDir(), "out")
	res := r.run("download", "lab1", "sub-f", "--to", out)
	if res.code != 1 || !strings.Contains(res.stderr, "1.2.3.7.1.dcm") || !strings.Contains(res.stderr, "more than the 100 bytes announced") {
		t.Errorf("a file longer than announced: exit %d\nstdout %q\nstderr %q", res.code, res.stdout, res.stderr)
	}
	if got := readTree(t, out); len(got) != 1 || got["sub-f/ses-1/scan-1/1.2.3.7.2.dcm"] == nil {
		t.Errorf("after a file longer than announced the tree holds %v", keysOf(got))
	}

	// The same file continued by range from a .part of its first 50 bytes.
	again := filepath.Join(t.TempDir(), "again")
	dir := filepath.Join(again, "sub-f", "ses-1", "scan-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.7.1.dcm.part"), long.Data[:50], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.2.3.7.1.dcm.part.sha256"), []byte(sum(long.Data)), 0o600); err != nil {
		t.Fatal(err)
	}
	res = r.run("download", "lab1", "sub-f", "--to", again)
	if res.code != 1 || !strings.Contains(res.stderr, "more than the 100 bytes announced") {
		t.Errorf("a range longer than announced: exit %d\nstdout %q\nstderr %q", res.code, res.stdout, res.stderr)
	}
	if got := readTree(t, again); len(got) != 1 || got["sub-f/ses-1/scan-1/1.2.3.7.2.dcm"] == nil {
		t.Errorf("after a range longer than announced the tree holds %v", keysOf(got))
	}
}

// keysOf returns the names of a tree read by readTree.
func keysOf(tree map[string][]byte) []string {
	out := make([]string, 0, len(tree))
	for k := range tree {
		out = append(out, k)
	}
	return out
}
