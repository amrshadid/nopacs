package cli_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestAuditVerify guards "audit verify": the summary for a person and the
// document with --json; a log verified in several calls followed to its end,
// each call from the seq the one before answered, with the values kept
// outside sent on each; a log with a problem listed with its kind and seq
// and exit 1 with one line; a digest that is not 64 hexadecimal digits
// refused before anything is sent; a tenant that does not exist.
func TestAuditVerify(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")

	expect(t, r.run("audit", "verify"), 0,
		"Log:             the installation's audit log\n"+
			"Intact:          yes\n"+
			"Checked:         12 entries, 3 checkpoints\n"+
			"Unsealed:        2 (sealed at the next pass; no verification covers them)\n"+
			"Last checkpoint: seq 9 to 12, 2026-11-22 12:00:00 +03:00\n"+
			"Its digest:      "+strings.Repeat("03", 32)+"\n", "")
	doc := document(t, r.run("audit", "verify", "--tenant", "lab1", "--json"))
	if doc["intact"] != true || doc["entriesChecked"] != "12" || doc["lastCheckpoint"].(map[string]any)["tenant"] != "lab1" {
		t.Errorf("verify --json: %v", doc)
	}

	// A long log: two answers that go on, then the last.
	r.fake.SetVerifyAnswer(nil, 2)
	before := len(r.fake.VerifyRequests())
	digest := strings.Repeat("ab", 32)
	res := r.run("audit", "verify", "--expect-seq", "40", "--expect-digest", digest, "--to", "100", "--json")
	reqs := r.fake.VerifyRequests()[before:]
	if res.code != 0 || len(reqs) != 3 || reqs[0].GetFromSeq() != 0 || reqs[1].GetFromSeq() != 5 || reqs[2].GetFromSeq() != 9 {
		t.Fatalf("a long log: exit %d, requests %v", res.code, reqs)
	}
	for _, q := range reqs {
		if q.GetExpectedMinSeq() != 40 || hex.EncodeToString(q.GetExpectedCheckpointDigest()) != digest || q.GetToSeq() != 100 || q.GetScope().GetInstallation() == nil {
			t.Errorf("a call of a long log asked %v", q)
		}
	}
	if d := document(t, res); d["entriesChecked"] != "20" || d["checkpointsChecked"] != "5" || d["nextSeq"] != "0" {
		t.Errorf("the calls summed: %v", d)
	}

	// A log with a problem.
	r.fake.SetVerifyAnswer(&v1.VerifyAuditResponse{Intact: false, EntriesChecked: 12, CheckpointsChecked: 3, Problems: []*v1.AuditVerifyProblem{
		{Kind: v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN, Seq: 7, Detail: "the chain does not reach the checkpoint's head"},
	}}, 0)
	res = r.run("audit", "verify")
	if res.code != 1 || !strings.Contains(res.stdout, "Intact:          NO\n") ||
		!strings.Contains(res.stdout, "PROBLEM       SEQ  TIME  DETAIL\nchain-broken  7    -     the chain does not reach the checkpoint's head\n") ||
		res.stderr != "nopacs: the installation's audit log did not verify: 1 problems\n" {
		t.Errorf("a log with a problem: %+v", res)
	}
	// Calls the server bounded by checkpoints, not entries: pruned or small
	// checkpoints check no entry, and the command still goes on from each
	// next seq to the end.
	r.fake.SetVerifyAnswer(nil, 0)
	r.fake.QueueVerifyAnswers(
		&v1.VerifyAuditResponse{Intact: true, CheckpointsChecked: 10000, NextSeq: 10001},
		&v1.VerifyAuditResponse{Intact: true, CheckpointsChecked: 10000, NextSeq: 20001},
	)
	before = len(r.fake.VerifyRequests())
	res = r.run("audit", "verify", "--json")
	reqs = r.fake.VerifyRequests()[before:]
	if res.code != 0 || len(reqs) != 3 || reqs[1].GetFromSeq() != 10001 || reqs[2].GetFromSeq() != 20001 {
		t.Fatalf("calls bounded by checkpoints: exit %d, requests %v", res.code, reqs)
	}
	if d := document(t, res); d["checkpointsChecked"] != "20003" || d["entriesChecked"] != "12" {
		t.Errorf("calls bounded by checkpoints, summed: %v", d)
	}

	for name, args := range map[string][]string{
		"short digest":   {"audit", "verify", "--expect-digest", "abcd"},
		"digest not hex": {"audit", "verify", "--expect-digest", strings.Repeat("zz", 32)},
		"an argument":    {"audit", "verify", "lab1"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 {
			t.Errorf("%s: %+v, want exit 2 and one line", name, res)
		}
	}
	if res := r.run("audit", "verify", "--tenant", "nope"); res.code != 4 || !strings.Contains(res.stderr, "no tenant has that name") {
		t.Errorf("a tenant that does not exist: %+v", res)
	}
}

// TestAuditCheckpoints guards "audit checkpoints": newest first in a table,
// the list document, paging with --limit and the note that more exist.
func TestAuditCheckpoints(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	expect(t, r.run("audit", "checkpoints"), 0,
		"FROM  TO  ENTRIES  TIME                        KEY  PRUNED  DIGEST\n"+
			"9     12  4        2026-11-22 12:00:00 +03:00  1    no      "+strings.Repeat("03", 32)+"\n"+
			"5     8   4        2026-11-21 12:00:00 +03:00  1    no      "+strings.Repeat("02", 32)+"\n"+
			"1     4   4        2026-11-20 12:00:00 +03:00  1    no      "+strings.Repeat("01", 32)+"\n", "")
	if cs := document(t, r.run("audit", "checkpoints", "--tenant", "lab1", "--json"))["checkpoints"].([]any); len(cs) != 3 || cs[0].(map[string]any)["tenant"] != "lab1" {
		t.Errorf("checkpoints --json: %v", cs)
	}
	res := r.run("audit", "checkpoints", "--limit", "2")
	if res.code != 0 || strings.Count(res.stdout, "\n") != 3 || !strings.Contains(res.stderr, "more exist; showing the first 2") {
		t.Errorf("--limit 2: %+v", res)
	}
}

// TestAuditExport guards "audit export": the file written whole as JSON
// lines, the header first, each line one object; the summary for a person
// and as a document; an existing file never replaced (exit 5) and no part
// file left; a range sent as asked; the lines on standard output without
// --out, where --json is refused.
func TestAuditExport(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "audit.jsonl")
	expect(t, r.run("audit", "export", "--out", out), 0,
		"File:        "+out+"\n"+
			"Log:         the installation's audit log (master), installation inst-fake-1\n"+
			"Exported:    seq 1 to 12: 12 entries, 3 checkpoints (0 pruned)\n"+
			"Last digest: "+strings.Repeat("03", 32)+" (keep it outside the installation, for audit verify --expect-digest)\n", "")
	b, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 1+12+3 {
		t.Fatalf("%d lines", len(lines))
	}
	kinds := ""
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || len(m) != 1 {
			t.Fatalf("a line that is not one message: %q (%v)", l, err)
		}
		for k := range m {
			kinds += k[:1]
		}
	}
	if kinds != "heeeeceeeeceeeec" {
		t.Errorf("the messages in order: %s", kinds)
	}
	if info, err := os.Stat(out); err != nil || (info.Mode().Perm()&0o077 != 0 && !strings.Contains(strings.ToLower(os.Getenv("OS")), "windows")) {
		t.Errorf("the export's mode: %v %v", info, err)
	}
	if res := r.run("audit", "export", "--out", out); res.code != 5 || !strings.Contains(res.stderr, "never replaces a file") {
		t.Errorf("an existing file: %+v", res)
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("a part file was left: %v", err)
	}
	second := filepath.Join(dir, "range.jsonl")
	doc := document(t, r.run("audit", "export", "--from", "6", "--to", "7", "--out", second, "--json"))
	if doc["fromSeq"] != float64(5) || doc["toSeq"] != float64(8) || doc["entries"] != float64(4) || doc["checkpoints"] != float64(1) || doc["file"] != second {
		t.Errorf("a range: %v", doc)
	}
	res := r.run("audit", "export")
	if res.code != 0 || res.stderr != "" || strings.Count(res.stdout, "\n") != 16 || !strings.HasPrefix(res.stdout, `{"header":{`) {
		t.Errorf("to standard output: %+v", res)
	}
	if res := r.run("audit", "export", "--json"); res.code != 2 || res.stdout != "" {
		t.Errorf("--json to standard output: %+v", res)
	}
}

