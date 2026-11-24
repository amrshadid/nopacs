package servecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/version"
)

// ProgramName is the name of the server program, in its messages and its
// usage lines.
const ProgramName = "nopacs-server"

// Env is what the server program takes from the process around it, so a
// test can run a command with its own streams and environment.
type Env struct {
	// Stdout receives what a command made; Stderr receives the server's log
	// and errors.
	Stdout, Stderr io.Writer
	// Getenv reads the environment; os.Getenv when nil.
	Getenv func(string) string
	// Services builds the API's handlers; APIServices when nil.
	Services Services
}

// runError marks an error that came from a command's own work, as opposed to
// one that cobra made while reading the arguments, which is always a usage
// mistake (exit 2).
type runError struct{ err error }

// Error returns the wrapped error's text.
func (e *runError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error.
func (e *runError) Unwrap() error { return e.err }

// Run is the whole server program: it runs the arguments (without the
// program's name) and returns the exit code, with the same codes by kind of
// failure as the command line.
func Run(ctx context.Context, args []string, env Env) int {
	root := Command(env)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	return report(cmd, env, err)
}

// Command returns the root command of the server program: init, serve,
// backup, tenant import and version, with the data directory flag they
// share. The commands are the ones that must touch the data directory with no
// server running, plus the server itself; every other command is a client of
// the API in the command line program.
func Command(env Env) *cobra.Command {
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	opts := Options{Stdout: env.Stdout, Stderr: env.Stderr, Getenv: env.Getenv, Services: env.Services}
	if opts.Services == nil {
		opts.Services = APIServices
	}
	root := &cobra.Command{
		Use:   ProgramName,
		Short: "Run a NoPACs installation, and the commands that touch its data directory while the server is stopped",
		Long: `nopacs-server runs a NoPACs installation: init makes the data directory, the
key file and the master database; serve runs the server; backup, tenant import
and the rest touch the data directory with the server stopped. Everything else
is done through the server's API with the nopacs command line.

The data directory is --data-dir, or NOPACS_DATA_DIR.

Exit codes: 0 done, 1 failed, 2 wrong usage, 3 refused, 4 not found, 5 a state
that forbids the change.`,
		Version:       version.Get().String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(*cobra.Command, []string) error {
			return fault.New(fault.Invalid, "no command (usage: "+ProgramName+" <command> [flags], for example "+ProgramName+" serve --data-dir DIR)")
		},
	}
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().String(DataDirFlag, "", "the installation's data directory (default: "+EnvDataDir+")")

	tenant := &cobra.Command{
		Use:   "tenant",
		Short: "Load a tenant another installation exported",
		Long:  "The tenant commands that run on this machine with the server stopped. The others are in the nopacs command line.",
		Args:  noArgs,
		RunE: func(*cobra.Command, []string) error {
			return fault.New(fault.Invalid, "tenant needs a verb: import")
		},
	}
	tenant.AddCommand(TenantImport(opts))
	root.AddCommand(versionCommand(env), Init(opts), Serve(opts), Backup(opts), tenant)
	own(root)
	return root
}

// versionCommand returns "version": the same line the command line prints, or
// with --json the same document.
func versionCommand(env Env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of this program",
		Long: `Print the version, the commit and the Go release this program was built with,
read from the program's own build information. It is the same line the nopacs
command line prints; --json prints the same document its version --json does.`,
		Example: `  nopacs-server version
  nopacs-server version --json`,
		Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			if asJSON {
				return output.JSONValue(env.Stdout, version.Get())
			}
			_, err := fmt.Fprintln(env.Stdout, version.Get())
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the version, the commit and the Go release as a JSON document")
	return cmd
}

// own marks the errors of every command under cmd that runs something as the
// command's own, so report gives them the exit code of their kind instead of
// treating them as usage mistakes.
func own(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		own(c)
	}
	inner := cmd.RunE
	if inner == nil || cmd.Parent() == nil {
		return
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if err := inner(c, args); err != nil {
			return &runError{err}
		}
		return nil
	}
}

// noArgs refuses any argument with a usage error that names it.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fault.New(fault.Invalid, fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath()))
}

// report writes the one line for an error and returns the exit code of its
// kind. A usage error says how to get help.
func report(cmd *cobra.Command, env Env, err error) int {
	if err == nil {
		return 0
	}
	line := func(s string) {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %s\n", ProgramName, output.Clean(s))
	}
	var re *runError
	if !errors.As(err, &re) {
		err = fault.Wrap(fault.Invalid, err, err.Error())
		help := ProgramName + " --help"
		if cmd != nil {
			help = cmd.CommandPath() + " --help"
		}
		line(fault.Message(err) + `; try "` + help + `"`)
		return fault.Invalid.ExitCode()
	}
	err = re.err
	kind := fault.KindOf(err)
	switch {
	case kind == fault.Invalid && cmd != nil:
		line(fmt.Sprintf("%s: %s", strings.TrimPrefix(cmd.CommandPath(), ProgramName+" "), fault.Message(err)))
	case kind == fault.Internal:
		// A failure of ours with no kind of its own: its text was written
		// for a person at this terminal, not taken from the network.
		line(err.Error())
	default:
		line(fault.Message(err))
	}
	return kind.ExitCode()
}
