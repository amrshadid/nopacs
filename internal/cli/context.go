package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/contexts"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// contextDoc is the JSON form of one context. It holds paths only, as the
// file does.
type contextDoc struct {
	Name          string `json:"name"`
	Current       bool   `json:"current"`
	Server        string `json:"server"`
	CAFile        string `json:"caFile"`
	TLSClientCert string `json:"tlsClientCert"`
	TLSClientKey  string `json:"tlsClientKey"`
	KeyFile       string `json:"keyFile"`
	Tenant        string `json:"tenant"`
}

// docOf makes the document of a context.
func docOf(name string, c contexts.Context, current bool) contextDoc {
	return contextDoc{Name: name, Current: current, Server: c.Server, CAFile: c.CAFile,
		TLSClientCert: c.TLSClientCert, TLSClientKey: c.TLSClientKey, KeyFile: c.KeyFile, Tenant: c.Tenant}
}

// newContext builds the context noun.
func (a *app) newContext() *cobra.Command {
	return group("context", "Name the servers and keys this machine uses: add, list, show, use, remove",
		`A context is a name for one way to reach one installation: the server's
address, the files that reach it over TLS, the file that holds a key, and
optionally a default tenant. It is kept in a file only you can read
($XDG_CONFIG_HOME/nopacs/contexts.json, ~/.config/nopacs/contexts.json when that is
unset, %AppData%\nopacs\contexts.json on Windows, or the file named by
NOPACS_CONFIG). The file holds paths, never a key.

A command uses --context NAME, else NOPACS_CONTEXT, else the current context of
the file, which "context use" sets. --server, --data-dir, --key-file, --ca-file,
--tls-client-cert and --tls-client-key, and the variables NOPACS_SERVER,
NOPACS_DATA_DIR, NOPACS_KEY, NOPACS_CA_FILE, NOPACS_TLS_CLIENT_CERT and
NOPACS_TLS_CLIENT_KEY, win over what a context holds. A context's key, authorities and
certificate are used only for the context's own server, never sent to another.
A context's default tenant fills --tenant of the commands where it is optional
(setting get, set, unset and list, audit list); --tenant "" says the installation
instead.`,
		a.newContextAdd(), a.newContextList(), a.newContextShow(), a.newContextUse(), a.newContextRemove())
}

// absPath makes a path absolute so a context works from any directory; an
// empty path stays empty.
func absPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fault.Wrap(fault.Invalid, err, "cannot use the path "+p)
	}
	return abs, nil
}

// location finds the file of contexts for this run.
func (a *app) location() (contexts.Location, error) { return contexts.Locate(a.env.Getenv) }

