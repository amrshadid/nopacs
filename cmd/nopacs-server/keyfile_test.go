package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mgserver "github.com/amrshadid/nopacs/internal/server"
)

// TestServeRefusesAnotherKeyFile guards the program's answer to a key file
// that is not the installation's: serve exits 1 (a failure, not a usage
// mistake) with one line that says so, and prints nothing on standard
// output.
func TestServeRefusesAnotherKeyFile(t *testing.T) {
	parent, err := os.MkdirTemp("", "mgk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	ours, theirs := filepath.Join(parent, "ours"), filepath.Join(parent, "theirs")
	for _, dir := range []string{ours, theirs} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"init", "--data-dir", dir}, &stdout, &stderr); code != exitOK {
			t.Fatalf("init %s: exit %d: %s", dir, code, stderr.String())
		}
	}
	key, err := os.ReadFile(filepath.Join(theirs, "key")) //nolint:gosec // G304: the test's own key file.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ours, "key"), key, 0o600); err != nil { //nolint:gosec // G703: the test's own temporary directory.
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"serve", "--data-dir", ours}, &stdout, &stderr)
	want := "nopacs-server: " + mgserver.MessageWrongKeyFile + "\n"
	if code != exitFailed || stderr.String() != want || stdout.Len() != 0 {
		t.Fatalf("serve with another key file: exit %d, standard error %q, standard output %q; want exit %d and %q", code, stderr.String(), stdout.String(), exitFailed, want)
	}
	if strings.Count(stderr.String(), "\n") != 1 {
		t.Fatal("the refusal is not one line")
	}
}
