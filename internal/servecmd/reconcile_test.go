package servecmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/backup"
)

// TestBackupRestoreReconcileOnly guards "nopacs-server backup restore
// --reconcile-only" on this machine: it reads no backup, so a FILE with it
// is refused, as is restore with neither; beside a running server it is
// refused by the data directory's lock; with the server stopped it
// reconciles the installation in place, says so and how to start the
// server, and a server then starts; a data directory holding the marker of
// an interrupted reconcile is refused by serve, with the advice to run the
// reconcile again, not to empty the directory.
func TestBackupRestoreReconcileOnly(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, nil, "backup", "restore", "some.backup", "--reconcile-only", "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "reads no backup") {
		t.Fatalf("--reconcile-only with a FILE answered %v", err)
	}
	if _, err := run(t, nil, "backup", "restore", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "--reconcile-only") {
		t.Fatalf("restore with neither a FILE nor --reconcile-only answered %v", err)
	}
	running := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, running)
	if _, err := run(t, nil, "backup", "restore", "--reconcile-only", "--data-dir", dir); err == nil ||
		!strings.Contains(err.Error(), "another nopacs-server serve is running") {
		t.Fatalf("a reconcile beside a running server answered %v", err)
	}
	stopServer(t, running)

	text, err := run(t, nil, "backup", "restore", "--reconcile-only", "--data-dir", dir)
	if err != nil {
		t.Fatalf("the reconcile failed: %v\n%s", err, text)
	}
	for _, want := range []string{"Reconciled installation", "No backup was read", "Keys: all 0 suspended", "nopacs-server serve --data-dir " + dir} {
		if !strings.Contains(text, want) {
			t.Fatalf("the reconcile does not say %q:\n%s", want, text)
		}
	}
	again := startChild(t, "serve", "--data-dir", dir)
	waitServing(t, dir, again)
	stopServer(t, again)

	if err := os.WriteFile(backup.ReconcileMarkerPath(dir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, nil, "serve", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "--reconcile-only again") ||
		strings.Contains(err.Error(), "remove the data directory") {
		t.Fatalf("serve on a data directory a reconcile was interrupted in answered %v", err)
	}
}

// TestOfflineCommandsRefuseAnInterruptedDataDirectory guards the commands
// that run on the data directory with the server stopped, besides serve:
// "backup create" and "tenant import" refuse a data directory a reconcile
// of a site's own copy, or a restore, was interrupted in, with that
// marker's advice, writing no backup file, since its databases may be
// reconciled or loaded part way.
func TestOfflineCommandsRefuseAnInterruptedDataDirectory(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "installation.backup")
	for _, m := range []struct{ marker, advice string }{
		{backup.ReconcileMarkerPath(dir), "--reconcile-only again"},
		{backup.MarkerPath(dir), "a restore was interrupted"},
	} {
		if err := os.WriteFile(m.marker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := run(t, nil, "backup", "create", "--data-dir", dir, "--out", out); err == nil || !strings.Contains(err.Error(), m.advice) {
			t.Fatalf("backup create beside %s answered %v", filepath.Base(m.marker), err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("backup create beside %s wrote %s", filepath.Base(m.marker), out)
		}
		if _, err := run(t, nil, "tenant", "import", "lab.export", "--transfer-key", "lab.key", "--data-dir", dir); err == nil ||
			!strings.Contains(err.Error(), m.advice) {
			t.Fatalf("tenant import beside %s answered %v", filepath.Base(m.marker), err)
		}
		if err := os.Remove(m.marker); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, nil, "backup", "create", "--data-dir", dir, "--out", out); err != nil {
		t.Fatalf("backup create with no marker failed: %v", err)
	}
}
