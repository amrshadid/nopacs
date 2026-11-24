package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInterruptCancelsInit guards that Ctrl-C reaches init as a canceled
// context, not as a kill: init undoes what it made and fails with one line.
// Without it the process dies on the signal with the half-made data directory
// left behind and no exit code of the program's own. It builds the real
// binary and points init at a database address whose server accepts the
// connection and never answers, so init is certainly in flight when the
// signal arrives.
func TestInterruptCancelsInit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped with -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a process cannot be sent an interrupt on Windows")
	}
	bin := filepath.Join(t.TempDir(), "nopacs-server")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// A server that accepts and says nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // a test listener.
	accepted := make(chan struct{}, 1)
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	defer func() {
		_ = ln.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	}()

	dir := filepath.Join(t.TempDir(), "data")
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "init", "--data-dir", dir, "--master-env", "NOPACS_TEST_SILENT_DB")
	cmd.Env = append(os.Environ(), "NOPACS_TEST_SILENT_DB=postgres://u:p@"+ln.Addr().String()+"/db?sslmode=disable")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("init never connected to the database address")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-waited //nolint:errcheck // the wait error of a killed process.
		t.Fatal("init did not stop 30 seconds after an interrupt")
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitFailed {
		t.Fatalf("init after an interrupt: %v, want exit code %d; standard error %q", err, exitFailed, stderr.String())
	}
	if n := strings.Count(strings.TrimRight(stderr.String(), "\r\n"), "\n"); n != 0 || !strings.HasPrefix(stderr.String(), "nopacs-server: ") {
		t.Errorf("standard error %q, want one line starting with the program's name", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("standard output %q, want nothing", stdout.String())
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data directory is left after an interrupted init: %v", err)
	}
}
