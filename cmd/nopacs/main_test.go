package main

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Exit codes of the command line, as the documentation fixes them.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// versionLine is the shape of what the version command prints: the version,
// the commit and the Go release, on one line.
var versionLine = regexp.MustCompile(`^nopacs \S+ \(commit \S+, \S*go1\S*\)\n$`)

// TestVersionPrintsOneLine guards both spellings of the version command: each
// prints the version line to standard output, nothing to standard error, and
// exits 0.
func TestVersionPrintsOneLine(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{arg}, &stdout, &stderr)
		if code != exitOK {
			t.Errorf("%s: exit code %d, want %d", arg, code, exitOK)
		}
		if !versionLine.MatchString(stdout.String()) {
			t.Errorf("%s: standard output %q is not a version line", arg, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("%s: standard error %q, want nothing", arg, stderr.String())
		}
	}
}

// TestAnythingElseIsAUsageError guards the usage error: an unknown word, no
// word, or extra words print one line to standard error naming what was wrong,
// nothing to standard output, and exit 2, so a script can tell a mistake in
// its arguments from a failure.
func TestAnythingElseIsAUsageError(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"unknown word":   {[]string{"nope"}, `nopacs: unknown command "nope" for "nopacs"; try "nopacs --help"` + "\n"},
		"no word":        {nil, `nopacs: no command (usage: nopacs <noun> <verb> [flags], for example nopacs tenant list); try "nopacs --help"` + "\n"},
		"extra words":    {[]string{"version", "now"}, `nopacs: unknown command "now" for "nopacs version"; try "nopacs version --help"` + "\n"},
		"terminal codes": {[]string{"\x1b[2J"}, `nopacs: unknown command "\x1b[2J" for "nopacs"; try "nopacs --help"` + "\n"},
	}
	for name, c := range cases {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), c.args, &stdout, &stderr)
		if code != exitUsage {
			t.Errorf("%s: exit code %d, want %d", name, code, exitUsage)
		}
		if stderr.String() != c.want {
			t.Errorf("%s: standard error %q, want %q", name, stderr.String(), c.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: standard output %q, want nothing", name, stdout.String())
		}
	}
}

// failingWriter is a standard output that cannot be written, as a full disk.
type failingWriter struct{}

// Write fails every time.
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// TestVersionFailsWhenOutputFails guards the exit code when the version cannot
// be written: a script reading the output must see a failure, not success
// with nothing printed.
func TestVersionFailsWhenOutputFails(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"version"}, failingWriter{}, &stderr); code != exitFailed {
		t.Errorf("exit code %d, want %d", code, exitFailed)
	}
	if !strings.Contains(stderr.String(), "no space left on device") {
		t.Errorf("standard error %q, want the write error", stderr.String())
	}
}

// TestTestSupportIsNotLinked guards the rule that test support never reaches
// the real binary: the test kit (the fake server among it) is made of ordinary
// packages, so nothing but this check stops a non-test file from importing
// one. It asks the go command for every package the binary is built from,
// tests excluded, and fails if one of the kit's, or the testing package it
// brings in, is among them.
func TestTestSupportIsNotLinked(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 20 {
		t.Fatalf("go list named only %d packages; the check is not looking at the binary", len(deps))
	}
	for _, d := range deps {
		if strings.Contains(d, "/internal/testkit") || d == "testing" {
			t.Errorf("the binary is built from %s, which is test support", d)
		}
	}
}

// serverSide are the packages the command line must never be built from: the
// database and storage drivers, the registry that opens tenants, the server and
// the commands that run it. The command line is a client of the API, so it can
// sit on a machine that must never hold the server, its drivers or its data;
// the same list is fenced by path in the lint configuration.
var serverSide = []string{
	"/internal/driver",
	"/internal/blob/dir",
	"/internal/blob/s3",
	"/internal/registry",
	"/internal/server",
	"/internal/servecmd",
}

