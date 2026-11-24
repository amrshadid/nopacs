//go:build interrupt

package interrupt

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// child runs this test binary again with Env set to step, running only the
// test named run, and returns its exit status and output.
func child(t *testing.T, run, step string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+run+"$", "-test.count=1") //nolint:gosec // G204: this test binary itself, running one of its own tests.
	cmd.Env = append(os.Environ(), Env+"="+step)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("the child did not run: %v", err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// TestStopsAtStep guards the kill itself: a process told a step goes past
// every other step and exits with Status at that one, after naming it on
// standard error, so a harness can tell the kill from any other end.
func TestStopsAtStep(t *testing.T) {
	if target != "" {
		At("correction", "replace", "object")
		At("rebuild", "replace", "activated")
		At("correction", "replace", "meta")
		os.Exit(0) // Never reached when At stops the process at the step.
	}
	code, out := child(t, "TestStopsAtStep", "correction:replace:meta")
	if code != Status || !strings.Contains(out, "interrupt: stopped at correction:replace:meta") {
		t.Fatalf("exit %d, output %q; want exit %d naming the step", code, out, Status)
	}
}

// TestRefusesUnknownStep guards against a misspelled step letting a run go
// on with no kill: the process ends at its start, not with Status.
func TestRefusesUnknownStep(t *testing.T) {
	if target != "" {
		t.Fatal("a process told an unknown step started")
	}
	code, out := child(t, "TestRefusesUnknownStep", "correction:replace:metta")
	if code != 2 || !strings.Contains(out, "names no step this build knows") {
		t.Fatalf("exit %d, output %q; want exit 2 refusing the name", code, out)
	}
}

// TestStepNames guards the table's shape: every name is scope:kind:step
// with a known scope and kind, and says what is durable there.
func TestStepNames(t *testing.T) {
	kinds := map[string][]string{"correction": {"delete", "relabel", "replace"}, "rebuild": {"compare", "replace"}}
	for name, durable := range steps {
		parts := strings.Split(name, ":")
		if len(parts) != 3 || parts[2] == "" || durable == "" {
			t.Errorf("step %q (%q) is not scope:kind:step with what is durable", name, durable)
			continue
		}
		known := false
		for _, k := range kinds[parts[0]] {
			known = known || k == parts[1]
		}
		if !known {
			t.Errorf("step %q names an unknown scope or kind", name)
		}
	}
}
