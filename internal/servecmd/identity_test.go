package servecmd_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestServeRefusesAnotherMaster guards that serve pointed at a master that is
// not the one init made refuses to start, and changes nothing in it. Without
// it, serve given another address would silently serve an empty or foreign
// master, with none of this installation's tenants, keys or grants. An empty
// database, one with the tables and no installation id, and another
// installation's master are each refused with a message saying the master is
// not this installation's; the right master is accepted (as far as the
// identity goes) in every other test.
func TestServeRefusesAnotherMaster(t *testing.T) {
	dir := shortDir(t)
	parent := filepath.Dir(dir)
	ours := "sqlite:" + filepath.Join(parent, "ours.db")
	if _, err := run(t, map[string]string{"M": ours}, "init", "--data-dir", dir, "--master-env", "M"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, "other")
	otherMaster := "sqlite:" + filepath.Join(parent, "theirs.db")
	if _, err := run(t, map[string]string{"M": otherMaster}, "init", "--data-dir", other, "--master-env", "M"); err != nil {
		t.Fatal(err)
	}
	// Tables and no id: a master migrated by something else.
	bare := filepath.Join(parent, "bare.db")
	m, err := sqlite.OpenMaster(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	empty := filepath.Join(parent, "empty.db")
	for _, c := range []struct {
		address, want string
	}{
		{"sqlite:" + empty, "it is empty"},
		{"sqlite:" + bare, "it holds no installation id"},
		{otherMaster, "it belongs to another installation"},
	} {
		// A serve that is not refused starts serving and does not return, so
		// a watchdog fails the test then.
		result := make(chan error, 1)
		go func() {
			_, err := run(t, map[string]string{"M": c.address}, "serve", "--data-dir", dir, "--master-env", "M")
			result <- err
		}()
		var err error
		select {
		case err = <-result:
		case <-time.After(10 * time.Second):
			t.Fatalf("serve started on %s, a master that is not this installation's", c.address)
		}
		if err == nil || !strings.Contains(err.Error(), "the master database is not the one this installation was made with") || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("serve on %s: %v", c.address, err)
		}
	}
	// The empty database was not migrated by the refused serve.
	m, err = sqlite.OpenMaster(context.Background(), empty)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if v, err := m.Version(context.Background()); err != nil || v != 0 {
		t.Fatalf("the refused serve changed the empty master: version %d, %v", v, err)
	}
}

// TestInitRefusesAMasterInUse guards that init does not take a master that
// already belongs to an installation.
func TestInitRefusesAMasterInUse(t *testing.T) {
	dir := shortDir(t)
	address := "sqlite:" + filepath.Join(filepath.Dir(dir), "shared.db")
	if _, err := run(t, map[string]string{"M": address}, "init", "--data-dir", dir, "--master-env", "M"); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(filepath.Dir(dir), "second")
	_, err := run(t, map[string]string{"M": address}, "init", "--data-dir", second, "--master-env", "M")
	if err == nil || !strings.Contains(err.Error(), "already belongs to an installation") {
		t.Fatalf("a second init on the same master: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, servecmd.KeyFileName)); err == nil {
		t.Fatal("the refused init left a key file")
	}
}

// TestHalfMadeDirectory guards that a data directory whose init was
// interrupted says so, and what to remove, for each way it can be half
// made.
func TestHalfMadeDirectory(t *testing.T) {
	full := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", full); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		remove []string
		want   string
	}{
		{"master only", []string{servecmd.KeyFileName, server.IDFileName}, "init was interrupted before it wrote the key file"},
		{"key and master, no id", []string{server.IDFileName}, "has a key file but no installation id: init was interrupted"},
		{"key and id, no master", []string{servecmd.MasterFileName}, "if init was interrupted, remove"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := shortDir(t)
			if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
				t.Fatal(err)
			}
			for _, name := range c.remove {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := run(t, nil, "serve", "--data-dir", dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("serve: %v", err)
			}
		})
	}
	// A directory a restore was interrupted in says so, and never advises
	// removing the key file the restore needs: restore's default key file is
	// the data directory's own, possibly its only copy.
	t.Run("restore interrupted", func(t *testing.T) {
		dir := shortDir(t)
		if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, server.IDFileName)); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(backup.MarkerPath(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backup.MarkerPath(dir), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, nil, "serve", "--data-dir", dir)
		if err == nil || !strings.Contains(err.Error(), "a restore was interrupted") || !strings.Contains(err.Error(), "except the key file") {
			t.Fatalf("serve after an interrupted restore: %v", err)
		}
		if strings.Contains(err.Error(), "Remove "+filepath.Join(dir, servecmd.KeyFileName)) {
			t.Fatalf("serve advises removing the key file: %v", err)
		}
	})
	// A directory with nothing of an installation says to run init.
	if _, err := run(t, nil, "serve", "--data-dir", shortDir(t)); err == nil || !strings.Contains(err.Error(), "make one with: nopacs-server init") {
		t.Fatalf("serve on nothing: %v", err)
	}
}

