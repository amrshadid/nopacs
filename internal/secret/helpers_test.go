package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// derivedBytes returns a copy of d's key bytes, for a test to compare or
// search for.
func derivedBytes(d *DerivedKey) []byte {
	return bytes.Clone(d.d.key[:])
}

// signerPrivate returns a copy of s's 64-byte Ed25519 private key, for a test
// to search for.
func signerPrivate(s *Signer) []byte {
	return bytes.Clone(s.s.private[:])
}

// testKey returns a fixed, synthetic 32-byte key that differs for each seed,
// so a test knows the key bytes it later searches for.
func testKey(seed byte) []byte {
	key := make([]byte, keySize)
	for i := range key {
		key[i] = seed*31 + byte(i)*7 + 1
	}
	return key
}

// keyLine returns one key file line for version with testKey(seed).
func keyLine(version uint32, seed byte) string {
	return keyLinePrefix + " " + strconv.FormatUint(uint64(version), 10) + " " +
		base64.StdEncoding.EncodeToString(testKey(seed)) + "\n"
}

// writePrivate writes data to a new file at path that only the current user
// may open, through the same function CreateKeyFile uses, so the file passes
// LoadKeyFile's permission check on every system. os.WriteFile would not: on
// Windows its file inherits the directory's access list.
func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := createPrivate(path)
	if err != nil {
		t.Fatalf("createPrivate: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// loadText writes text as a private key file in a new directory and loads it.
func loadText(t *testing.T, text string) (*Keys, error) {
	t.Helper()
	path := filepath.Join(privateDir(t), "nopacs.key")
	writePrivate(t, path, []byte(text))
	return LoadKeyFile(path)
}

// mustKeys loads text as a key file and fails the test if it does not load.
func mustKeys(t *testing.T, text string) *Keys {
	t.Helper()
	keys, err := loadText(t, text)
	if err != nil {
		t.Fatalf("LoadKeyFile: %v", err)
	}
	return keys
}

// mustSeal seals plaintext for context and fails the test on an error.
func mustSeal(t *testing.T, keys *Keys, context string, plaintext []byte) []byte {
	t.Helper()
	sealed, err := keys.Seal(context, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

// keyForms returns the forms in which a key could leak into printed text:
// its raw bytes, base64 (standard, unpadded and URL), hex, its bytes as fmt
// lists them in decimal (%v, %d) and in Go syntax (%#v), and each of its
// eight 32-bit words in decimal, read in both byte orders, which is how fmt
// prints the uint32 words of an AES key schedule.
func keyForms(key []byte) [][]byte {
	goSyntax := make([]string, len(key))
	for i, b := range key {
		goSyntax[i] = fmt.Sprintf("%#x", b)
	}
	forms := [][]byte{
		key,
		[]byte(base64.StdEncoding.EncodeToString(key)),
		[]byte(base64.RawStdEncoding.EncodeToString(key)),
		[]byte(base64.URLEncoding.EncodeToString(key)),
		[]byte(hex.EncodeToString(key)),
		decimalList(key),
		[]byte(strings.Join(goSyntax, ", ")),
	}
	for i := 0; i+4 <= len(key); i += 4 {
		forms = append(forms,
			[]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(key[i:])), 10)),
			[]byte(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(key[i:])), 10)))
	}
	return forms
}

// decimalList returns b's bytes in decimal, separated by spaces, as fmt
// lists a byte slice or array under %v and %d.
func decimalList(b []byte) []byte {
	out := make([]byte, 0, 4*len(b))
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = strconv.AppendUint(out, uint64(c), 10)
	}
	return out
}

// privateDir returns a new directory that only the current user may change,
// as LoadKeyFile and CreateKeyFile require on Linux and macOS. t.TempDir
// already makes one with mode 0700; the chmod says so instead of relying on
// it.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	return dir
}

// containsAny reports whether s contains any of the needles, ignoring empty
// ones, and returns the first it found.
func containsAny(s string, needles ...[]byte) ([]byte, bool) {
	for _, n := range needles {
		if len(n) > 0 && bytes.Contains([]byte(s), n) {
			return n, true
		}
	}
	return nil, false
}
