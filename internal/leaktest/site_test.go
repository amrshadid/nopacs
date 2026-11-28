package leaktest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a buffer more than one goroutine writes to: a process's
// standard output and standard error.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns what was written.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// treeRoot returns the module root to build from: the directory named by
// NOPACS_LEAKTEST_TREE, else the checkout this package is in.
func treeRoot(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("NOPACS_LEAKTEST_TREE"); d != "" {
		return d
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

// buildBinaries builds the server and the command line from the tree into a
// new directory and returns it: the test runs what the tree builds, not a
// binary left from another run.
func buildBinaries(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/nopacs-server", "./cmd/nopacs") // #nosec G204 -- fixed arguments.
	cmd.Dir = treeRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binaries: %v\n%s", err, out)
	}
	return bin
}

// site is a real server process on a new installation: its binaries, its
// data directory, the address it listens on, the log it wrote and a client
// for its local socket, which acts as the local operator.
type site struct {
	t       *testing.T
	bin     string
	dataDir string
	roots   string
	listen  string
	log     *syncBuffer
	cmd     *exec.Cmd
	done    chan error
	local   *http.Client
	stopped bool
}

// newSite makes an installation and starts the server on it with its log at
// the most detailed level, so that whatever the server would write is in
// the log the test searches.
func newSite(t *testing.T, bin string) *site {
	t.Helper()
	parent, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	s := &site{t: t, bin: bin, dataDir: filepath.Join(parent, "d"), roots: filepath.Join(parent, "roots"), log: &syncBuffer{}}
	if out, err := exec.CommandContext(t.Context(), filepath.Join(bin, "nopacs-server"), "init", "--data-dir", s.dataDir).CombinedOutput(); err != nil { // #nosec G204 -- the test's own binary.
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(s.roots, 0o700); err != nil {
		t.Fatal(err)
	}
	s.listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	s.cmd = exec.Command(filepath.Join(bin, "nopacs-server"), "serve", "--data-dir", s.dataDir, "--listen", s.listen, "--log-level", "debug", "--drain", "20s") // #nosec G204 -- the test's own binary.
	s.cmd.Stdout, s.cmd.Stderr = s.log, s.log
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.done = make(chan error, 1)
	go func() { s.done <- s.cmd.Wait() }()
	t.Cleanup(func() {
		if !s.stopped {
			_ = s.cmd.Process.Kill()
			if err := <-s.done; err != nil {
				t.Logf("the server killed at the test's end: %v", err)
			}
		}
	})
	s.local = &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", filepath.Join(s.dataDir, "run", "nopacs.sock"))
		},
	}}
	t.Cleanup(s.local.CloseIdleConnections)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if status, _ := s.tryPost("/nopacs.v1.AccessService/WhoAmI", "{}"); status == http.StatusOK {
			return s
		}
		select {
		case err := <-s.done:
			t.Fatalf("the server exited: %v\n%s", err, s.log.String())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server did not answer:\n%s", s.log.String())
		}
	}
}

// stop ends the server with the signal that makes it drain and write the
// audit entries it holds, and waits for it.
func (s *site) stop() {
	s.t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	s.local.CloseIdleConnections()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.t.Fatal(err)
	}
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("the server ended with %v\n%s", err, s.log.String())
		}
	case <-time.After(40 * time.Second):
		_ = s.cmd.Process.Kill()
		s.t.Fatal("the server did not stop")
	}
}

// freePort returns a TCP port on loopback no one listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// tryPost sends a Connect JSON request on the local socket, as the local
// operator, and returns the status and the body.
func (s *site) tryPost(procedure, body string) (int, []byte) {
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, "http://local"+procedure, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.local.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

// post is tryPost that fails the test unless the answer is 200, and returns
// the body.
func (s *site) post(procedure, body string) []byte {
	s.t.Helper()
	status, out := s.tryPost(procedure, body)
	if status != http.StatusOK {
		s.t.Fatalf("%s: %d %s", procedure, status, out)
	}
	return out
}

// postInto is post with the answer decoded into out.
func (s *site) postInto(procedure, body string, out any) {
	s.t.Helper()
	if err := json.Unmarshal(s.post(procedure, body), out); err != nil {
		s.t.Fatalf("%s: %v", procedure, err)
	}
}

// waitFor polls cond until it holds, failing the test with what it waited
// for after d.
func (s *site) waitFor(what string, d time.Duration, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s\n%s", what, s.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