// newContextAdd builds "context add".
func (a *app) newContextAdd() *cobra.Command {
	var tenant string
	var replace bool
	cmd := &cobra.Command{
		Use:   "add <name> --server URL --key-file PATH [--ca-file PATH] [--tls-client-cert PATH --tls-client-key PATH] [--tenant NAME] [--replace]",
		Short: "Save a context after checking its key against the server",
		Long: `Save a context. The key in --key-file is first sent to --server with a WhoAmI
call, and the context is saved only if the server accepts it; the answer, who the
key is and what it is granted, is printed. Only the key file's path is saved,
never the key. The first context saved becomes the current one. A name that is
taken is refused unless --replace is given. The flags are the ones every command
takes, and only the flags are used here, not the environment, so what is checked
is what is saved.`,
		Example: `  nopacs context add prod --server https://nopacs.example.org:8443 --key-file ~/.nopacs/prod.key --ca-file ~/.nopacs/ca.pem
  nopacs context add lab1 --server https://nopacs.example.org:8443 --key-file ~/.nopacs/lab1.key --tenant lab1`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := contexts.ValidName(name); err != nil {
				return err
			}
			if a.opts.server == "" || a.opts.keyFile == "" {
				return fault.New(fault.Invalid, "a context needs --server and --key-file")
			}
			if _, err := client.ParseServer(a.opts.server); err != nil {
				return err
			}
			c := contexts.Context{Server: a.opts.server, Tenant: tenant}
			var err error
			for _, p := range []struct {
				dst *string
				src string
			}{{&c.KeyFile, a.opts.keyFile}, {&c.CAFile, a.opts.caFile}, {&c.TLSClientCert, a.opts.clientCert}, {&c.TLSClientKey, a.opts.clientKey}} {
				if *p.dst, err = absPath(p.src); err != nil {
					return err
				}
			}
			if err := contexts.Validate(name, c); err != nil {
				return err
			}
			loc, err := a.location()
			if err != nil {
				return err
			}
			file, err := contexts.Load(loc)
			if err != nil {
				return err
			}
			if _, taken := file.Contexts[name]; taken && !replace {
				return fault.New(fault.Precondition, "the context "+name+" exists in "+loc.Path+"; use --replace to change it")
			}
			cl, err := client.New(client.Config{
				Server: c.Server, KeyFile: c.KeyFile, CAFile: c.CAFile, ClientCert: c.TLSClientCert, ClientKey: c.TLSClientKey,
				Getenv: a.env.Getenv, Timeout: a.opts.timeout,
			})
			if err != nil {
				return err
			}
			defer cl.Close()
			res, err := call(ctx, cl, cmd, func(ctx context.Context) (*v1.WhoAmIResponse, error) {
				return cl.Access.WhoAmI(ctx, &v1.WhoAmIRequest{})
			})
			if err != nil {
				// The error's own line says what the server answered; this one
				// says what it means for the context.
				a.line("context add: the key file of context " + name + " does not work with " + c.Server + ", so nothing was saved")
				return err
			}
			if file.Contexts == nil {
				file.Contexts = map[string]contexts.Context{}
			}
			file.Contexts[name] = c
			if file.Current == "" {
				file.Current = name
			}
			if err := file.Save(loc); err != nil {
				return err
			}
			current := file.Current == name
			if a.opts.json {
				raw, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(res.GetPrincipal())
				if err != nil {
					return fault.Wrap(fault.Internal, err, "encoding the answer: "+err.Error())
				}
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, struct {
						contextDoc
						File      string  `json:"file"`
						Principal rawJSON `json:"principal"`
					}{docOf(name, c, current), loc.Path, rawJSON(raw)})
				})
			}
			return a.write(func(w io.Writer) error {
				if err := a.contextTable(w, name, c, current, loc.Path); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(w); err != nil {
					return err
				}
				return a.whoamiTable(w, res.GetPrincipal())
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "the tenant to act on where a command's tenant is optional")
	cmd.Flags().BoolVar(&replace, "replace", false, "replace a context of the same name")
	return cmd
}

// rawJSON is JSON already encoded, kept as it is.
type rawJSON []byte

// MarshalJSON returns the encoded document unchanged.
func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

// contextTable writes the details of one context.
func (a *app) contextTable(w io.Writer, name string, c contexts.Context, current bool, path string) error {
	none := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	cur := "no"
	if current {
		cur = "yes"
	}
	return output.Details(w, a.style(), []output.Pair{
		{Label: "Context", Value: name},
		{Label: "Current", Value: cur},
		{Label: "Server", Value: c.Server},
		{Label: "Key file", Value: c.KeyFile},
		{Label: "CA file", Value: none(c.CAFile)},
		{Label: "Client certificate", Value: none(c.TLSClientCert)},
		{Label: "Client key", Value: none(c.TLSClientKey)},
		{Label: "Tenant", Value: none(c.Tenant)},
		{Label: "File", Value: path},
	})
}

