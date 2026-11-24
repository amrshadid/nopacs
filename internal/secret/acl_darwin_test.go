//go:build darwin

package secret

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// chmodACL runs /bin/chmod with args, the system's own way to edit an access
// list, and fails the test if it fails.
func chmodACL(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", args...).CombinedOutput(); err != nil {
		t.Fatalf("chmod %v: %v\n%s", args, err, out)
	}
}

// lsACL returns what ls -le shows for path, for the test log.
func lsACL(path string) string {
	out, _ := exec.Command("/bin/ls", "-led", path).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// TestLoadRefusesPermitACL guards that a key file of mode 0600 with an access
// list entry granting read to everyone, invisible in the mode bits, is
// refused with the command that removes the list; and that an entry which
// only denies is accepted.
func TestLoadRefusesPermitACL(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "nopacs.key")
	writePrivate(t, path, []byte(keyLine(1, 1)))
	chmodACL(t, "+a", "everyone allow read", path)
	t.Logf("%s", lsACL(path))
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o, want 0600: the test needs the access list to be the only way in", info.Mode().Perm())
	}
	_, err := LoadKeyFile(path)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "chmod -N "+path) {
		t.Fatalf("a key file readable by everyone through its access list: %v, want Invalid with the fix", err)
	}
	t.Logf("refusal: %s", fault.Message(err))

	chmodACL(t, "-N", path)
	if _, err := LoadKeyFile(path); err != nil {
		t.Fatalf("after chmod -N: %v", err)
	}

	chmodACL(t, "+a", "everyone deny write", path)
	t.Logf("%s", lsACL(path))
	if _, err := LoadKeyFile(path); err != nil {
		t.Errorf("a key file whose access list only denies: %v, want loaded", err)
	}
}

// TestDirectoryRefusesPermitACL guards that a directory of mode 0700 whose
// access list lets everyone add files is refused for loading and creating.
func TestDirectoryRefusesPermitACL(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "nopacs.key")
	writePrivate(t, path, []byte(keyLine(1, 1)))
	chmodACL(t, "+a", "everyone allow add_file,delete_child", dir)
	t.Logf("%s", lsACL(dir))
	if _, err := LoadKeyFile(path); fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "chmod -N "+dir) {
		t.Errorf("load from a directory open to everyone through its access list: %v, want Invalid", err)
	}
	if err := CreateKeyFile(filepath.Join(dir, "new.key")); fault.KindOf(err) != fault.Invalid {
		t.Errorf("create in that directory: %v, want Invalid", err)
	}
}

// TestParseACLReplyMalformed guards the reader of the kernel's reply against
// a reply it does not understand: each is an error, so the file is refused,
// never passed as having no access list.
func TestParseACLReplyMalformed(t *testing.T) {
	le := binary.LittleEndian
	good := make([]byte, 100)
	le.PutUint32(good, 100)
	le.PutUint32(good[4:], attrCmnReturnedAttrs|attrCmnExtendedSecurity)
	le.PutUint32(good[24:], 8)  // the filesec starts 8 bytes after the reference
	le.PutUint32(good[28:], 68) // and is 68 bytes long: one entry
	le.PutUint32(good[32:], kauthFilesecMagic)
	le.PutUint32(good[68:], 1)                 // one entry
	le.PutUint32(good[76+16:], kauthACEPermit) // that permits
	if permit, err := parseACLReply(good); err != nil || !permit {
		t.Fatalf("the well-formed reply: %v, %v; want a permit entry", permit, err)
	}

	cases := map[string]func(b []byte) []byte{
		"short":            func(b []byte) []byte { return b[:10] },
		"length too large": func(b []byte) []byte { le.PutUint32(b, 1000); return b },
		"offset past end":  func(b []byte) []byte { le.PutUint32(b[24:], 500); return b },
		"negative offset":  func(b []byte) []byte { le.PutUint32(b[24:], 0xfffffff0); return b },
		"size past end":    func(b []byte) []byte { le.PutUint32(b[28:], 500); return b },
		"bad magic":        func(b []byte) []byte { le.PutUint32(b[32:], 0); return b },
		"too many entries": func(b []byte) []byte { le.PutUint32(b[68:], 50); return b },
	}
	for name, change := range cases {
		b := change(append([]byte(nil), good...))
		if _, err := parseACLReply(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestCheckPrivateDirRefusesPermitACL guards that a directory of mode 0700
// with an access list entry that grants access is refused, as a key file's is.
func TestCheckPrivateDirRefusesPermitACL(t *testing.T) {
	dir := privateDir(t)
	chmodACL(t, "+a", "everyone allow list,add_file,search", dir)
	t.Logf("%s", lsACL(dir))
	if err := CheckPrivateDir(dir, false); err == nil || !strings.Contains(err.Error(), "access list") {
		t.Errorf("a directory with a permitting entry: %v", err)
	}
	chmodACL(t, "-N", dir)
	if err := CheckPrivateDir(dir, false); err != nil {
		t.Errorf("after removing the list: %v", err)
	}
}
