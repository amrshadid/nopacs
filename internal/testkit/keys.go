package testkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/secret"
)

// InstallationKeys returns the keys of a new installation key file, made in
// a directory of the test's own, for a test that writes or reads what the
// installation key authenticates (correction records among them).
func InstallationKeys(t testing.TB) *secret.Keys {
	t.Helper()
	dir := t.TempDir()
	// LoadKeyFile refuses a key file in a directory others may change.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nopacs.key")
	if err := secret.CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
