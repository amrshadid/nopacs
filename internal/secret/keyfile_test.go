package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCreateThenLoad guards the whole life of a new key file: it is one line
// in the documented format, version 1, it loads, and no temporary file is
// left beside it.
func TestCreateThenLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nopacs.key")
	if err := CreateKeyFile(path); err != nil {
		t.Fatalf("CreateKeyFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^nopacs-key-v1 1 [A-Za-z0-9+/]{43}=\n$`).Match(data) {
		t.Errorf("the key file is not one line of the documented format (%d bytes)", len(data))
	}
	keys, err := LoadKeyFile(path)
	if err != nil {
		t.Fatalf("LoadKeyFile: %v", err)
	}
	if keys.Versions() != 1 || keys.SealVersion() != 1 {
		t.Errorf("loaded %d versions sealing with %d, want 1 and 1", keys.Versions(), keys.SealVersion())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want the key file alone", names)
	}
}

// TestCreateMakesDifferentKeys guards that each new key file has its own
// random key, not a fixed or repeated one.
func TestCreateMakesDifferentKeys(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.key"), filepath.Join(dir, "b.key")
	if err := CreateKeyFile(a); err != nil {
		t.Fatal(err)
	}
	if err := CreateKeyFile(b); err != nil {
		t.Fatal(err)
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if string(da) == string(db) {
		t.Error("two key files hold the same key")
	}
}

// TestCreateRefusesExisting guards the rule that a key file is never
// replaced: losing the key that sealed the secrets loses the secrets.
func TestCreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nopacs.key")
	const before = "not to be replaced\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CreateKeyFile(path)
	if fault.KindOf(err) != fault.Conflict {
		t.Fatalf("CreateKeyFile over a file: %v (kind %v), want a Conflict", err, fault.KindOf(err))
	}
	after, _ := os.ReadFile(path)
	if string(after) != before {
		t.Errorf("the existing file was changed to %q", after)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the refused create left %d entries in the directory, want 1", len(entries))
	}
}

// TestCreatePrivateRefusesExisting guards that the temporary file is created
// only if nothing is at its path, so a file or link planted there in advance
// is never opened and written through.
func TestCreatePrivateRefusesExisting(t *testing.T) {
	path := filepath.Join(privateDir(t), "planted")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := createPrivate(path)
	if err == nil {
		_ = f.Close()
		t.Fatal("createPrivate opened a file that already existed")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("createPrivate over a file: %v, want os.ErrExist", err)
	}
}

// TestCreateInMissingDirectory guards that a key file in a directory that
// does not exist fails with an error rather than being written elsewhere.
func TestCreateInMissingDirectory(t *testing.T) {
	path := filepath.Join(privateDir(t), "no-such-dir", "nopacs.key")
	if err := CreateKeyFile(path); err == nil {
		t.Fatal("CreateKeyFile succeeded in a directory that does not exist")
	}
}

// TestLoadMissing guards that a missing key file is NotFound and names the
// path, so the operator knows which file to restore.
func TestLoadMissing(t *testing.T) {
	path := filepath.Join(privateDir(t), "absent.key")
	_, err := LoadKeyFile(path)
	if fault.KindOf(err) != fault.NotFound {
		t.Fatalf("LoadKeyFile of a missing file: kind %v, want NotFound", fault.KindOf(err))
	}
	if !strings.Contains(fault.Message(err), path) {
		t.Errorf("message %q does not name the path", fault.Message(err))
	}
}

// TestLoadTooLarge guards the bound on what is read, so a mistaken path to a
// large file cannot exhaust memory.
func TestLoadTooLarge(t *testing.T) {
	_, err := loadText(t, strings.Repeat(keyLine(1, 1), maxKeyFileSize/len(keyLine(1, 1))+1))
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "larger") {
		t.Fatalf("LoadKeyFile of a large file: %v, want Invalid and too large", err)
	}
}

