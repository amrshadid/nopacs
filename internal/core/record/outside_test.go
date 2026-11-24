package record

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/secret"
)

// recordMACPythonEnv names the Python interpreter that TestRecordMACInPython
// runs. make check-record-mac sets it.
const recordMACPythonEnv = "NOPACS_CHECK_RECORD_MAC_PYTHON"

// TestRecordMACInPython guards the record's MAC against another
// implementation: scripts/check/record-mac.py, written from the format's
// description with HKDF over Python's hmac and hashlib, computes the MAC of
// a record this package wrote under a real installation key file and finds
// it equal to the record's, and refuses the record for another tenant, with
// a byte changed and with its MAC taken out. It runs only when
// NOPACS_CHECK_RECORD_MAC_PYTHON names an interpreter; set, even to nothing,
// a missing interpreter is a failure.
func TestRecordMACInPython(t *testing.T) {
	python, asked := os.LookupEnv(recordMACPythonEnv)
	if !asked {
		t.Skip(recordMACPythonEnv + " is not set; run make check-record-mac")
	}
	if python == "" {
		t.Fatal(recordMACPythonEnv + " is set but empty: name a Python interpreter")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "check", "record-mac.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// LoadKeyFile refuses a key file in a directory others may change.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "nopacs.key")
	if err := secret.CreateKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	c := published(deleteRec(), StatePurged)
	c.Purge = &Purge{Removed: 2}
	b, err := EncodeCorrection(c, tenantA, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCorrection(bytes.NewReader(b), tenantA, keys); err != nil {
		t.Fatalf("the record does not verify in Go: %v", err)
	}
	recFile := filepath.Join(dir, "record.json")
	if err := os.WriteFile(recFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", script, keyFile, tenantA, recFile) //nolint:gosec // G702: the interpreter is the one the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t.Logf("python: %s", line)
	}
	if err != nil {
		t.Fatalf("record-mac.py: %v\n%s", err, stderr.String())
	}
	back, err := DecodeCorrection(bytes.NewReader(b), tenantA, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "mac "+back.MAC+"\n") {
		t.Fatalf("Python computed another MAC than the record's %s:\n%s", back.MAC, out)
	}
	t.Logf("go: the record's MAC %s, key version %d, equals Python's", back.MAC, back.KeyVersion)
}
