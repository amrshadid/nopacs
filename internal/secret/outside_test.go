package secret

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pythonEnv names the Python interpreter, with the cryptography package
// installed, that TestSealOpensInPython runs. make check-seal sets it.
const pythonEnv = "NOPACS_CHECK_SEAL_PYTHON"

// TestSealOpensInPython guards the sealed format against another
// implementation: scripts/check/seal.py, written from the package
// documentation with Python's cryptography package (OpenSSL's AES-GCM), opens
// a value this package sealed and refuses it under another context or with a
// byte changed; then this package opens a value Python sealed, under an older
// key version than the one Go seals with, and refuses it under another
// context. It runs only when NOPACS_CHECK_SEAL_PYTHON names an interpreter,
// as make check-seal and CI do, because the other tests must not need Python.
func TestSealOpensInPython(t *testing.T) {
	// Unset means the outside check was not asked for, so the test skips.
	// Set, even to nothing, means it was, and then a missing interpreter is a
	// failure: a check that was asked for and quietly did not run would be
	// reported as passed.
	python, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skip(pythonEnv + " is not set; run make check-seal")
	}
	if python == "" {
		t.Fatal(pythonEnv + " is set but empty: name a Python interpreter that has the cryptography package")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check", "seal.py"))
	if err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(privateDir(t), "nopacs.key")
	writePrivate(t, keyFile, []byte(keyLine(1, 1)+keyLine(2, 2)))
	keys, err := LoadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(testPlaintext + " ünïcödé")
	goSealed := mustSeal(t, keys, testContext, plaintext)
	if v := binary.BigEndian.Uint32(goSealed[1:5]); v != 2 {
		t.Fatalf("Go sealed with version %d, want 2", v)
	}

	// -I keeps the interpreter from loading modules from the current
	// directory or the environment, so the script runs against the installed
	// cryptography package and nothing else.
	args := []string{"-I", script, keyFile, testContext,
		hex.EncodeToString(goSealed), hex.EncodeToString(plaintext), "1"}
	cmd := exec.Command(python, args...) //nolint:gosec // G702: the interpreter is the one the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("seal.py: %v\n%s%s", err, out, stderr.String())
	}

	var pySealed []byte
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if h, ok := strings.CutPrefix(line, "value "); ok {
			if pySealed, err = hex.DecodeString(h); err != nil {
				t.Fatalf("seal.py printed a value that is not hex: %v", err)
			}
			continue
		}
		t.Logf("python: %s", line)
	}
	if pySealed == nil {
		t.Fatalf("seal.py printed no sealed value:\n%s", out)
	}
	if pySealed[0] != formatV1 || binary.BigEndian.Uint32(pySealed[1:5]) != 1 {
		t.Fatalf("Python's value has header %x, want 01 00000001", pySealed[:5])
	}
	opened, err := keys.Open(testContext, pySealed)
	if err != nil {
		t.Fatalf("the value Python sealed does not open in Go: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatal("the value Python sealed opens in Go to a different plaintext")
	}
	t.Logf("go: opened the %d-byte value Python sealed with key version 1", len(pySealed))
	if _, err := keys.Open(testContext+"x", pySealed); err == nil {
		t.Fatal("the value Python sealed opens in Go under another context")
	}
	t.Log("go: refused it under another context")
}

// backupPythonEnv names the Python interpreter, with the cryptography
// package installed, that TestBackupOpensInPython runs. make
// check-backup-cipher sets it.
const backupPythonEnv = "NOPACS_CHECK_BACKUP_PYTHON"

// TestBackupOpensInPython guards the backup format against another
// implementation: scripts/check/backup-cipher.py, written from the package
// documentation with HKDF over Python's hmac and hashlib and AES-GCM from
// the cryptography package, opens a backup of several chunks this package
// wrote under the highest of three key versions, compares the
// plaintext's SHA-256, and refuses the file cut at a chunk boundary, with
// two chunks swapped, a bit flipped and a chunk appended; then this package
// opens the backup Python wrote of the same plaintext, and refuses it cut
// short. It runs only when NOPACS_CHECK_BACKUP_PYTHON names an interpreter.
func TestBackupOpensInPython(t *testing.T) {
	python, asked := os.LookupEnv(backupPythonEnv)
	if !asked {
		t.Skip(backupPythonEnv + " is not set; run make check-backup-cipher")
	}
	if python == "" {
		t.Fatal(backupPythonEnv + " is set but empty: name a Python interpreter that has the cryptography package")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check", "backup-cipher.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := privateDir(t)
	keyFile := filepath.Join(dir, "nopacs.key")
	// Version 3 is listed first and version 2 last, so the file's order is
	// not what picks the version.
	writePrivate(t, keyFile, []byte(keyLine(3, 3)+keyLine(1, 1)+keyLine(2, 2)))
	keys, err := LoadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 2*BackupChunkSize+12345)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := NewBackupWriter(&buf, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(dir, "go.backup")
	pyFile := filepath.Join(dir, "python.backup")
	if err := os.WriteFile(goFile, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(plain)
	cmd := exec.Command(python, "-I", script, "open", keyFile, goFile, hex.EncodeToString(sum[:]), pyFile) //nolint:gosec // G702: the interpreter is the one the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t.Logf("python: %s", line)
	}
	if err != nil {
		t.Fatalf("backup-cipher.py: %v\n%s", err, stderr.String())
	}
	pyBackup, err := os.ReadFile(pyFile) //nolint:gosec // G304: a file this test's own temporary directory holds.
	if err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint32(pyBackup[9:13]); v != 3 {
		t.Fatalf("Python wrote under key version %d, want 3", v)
	}
	got, err := readBackup(keys, pyBackup)
	if err != nil {
		t.Fatalf("the backup Python wrote does not open in Go: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("the backup Python wrote opens in Go to a different plaintext")
	}
	t.Logf("go: opened the %d-byte backup Python wrote", len(pyBackup))
	if _, err := readBackup(keys, pyBackup[:len(pyBackup)-1]); err == nil {
		t.Fatal("the backup Python wrote opens in Go cut short")
	}
	t.Log("go: refused it cut short by one byte")
}
