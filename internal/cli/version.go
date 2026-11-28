package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/version"
)

// newVersion builds "nopacs version". It asks nothing of the server: it says
// which build is running, so a report can name the build that wrote the data.
func (a *app) newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this program",
		Long: `Print the version, the commit and the Go release this program was built with,
read from the program's own build information. It does not contact a server.`,
		Example: `  nopacs version
  nopacs version --json`,
		Args: noArgs,
		RunE: a.run(func(_ context.Context, _ *cobra.Command, _ []string) error {
			info := version.Get()
			if a.opts.json {
				return a.write(func(w io.Writer) error {
					return output.JSONValue(w, info)
				})
			}
			return a.write(func(w io.Writer) error {
				_, err := fmt.Fprintln(w, info)
				return err
			})
		}),
	}
}

// newWhoami builds "nopacs whoami".
func (a *app) newWhoami() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the server takes this caller to be, and what it may do",
		Long: `Ask the server who is calling: the key the request carried, with its grants, or
the local operator when the call came on the local socket. It is the quickest
way to check that a key works and what it may do. It also shows the server's
version and commit; when the server's version differs from this program's (a
different major or minor release, or a development build that is not the same
build) one warning line goes to standard error and the command still succeeds.`,
		Example: `  nopacs whoami
  nopacs --server https://nopacs.example.org:8443 --key-file ~/.nopacs-key whoami`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.WhoAmIResponse, error) {
				return c.Access.WhoAmI(ctx, &v1.WhoAmIRequest{})
			})
			if err != nil {
				return err
			}
			a.warnVersion(res, "")
			return messageOut(a, res, func(w io.Writer) error { return a.whoamiTable(w, res) })
		}),
	}
}

// serverText says which build answered: its version and commit, or "-" from a
// server too old to say.
func serverText(res *v1.WhoAmIResponse) string {
	if res.GetServerVersion() == "" {
		return "-"
	}
	return output.Clean(res.GetServerVersion() + " (commit " + res.GetServerCommit() + ")")
}

// remoteInfo is the build a WhoAmI answer names, in the form the comparison
// takes.
func remoteInfo(res *v1.WhoAmIResponse) version.Info {
	return version.Info{Version: res.GetServerVersion(), Commit: res.GetServerCommit()}
}

// warnVersion writes one line to standard error when the server's build is not
// one this program is meant to talk to (version.Differ says when). It only
// warns: a call that worked is never failed for it. who names the context or
// server when several are shown, empty for the one this run uses.
func (a *app) warnVersion(res *v1.WhoAmIResponse, who string) {
	if !version.Differ(version.Get(), remoteInfo(res)) {
		return
	}
	if who != "" {
		who = " (" + who + ")"
	}
	a.line("warning: the server" + who + " is " + res.GetServerVersion() + " and this program is " +
		version.Get().Version + "; they may not understand each other (see nopacs version)")
}

// whoamiTable writes the caller, the server's build and, for a key, its grants.
func (a *app) whoamiTable(w io.Writer, res *v1.WhoAmIResponse) error {
	style := a.style()
	p := res.GetPrincipal()
	switch caller := p.GetCaller().(type) {
	case *v1.Principal_LocalOperator:
		return output.Details(w, style, []output.Pair{
			{Label: "Caller", Value: "local operator (user " + caller.LocalOperator.GetUserId() + ")"},
			{Label: "Access", Value: "admin on the installation and on every tenant"},
			{Label: "Server", Value: serverText(res)},
		})
	case *v1.Principal_Key:
		k := caller.Key
		if err := output.Details(w, style, []output.Pair{
			{Label: "Caller", Value: "key " + keyLabel(k)},
			{Label: "Expires", Value: a.whenOr(k.GetExpireTime(), "never")},
			{Label: "Server", Value: serverText(res)},
		}); err != nil {
			return err
		}
		if len(p.GetGrants()) == 0 {
			_, err := fmt.Fprintln(w, "Grants: none")
			return err
		}
		rows := make([][]string, 0, len(p.GetGrants()))
		for _, g := range p.GetGrants() {
			rows = append(rows, []string{scopeText(g.GetScope()), roleText(g.GetRole())})
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		return output.Table(w, style, []string{"SCOPE", "ROLE"}, rows)
	}
	_, err := fmt.Fprintln(w, "Caller: unknown")
	return err
}
