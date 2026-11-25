//go:build clock

package clock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// near reports whether got is want from the system's time, within a second.
func near(got time.Time, want time.Duration) bool {
	d := time.Until(got) - want
	return d < time.Second && d > -time.Second
}

// TestParse guards the forms of an offset: hours and minutes as Go reads
// them, days with a "d", a negative one, and a refusal of the rest.
func TestParse(t *testing.T) {
	for text, want := range map[string]time.Duration{"36h": 36 * time.Hour, "-90m": -90 * time.Minute, "400d": 400 * 24 * time.Hour, "0.5d": 12 * time.Hour, " 0 ": 0, "34560000s": 400 * 24 * time.Hour} {
		got, err := parse(text)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "soon", "d", "99999d", "12"} {
		if _, err := parse(text); err == nil {
			t.Errorf("parse(%q) was accepted", text)
		}
	}
}

// TestFixedOffset guards the variable: a fixed offset moves Now by that much.
func TestFixedOffset(t *testing.T) {
	s := load("30d", "")
	if !near(s.now(), 30*24*time.Hour) {
		t.Fatalf("a fixed offset of 30d gave %v", time.Until(s.now()))
	}
}

// TestFileMoves guards the file: a missing file is the fixed offset, a file
// written later moves a running source after its interval, and the file wins
// over the variable.
func TestFileMoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offset")
	s := load("1h", path)
	s.every = time.Millisecond
	if !near(s.now(), time.Hour) {
		t.Fatalf("with no file the offset is %v, want the variable's 1h", time.Until(s.now()))
	}
	if err := os.WriteFile(path, []byte("400d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if !near(s.now(), 400*24*time.Hour) {
		t.Fatalf("with a file of 400d the offset is %v", time.Until(s.now()))
	}
	if err := os.WriteFile(path, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if !near(s.now(), 0) {
		t.Fatalf("with a file of 0 the offset is %v", time.Until(s.now()))
	}
}

// TestFileIsKept guards the read interval: a file rewritten inside the
// interval is not read again, so a hot path does not read a file per call.
func TestFileIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offset")
	if err := os.WriteFile(path, []byte("2h"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := load("", path)
	s.every = time.Hour
	_ = s.now()
	if err := os.WriteFile(path, []byte("9h"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !near(s.now(), 2*time.Hour) {
		t.Fatalf("the file was read again inside its interval: %v", time.Until(s.now()))
	}
}

// child runs this test binary again with the environment given, running only
// the test named run, and returns its exit status and output.
func child(t *testing.T, run string, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+run+"$", "-test.count=1") //nolint:gosec // G204: this test binary itself, running one of its own tests.
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("the child did not run: %v", err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// TestEnvAtStart guards the process's own source: a process started with an
// offset in the environment has it in Now, and one with an offset it cannot
// read ends with status 2 and says so.
func TestEnvAtStart(t *testing.T) {
	if os.Getenv("NOPACS_CLOCK_CHILD") != "" {
		if !near(Now(), 48*time.Hour) {
			t.Fatalf("Now is %v from the system's time, want 48h", time.Until(Now()))
		}
		return
	}
	if code, out := child(t, "TestEnvAtStart", "NOPACS_CLOCK_CHILD=1", EnvOffset+"=2d"); code != 0 {
		t.Fatalf("the child with 2d ended %d: %s", code, out)
	}
	code, out := child(t, "TestEnvAtStart", EnvOffset+"=tomorrow")
	if code != 2 || !strings.Contains(out, "clock: "+EnvOffset) {
		t.Fatalf("the child with a bad offset ended %d: %s", code, out)
	}
}
