package cli_test

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/tui"
)

// openRun is what one run of the command line did about the interactive
// program.
type openRun struct {
	result
	opened  int
	options tui.Options
}

// runInteractive runs the command line with a terminal that is, or is not,
// there, and an interactive program that only records that it was opened.
func runInteractive(env map[string]string, terminal bool, ended error, args ...string) openRun {
	var out openRun
	var stdout, stderr bytes.Buffer
	out.code = cli.Run(context.Background(), args, cli.Env{
		Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
		Getenv: func(k string) string { return env[k] },
		Now:    func() time.Time { return fixedNow }, Location: zone,
		Terminal: func(io.Reader, io.Writer) bool { return terminal },
		Interactive: func(_ context.Context, o tui.Options) error {
			out.opened++
			out.options = o
			return ended
		},
	})
	out.stdout, out.stderr = stdout.String(), stderr.String()
	return out
}

// TestInteractiveOpensOnlyOnATerminalWithNoArguments guards the rule for
// opening the interactive program: it opens when there are no arguments and
// input and output are terminals, using the server the flags and environment
// name, and in no other case. Every other case keeps the usage line and exit 2,
// or the command's own behavior, exactly.
func TestInteractiveOpensOnlyOnATerminalWithNoArguments(t *testing.T) {
	r := newRig(t)
	const usage = "nopacs: no command (usage: nopacs <noun> <verb> [flags], for example nopacs tenant list); try \"nopacs --help\"\n"

	t.Run("terminal and no arguments opens it", func(t *testing.T) {
		got := runInteractive(r.env, true, nil)
		if got.opened != 1 || got.code != 0 || got.stdout != "" || got.stderr != "" {
			t.Fatalf("%+v", got)
		}
		if want := client.SocketPath(r.env[client.EnvDataDir]); got.options.Where != want || got.options.Client == nil {
			t.Errorf("the program was given %q and a client %v, want the socket %q", got.options.Where, got.options.Client, want)
		}
	})
	t.Run("the flags that find the server are not arguments", func(t *testing.T) {
		got := runInteractive(nil, true, nil, "--data-dir", r.env[client.EnvDataDir])
		if got.opened != 1 || got.code != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("no terminal gives the usage line and exit 2", func(t *testing.T) {
		got := runInteractive(r.env, false, nil)
		if got.opened != 0 || got.code != 2 || got.stdout != "" || got.stderr != usage {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("an argument never opens it", func(t *testing.T) {
		for _, args := range [][]string{{"nonsense"}, {"tenant"}, {"tenant", "list"}, {"version"}, {"--version"}, {"--help"}} {
			got := runInteractive(r.env, true, nil, args...)
			if got.opened != 0 {
				t.Errorf("%v opened the interactive program", args)
			}
		}
		got := runInteractive(r.env, true, nil, "nonsense")
		if got.code != 2 || !strings.Contains(got.stderr, `unknown command "nonsense"`) {
			t.Errorf("%+v", got)
		}
	})
	t.Run("without a server named it is a usage error and does not open", func(t *testing.T) {
		got := runInteractive(nil, true, nil)
		if got.opened != 0 || got.code != 2 || !strings.Contains(got.stderr, "do not know where the server is") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("without streams that are terminals it does not open", func(t *testing.T) {
		// No Terminal override: the real test, on a buffer and a reader that
		// are not files, says no.
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), nil, cli.Env{
			Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
			Getenv: func(k string) string { return r.env[k] },
			Interactive: func(context.Context, tui.Options) error {
				t.Error("opened on streams that are not terminals")
				return nil
			},
		})
		if code != 2 || stderr.String() != usage {
			t.Errorf("code %d, stderr %q", code, stderr.String())
		}
	})
}

// TestInteractiveEndsAsTheProgramDid guards what the command line makes of how
// the program ended: a signal is the conventional status 130 with nothing
// printed, and a failure is exit 1 with its one line.
func TestInteractiveEndsAsTheProgramDid(t *testing.T) {
	r := newRig(t)
	got := runInteractive(r.env, true, tui.ErrInterrupted)
	if got.code != 130 || got.stderr != "" || got.stdout != "" {
		t.Errorf("an interrupt: %+v", got)
	}
	got = runInteractive(r.env, true, io.ErrUnexpectedEOF)
	if got.code != 1 || !strings.HasPrefix(got.stderr, "nopacs: ") || strings.Count(got.stderr, "\n") != 1 {
		t.Errorf("a failure: %+v", got)
	}
}

// TestInteractiveColorFollowsTheCommands guards that the program is given color
// by the rule the commands use: not with NO_COLOR set, not with a dumb
// terminal.
func TestInteractiveColorFollowsTheCommands(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"TERM": "xterm-256color"}, true},
		{map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, false},
		{map[string]string{"TERM": "dumb"}, false},
	}
	for _, c := range cases {
		env := map[string]string{client.EnvDataDir: r.env[client.EnvDataDir]}
		for k, v := range c.env {
			env[k] = v
		}
		got := runInteractive(env, true, nil)
		want := c.want && runtime.GOOS != "windows" // a Windows console never gets escape codes
		if got.options.Color != cli.ColorAllowed(true, func(k string) string { return env[k] }, runtime.GOOS) || got.options.Color != want {
			t.Errorf("%v: Color = %v, want %v", c.env, got.options.Color, want)
		}
	}
}
