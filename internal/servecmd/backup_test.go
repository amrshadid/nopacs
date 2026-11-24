package servecmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/server"
)

// TestBackupCommands guards "nopacs-server backup create", "nopacs-server backup verify"
// and "nopacs-server backup restore" on this machine: create refuses while a server
// holds the data directory and leaves no file; with the server stopped it
// writes the backup and says its size and SHA-256; verify says the backup
// is whole and that nothing was written, and refuses one cut short;
// restore into a new directory, with
// the key file given from the old one, says the keys are suspended and how
// to start the server, writes the installation's id last, and a server
// then starts on the restored directory; a second restore into it is
// refused as not empty.
func TestBackupCommands(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(filepath.Dir(dir), "inst.backup")
	running := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, running)
	if _, err := run(t, nil, "backup", "create", "--data-dir", dir, "--out", out); err == nil ||
		!strings.Contains(err.Error(), "another nopacs-server serve is running") {
		t.Fatalf("a backup beside a running server answered %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused backup left its file")
	}
	if err := running.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-running.done; err == nil {
		t.Fatal("the server exited cleanly after it was killed")
	}
	text, err := run(t, nil, "backup", "create", "--data-dir", dir, "--out", out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Wrote a backup of installation") || !strings.Contains(text, "SHA-256") {
		t.Fatalf("create says:\n%s", text)
	}
	text, err = run(t, nil, "backup", "verify", out, "--data-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"is whole", "every schema version one this nopacs restores", "Nothing was written", "master"} {
		if !strings.Contains(text, want) {
			t.Fatalf("verify does not say %q:\n%s", want, text)
		}
	}
	cut := filepath.Join(filepath.Dir(dir), "cut.backup")
	whole, err := os.ReadFile(out) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cut, whole[:len(whole)-1], 0o600); err != nil { // #nosec G703 -- the test's own file in its own directory.
		t.Fatal(err)
	}
	if _, err := run(t, nil, "backup", "verify", cut, "--key-file", filepath.Join(dir, servecmd.KeyFileName)); err == nil ||
		!strings.Contains(err.Error(), "does not open") {
		t.Fatalf("verify of a backup cut short answered %v", err)
	}
	target := shortDir(t)
	text, err = run(t, nil, "backup", "restore", out, "--data-dir", target, "--key-file", filepath.Join(dir, servecmd.KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Restored installation", "Keys: all 0 suspended", "nopacs-server serve --data-dir " + target} {
		if !strings.Contains(text, want) {
			t.Fatalf("restore does not say %q:\n%s", want, text)
		}
	}
	a, _ := server.ReadInstallationID(dir)
	b, err := server.ReadInstallationID(target)
	if err != nil || a != b {
		t.Fatalf("the restored directory's id is %q (%v), want %q", b, err, a)
	}
	restored := startChild(t, "serve", "--data-dir", target)
	waitServing(t, target, restored)
	if err := restored.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-restored.done; err == nil {
		t.Fatal("the restored server exited cleanly after it was killed")
	}
	if _, err := run(t, nil, "backup", "restore", out, "--data-dir", target, "--key-file", filepath.Join(dir, servecmd.KeyFileName)); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a second restore into the same directory answered %v", err)
	}
}
