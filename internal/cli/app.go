package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tui"
	"github.com/amrshadid/nopacs/internal/version"
)

// Env is everything the commands take from the process around them, so a test
// can run a command with its own streams, environment and clock.
type Env struct {
	// Stdout receives what a command found.
	Stdout io.Writer
	// Stderr receives errors and notes.
	Stderr io.Writer
	// Getenv reads the environment; os.Getenv when nil.
	Getenv func(string) string
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Location is the zone times are shown in; time.Local when nil.
	Location *time.Location
	// Stdin is standard input. With Stdout it decides whether "nopacs" with
	// no arguments opens the interactive program; nil is not a terminal.
	Stdin io.Reader
	// Terminal says whether input and output are both terminals; when nil it
	// asks the operating system about each. A test sets it to open the
	// interactive program without a terminal.
	Terminal func(in io.Reader, out io.Writer) bool
	// Interactive runs the interactive program; tui.Run when nil. A test sets
	// it to see that the program was, or was not, opened.
	Interactive func(ctx context.Context, o tui.Options) error
	// WatchPause is how long "job watch" waits before it follows the event
	// feed again after the server ended a stream that did not end the job; one
	// second when zero. A test sets it short.
	WatchPause time.Duration
}

// withDefaults fills what a caller left unset.
func (e Env) withDefaults() Env {
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Location == nil {
		e.Location = time.Local
	}
	return e
}

// app is one run of the command line: its environment, the options every
// command shares, and the client the first call makes.
type app struct {
	env Env
	// args are the arguments Run was given, for an error cobra makes
	// before it has read --json (jsonAsked).
	args []string
	// picked caches the context this run uses, found once.
	picked *pick
	opts   struct {
		dataDir string
		context string
		server  string
		keyFile string
		caFile  string
		// clientCert and clientKey are the TLS client certificate and its
		// key's files, for --server.
		clientCert, clientKey string
		timeout               time.Duration
		json                  bool
	}
}

// runError marks an error that came from a command's own work, as opposed to
// one that cobra made while reading the arguments. The two are told apart
// because cobra's errors are all usage mistakes (exit 2) and carry no kind of
// ours.
type runError struct{ err error }

