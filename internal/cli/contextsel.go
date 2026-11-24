package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/contexts"
	"github.com/amrshadid/nopacs/internal/fault"
)

// EnvContext names the client context to use, in place of the file's current
// one.
const EnvContext = "NOPACS_CONTEXT"

// chosen is the context a run uses and where it came from.
type chosen struct {
	name string
	contexts.Context
	path string
}

// selectContext finds the context this run uses, or nil for none. The order
// is --context, then NOPACS_CONTEXT, then the file's current context. A context
// the person named does not mix with a server named another way (--server,
// --data-dir, NOPACS_SERVER, NOPACS_DATA_DIR): that is a usage error naming
// both, because the context's key, files and default tenant belong to its own
// server. The file's current context applies only when none of those is given,
// so a file that is refused, or a current context nobody asked for, never gets
// in the way of a command that names its server. A name that is not in the
// file is an error that names the context and the file.
func (a *app) selectContext(cmd *cobra.Command) (*chosen, error) {
	if a.picked == nil {
		sel, err := a.pickContext(cmd)
		a.picked = &pick{sel: sel, err: err}
	}
	return a.picked.sel, a.picked.err
}

// pick is the cached result of selectContext.
type pick struct {
	sel *chosen
	err error
}

// namedServer lists the ways this run names a server or a data directory.
func (a *app) namedServer(cmd *cobra.Command) []string {
	var named []string
	flags := cmd.Flags()
	if flags.Changed("server") {
		named = append(named, "--server")
	}
	if flags.Changed("data-dir") {
		named = append(named, "--data-dir")
	}
	if a.env.Getenv(client.EnvServer) != "" {
		named = append(named, client.EnvServer)
	}
	if a.env.Getenv(client.EnvDataDir) != "" {
		named = append(named, client.EnvDataDir)
	}
	return named
}

func (a *app) pickContext(cmd *cobra.Command) (*chosen, error) {
	name, asked, via := "", false, ""
	named := a.namedServer(cmd)
	switch {
	case cmd.Flags().Changed("context"):
		name, asked, via = a.opts.context, true, "--context"
		if name == "" {
			return nil, fault.New(fault.Invalid, "--context needs a name")
		}
	case a.env.Getenv(EnvContext) != "":
		name, asked, via = a.env.Getenv(EnvContext), true, EnvContext
	case len(named) > 0:
		return nil, nil
	}
	if asked && len(named) > 0 {
		return nil, fault.New(fault.Invalid, via+" cannot be used with "+strings.Join(named, " or ")+": a context brings its own server; use one or the other")
	}
	loc, err := a.location()
	if err != nil {
		return nil, err
	}
	file, err := contexts.Load(loc)
	if err != nil {
		return nil, err
	}
	if !asked {
		name = file.Current
		if name == "" {
			return nil, nil
		}
	}
	c, ok := file.Contexts[name]
	if !ok {
		return nil, fault.New(fault.NotFound, "there is no context "+shown(name)+" in "+loc.Path)
	}
	return &chosen{name: name, Context: c, path: loc.Path}, nil
}

// shown makes a name from the person safe to put in a message.
func shown(s string) string {
	if len(s) > 63 {
		s = s[:63]
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return "(not a valid name)"
		}
	}
	return s
}

// defaultTenant is what a command's optional --tenant means: the flag when it
// was given (an empty one says the installation), else the default tenant of
// the selected context, else nothing. A selected context always brings the
// server, so its tenant is never applied to another server. When it fills the
// tenant it says so on standard error, because the command then acts on a
// tenant where it would otherwise act on the installation.
func (a *app) defaultTenant(cmd *cobra.Command, tenant string) (string, error) {
	if cmd.Flags().Changed("tenant") {
		return tenant, nil
	}
	sel, err := a.selectContext(cmd)
	if err != nil || sel == nil || sel.Tenant == "" {
		return "", err
	}
	a.line("using tenant " + sel.Tenant + " from context " + sel.name)
	return sel.Tenant, nil
}
