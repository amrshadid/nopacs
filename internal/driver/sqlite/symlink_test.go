package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestOpenRefusesSymlinks guards that a symbolic link planted at a
// database's path, or at its -wal path, is refused as Invalid, for the
// master and a tenant alike, instead of being followed to a file of the
// planter's choosing; the message names the database and no path.
func TestOpenRefusesSymlinks(t *testing.T) {
	ctx := context.Background()
	opens := map[string]func(path string) error{
		"master": func(p string) error {
			m, err := OpenMaster(ctx, p)
			if err == nil {
				_ = m.Close()
			}
			return err
		},
		"tenant": func(p string) error {
			s, err := OpenTenant(ctx, p, "t1")
			if err == nil {
				_ = s.Close()
			}
			return err
		},
	}
	for name, open := range opens {
		for _, suffix := range []string{"", "-wal"} {
			t.Run(name+suffix, func(t *testing.T) {
				dir := t.TempDir()
				target := filepath.Join(dir, "elsewhere.db")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "db.db")
				if err := os.Symlink(target, path+suffix); err != nil {
					t.Skipf("cannot make a symbolic link here: %v", err)
				}
				err := open(path)
				if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "is not a regular file") ||
					!strings.Contains(fault.Message(err), name+" database") {
					t.Errorf("a link at the %s's%s path: %v, want Invalid saying it is not a regular file", name, suffix, err)
				}
				if err != nil && strings.Contains(err.Error(), dir) {
					t.Errorf("the error names the path: %v", err)
				}
			})
		}
	}
}