// parts returns the temporaries an export left in dir.
func parts(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, ".*.part"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestAuditExportFile guards how "audit export --out" writes its file: a
// symbolic link at FILE.part is never written through (the file it points
// to keeps its content, and FILE is a file of its own, not a link); a
// FILE.part that was there before, 0644, is kept as it was, after an export
// and after one that failed; FILE is readable by its owner alone; a FILE
// that appears while the export is written is refused (exit 5), kept as it
// is, and the export's temporary removed.
func TestAuditExportFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links and modes are of POSIX file systems")
	}
	r := newRig(t)
	dir := t.TempDir()

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not the export"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "linked.jsonl")
	if err := os.Symlink(victim, out+".part"); err != nil {
		t.Fatal(err)
	}
	if res := r.run("audit", "export", "--out", out); res.code != 0 {
		t.Fatalf("an export beside a symbolic link: %+v", res)
	}
	if b, _ := os.ReadFile(victim); string(b) != "not the export" { //nolint:gosec // G304: the test's own file.
		t.Errorf("the file a symbolic link at the part path names was written: %.40q", b)
	}
	info, err := os.Lstat(out)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Errorf("the export: %v %v; want a regular file of mode 0600", info, err)
	}
	if l, err := os.Readlink(out + ".part"); err != nil || l != victim {
		t.Errorf("the symbolic link at the part path: %q %v; want it left as it was", l, err)
	}

	kept := filepath.Join(dir, "kept.jsonl")
	if err := os.WriteFile(kept+".part", []byte("someone's file"), 0o644); err != nil { //nolint:gosec // G306: a file of another, as a person may have.
		t.Fatal(err)
	}
	if res := r.run("audit", "export", "--out", kept); res.code != 0 {
		t.Fatalf("an export beside an existing part file: %+v", res)
	}
	if info, err := os.Stat(kept); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the export beside a 0644 part file: %v %v; want mode 0600", info, err)
	}

	late := filepath.Join(dir, "late.jsonl")
	if err := os.WriteFile(late+".part", []byte("someone's file"), 0o644); err != nil { //nolint:gosec // G306: a file of another, as a person may have.
		t.Fatal(err)
	}
	r.fake.SetExportHook(func() {
		if err := os.WriteFile(late, []byte("made while the export ran"), 0o600); err != nil {
			t.Error(err)
		}
	})
	res := r.run("audit", "export", "--out", late)
	r.fake.SetExportHook(nil)
	if res.code != 5 || !strings.Contains(res.stderr, "appeared while the export was written") {
		t.Errorf("a file that appeared mid-export: %+v; want exit 5", res)
	}
	if b, _ := os.ReadFile(late); string(b) != "made while the export ran" { //nolint:gosec // G304: the test's own file.
		t.Errorf("the file that appeared mid-export was replaced: %q", b)
	}
	for _, p := range []string{kept + ".part", late + ".part"} {
		if b, _ := os.ReadFile(p); string(b) != "someone's file" { //nolint:gosec // G304: the test's own file.
			t.Errorf("%s, there before the export: %q; want it kept", filepath.Base(p), b)
		}
	}
	if left := parts(t, dir); len(left) != 0 {
		t.Errorf("temporaries left: %v", left)
	}
}

