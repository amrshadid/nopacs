package backup

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The variables of the outside checks: the Python that runs
// scripts/prove/catalog_dump.py and scripts/prove/backup_open.py (the
// standard library, and the cryptography package for the second), and,
// for a catalog on Postgres, the command line of the psql it reads with.
const (
	checkPythonEnv = "NOPACS_CHECK_BACKUP_PYTHON"
	checkPsqlEnv   = "NOPACS_CHECK_BACKUP_PSQL"
)

// outside runs the tools of the outside checks.
type outside struct {
	t      *testing.T
	python string
	psql   string
}

// newOutside returns the tools and true when checkPythonEnv is set (set,
// even to nothing, the check was asked for, and a Python that is not there
// fails the test); false when it is not, and the check is left out. A
// catalog on Postgres is checked only when checkPsqlEnv names a psql too.
func newOutside(t *testing.T) (outside, bool) {
	t.Helper()
	python, asked := os.LookupEnv(checkPythonEnv)
	if !asked {
		return outside{}, false
	}
	if python == "" {
		python = "python3"
	}
	if _, err := exec.LookPath(python); err != nil {
		t.Fatalf("%s names no Python: %v", checkPythonEnv, err)
	}
	return outside{t: t, python: python, psql: os.Getenv(checkPsqlEnv)}, true
}

// script returns the path of a script of scripts/prove.
func script(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "prove", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// run runs a script with args and returns its standard output, failing the
// test on a non-zero exit unless failOK.
func (o outside) run(env []string, failOK bool, args ...string) (string, error) {
	o.t.Helper()
	cmd := exec.Command(o.python, append([]string{"-I"}, args...)...) // #nosec G204 -- the Python the person running the check named.
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil && !failOK {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		o.t.Fatalf("%v: %v %s", args, err, stderr)
	}
	return string(out), err
}

// dump runs catalog_dump.py on the SQLite file at path, or on the Postgres
// schema of address, and returns the canonical catalog. On Postgres with no
// psql named it returns "", and the comparison of two such is empty.
func (o outside) dump(path, address string) string {
	o.t.Helper()
	if address == "" {
		out, _ := o.run(nil, false, script(o.t, "catalog_dump.py"), "--sqlite", path)
		return out
	}
	if o.psql == "" {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil {
		o.t.Fatal(err)
	}
	out, _ := o.run([]string{"PSQL=" + o.psql}, false, script(o.t, "catalog_dump.py"), "--postgres", strings.TrimPrefix(u.Path, "/"),
		"--schema", u.Query().Get("search_path"))
	return out
}

// TestBackupOpensOutside checks a backup this package wrote with a tool
// that is not NoPACs: backup_open.py opens it with the installation key
// file, parses its sections and checks every section's SHA-256 against the
// footer, and names each with the digest the summary gave; a backup with
// one byte changed is refused.
func TestBackupOpensOutside(t *testing.T) {
	tool, asked := newOutside(t)
	if !asked {
		t.Skip(checkPythonEnv + " is not set: the outside check of backups was not asked for")
	}
	r := newRig(t, false)
	r.archive(studyFiles[:2]...)
	path, sum := r.backup()
	out, _ := tool.run(nil, false, script(t, "backup_open.py"), path, "--key-file", r.keyPath, "--sections")
	for _, s := range sum.Sections {
		name := "master"
		if s.Tenant != "" {
			name = "tenant " + s.Tenant
		}
		if !strings.Contains(out, name) || !strings.Contains(out, "sha256 "+s.SHA256+" ok") {
			t.Fatalf("backup_open.py did not check the section of %s (sha256 %s):\n%s", name, s.SHA256, out)
		}
	}
	if !strings.Contains(out, "sections: 2, every SHA-256 as the footer says") {
		t.Fatalf("backup_open.py did not check every section:\n%s", out)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 1
	if err := os.WriteFile(path, b, 0o600); err != nil { // #nosec G703 -- the test's own file in its own directory.
		t.Fatal(err)
	}
	if _, err := tool.run(nil, true, script(t, "backup_open.py"), path, "--key-file", r.keyPath, "--sections"); err == nil {
		t.Fatal("backup_open.py opened a backup with a byte changed")
	}
}