// TestNoServerCodeIsLinked guards that nothing the command line imports, at
// any depth, is server code: it asks the go command for every package the
// binary is built from and fails on any of serverSide. The fence in the lint
// configuration sees direct imports; this sees all of them.
func TestNoServerCodeIsLinked(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 20 {
		t.Fatalf("go list named only %d packages; the check is not looking at the binary", len(deps))
	}
	for _, d := range deps {
		for _, banned := range serverSide {
			if strings.Contains(d, banned) {
				t.Errorf("the command line is built from %s, which is server code", d)
			}
		}
	}
}

// TestMovedCommandsNameTheServerProgram guards the answer to a command that
// now belongs to nopacs-server: one line on standard error that names the
// command to run there, nothing on standard output, and exit 2, whatever
// arguments and flags followed, so nobody is left guessing.
func TestMovedCommandsNameTheServerProgram(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"init":           {[]string{"init", "--data-dir", "x"}, "nopacs: init: is not a nopacs command; run nopacs-server init\n"},
		"serve":          {[]string{"serve"}, "nopacs: serve: is not a nopacs command; run nopacs-server serve\n"},
		"backup":         {[]string{"backup"}, "nopacs: backup: is not a nopacs command; run nopacs-server backup\n"},
		"backup create":  {[]string{"backup", "create", "--out", "f"}, "nopacs: backup: is not a nopacs command; run nopacs-server backup create\n"},
		"backup restore": {[]string{"backup", "restore", "f"}, "nopacs: backup: is not a nopacs command; run nopacs-server backup restore\n"},
		"tenant import":  {[]string{"tenant", "import", "f", "--transfer-key", "k"}, "nopacs: tenant import: is not a nopacs command; run nopacs-server tenant import\n"},
		"flags first":    {[]string{"backup", "--help-me"}, "nopacs: backup: is not a nopacs command; run nopacs-server backup\n"},
	}
	for name, c := range cases {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), c.args, &stdout, &stderr)
		if code != exitUsage || stderr.String() != c.want || stdout.Len() != 0 {
			t.Errorf("%s: exit %d, standard error %q, standard output %q; want exit %d and %q", name, code, stderr.String(), stdout.String(), exitUsage, c.want)
		}
	}
}

// TestBuiltBinary guards what run cannot show: that main passes run's code to
// the operating system, and that -ldflags reaches the variables under the
// package path written in the documentation and the Makefile. It builds the
// real binary and runs it.
func TestBuiltBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped with -short")
	}
	bin := filepath.Join(t.TempDir(), "nopacs")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	const pkg = "github.com/amrshadid/nopacs/internal/version"
	build := exec.Command("go", "build", "-o", bin,
		"-ldflags", "-X "+pkg+".Version=v9.9.9-test -X "+pkg+".Commit=abcdef123456", ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "version")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("nopacs version: %v; standard error %q", err, stderr.String())
	}
	want := "nopacs v9.9.9-test (commit abcdef123456, " + runtime.Version() + ")\n"
	if got := strings.ReplaceAll(stdout.String(), "\r\n", "\n"); got != want {
		t.Errorf("nopacs version printed %q, want %q", got, want)
	}

	// An unknown command exits 2 with one line, and a command that cannot
	// reach a server exits 1 with one line: the codes cross the process
	// boundary as the table says.
	for name, c := range map[string]struct {
		args []string
		exit int
		line string
	}{
		"usage":  {[]string{"nope"}, exitUsage, `nopacs: unknown command "nope" for "nopacs"; try "nopacs --help"`},
		"failed": {[]string{"--data-dir", t.TempDir(), "whoami"}, exitFailed, "nopacs: whoami: cannot reach the server at "},
	} {
		stdout.Reset()
		stderr.Reset()
		cmd = exec.Command(bin, c.args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != c.exit {
			t.Fatalf("%s: %v, want exit code %d", name, err, c.exit)
		}
		if got := strings.TrimRight(stderr.String(), "\r\n"); !strings.HasPrefix(got, c.line) || strings.Count(stderr.String(), "\n") != 1 {
			t.Errorf("%s wrote %q to standard error, want one line starting %q", name, stderr.String(), c.line)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s wrote %q to standard output, want nothing", name, stdout.String())
		}
	}
}
