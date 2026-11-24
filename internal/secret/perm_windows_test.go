//go:build windows

package secret

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// currentAccount returns the current user as DOMAIN\name, the form icacls
// prints, looked up from the process token's SID. whoami is not used: on a
// system with Git installed, PATH may find Git's whoami, which prints the
// name without its domain.
func currentAccount(t *testing.T) (sid, account string) {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	s, err := syscall.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	name, domain, _, err := s.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	return sid, domain + `\` + name
}

// readSecurity returns the owner and the granting SIDs of the file at path,
// read through the package's own fileSecurity.
func readSecurity(t *testing.T, path string) (owner string, sids []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	owner, sids, err = fileSecurity(f)
	if err != nil {
		t.Fatalf("fileSecurity: %v", err)
	}
	return owner, sids
}

// TestCreatedKeyFileACL guards that a new key file is owned by the current
// user and that its access list names the current user and nobody else. It
// reads the list twice: through the package's own fileSecurity, compared
// with the current user's SID, and through icacls, the system's own tool,
// compared with the account name of that SID, so a mistake in how the
// package reads a DACL cannot hide one in how it writes one.
func TestCreatedKeyFileACL(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	if err := CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	user, account := currentAccount(t)

	owner, sids := readSecurity(t, path)
	if owner != user {
		t.Errorf("the key file is owned by %s, want the current user %s", owner, user)
	}
	if len(sids) != 1 || sids[0] != user {
		t.Errorf("the access list grants %v, want only the current user %s", sids, user)
	}
	// The owner seen by the system's own tool as well.
	getACL := exec.Command("powershell", "-NoProfile", "-Command", "(Get-Acl -LiteralPath '"+path+"').Owner") //nolint:gosec // G204: the test's own file.
	if out, err := getACL.CombinedOutput(); err != nil {
		t.Errorf("Get-Acl: %v\n%s", err, out)
	} else if got := strings.TrimSpace(string(out)); !strings.EqualFold(got, account) {
		t.Errorf("Get-Acl reports the owner %q, want %s", got, account)
	}

	out, err := icacls(path)
	if err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	t.Logf("icacls %s:\n%s", filepath.Base(path), out)
	entries := icaclsEntries(out, filepath.Base(path))
	if len(entries) != 1 {
		t.Fatalf("icacls lists %d entries %q, want one, for %s", len(entries), entries, account)
	}
	name, rights, ok := strings.Cut(entries[0], ":")
	if !ok || !strings.EqualFold(name, account) || rights != "(F)" {
		t.Errorf("icacls lists %q, want %s:(F)", entries[0], account)
	}
}

// icacls runs icacls on path from its own directory with the bare file name,
// so the output starts with that name and not with a spelling of the full
// path (a short 8.3 name, say) the test would have to predict.
func icacls(path string, args ...string) (string, error) {
	cmd := exec.Command("icacls", append([]string{filepath.Base(path)}, args...)...) //nolint:gosec // G204: the test's own file and fixed arguments.
	cmd.Dir = filepath.Dir(path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// icaclsEntries returns the access entries in icacls output: the first line
// starts with the file name, each further entry is on its own indented line,
// and the output ends with a summary line after a blank one.
func icaclsEntries(out, name string) []string {
	var entries []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, name))
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}

// TestLoadRefusesWideACL guards that a key file whose access list grants
// anyone besides the current user is refused, naming them and giving the
// icacls commands that fix it. A file made by os.WriteFile in the temporary
// directory is granted to SYSTEM and the Administrators group as well as the
// user.
func TestLoadRefusesWideACL(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	if err := os.WriteFile(path, []byte(keyLine(1, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := icacls(path)
	t.Logf("icacls %s:\n%s", filepath.Base(path), out)

	_, err := LoadKeyFile(path)
	if err == nil {
		t.Fatal("loaded a key file other users may open")
	}
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("kind %v, want Invalid: %v", fault.KindOf(err), err)
	}
	if msg := fault.Message(err); !strings.Contains(msg, "icacls \""+path+"\" /inheritance:r") {
		t.Errorf("message %q does not say how to fix it", msg)
	}
	t.Logf("refusal: %s", fault.Message(err))
}

// TestLoadAfterSuggestedFix guards that the commands the refusal suggests do
// make the file load: an instruction that does not work is worse than none.
// It runs exactly the commands the message is built from.
func TestLoadAfterSuggestedFix(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	if err := os.WriteFile(path, []byte(keyLine(1, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	user, _ := currentAccount(t)
	owner, sids := readSecurity(t, path)
	t.Logf("before the fix: owner %s, granted to %v", owner, sids)
	var others []string
	for _, s := range sids {
		if s != user {
			others = append(others, s)
		}
	}
	if len(others) == 0 && owner == user {
		t.Fatalf("the file is already private (owner %s, %v), so there is nothing to fix", owner, sids)
	}
	_, loadErr := LoadKeyFile(path)
	cmds := fixCommands(path, user, owner != user, others)
	if !strings.Contains(fault.Message(loadErr), renderCommands(cmds)) {
		t.Fatalf("the refusal %q does not suggest %q", fault.Message(loadErr), renderCommands(cmds))
	}
	for _, args := range cmds {
		if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own file.
			t.Fatalf("icacls %v: %v\n%s", args, err, out)
		}
	}
	out, _ := icacls(path)
	t.Logf("icacls %s after the fix:\n%s", filepath.Base(path), out)
	if _, err := LoadKeyFile(path); err != nil {
		t.Fatalf("LoadKeyFile after the suggested commands: %v", err)
	}
}

// TestLoadRefusesOtherOwner guards that a key file whose access list names
// only the current user, but whose owner is someone else, is refused: an
// owner may always rewrite the access list. The test gives the file to the
// Administrators group with icacls, which needs an elevated process; it
// skips, saying so, when the process is not elevated.
func TestLoadRefusesOtherOwner(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	writePrivate(t, path, []byte(keyLine(1, 1)))
	const administrators = "S-1-5-32-544"
	if out, err := icacls(path, "/setowner", "*"+administrators); err != nil {
		t.Skipf("icacls /setowner needs an elevated process: %v\n%s", err, out)
	}
	owner, sids := readSecurity(t, path)
	t.Logf("owner %s, granted to %v", owner, sids)
	if owner != administrators {
		t.Fatalf("the owner is %s after /setowner, want %s", owner, administrators)
	}
	_, err := LoadKeyFile(path)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "is owned by "+administrators) ||
		!strings.Contains(fault.Message(err), "/setowner") {
		t.Fatalf("a key file owned by Administrators: %v, want Invalid naming the owner and the fix", err)
	}
	t.Logf("refusal: %s", fault.Message(err))
}
