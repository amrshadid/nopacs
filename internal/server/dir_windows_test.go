//go:build windows

package server_test

import (
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/amrshadid/nopacs/internal/server"
)

// account returns the current user as DOMAIN\name, the form icacls prints,
// from the process token.
func account(t *testing.T) string {
	t.Helper()
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	name, domain, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	return domain + `\` + name
}

// aclEntries returns the access entries icacls prints for path, run from its
// directory with the bare name so the output starts with that name.
func aclEntries(t *testing.T, path string) []string {
	t.Helper()
	cmd := exec.Command("icacls", filepath.Base(path)) //nolint:gosec // G204: the test's own path.
	cmd.Dir = filepath.Dir(path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v\n%s", path, err, out)
	}
	var entries []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if line = strings.TrimSpace(strings.TrimPrefix(line, filepath.Base(path))); line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}

// TestRunDirACLWindows guards the only guard of the local socket on
// Windows, read back with the system's own icacls: after EnsurePrivateDir,
// <data-dir>\run grants full control, inherited by what is made inside, to
// the current user and to nobody else (not SYSTEM, not Administrators, not
// Users), and the socket the server makes inside it carries the same single
// entry, inherited.
func TestRunDirACLWindows(t *testing.T) {
	me := account(t)
	e := newEnv(t, options{noNetwork: true})
	run := server.RunDir(e.dir)
	entries := aclEntries(t, run)
	if len(entries) != 1 || !strings.EqualFold(entries[0], me+":(OI)(CI)(F)") {
		t.Fatalf("run\\ grants %q, want only %s:(OI)(CI)(F)", entries, me)
	}
	sock := aclEntries(t, server.SocketPath(e.dir))
	if len(sock) != 1 || !strings.HasPrefix(strings.ToLower(sock[0]), strings.ToLower(me)+":") {
		t.Fatalf("the socket grants %q, want only %s", sock, me)
	}
	t.Logf("run\\: %q; socket: %q", entries, sock)
	c, err := net.Dial("unix", server.SocketPath(e.dir))
	if err != nil {
		t.Fatalf("the server's own user cannot connect: %v", err)
	}
	_ = c.Close()
}

// TestRunDirNarrowedWindows guards that a run directory someone widened is
// narrowed again at the next start: it is made with the inherited access
// list of the temporary directory (several entries), and EnsurePrivateDir
// leaves the one entry.
func TestRunDirNarrowedWindows(t *testing.T) {
	me := account(t)
	dir := shortTempDir(t)
	run := server.RunDir(dir)
	if out, err := exec.Command("cmd", "/c", "mkdir", run).CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own path.
		t.Fatalf("mkdir: %v\n%s", err, out)
	}
	if before := aclEntries(t, run); len(before) < 2 {
		t.Logf("the temporary directory's own list has %d entries: %q", len(before), before)
	}
	if err := server.EnsurePrivateDir(run); err != nil {
		t.Fatal(err)
	}
	after := aclEntries(t, run)
	if len(after) != 1 || !strings.EqualFold(after[0], me+":(OI)(CI)(F)") {
		t.Fatalf("after EnsurePrivateDir, run\\ grants %q", after)
	}
}
