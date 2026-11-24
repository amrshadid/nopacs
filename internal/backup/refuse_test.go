package backup

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// restoreInto runs a restore of the file at path into a new directory and
// returns its error and the directory.
func (r *rig) restoreInto(path string) (string, error) {
	r.t.Helper()
	target := filepath.Join(r.t.TempDir(), "restored")
	_, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")})
	return target, err
}

// wantNothingWritten fails unless dir does not exist.
func wantNothingWritten(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("a refused restore wrote %s (%d entries)", dir, len(entries))
	}
}

// plaintext decrypts a backup whole.
func plaintext(t *testing.T, b []byte, keys *secret.Keys) []byte {
	t.Helper()
	br, err := secret.NewBackupReader(bytes.NewReader(b), keys)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// encrypt encrypts a plaintext as a backup.
func encrypt(t *testing.T, plain []byte, keys *secret.Keys) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw, err := secret.NewBackupWriter(&buf, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRestoreRefusesBrokenBackup guards that a backup changed or cut short
// restores nothing: no data directory, no database, before anything is
// written. Each is refused: a byte changed, the last byte cut, half the
// file cut, a row changed in a plaintext encrypted again with the right key
// (its section's SHA-256 no longer the footer's), a section's footer line
// dropped, a line appended after the footer, and a backup opened with
// another key file.
func TestRestoreRefusesBrokenBackup(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:2]...)
	path, _ := r.backup()
	good, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	plain := plaintext(t, good, r.keys)
	holder := []byte(`"pipeline robot"`)
	if !bytes.Contains(plain, holder) {
		t.Fatal("the plaintext holds no key's holder to change")
	}
	lines := bytes.SplitAfter(plain, []byte("\n"))
	lines = lines[:len(lines)-1] // The empty piece after the last "\n".
	noFooter := bytes.Join(lines[:len(lines)-1], nil)
	flipped := slicesClone(good)
	flipped[len(flipped)/2] ^= 0x01
	cases := []struct {
		name string
		body []byte
		keys *secret.Keys
		want string
	}{
		{"a byte changed", flipped, r.keys, "does not open"},
		{"the last byte cut", good[:len(good)-1], r.keys, "does not open"},
		{"half cut", good[:len(good)/2], r.keys, "does not open"},
		{"a row changed under the right key", encrypt(t, bytes.Replace(plain, holder, []byte(`"pipeline robou"`), 1), r.keys), r.keys,
			"SHA-256"},
		{"the footer dropped", encrypt(t, noFooter, r.keys), r.keys, "footer"},
		{"a line after the footer", encrypt(t, append(slicesClone(plain), []byte("{\"kind\":\"footer\"}\n")...), r.keys), r.keys,
			"follows its footer"},
		{"another key file", good, otherKeys(t), "does not open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "broken.nopacs")
			if err := os.WriteFile(p, c.body, 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored")
			_, err := Restore(r.ctx, RestoreConfig{File: p, Keys: c.keys, DataDir: target,
				MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")})
			if err == nil || fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), c.want) {
				t.Fatalf("the restore answered %v, want Invalid saying %q", err, c.want)
			}
			wantNothingWritten(t, target)
		})
	}
	// The good file still restores, so what was refused was the change.
	if _, err := r.restoreInto(path); err != nil {
		t.Fatalf("the unchanged backup does not restore: %v", err)
	}
}

// otherKeys returns a key file that is not the rig's.
func otherKeys(t *testing.T) *secret.Keys {
	t.Helper()
	p := filepath.Join(t.TempDir(), "other.key")
	if err := secret.CreateKeyFile(p); err != nil {
		t.Fatal(err)
	}
	k, err := secret.LoadKeyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// slicesClone copies b.
func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

// TestRestoreRefusesNonEmptyDataDir guards that a restore never writes into
// a data directory that holds anything but the key file: a stray file, or
// a whole installation (the rig's own), is refused as a conflict, and what
// was there is left as it was; a directory holding only the key file is
// restored into.
func TestRestoreRefusesNonEmptyDataDir(t *testing.T) {
	r := newRig(t, false)
	path, _ := r.backup()
	stray := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "notes.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{stray, r.dataDir} {
		before, _ := os.ReadDir(dir)
		_, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: dir,
			MasterAddress: driver.SQLiteScheme + filepath.Join(dir, "master.db")})
		if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "not empty") {
			t.Fatalf("a restore into %s answered %v, want a conflict saying it is not empty", dir, err)
		}
		after, _ := os.ReadDir(dir)
		if len(before) != len(after) {
			t.Fatalf("a refused restore changed %s: %d entries, then %d", dir, len(before), len(after))
		}
	}
	if b, err := os.ReadFile(filepath.Join(stray, "notes.txt")); err != nil || string(b) != "keep me" { // #nosec G304 -- the test's own file.
		t.Fatalf("the stray file is %q (%v)", b, err)
	}
	onlyKey := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(onlyKey, 0o700); err != nil {
		t.Fatal(err)
	}
	k, err := os.ReadFile(r.keyPath) // #nosec G304 -- the test's own key file.
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(onlyKey, "key")
	if err := os.WriteFile(keyPath, k, 0o600); err != nil { // #nosec G703 -- the test's own key file in its own directory.
		t.Fatal(err)
	}
	if _, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: keyPath, DataDir: onlyKey,
		MasterAddress: driver.SQLiteScheme + filepath.Join(onlyKey, "master.db")}); err != nil {
		t.Fatalf("a restore into a directory holding only the key file failed: %v", err)
	}
}
