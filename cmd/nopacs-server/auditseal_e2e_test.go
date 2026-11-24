package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The variables of TestAuditExportChecksOutside. The Python is the one that
// runs scripts/prove/audit_verify.py, standard library only; set, even to
// nothing, it means the check was asked for and a missing interpreter
// fails. The openssl verifies each checkpoint's Ed25519 signature: 3.0 or
// later, which the macOS system LibreSSL is not.
const (
	auditCheckPythonEnv  = "NOPACS_CHECK_AUDIT_EXPORT_PYTHON"
	auditCheckOpenSSLEnv = "NOPACS_CHECK_AUDIT_EXPORT_OPENSSL"
)

// lastDigest finds the digest "audit export" says to keep.
var lastDigest = regexp.MustCompile(`Last digest: ([0-9a-f]{64})`)

// TestAuditExportChecksOutside is the outside check of the audit seal
// through the real program: a server writes audit in the installation's log
// and a tenant's, stops (its last pass seals every entry), starts again,
// and the commands verify both logs, export them and write the public key;
// then scripts/prove/audit_verify.py, which is Python's hashlib and openssl
// and nothing of NoPACs', checks each export with that key alone and exits
// 0, accepts the digest the export said to keep, and refuses a copy with one
// entry's method changed, naming that entry's seq.
func TestAuditExportChecksOutside(t *testing.T) {
	python, asked := os.LookupEnv(auditCheckPythonEnv)
	if !asked {
		t.Skip("set " + auditCheckPythonEnv + " to a Python 3 to run the outside check of an audit export")
	}
	if python == "" {
		t.Fatal(auditCheckPythonEnv + " is set but empty: name the Python that runs scripts/prove/audit_verify.py")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the seal at stop needs a signal the process can catch; Windows has none to send it")
	}
	openssl := os.Getenv(auditCheckOpenSSLEnv)
	if openssl == "" {
		openssl = "openssl"
	}
	tool, err := filepath.Abs(filepath.Join("..", "..", "scripts", "prove", "audit_verify.py"))
	if err != nil {
		t.Fatal(err)
	}

	s := newSession(t)
	s.ok(nil, "init", "--data-dir", s.dir)
	srv := s.serve()
	s.ok(s.local, "tenant", "create", "lab1")
	s.ok(s.local, "setting", "set", "events.retention", "30d", "--tenant", "lab1")
	s.ok(s.local, "setting", "set", "key.default-lifetime", "30d")
	// The stop seals every log after the drain; the start's first pass
	// seals what the start wrote.
	s.stop(srv)
	srv = s.serve()

	work := t.TempDir()
	key := filepath.Join(work, "audit-key.pem")
	if err := os.WriteFile(key, []byte(s.ok(s.local, "audit", "public-key")), 0o600); err != nil {
		t.Fatal(err)
	}
	s.want("audit verify", s.ok(s.local, "audit", "verify"), "Intact:          yes")
	s.want("audit verify of lab1", s.ok(s.local, "audit", "verify", "--tenant", "lab1"), "Intact:          yes")
	s.want("audit checkpoints", s.ok(s.local, "audit", "checkpoints"), "FROM", "DIGEST")
	exports := map[string]string{"installation": filepath.Join(work, "master.jsonl"), "lab1": filepath.Join(work, "lab1.jsonl")}
	digests := map[string]string{}
	for name, path := range exports {
		args := []string{"audit", "export", "--out", path}
		if name != "installation" {
			args = append(args, "--tenant", name)
		}
		out := s.ok(s.local, args...)
		m := lastDigest.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("audit export of %s names no last digest:\n%s", name, out)
		}
		digests[name] = m[1]
	}
	s.stop(srv)

	check := func(export string, extra ...string) (int, string, string) {
		t.Helper()
		args := append([]string{"-I", tool, export, "--public-key", "1=" + key, "--openssl", openssl}, extra...)
		cmd := exec.Command(python, args...) //nolint:gosec // G204: the Python and tool the test was given.
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			code = ee.ExitCode()
		case err != nil:
			t.Fatalf("running %s: %v", tool, err)
		}
		t.Logf("$ audit_verify.py %s  [exit %d]\n%s%s", filepath.Base(export), code, stdout.String(), stderr.String())
		return code, stdout.String(), stderr.String()
	}
	for name, path := range exports {
		if code, out, errText := check(path); code != 0 || !strings.Contains(out, "audit_verify: ok") || !strings.Contains(out, "openssl verified every signature") {
			t.Errorf("the export of %s: exit %d\n%s%s", name, code, out, errText)
		}
		if code, _, errText := check(path, "--expect-digest", digests[name]); code != 0 {
			t.Errorf("the export of %s with the digest it said to keep: exit %d: %s", name, code, errText)
		}
	}

	// One entry's method changed, its encoded form left as sealed.
	b, err := os.ReadFile(exports["installation"])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	changed := -1
	var seqText string
	for i, l := range lines {
		var m map[string]map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		e, ok := m["entry"]
		if !ok || i < 3 {
			continue
		}
		entry := e["entry"].(map[string]any)
		entry["method"] = "/nopacs.v1.TenantService/Nothing"
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		// Protobuf JSON writes an int64 as a decimal string.
		seqText, _ = e["seq"].(string)
		lines[i], changed = string(raw), i
		break
	}
	if changed < 0 || seqText == "" {
		t.Fatalf("the export holds no entry to change:\n%s", b)
	}
	tampered := filepath.Join(work, "tampered.jsonl")
	if err := os.WriteFile(tampered, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil { //nolint:gosec // G703: a path in the test's own temporary directory.
		t.Fatal(err)
	}
	if code, _, errText := check(tampered); code == 0 || !strings.Contains(errText, " at seq "+seqText+":") {
		t.Errorf("a changed entry at seq %s: exit %d, %q; want a refusal naming the seq", seqText, code, errText)
	}
	// The same entry removed.
	removed := filepath.Join(work, "removed.jsonl")
	rest := append(append([]string{}, lines[:changed]...), lines[changed+1:]...)
	if err := os.WriteFile(removed, []byte(strings.Join(rest, "\n")+"\n"), 0o600); err != nil { //nolint:gosec // G703: a path in the test's own temporary directory.
		t.Fatal(err)
	}
	if code, _, errText := check(removed); code == 0 || !strings.Contains(errText, "ENTRY_MISSING at seq "+seqText+":") {
		t.Errorf("a removed entry at seq %s: exit %d, %q; want ENTRY_MISSING naming the seq", seqText, code, errText)
	}
}
