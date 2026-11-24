//go:build unix

package secret

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCreatedKeyFileMode guards that a new key file gives nobody but its
// owner any access: mode 0600 exactly.
func TestCreatedKeyFileMode(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	if err := CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode %04o, want 0600", got)
	}
}

// TestLoadRefusesOpenModes guards that a key file the group or other users
// may read or write is refused, with the chmod that fixes it, and that a
// file only its owner may open loads.
func TestLoadRefusesOpenModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620, 0o602, 0o660, 0o666, 0o610, 0o601} {
		path := filepath.Join(privateDir(t), "nopacs.key")
		if err := os.WriteFile(path, []byte(keyLine(1, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		// Chmod after the write, because the umask would remove bits from
		// the mode given to WriteFile.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(path)
		if err == nil {
			t.Errorf("mode %04o: loaded, want refused", mode)
			continue
		}
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("mode %04o: kind %v, want Invalid", mode, fault.KindOf(err))
		}
		if msg := fault.Message(err); !strings.Contains(msg, "chmod 600 "+path) {
			t.Errorf("mode %04o: message %q does not say how to fix it", mode, msg)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		path := filepath.Join(privateDir(t), "nopacs.key")
		if err := os.WriteFile(path, []byte(keyLine(1, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyFile(path); err != nil {
			t.Errorf("mode %04o: %v, want loaded", mode, err)
		}
	}
}

// TestLoadRefusesDirectory guards that a directory at the key file's path is
// refused as not a regular file, rather than read as an empty key file.
func TestLoadRefusesDirectory(t *testing.T) {
	dir := filepath.Join(privateDir(t), "nopacs.key")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(dir); fault.KindOf(err) != fault.Invalid {
		t.Errorf("LoadKeyFile of a directory: %v, want Invalid", err)
	}
}

// asOtherUID returns a file system on which the package believes it runs
// as a uid one above the real effective uid, and both uids. A test cannot
// give a file away without being root, so it changes who "we" are instead.
func asOtherUID() (fsys fileSystem, real, other int) {
	real = os.Geteuid()
	return fileSystem{uid: func() int { return real + 1 }}, real, real + 1
}

// TestLoadRefusesOtherOwner guards that a key file belonging to another user
// is refused, whatever its mode, naming the owner by uid and the chown that
// fixes it.
func TestLoadRefusesOtherOwner(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	writePrivate(t, path, []byte(keyLine(1, 1)))
	f, err := openKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	asOther, real, other := asOtherUID()

	// checkPrivate directly, because LoadKeyFile checks the directory first,
	// and the directory is not "ours" either now.
	err = asOther.checkPrivate(f, path)
	if fault.KindOf(err) != fault.Invalid {
		t.Fatalf("a key file owned by another uid: %v, want Invalid", err)
	}
	msg := fault.Message(err)
	want := "key file " + path + " is owned by uid " + strconv.Itoa(real) + ", not by uid " + strconv.Itoa(other)
	if !strings.Contains(msg, want) || !strings.Contains(msg, "chown "+strconv.Itoa(other)+" "+path) {
		t.Errorf("message %q does not name the owner and the fix", msg)
	}
	if _, err := asOther.loadKeyFile(path); fault.KindOf(err) != fault.Invalid {
		t.Errorf("LoadKeyFile as another uid: %v, want Invalid", err)
	}
}

// TestLoadRefusesFIFO guards that a FIFO at the key file's path is refused at
// once as not a regular file. Opened for reading without O_NONBLOCK, a FIFO
// blocks until a writer appears, which would hang the server at start.
func TestLoadRefusesFIFO(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadKeyFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "not a regular file") {
			t.Errorf("LoadKeyFile of a FIFO: %v, want Invalid, not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		// Open the FIFO for writing so the stuck open returns and the test
		// binary can exit.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("LoadKeyFile of a FIFO was still blocked after 5 seconds")
	}
}

// TestDirectoryMustBePrivate guards that a key file is neither loaded from
// nor created in a directory that other users may change (group- or
// other-writable, including a sticky directory such as /tmp) or that belongs
// to another user, with the command that fixes it; and that a directory
// writable by its owner alone is accepted, whoever may read it.
func TestDirectoryMustBePrivate(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o730, 0o707, 0o703, 0o777, 0o1777} {
		dir := privateDir(t)
		path := filepath.Join(dir, "nopacs.key")
		writePrivate(t, path, []byte(keyLine(1, 1)))
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(path)
		if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "chmod go-w "+dir) {
			t.Errorf("load from a directory of mode %04o: %v, want Invalid with the fix", mode, err)
		}
		newPath := filepath.Join(dir, "new.key")
		if err := CreateKeyFile(newPath); fault.KindOf(err) != fault.Invalid {
			t.Errorf("create in a directory of mode %04o: %v, want Invalid", mode, err)
		}
		if _, err := os.Lstat(newPath); err == nil {
			t.Errorf("a key file was created in a directory of mode %04o", mode)
		}
		_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs its execute bit to be entered.
	}
	for _, mode := range []os.FileMode{0o700, 0o755, 0o711, 0o750} {
		dir := privateDir(t)
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "nopacs.key")
		if err := CreateKeyFile(path); err != nil {
			t.Errorf("create in a directory of mode %04o: %v", mode, err)
			continue
		}
		if _, err := LoadKeyFile(path); err != nil {
			t.Errorf("load from a directory of mode %04o: %v", mode, err)
		}
	}

	dir := privateDir(t)
	asOther, real, _ := asOtherUID()
	err := asOther.createKeyFile(filepath.Join(dir, "nopacs.key"))
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "directory "+dir+" is owned by uid "+strconv.Itoa(real)) {
		t.Errorf("create in a directory owned by another uid: %v, want Invalid naming the owner", err)
	}
}

// TestLoadMissingDirectory guards that a key file path in a directory that
// does not exist is NotFound, like a missing file.
func TestLoadMissingDirectory(t *testing.T) {
	path := filepath.Join(privateDir(t), "absent", "nopacs.key")
	if _, err := LoadKeyFile(path); fault.KindOf(err) != fault.NotFound {
		t.Errorf("LoadKeyFile in a missing directory: %v, want NotFound", err)
	}
}

// TestCreatePrivateTempIsPrivate guards that a temporary file made for a
// private credential has mode 0600 from creation, passes the same check as a
// key file, and that its directory is checked for other users' access.
func TestCreatePrivateTempIsPrivate(t *testing.T) {
	dir := privateDir(t)
	f, err := CreatePrivateTemp(dir, ".x-*")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	_ = f.Close()
	if !strings.HasPrefix(filepath.Base(name), ".x-") {
		t.Errorf("name %q does not follow the pattern", name)
	}
	if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v; want 0600", info.Mode().Perm(), err)
	}
	g, err := OpenPrivate(name)
	if err != nil {
		t.Fatalf("OpenPrivate refused the file: %v", err)
	}
	_ = g.Close()
}

// TestCheckPrivateDir guards the directory rule: other users writing is
// refused, other users reading only with noRead, a missing one is NotFound.
func TestCheckPrivateDir(t *testing.T) {
	dir := privateDir(t)
	if err := CheckPrivateDir(dir, true); err != nil {
		t.Fatalf("a private directory: %v", err)
	}
	for _, c := range []struct {
		mode    os.FileMode
		noRead  bool
		refused bool
	}{{0o755, false, false}, {0o755, true, true}, {0o770, false, true}, {0o702, false, true}, {0o750, true, true}} {
		if err := os.Chmod(dir, c.mode); err != nil { //nolint:gosec // G302: a test of the refusal of an open directory.
			t.Fatal(err)
		}
		if err := CheckPrivateDir(dir, c.noRead); (err != nil) != c.refused {
			t.Errorf("mode %04o noRead %v: %v, refused want %v", c.mode, c.noRead, err, c.refused)
		}
	}
	_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: put back.
	if err := CheckPrivateDir(filepath.Join(dir, "none"), false); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a missing directory: %v", err)
	}
}