// Error returns the wrapped error's text.
func (e *runError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error.
func (e *runError) Unwrap() error { return e.err }

// run adapts a command's work to cobra: the error it returns is marked as the
// command's own.
func (a *app) run(fn func(ctx context.Context, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if err := fn(cmd.Context(), cmd, args); err != nil {
			return &runError{err}
		}
		return nil
	}
}

// Run is the whole command line: it runs the arguments (without the program's
// name) and returns the exit code. Standard output and standard error are the
// ones in env, so nothing here touches the process's own.
func Run(ctx context.Context, args []string, env Env) int {
	env = env.withDefaults()
	a := &app{env: env, args: args}
	root := a.newRoot()
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	return a.report(cmd, err)
}

// Command returns the root command for an environment, without running it.
// Tests and tools that walk the command tree (the check that no flag can carry
// a secret, the check that every command has help text) use it; Run is how a
// program runs the commands.
func Command(env Env) *cobra.Command {
	return (&app{env: env.withDefaults()}).newRoot()
}

// report writes the one line for an error (fail: one JSON object with
// --json) and returns the exit code of its kind. A usage error says how to
// get help; a failure of a call already holds the request id.
func (a *app) report(cmd *cobra.Command, err error) int {
	var re *runError
	if !errors.As(err, &re) {
		// cobra's own: an unknown flag, a missing required flag, a flag group.
		err = fault.Wrap(fault.Invalid, err, err.Error())
		help := "nopacs --help"
		if cmd != nil {
			help = cmd.CommandPath() + " --help"
		}
		// cobra may have refused the arguments before it read --json.
		a.opts.json = a.opts.json || jsonAsked(a.args)
		a.fail(fmt.Sprintf("%s; try \"%s\"", fault.Message(err), help), err)
		return fault.Invalid.ExitCode()
	}
	err = re.err
	var ee *exitError
	if errors.As(err, &ee) {
		// The program ended by a signal: nothing to say, the terminal has
		// been restored, and the status is the conventional one.
		return ee.code
	}
	kind := fault.KindOf(err)
	var ce *client.CallError
	switch {
	case errors.As(err, &ce):
		a.fail(ce.Error(), err)
	case kind == fault.Invalid && cmd != nil:
		a.fail(fmt.Sprintf("%s: %s", strings.TrimPrefix(cmd.CommandPath(), "nopacs "), fault.Message(err)), err)
	case kind == fault.Internal:
		// A failure of ours with no kind of its own: its text was written for
		// a person at this terminal, not taken from the network.
		a.fail(err.Error(), err)
	default:
		a.fail(fault.Message(err), err)
	}
	return kind.ExitCode()
}

// line writes one line to standard error, cleaned of anything that would act
// on the terminal.
func (a *app) line(s string) {
	_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: %s\n", output.Clean(s))
}

// style returns the emphasis allowed on standard output.
func (a *app) style() output.Style {
	return output.Style{Color: ColorAllowed(output.IsTerminal(a.env.Stdout), a.env.Getenv, runtime.GOOS)}
}

// ColorAllowed says whether color may be used: only on a terminal, never when
// NO_COLOR is set to anything (no-color.org), never on a dumb terminal, and
// not on Windows, where older consoles print the escape codes as text.
func ColorAllowed(terminal bool, getenv func(string) string, goos string) bool {
	return terminal && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb" && goos != "windows"
}

// client makes the client for this run from the shared options. A flag beats
// the environment, and --server beats --data-dir only when the environment is
// what set it; two flags that disagree are a usage error.
func (a *app) client(cmd *cobra.Command) (*client.Client, error) {
	flags := cmd.Flags()
	serverFlag, dataFlag := flags.Changed("server"), flags.Changed("data-dir")
	if serverFlag && dataFlag {
		return nil, fault.New(fault.Invalid, "give --server or --data-dir, not both")
	}
	sel, err := a.selectContext(cmd)
	if err != nil {
		return nil, err
	}
	cfg := client.Config{
		KeyFile: a.opts.keyFile,
		Getenv:  a.env.Getenv,
		Timeout: a.opts.timeout,
	}
	cfg.CAFile = flagOrEnv(flags.Changed("ca-file"), a.opts.caFile, a.env.Getenv(client.EnvCAFile))
	cfg.ClientCert = flagOrEnv(flags.Changed("tls-client-cert"), a.opts.clientCert, a.env.Getenv(client.EnvClientCert))
	cfg.ClientKey = flagOrEnv(flags.Changed("tls-client-key"), a.opts.clientKey, a.env.Getenv(client.EnvClientKey))
	envServer, envData := a.env.Getenv(client.EnvServer), a.env.Getenv(client.EnvDataDir)
	fromContext := false
	switch {
	case serverFlag:
		cfg.Server = a.opts.server
	case dataFlag:
		cfg.DataDir = a.opts.dataDir
	case envServer != "":
		cfg.Server = envServer
	case sel != nil:
		// A context is selected only when nothing else named the server, so
		// its key, files and tenant go to its own server. An exported key
		// would otherwise travel to whichever context is current, so it is
		// refused; --key-file is the explicit override.
		fromContext = true
		cfg.Server = sel.Server
		if cfg.KeyFile == "" {
			if a.env.Getenv(client.EnvKey) != "" {
				return nil, fault.New(fault.Invalid, client.EnvKey+" is set; it would be sent to the server of context "+shown(sel.name)+": unset it or pass --key-file")
			}
			cfg.KeyFile = sel.KeyFile
		}
		if cfg.CAFile == "" {
			cfg.CAFile = sel.CAFile
		}
		if cfg.ClientCert == "" && cfg.ClientKey == "" {
			cfg.ClientCert, cfg.ClientKey = sel.TLSClientCert, sel.TLSClientKey
		}
	case envData != "":
		cfg.DataDir = envData
	default:
		return nil, fault.New(fault.Invalid, "do not know where the server is: give --data-dir (or set "+client.EnvDataDir+") for the local socket, --server (or set "+client.EnvServer+") for the network, or --context (or set "+EnvContext+") for a saved context")
	}
	c, err := client.New(cfg)
	if err != nil && fromContext {
		return nil, fault.Wrap(fault.KindOf(err), err, "context "+shown(sel.name)+" ("+sel.path+"): "+fault.Message(err))
	}
	return c, err
}

// flagOrEnv returns a flag's value when the flag was given, else the
// environment's.
func flagOrEnv(changed bool, flag, env string) string {
	if changed {
		return flag
	}
	return env
}

// newRoot builds the root command with the options every command shares.
func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "nopacs",
		Short: "Operate a NoPACs installation: tenants, keys, grants, settings, the audit log, and a tenant's storages, receivers, senders, jobs, receptions and quarantine",
		Long: `nopacs operates a NoPACs installation through its API.

By default it reaches the server on the local socket in the data directory
(--data-dir, or NOPACS_DATA_DIR), as the operating system user that runs the
server, with no key. With --server (or NOPACS_SERVER) it reaches a server on
the network over TLS, with the key read from NOPACS_KEY or from the file named
by --key-file. A key is never an argument: arguments are visible to every user
of the machine and are kept in shell history. A key that works only with a TLS
client certificate is used with --tls-client-cert and --tls-client-key. A
context (nopacs context --help) saves a server with its key file and TLS files
under a name: --context NAME, NOPACS_CONTEXT, or the current context.

Output is a table for a person. With --json it is one JSON document with a
"schemaVersion" field, for a program. An error is one line on standard error;
with --json that line is a JSON object whose "reason" is a fixed word.

Exit codes: 0 done, 1 failed, 2 wrong usage, 3 refused (not authenticated, or
not allowed), 4 not found, 5 a state that forbids the change, 6 a position
that can no longer be answered, 7 a bound of the server reached.`,
		Version:       version.Get().String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Only with no arguments, and only when input and output are both
			// a terminal, does the program open; a script or a pipe gets the
			// usage line and exit 2 as it always did.
			if a.canOpenInteractive() {
				return a.openInteractive(cmd)
			}
			// Not marked as a command's own error: it is a usage mistake, so
			// report adds the pointer to the help.
			return fault.New(fault.Invalid, "no command (usage: nopacs <noun> <verb> [flags], for example nopacs tenant list)")
		},
	}
	root.SetOut(a.env.Stdout)
	root.SetErr(a.env.Stderr)
	// The version flag prints the same line as the version command.
	root.SetVersionTemplate("{{.Version}}\n")

	f := root.PersistentFlags()
	f.StringVar(&a.opts.context, "context", "", "name of a saved context, a server with its key and files (default: "+EnvContext+", then the current context; see nopacs context --help)")
	f.StringVar(&a.opts.dataDir, "data-dir", "", "the installation's data directory; the server is reached on its local socket, run/nopacs.sock (default: "+client.EnvDataDir+")")
	f.StringVar(&a.opts.server, "server", "", "address of a server on the network, https://host:port (http:// only for a loopback host); needs a key (default: "+client.EnvServer+")")
	f.StringVar(&a.opts.keyFile, "key-file", "", "path of a file holding the key to send with --server (default: the "+client.EnvKey+" variable); the key itself is never an argument")
	f.StringVar(&a.opts.caFile, "ca-file", "", "path of a PEM file of certificate authorities to trust for --server, instead of the system's, for a private authority (default: "+client.EnvCAFile+"); it has no effect on the local socket")
	f.StringVar(&a.opts.clientCert, "tls-client-cert", "", "path of a PEM file of a TLS client certificate to present to --server, for a key that works only with it; needs --tls-client-key (default: "+client.EnvClientCert+")")
	f.StringVar(&a.opts.clientKey, "tls-client-key", "", "path of the PEM file of that certificate's private key, private to you (default: "+client.EnvClientKey+")")
	f.DurationVar(&a.opts.timeout, "timeout", client.DefaultTimeout, "how long one call to the server may take")
	f.BoolVar(&a.opts.json, "json", false, "write one JSON document with schemaVersion instead of a table")

	root.AddCommand(
		a.newVersion(),
		a.newAgentGuide(),
		moved("init", "nopacs-server init"), moved("serve", "nopacs-server serve"),
		a.newWhoami(),
		a.newContext(),
		a.newTenant(),
		a.newKey(),
		a.newGrant(),
		a.newSetting(),
		a.newAudit(),
		a.newStorage(),
		a.newReplica(),
		a.newRepair(),
		a.newRecall(),
		a.newSwitch(),
		a.newRetention(),
		a.newHold(),
		a.newReceiver(),
		a.newReception(),
		a.newSender(),
		a.newJob(),
		a.newDeid(),
		a.newWatch(),
		a.newProfile(),
		a.newQuarantine(),
		a.newCorrection(),
		a.newCatalog(),
		a.newScan(),
		a.newDownload(),
		a.newBackup(),
	)
	return root
}