// TestDrainMustBePositive guards that --drain 0 and a negative drain are
// usage errors, not a silent default.
func TestDrainMustBePositive(t *testing.T) {
	dir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0", "-1s"} {
		if _, err := run(t, nil, "serve", "--data-dir", dir, "--drain", v); err == nil || !strings.Contains(err.Error(), "--drain is a positive duration") {
			t.Fatalf("--drain %s: %v", v, err)
		}
	}
}

// firstSchemaUndo turns a master made by init back into the schema its first
// migration made: the keys' suspension column the fifth added, the audit
// seal's tables, indexes and column the fourth added, the index the third
// added and the columns the second added to api_keys dropped, and the
// records of those migrations removed, as an installation made before them
// holds.
var firstSchemaUndo = []string{
	"ALTER TABLE api_keys DROP COLUMN suspended",
	"DROP TABLE audit_anchors",
	"DROP TABLE audit_checkpoints",
	"DROP INDEX audit_unsealed",
	"DROP INDEX audit_by_seq",
	"ALTER TABLE audit DROP COLUMN seq",
	"DROP INDEX api_keys_by_successor",
	"ALTER TABLE api_keys DROP COLUMN networks",
	"ALTER TABLE api_keys DROP COLUMN client_cert",
	"ALTER TABLE api_keys DROP COLUMN last_address",
	"ALTER TABLE api_keys DROP COLUMN successor",
	"DELETE FROM schema_migrations WHERE version >= 2",
}

// TestServeStartsAnOlderMaster guards an installation made before the
// master's latest migration: serve migrates it before anything reads it,
// and starts. Reading the installation id first made the SQLite driver
// prepare every statement, some naming columns the older schema lacks, so
// such an installation exited with "the master database is not ready; has
// it been migrated?". Each master is brought back to its first schema, then
// served; it ends at the latest schema, and its key check still holds.
func TestServeStartsAnOlderMaster(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := shortDir(t)
		if _, err := run(t, nil, "init", "--data-dir", dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, servecmd.MasterFileName)
		latest := masterVersion(t, "sqlite:"+path)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range firstSchemaUndo {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		_ = db.Close()
		if v := masterVersion(t, "sqlite:"+path); v != 1 {
			t.Fatalf("the master is at schema %d, want 1", v)
		}
		c := startChild(t, "serve", "--data-dir", dir)
		waitServing(t, dir, c)
		stopChild(t, c)
		if v := masterVersion(t, "sqlite:"+path); v != latest {
			t.Errorf("after serve the master is at schema %d, want %d", v, latest)
		}
	})
	t.Run("postgres", func(t *testing.T) {
		address, ok := testkit.PostgresSchema(t)
		if !ok {
			t.Skip(testkit.PostgresEnv + " is not set")
		}
		dir := shortDir(t)
		env := map[string]string{"M": address}
		if _, err := run(t, env, "init", "--data-dir", dir, "--master-env", "M"); err != nil {
			t.Fatal(err)
		}
		latest := masterVersion(t, address)
		testkit.PostgresExec(t, address, firstSchemaUndo...)
		if v := masterVersion(t, address); v != 1 {
			t.Fatalf("the master is at schema %d, want 1", v)
		}
		c := startChildEnv(t, []string{"M=" + address}, "serve", "--data-dir", dir, "--master-env", "M")
		waitServing(t, dir, c)
		stopChild(t, c)
		if v := masterVersion(t, address); v != latest {
			t.Errorf("after serve the master is at schema %d, want %d", v, latest)
		}
	})
}

// masterVersion returns the schema version of the master at address.
func masterVersion(t *testing.T, address string) int {
	t.Helper()
	m, err := driver.OpenMaster(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	v, err := m.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// stopChild kills a server child and waits for it to exit.
func stopChild(t *testing.T, c *child) {
	t.Helper()
	_ = c.cmd.Process.Kill()
	select {
	case err := <-c.done:
		t.Logf("the killed server exited: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not exit")
	}
}
