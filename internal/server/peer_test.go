//go:build linux || darwin

package server

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestPeerCheckReadsTheKernel guards the peer check's refusal path on the
// systems that read peer credentials (SO_PEERCRED on Linux,
// LOCAL_PEERCRED on macOS), with no second user: the server is told its own
// user is another uid, so this test's connection, made as the real user, is
// a stranger. A peer function that reads the kernel returns the real uid
// and the connection is closed unanswered; one that returned the server's
// user without asking the kernel would match the injected uid and let the
// connection through, which fails this test. A first connection, read
// before the listener is told the other uid, shows the kernel's answer is
// this process's uid, so the refusal is not a broken socket.
func TestPeerCheckReadsTheKernel(t *testing.T) {
	dir, err := os.MkdirTemp("", "mgp")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	real := strconv.Itoa(os.Geteuid())
	got, err := peerOf(t, ln, path)
	if err != nil || got != real {
		t.Fatalf("the kernel's peer uid is %q (%v), want this process's %s", got, err, real)
	}

	pl := &peerListener{Listener: ln, self: "4242424242", peerUser: peerUser, log: slog.New(slog.DiscardHandler)}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := pl.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a peer who is not the expected user was not closed: %v", err)
	}
	select {
	case ac := <-accepted:
		_ = ac.Close()
		t.Fatal("a peer who is not the expected user was accepted")
	case <-time.After(200 * time.Millisecond):
	}
	_ = ln.Close()
}

// peerOf accepts one connection on ln, made by this process to path, and
// returns what peerUser reads from it.
func peerOf(t *testing.T, ln net.Listener, path string) (string, error) {
	t.Helper()
	done := make(chan struct{})
	var user string
	var perr error
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			perr = err
			return
		}
		defer func() { _ = c.Close() }()
		user, perr = peerUser(c)
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	_ = c.Close()
	return user, perr
}