// moved returns a command that exists only to say where it went: init, serve,
// backup verify and restore, and tenant import must touch the data directory
// or the key file on the server's machine, so they belong to the
// nopacs-server program, and a person who types one here is told so in one
// line (a usage error, exit 2) instead of "unknown command". It takes any
// arguments and flags, so what was typed after the name never changes the
// answer. It is hidden from the help: the command line lists only what it
// does. target is what to run instead.
func moved(use, target string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Hidden:             true,
		DisableFlagParsing: true,
		Short:              "Moved to " + target,
		Long:               "This command is part of the nopacs-server program, which holds the commands that run on the server's machine.",
		Example:            "  " + target,
		Args:               cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error {
			return &runError{fault.New(fault.Invalid, "is not a nopacs command; run "+target)}
		},
	}
}

// noArgs refuses any argument with a usage error that names it.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fault.New(fault.Invalid, fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath()))
}

// group makes the command of a noun, such as "tenant": it runs nothing by
// itself, and with no verb or an unknown one it is a usage error rather than a
// silent help text, so a script that misspells a verb fails.
func group(use, short, long string, verbs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := make([]string, 0, len(cmd.Commands()))
			for _, c := range cmd.Commands() {
				names = append(names, c.Name())
			}
			return fault.New(fault.Invalid, fmt.Sprintf("%s needs a verb: %s", what(cmd), strings.Join(names, ", ")))
		},
	}
	cmd.AddCommand(verbs...)
	return cmd
}

// exactArgs requires n positional arguments and names the usage when they are
// not given.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return fault.New(fault.Invalid, fmt.Sprintf("wrong number of arguments (want %d, got %d); usage: %s", n, len(args), cmd.UseLine()))
		}
		return nil
	}
}

// what names a command for an error: its path without the program's name,
// such as "tenant create".
func what(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), "nopacs ")
}

// write runs a function that writes to standard output and returns its error
// with a message that says the output failed, because a script that reads the
// output must see a failure in the exit code, not success with nothing
// printed.
func (a *app) write(fn func(w io.Writer) error) error {
	if err := fn(a.env.Stdout); err != nil {
		return fault.Wrap(fault.Internal, err, "writing the output: "+err.Error())
	}
	return nil
}
