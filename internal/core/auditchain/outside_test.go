package auditchain

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/secret"
)

// The variables that ask for the outside check. make check-auditchain sets
// both.
const (
	// pythonEnv names the Python interpreter that runs
	// scripts/check/auditchain.py; it needs nothing beyond the standard
	// library.
	pythonEnv = "NOPACS_CHECK_AUDITCHAIN_PYTHON"
	// opensslEnv names the openssl that verifies the signatures; unset, the
	// one on the PATH. It must support Ed25519 in pkeyutl -rawin (OpenSSL
	// 3.0 or later).
	opensslEnv = "NOPACS_CHECK_AUDITCHAIN_OPENSSL"
)

// jsonEntry is an entry as the outside check reads it.
type jsonEntry struct {
	Seq        uint64            `json:"seq"`
	ID         string            `json:"id"`
	AtUS       int64             `json:"at_us"`
	KeyID      string            `json:"key_id"`
	LocalUser  string            `json:"local_user"`
	RemoteAddr string            `json:"remote_addr"`
	Method     string            `json:"method"`
	TenantName string            `json:"tenant_name"`
	Object     string            `json:"object"`
	Outcome    string            `json:"outcome"`
	Reason     string            `json:"reason"`
	Details    map[string]string `json:"details"`
	RequestID  string            `json:"request_id"`
	Encoded    string            `json:"encoded"`
}

// jsonCheckpoint is a checkpoint as the outside check reads it.
type jsonCheckpoint struct {
	Installation string `json:"installation"`
	Log          string `json:"log"`
	From         uint64 `json:"from"`
	To           uint64 `json:"to"`
	Head         string `json:"head"`
	Previous     string `json:"previous"`
	TimeUS       int64  `json:"time_us"`
	KeyVersion   uint32 `json:"key_version"`
	Signature    string `json:"signature"`
}

// derivedSigners writes a key file with two random key versions and returns
// the audit signer of each, derived as the server derives it.
func derivedSigners(t *testing.T, dir string) (*secret.Signer, *secret.Signer) {
	t.Helper()
	var text strings.Builder
	for v := 1; v <= 2; v++ {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		text.WriteString("nopacs-key-v1 " + strconv.Itoa(v) + " " + base64.StdEncoding.EncodeToString(key) + "\n")
	}
	path := filepath.Join(dir, "nopacs.key")
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signers := make([]*secret.Signer, 2)
	for i := range signers {
		d, err := keys.DeriveVersion(secret.AuditSeal, uint32(i+1)) //nolint:gosec // G115: 1 or 2.
		if err != nil {
			t.Fatal(err)
		}
		if signers[i], err = d.Signer(); err != nil {
			t.Fatal(err)
		}
	}
	return signers[0], signers[1]
}

// writeJSONLines writes one JSON value per line.
func writeJSONLines[T any](t *testing.T, path string, values []T) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, v := range values {
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestChainChecksInPython guards the chain's format against another
// implementation: a log of entries with empty, non-ASCII and detailed
// fields, sealed in three checkpoints by signers derived from a key file
// with secret.Keys (the second key version after the first checkpoint), is
// written out as JSON lines with the public keys as PEM. Then
// scripts/check/auditchain.py, using only Python's hashlib and struct,
// encodes every entry as Go did, folds the chain to each checkpoint's head,
// checks each checkpoint's range and previous digest, has openssl verify
// each signature, and shows it would notice a changed entry, a flipped
// signature and a changed signed text; the heads it prints equal Go's. It
// runs only when NOPACS_CHECK_AUDITCHAIN_PYTHON names an interpreter.
func TestChainChecksInPython(t *testing.T) {
	python, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skip(pythonEnv + " is not set; run make check-auditchain")
	}
	if python == "" {
		t.Fatal(pythonEnv + " is set but empty: name a Python interpreter")
	}
	openssl := os.Getenv(opensslEnv)
	if openssl == "" {
		openssl = "openssl"
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "check", "auditchain.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	s1, s2 := derivedSigners(t, dir)
	for _, s := range []*secret.Signer{s1, s2} {
		pem, err := MarshalPublicKey(s.Public())
		if err != nil {
			t.Fatal(err)
		}
		name := "public-" + strconv.FormatUint(uint64(s.KeyVersion()), 10) + ".pem"
		if err := os.WriteFile(filepath.Join(dir, name), pem, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var (
		entries     []jsonEntry
		checkpoints []jsonCheckpoint
		state       State
		heads       = map[uint64]string{}
	)
	for i, size := range []int{4, 1, 7} {
		batch := testEntries(state.Seq+1, size)
		var signer Signer = s1
		if i > 0 {
			signer = s2
		}
		c, next, err := Seal(signer, "inst-outside", "tenant-3", state, batch, baseTime.Add(time.Duration(i+1)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range batch {
			enc, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			details := e.Details
			if details == nil {
				details = map[string]string{}
			}
			entries = append(entries, jsonEntry{
				Seq: e.Seq, ID: e.ID, AtUS: e.Time.UnixMicro(), KeyID: e.KeyID, LocalUser: e.LocalUser,
				RemoteAddr: e.RemoteAddress, Method: e.Method, TenantName: e.TenantName, Object: e.Object,
				Outcome: e.Outcome, Reason: e.Reason, Details: details, RequestID: e.RequestID,
				Encoded: hex.EncodeToString(enc),
			})
		}
		checkpoints = append(checkpoints, jsonCheckpoint{
			Installation: c.Installation, Log: c.Log, From: c.From, To: c.To,
			Head: c.Head.String(), Previous: c.Previous.String(), TimeUS: c.Time.UnixMicro(),
			KeyVersion: c.KeyVersion, Signature: hex.EncodeToString(c.Signature),
		})
		heads[c.To] = c.Head.String()
		state = next
	}
	writeJSONLines(t, filepath.Join(dir, "entries.jsonl"), entries)
	writeJSONLines(t, filepath.Join(dir, "checkpoints.jsonl"), checkpoints)

	cmd := exec.Command(python, "-I", script, dir, openssl) //nolint:gosec // G702: the interpreter and openssl are the ones the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t.Logf("python: %s", line)
	}
	if err != nil {
		t.Fatalf("auditchain.py: %v\n%s", err, stderr.String())
	}
	seen := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "head" {
			continue
		}
		seq, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if heads[seq] != f[2] {
			t.Errorf("Python's head at seq %d is %s, Go's %s", seq, f[2], heads[seq])
		}
		seen++
	}
	if seen != len(heads) {
		t.Fatalf("Python printed %d heads, want %d", seen, len(heads))
	}
}
