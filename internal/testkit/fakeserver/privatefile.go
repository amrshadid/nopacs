package fakeserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/secret"
)

// PrivateFile writes content to a new file that only the current user may
// open, and returns its path, for a test of --key-file. The file is made by
// the installation key file's own function, so its access list is the
// narrowest the system has (an ordinary os.WriteFile on Windows inherits the
// directory's wider list and would be refused), and its content is then
// replaced in place, which keeps that access list.
func PrivateFile(tb testing.TB, content string) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", "corkey")
	if err != nil {
		tb.Fatalf("making a directory: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "key")
	if err := secret.CreateKeyFile(path); err != nil {
		tb.Fatalf("making a private file: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		tb.Fatalf("writing the private file: %v", err)
	}
	return path
}
