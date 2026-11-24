package server_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/server"
)

// The environment of the other-user test.
const (
	// otherUserEnv names a second operating system user. The test runs only
	// when it is set, and needs "sudo -n -u <user>" to work without a
	// password, as it does on GitHub's Linux runners. CI runs it as:
	//
	//	sudo useradd --no-create-home --shell /usr/sbin/nologin nopacs-other
	//	NOPACS_TEST_OTHER_USER=nopacs-other go test -count=1 -v -run '^TestSocketRefusesOtherUser$' ./internal/server
	otherUserEnv = "NOPACS_TEST_OTHER_USER"
	// otherUserHelperEnv, when set, makes this test binary a helper that
	// dials the socket at that path, as whatever user started it, and says
	// what happened.
	otherUserHelperEnv = "NOPACS_OTHER_USER_DIAL"
)

// runOtherUserHelper dials the local socket named by otherUserHelperEnv,
// sends one request, and prints one word: "denied" when the system refused
// the connect, "closed" when the server closed the connection without an
// answer, or "served" with the status when it answered.
func runOtherUserHelper() int {
	path := os.Getenv(otherUserHelperEnv)
	c, err := net.DialTimeout("unix", path, 5*time.Second) //nolint:gosec // G704: the path is the test's own socket, passed by the test.
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, os.ErrPermission) {
			fmt.Println("denied")
		} else {
			fmt.Println("dial-error:", err)
		}
		return 0
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(c, "POST /nopacs.v1.AccessService/WhoAmI HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	if err != nil {
		fmt.Println("closed")
		return 0
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		fmt.Println("closed")
		return 0
	}
	fmt.Println("served", strings.TrimSpace(line))
	return 0
}

// TestSocketRefusesOtherUser guards the local socket, which makes its caller
// the local operator: another operating system user cannot use the local
// socket. It runs this test binary as that user (sudo) twice: first with the
// directory as the server keeps it, where the system refuses the connect
// ("denied"); then with the directory and the socket opened to everyone, so
// the peer check is the only guard left, and the server must close the
// connection unanswered ("closed"). It needs a second user and passwordless
// sudo, so it is skipped unless NOPACS_TEST_OTHER_USER names the user (CI
// on Linux; see otherUserEnv).
func TestSocketRefusesOtherUser(t *testing.T) {
	other := os.Getenv(otherUserEnv)
	if other == "" {
		t.Skip("set " + otherUserEnv + " to a second user's name, with passwordless sudo, to run this test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no sudo; its guard is the directory's access list")
	}
	// Under /tmp, which every user may enter, unlike a per-user TMPDIR.
	dir, err := os.MkdirTemp("/tmp", "mgo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, private to its owner.
		t.Fatal(err)
	}
	e := startedIn(t, dir)

	// The helper is this test binary, copied where the other user can run
	// it; the data directory is made enterable (not listable) for that.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "helper.test")
	b, err := os.ReadFile(self) //nolint:gosec // G304: this test's own binary.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, b, 0o755); err != nil { //nolint:gosec // G306: the other user must run it.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o711); err != nil { //nolint:gosec // G302: the other user must enter it to run the helper.
		t.Fatal(err)
	}
	sock := server.SocketPath(e.dir)
	run := func() string {
		cmd := exec.Command("sudo", "-n", "-u", other, "env", otherUserHelperEnv+"="+sock, helper) //nolint:gosec // G204: fixed program, test's own paths.
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("running the helper as %s: %v\n%s", other, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run(); got != "denied" {
		t.Fatalf("with the private directory, the other user got %q, want denied", got)
	}

	if err := os.Chmod(server.RunDir(e.dir), 0o711); err != nil { //nolint:gosec // G302: opened on purpose, so the peer check is the only guard.
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o777); err != nil { //nolint:gosec // G302: opened on purpose, so the peer check is the only guard.
		t.Fatal(err)
	}
	if got := run(); got != "closed" {
		t.Fatalf("with only the peer check, the other user got %q, want closed", got)
	}
	t.Logf("as %s: private directory -> denied; peer check alone -> closed", other)
}
