package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupCreate guards "backup create" against what the server streams:
// a whole backup is written to --out byte for byte, with its summary for a
// person and as a document; a stream that ends without its summary, and a
// summary whose size or SHA-256 is not that of the bytes written, each fail
// (exit 1) naming why, leave no file at --out and no part file; a file
// already at --out is refused (exit 5) and left as it was.
func TestBackupCreate(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	data := bytes.Repeat([]byte("encrypted backup bytes "), 100000) // Past one message of 1 MiB.
	r.fake.SetBackup(data, false, 0, nil)
	out := filepath.Join(dir, "whole.backup")
	res := r.run("backup", "create", "--out", out)
	got, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file.
	if res.code != 0 || err != nil || !bytes.Equal(got, data) || !strings.Contains(res.stdout, "SHA-256") ||
		!strings.Contains(res.stdout, "nopacs-server backup verify "+out) {
		t.Fatalf("a whole backup: %+v (%d bytes written, %v)", res, len(got), err)
	}
	doc := document(t, r.run("backup", "create", "--out", filepath.Join(dir, "json.backup"), "--json"))
	if doc["format"] != "nopacs-backup" || doc["installationId"] != "inst-fake-1" {
		t.Fatalf("--json: %v", doc)
	}

	res = r.run("backup", "create", "--out", out)
	if after, _ := os.ReadFile(out); res.code != 5 || !strings.Contains(res.stderr, "writing the backup never replaces a file") || !bytes.Equal(after, data) { //nolint:gosec // G304: the test's own file.
		t.Fatalf("a backup onto an existing file: %+v", res)
	}

	for name, c := range map[string]struct {
		withoutEnd bool
		size       int64
		sha        []byte
		want       string
	}{
		"no summary":   {withoutEnd: true, want: "ended before its summary"},
		"another size": {size: int64(len(data)) + 1, want: "is not the one the server's summary describes"},
		"another SHA":  {sha: bytes.Repeat([]byte{7}, 32), want: "is not the one the server's summary describes"},
	} {
		r.fake.SetBackup(data, c.withoutEnd, c.size, c.sha)
		failed := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".backup")
		res := r.run("backup", "create", "--out", failed)
		if res.code != 1 || !strings.Contains(res.stderr, c.want) || res.stdout != "" {
			t.Errorf("%s: %+v", name, res)
		}
		if _, err := os.Lstat(failed); !os.IsNotExist(err) {
			t.Errorf("%s: a file was left at --out: %v", name, err)
		}
	}
	if left := parts(t, dir); len(left) != 0 {
		t.Fatalf("part files were left: %v", left)
	}
}
