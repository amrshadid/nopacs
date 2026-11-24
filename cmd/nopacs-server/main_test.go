package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Exit codes of the program, as the documentation fixes them.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// versionLine is the shape of what the version command prints: the version,
// the commit and the Go release, on one line, the same line the command line
// prints.
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

// TestVersionJSON guards "version --json": the same document the command line's
// version --json prints (schemaVersion, version, commit, goVersion), as one
// JSON object on standard output and nothing on standard error.
func TestVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"version", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code %d, want %d; standard error %q", code, exitOK, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("standard output %q is not JSON: %v", stdout.String(), err)
	}
	for _, k := range []string{"version", "commit", "goVersion"} {
		if s, _ := doc[k].(string); s == "" {
			t.Errorf("version --json has no %s: %v", k, doc)
		}
	}
	if _, ok := doc["schemaVersion"]; !ok || len(doc) != 4 {
		t.Errorf("version --json is %v, want schemaVersion, version, commit and goVersion", doc)
	}
	if stderr.Len() != 0 {
		t.Errorf("standard error %q, want nothing", stderr.String())
	}
}

// TestAnythingElseIsAUsageError guards the usage error: an unknown word, no
// word, or extra words print one line to standard error naming what was wrong,
// nothing to standard output, and exit 2, so a script can tell a mistake in
// its arguments from a failure. A command of the command line, typed here, is
// an unknown word.
func TestAnythingElseIsAUsageError(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"unknown word":     {[]string{"nope"}, `nopacs-server: unknown command "nope" for "nopacs-server"; try "nopacs-server --help"` + "\n"},
		"a client command": {[]string{"whoami"}, `nopacs-server: unknown command "whoami" for "nopacs-server"; try "nopacs-server --help"` + "\n"},
		"no word":          {nil, `nopacs-server: no command (usage: nopacs-server <command> [flags], for example nopacs-server serve --data-dir DIR); try "nopacs-server --help"` + "\n"},
		"extra words":      {[]string{"version", "now"}, `nopacs-server: unknown command "now" for "nopacs-server version"; try "nopacs-server version --help"` + "\n"},
		"terminal codes":   {[]string{"\x1b[2J"}, `nopacs-server: unknown command "\x1b[2J" for "nopacs-server"; try "nopacs-server --help"` + "\n"},
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

// TestHelpListsTheServerCommands guards the list of commands of the program:
// exactly the ones that belong to the server host, so a command added here by
// accident, or lost, shows.
func TestHelpListsTheServerCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"--help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("--help: exit %d: %s", code, stderr.String())
	}
	var got []string
	inCommands := false
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "Available Commands:"):
			inCommands = true
		case inCommands && strings.HasPrefix(line, "  "):
			got = append(got, strings.Fields(line)[0])
		case inCommands:
			inCommands = false
		}
	}
	want := []string{"backup", "completion", "help", "init", "serve", "tenant", "version"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("--help lists %v, want %v", got, want)
	}
}

// TestTestSupportIsNotLinked guards the rule that test support never reaches
// the real binary: the test kit is made of ordinary packages, so nothing but
// this check stops a non-test file from importing one. It asks the go command
// for every package the binary is built from, tests excluded, and fails if one
// of the kit's, or the testing package it brings in, is among them.
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

// TestBuiltBinary guards what run cannot show: that main passes run's code to
// the operating system, and that -ldflags reaches the variables under the
// package path written in the documentation and the Makefile. It builds the
// real binary and runs it.
func TestBuiltBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped with -short")
	}
	bin := filepath.Join(t.TempDir(), "nopacs-server")
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
		t.Fatalf("nopacs-server version: %v; standard error %q", err, stderr.String())
	}
	want := "nopacs v9.9.9-test (commit abcdef123456, " + runtime.Version() + ")\n"
	if got := strings.ReplaceAll(stdout.String(), "\r\n", "\n"); got != want {
		t.Errorf("nopacs-server version printed %q, want %q", got, want)
	}

	// An unknown command exits 2 with one line, and serve on a directory
	// with no installation exits 4 with one line: the codes cross the
	// process boundary as the table says.
	for name, c := range map[string]struct {
		args []string
		exit int
		line string
	}{
		"usage":  {[]string{"nope"}, exitUsage, `nopacs-server: unknown command "nope" for "nopacs-server"; try "nopacs-server --help"`},
		"failed": {[]string{"serve", "--data-dir", t.TempDir()}, 4, "nopacs-server: no installation in "},
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