// TestParseKeyFileAccepts guards the valid spellings: several versions in
// any order (the highest seals), CRLF line ends, and a missing final line
// feed.
func TestParseKeyFileAccepts(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		versions int
		seal     uint32
	}{
		{"one", keyLine(1, 1), 1, 1},
		{"no final line feed", strings.TrimSuffix(keyLine(1, 1), "\n"), 1, 1},
		{"crlf", strings.ReplaceAll(keyLine(1, 1)+keyLine(2, 2), "\n", "\r\n"), 2, 2},
		{"out of order", keyLine(3, 3) + keyLine(1, 1) + keyLine(2, 2), 3, 3},
		{"largest version", keyLine(MaxKeyVersion, 1), 1, MaxKeyVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, err := parseKeyFile([]byte(tt.text))
			if err != nil {
				t.Fatalf("parseKeyFile: %v", err)
			}
			if keys.Versions() != tt.versions || keys.SealVersion() != tt.seal {
				t.Errorf("%d versions sealing with %d, want %d and %d",
					keys.Versions(), keys.SealVersion(), tt.versions, tt.seal)
			}
		})
	}
}

// malformedKeyFiles are key file texts that must be refused, each for the
// reason in its name. Several hold a real test key, so the leak test can
// check the refusal does not quote it.
func malformedKeyFiles() map[string]string {
	b64 := base64.StdEncoding.EncodeToString(testKey(1))
	b64Short := base64.StdEncoding.EncodeToString(testKey(1)[:31])
	b64Long := base64.StdEncoding.EncodeToString(append(testKey(1), 0))
	b64Unpadded := base64.RawStdEncoding.EncodeToString(testKey(1))
	// All bits set, so the URL alphabet's "_" appears where standard base64
	// has "/".
	b64URL := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, keySize))
	return map[string]string{
		"empty":               "",
		"blank line":          keyLine(1, 1) + "\n" + keyLine(2, 2),
		"only a line feed":    "\n",
		"wrong prefix":        "nopacs-key-v2 1 " + b64 + "\n",
		"two spaces":          "nopacs-key-v1  1 " + b64 + "\n",
		"tab":                 "nopacs-key-v1\t1 " + b64 + "\n",
		"trailing space":      "nopacs-key-v1 1 " + b64 + " \n",
		"extra field":         "nopacs-key-v1 1 " + b64 + " extra\n",
		"missing key":         "nopacs-key-v1 1\n",
		"version zero":        "nopacs-key-v1 0 " + b64 + "\n",
		"leading zero":        "nopacs-key-v1 01 " + b64 + "\n",
		"plus sign":           "nopacs-key-v1 +1 " + b64 + "\n",
		"negative version":    "nopacs-key-v1 -1 " + b64 + "\n",
		"version too large":   "nopacs-key-v1 4294967296 " + b64 + "\n",
		"version over int32":  "nopacs-key-v1 2147483648 " + b64 + "\n",
		"version 2^32-1":      "nopacs-key-v1 4294967295 " + b64 + "\n",
		"version not number":  "nopacs-key-v1 one " + b64 + "\n",
		"key 31 bytes":        "nopacs-key-v1 1 " + b64Short + "\n",
		"key 33 bytes":        "nopacs-key-v1 1 " + b64Long + "\n",
		"key without padding": "nopacs-key-v1 1 " + b64Unpadded + "\n",
		"key in url base64":   "nopacs-key-v1 1 " + b64URL + "\n",
		"key with cr inside":  "nopacs-key-v1 1 " + b64[:20] + "\r" + b64[20:] + "\n",
		"key not base64":      "nopacs-key-v1 1 " + strings.Repeat("!", 44) + "\n",
		"repeated version":    keyLine(1, 1) + keyLine(1, 2),
		"good then bad":       keyLine(1, 1) + "nopacs-key-v1 2 " + b64Short + "\n",
	}
}

// TestParseKeyFileRefuses guards that every malformed key file is refused
// with kind Invalid, rather than loaded in some reading of it.
func TestParseKeyFileRefuses(t *testing.T) {
	for name, text := range malformedKeyFiles() {
		t.Run(name, func(t *testing.T) {
			keys, err := loadText(t, text)
			if err == nil {
				t.Fatalf("loaded %v from a malformed file", keys)
			}
			if fault.KindOf(err) != fault.Invalid {
				t.Errorf("kind %v, want Invalid: %v", fault.KindOf(err), err)
			}
		})
	}
}

// TestParseKeyFileNamesLine guards that a refusal says which line is wrong,
// so an operator with several versions can find it.
func TestParseKeyFileNamesLine(t *testing.T) {
	_, err := parseKeyFile([]byte(keyLine(1, 1) + keyLine(2, 2) + "nopacs-key-v1 3 short\n"))
	if err == nil || !strings.Contains(fault.Message(err), "line 3:") {
		t.Fatalf("error %v does not name line 3", err)
	}
	_, err = parseKeyFile([]byte(keyLine(5, 1) + keyLine(5, 2)))
	if err == nil || !strings.Contains(fault.Message(err), "line 2: version 5 appears twice") {
		t.Fatalf("error %v does not name the repeated version on line 2", err)
	}
}

