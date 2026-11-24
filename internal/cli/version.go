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
way to check that a key works and what it may do.`,
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
			return messageOut(a, res, func(w io.Writer) error { return a.whoamiTable(w, res.GetPrincipal()) })
		}),
	}
}

// whoamiTable writes the caller and, for a key, its grants.
func (a *app) whoamiTable(w io.Writer, p *v1.Principal) error {
	style := a.style()
	switch caller := p.GetCaller().(type) {
	case *v1.Principal_LocalOperator:
		return output.Details(w, style, []output.Pair{
			{Label: "Caller", Value: "local operator (user " + caller.LocalOperator.GetUserId() + ")"},
			{Label: "Access", Value: "admin on the installation and on every tenant"},
		})
	case *v1.Principal_Key:
		k := caller.Key
		if err := output.Details(w, style, []output.Pair{
			{Label: "Caller", Value: "key " + keyLabel(k)},
			{Label: "Expires", Value: a.whenOr(k.GetExpireTime(), "never")},
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
