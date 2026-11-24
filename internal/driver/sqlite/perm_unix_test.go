//go:build unix

package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestOpenRefusesSharedFiles guards the rule that a database file or its -wal
// and -shm files that another user can read or write, and a directory that
// another user can write, are refused with a message saying how to fix it
// and naming no path, and that the same database opens once the mode is
// fixed. A directory others can only list is allowed (TestPragmas and every
// other test open in one, since t.TempDir makes them 0755).
func TestOpenRefusesSharedFiles(t *testing.T) {
	ctx := context.Background()
	made := func(t *testing.T) (string, string) {
		dir := t.TempDir()
		path := filepath.Join(dir, "master.db")
		m, err := OpenMaster(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		// Closing the last connection removes -wal and -shm; make them
		// again so their modes can be tested too.
		for _, f := range []string{path + "-wal", path + "-shm"} {
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir, path
	}
	cases := []struct {
		name, file string
		mode       os.FileMode
		want       string
	}{
		{"file readable by others", "", 0o644, "chmod 600"},
		{"file writable by the group", "", 0o620, "chmod 600"},
		{"-wal readable by the group", "-wal", 0o640, "chmod 600"},
		{"-shm writable by others", "-shm", 0o602, "chmod 600"},
		{"directory writable by others", "dir", 0o703, "chmod go-w"},
		{"directory writable by the group", "dir", 0o770, "chmod go-w"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, path := made(t)
			target := path + c.file
			fixed := os.FileMode(0o600)
			if c.file == "dir" {
				target, fixed = dir, 0o700
			}
			if err := os.Chmod(target, c.mode); err != nil {
				t.Fatal(err)
			}
			_, err := OpenMaster(ctx, path)
			if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), c.want) {
				t.Fatalf("OpenMaster = %v, want Invalid saying %q", err, c.want)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("the error names the path: %v", err)
			}
			if err := os.Chmod(target, fixed); err != nil {
				t.Fatal(err)
			}
			m, err := OpenMaster(ctx, path)
			if err != nil {
				t.Fatalf("OpenMaster after the fix: %v", err)
			}
			_ = m.Close()
		})
	}
}

// TestOpenRefusesAnotherOwner guards that a database file belonging to
// another user is refused, by checking it as another user rather than
// needing root; the same file checked as its owner passes, so the refusal is
// the owner's.
func TestOpenRefusesAnotherOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateAs(path, masterDB, os.Geteuid()); err != nil {
		t.Fatalf("the file checked as its owner: %v", err)
	}
	err := checkPrivateAs(path, masterDB, os.Geteuid()+1)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "belongs to another user") {
		t.Errorf("a check of another user's file = %v, want Invalid saying so", err)
	}
}