// TestParseKeyFileBoundsVersion guards that a key version above 2^31-1 is
// refused with a message naming the bound: the API carries a key version as
// an int32, so a larger one could be used to seal and sign yet never be
// named back through it.
func TestParseKeyFileBoundsVersion(t *testing.T) {
	for _, v := range []uint32{MaxKeyVersion + 1, 1<<32 - 1} {
		_, err := parseKeyFile([]byte(keyLine(v, 1)))
		if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "from 1 to 2147483647") {
			t.Errorf("version %d: err = %v, want Invalid naming 2147483647", v, err)
		}
	}
	if _, err := parseKeyFile([]byte(keyLine(MaxKeyVersion, 1))); err != nil {
		t.Errorf("version %d: %v", MaxKeyVersion, err)
	}
}

// FuzzParseKeyFile guards the key file parser against any input: it never
// panics, and whatever it accepts holds at least one key, every key usable,
// and seals with the highest version.
func FuzzParseKeyFile(f *testing.F) {
	f.Add([]byte(keyLine(1, 1)))
	f.Add([]byte(keyLine(2, 2) + keyLine(1, 1)))
	f.Add([]byte(strings.ReplaceAll(keyLine(7, 7), "\n", "\r\n")))
	for _, text := range malformedKeyFiles() {
		f.Add([]byte(text))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		keys, err := parseKeyFile(data)
		if err != nil {
			if keys != nil {
				t.Fatal("an error came with keys")
			}
			return
		}
		if keys.Versions() == 0 {
			t.Fatal("accepted a file with no key")
		}
		for v := range keys.r.aeads {
			if v == 0 || v > keys.SealVersion() {
				t.Fatalf("version %d with seal version %d", v, keys.SealVersion())
			}
		}
		sealed, err := keys.Seal("fuzz", []byte("p"))
		if err != nil {
			t.Fatalf("an accepted file cannot seal: %v", err)
		}
		if _, err := keys.Open("fuzz", sealed); err != nil {
			t.Fatalf("an accepted file cannot open its own seal: %v", err)
		}
	})
}

// TestCreateRefusesFileMadeBeforeLink guards the last moment of
// CreateKeyFile: a file that appears at the key file's path after the
// existence check, just before the new file is given its name, is left
// untouched and the call fails with Conflict. Replacing os.Link with
// os.Rename, which replaces an existing file, fails this test.
func TestCreateRefusesFileMadeBeforeLink(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "nopacs.key")
	const other = "a key file someone else made at the same moment\n"
	fsys := fileSystem{beforeLink: func() {
		if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
			t.Error(err)
		}
	}}

	err := fsys.createKeyFile(path)
	if fault.KindOf(err) != fault.Conflict {
		t.Fatalf("CreateKeyFile with a file made before the link: %v (kind %v), want Conflict", err, fault.KindOf(err))
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != other {
		t.Errorf("the file made before the link was changed or removed (%d bytes, %v)", len(got), readErr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want the other file alone", len(entries))
	}
}

// TestCreateSyncFailureSaysCreated guards the error when the directory
// cannot be synced after the key file was linked: the file exists, whole and
// loadable, so the error must say it was created and may not survive a
// crash, never that nothing was created.
func TestCreateSyncFailureSaysCreated(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	fsys := fileSystem{syncDir: func(string) error { return errors.New("synthetic sync failure") }}

	err := fsys.createKeyFile(path)
	if err == nil {
		t.Fatal("CreateKeyFile succeeded although the directory sync failed")
	}
	msg := fault.Message(err)
	if !strings.Contains(msg, "was created") || !strings.Contains(msg, "may not survive a crash") {
		t.Errorf("message %q does not say the file was created but may not survive a crash", msg)
	}
	if strings.Contains(msg, "cannot be created") {
		t.Errorf("message %q reads as if nothing was created", msg)
	}
	if fault.KindOf(err) != fault.Internal {
		t.Errorf("kind %v, want Internal", fault.KindOf(err))
	}
	if _, err := LoadKeyFile(path); err != nil {
		t.Errorf("the created file does not load: %v", err)
	}
}