// newContextList builds "context list".
func (a *app) newContextList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the contexts, the current one marked",
		Long: `List the contexts in the file, with the current one marked "*". It reads only
the file and does not contact any server.`,
		Example: `  nopacs context list
  nopacs context list --json`,
		Args: exactArgs(0),
		RunE: a.run(func(_ context.Context, _ *cobra.Command, _ []string) error {
			loc, err := a.location()
			if err != nil {
				return err
			}
			file, err := contexts.Load(loc)
			if err != nil {
				return err
			}
			names := file.Names()
			if a.opts.json {
				docs := make([]contextDoc, 0, len(names))
				for _, n := range names {
					docs = append(docs, docOf(n, file.Contexts[n], n == file.Current))
				}
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, struct {
						Current  string       `json:"current"`
						Contexts []contextDoc `json:"contexts"`
						More     bool         `json:"more"`
						File     string       `json:"file"`
					}{file.Current, docs, false, loc.Path})
				})
			}
			rows := make([][]string, 0, len(names))
			for _, n := range names {
				mark := ""
				if n == file.Current {
					mark = "*"
				}
				c := file.Contexts[n]
				rows = append(rows, []string{mark, n, c.Server, c.Tenant})
			}
			return a.write(func(w io.Writer) error {
				return output.Table(w, a.style(), []string{"", "NAME", "SERVER", "TENANT"}, rows)
			})
		}),
	}
}

// named loads the file and finds a context by name, with the errors that name
// the context and the file.
func (a *app) named(name string) (contexts.Location, *contexts.File, contexts.Context, error) {
	loc, err := a.location()
	if err != nil {
		return loc, nil, contexts.Context{}, err
	}
	file, err := contexts.Load(loc)
	if err != nil {
		return loc, nil, contexts.Context{}, err
	}
	c, ok := file.Contexts[name]
	if !ok {
		return loc, file, c, fault.New(fault.NotFound, "there is no context "+output.Clean(name)+" in "+loc.Path)
	}
	return loc, file, c, nil
}

// newContextShow builds "context show".
func (a *app) newContextShow() *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Short:   "Show one context",
		Long:    `Show what a context holds: the server, the paths of its files and its default tenant. It reads only the file.`,
		Example: `  nopacs context show prod`,
		Args:    exactArgs(1),
		RunE: a.run(func(_ context.Context, _ *cobra.Command, args []string) error {
			loc, file, c, err := a.named(args[0])
			if err != nil {
				return err
			}
			current := file.Current == args[0]
			if a.opts.json {
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, struct {
						contextDoc
						File string `json:"file"`
					}{docOf(args[0], c, current), loc.Path})
				})
			}
			return a.write(func(w io.Writer) error { return a.contextTable(w, args[0], c, current, loc.Path) })
		}),
	}
}

// newContextUse builds "context use".
func (a *app) newContextUse() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Make a context the current one",
		Long: `Make a context the one commands use when neither --context nor NOPACS_CONTEXT
names another, and no --server, --data-dir, NOPACS_SERVER or NOPACS_DATA_DIR says
where the server is.`,
		Example: `  nopacs context use prod`,
		Args:    exactArgs(1),
		RunE: a.run(func(_ context.Context, _ *cobra.Command, args []string) error {
			loc, file, _, err := a.named(args[0])
			if err != nil {
				return err
			}
			file.Current = args[0]
			if err := file.Save(loc); err != nil {
				return err
			}
			if a.opts.json {
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, struct {
						Current string `json:"current"`
						File    string `json:"file"`
					}{args[0], loc.Path})
				})
			}
			return a.write(func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Current context: %s\n", output.Clean(args[0]))
				return err
			})
		}),
	}
}

// newContextRemove builds "context remove".
func (a *app) newContextRemove() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a context",
		Long: `Remove a context from the file. The key file it names is not touched. When it
was the current context there is none afterward, and commands without a server
use the local socket again.`,
		Example: `  nopacs context remove prod`,
		Args:    exactArgs(1),
		RunE: a.run(func(_ context.Context, _ *cobra.Command, args []string) error {
			loc, file, _, err := a.named(args[0])
			if err != nil {
				return err
			}
			delete(file.Contexts, args[0])
			if file.Current == args[0] {
				file.Current = ""
			}
			if err := file.Save(loc); err != nil {
				return err
			}
			if a.opts.json {
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, struct {
						Removed string `json:"removed"`
						Current string `json:"current"`
						File    string `json:"file"`
					}{args[0], file.Current, loc.Path})
				})
			}
			return a.write(func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Removed context %s\n", output.Clean(args[0]))
				return err
			})
		}),
	}
}