// TestAuditPublicKey guards "audit public-key": the PEM of the key that
// signs now and nothing else, so the output is a key file as it is; a
// version the server does not hold (exit 4); --installation alone refused;
// an installation the server holds no key of not found (exit 4) with --json
// too; every key in the --json document.
func TestAuditPublicKey(t *testing.T) {
	r := newRig(t)
	res := r.run("audit", "public-key")
	if res.code != 0 || res.stderr != "" || !strings.HasPrefix(res.stdout, "-----BEGIN PUBLIC KEY-----\n") || !strings.HasSuffix(res.stdout, "-----END PUBLIC KEY-----\n") ||
		strings.Count(res.stdout, "BEGIN") != 1 {
		t.Errorf("public-key: %+v", res)
	}
	if v1res := r.run("audit", "public-key", "--version", "1"); v1res.stdout != res.stdout {
		t.Errorf("--version 1 differs from the current key: %q", v1res.stdout)
	}
	if res := r.run("audit", "public-key", "--version", "2"); res.code != 4 || res.stdout != "" {
		t.Errorf("a version not held: %+v", res)
	}
	if res := r.run("audit", "public-key", "--installation", "other"); res.code != 2 {
		t.Errorf("--installation alone: %+v", res)
	}
	// An installation the server holds no key of is not found, as a PEM and
	// as a document, never an empty list.
	for _, args := range [][]string{
		{"audit", "public-key", "--version", "1", "--installation", "other"},
		{"audit", "public-key", "--version", "1", "--installation", "other", "--json"},
	} {
		if res := r.run(args...); res.code != 4 || res.stdout != "" || !strings.Contains(res.stderr, "no audit public key of installation other") {
			t.Errorf("%v: %+v; want exit 4", args, res)
		}
	}
	if res := r.run("audit", "public-key", "--version", "1", "--installation", "inst-fake-1"); res.code != 0 || !strings.HasPrefix(res.stdout, "-----BEGIN PUBLIC KEY-----\n") {
		t.Errorf("this installation named: %+v", res)
	}
	doc := document(t, r.run("audit", "public-key", "--json"))
	if ks := doc["publicKeys"].([]any); len(ks) != 1 || doc["installationId"] != "inst-fake-1" || ks[0].(map[string]any)["current"] != true {
		t.Errorf("public-key --json: %v", doc)
	}
}
