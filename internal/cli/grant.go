package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newGrant builds the grant noun.
func (a *app) newGrant() *cobra.Command {
	return group("grant", "Give a key a role on a tenant or on the installation, and take it back",
		`A grant gives one key one role on one scope: a tenant, or the installation. A
key holds at most one grant per scope, and a role includes the roles below it:
reader, writer, operator, admin. The writer role is not given on the
installation. These commands need the admin role on the installation.`,
		a.newGrantSet(), a.newGrantList(), a.newGrantRemove())
}

// grantRoles are the role names the completion offers.
var grantRoles = []string{"reader", "writer", "operator", "admin"}

// newGrantSet builds "grant set".
func (a *app) newGrantSet() *cobra.Command {
	var role string
	var sc scopeFlags
	cmd := &cobra.Command{
		Use:   "set <key-id> --role ROLE (--tenant NAME | --installation)",
		Short: "Set a key's role on a scope",
		Long: `Set the role of a key on a tenant or on the installation. When the key already
holds a role there, it is replaced in one step, so there is no moment without
access. The answer says what the role was before. A revoked key cannot be given
a grant.`,
		Example: `  nopacs grant set k8Zp2QvXn5Rt --role reader --tenant lab1
  nopacs grant set k8Zp2QvXn5Rt --role admin --installation`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			r, err := parseRole(role)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetGrantResponse, error) {
				return c.Access.SetGrant(ctx, &v1.SetGrantRequest{KeyId: args[0], Scope: sc.scope(), Role: r})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "key %s now holds %s on %s (before: %s)\n", output.Clean(args[0]), roleText(res.GetGrant().GetRole()), output.Clean(scopeText(res.GetGrant().GetScope())), roleBefore(res.GetPreviousRole()))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&role, "role", "", "the role to give: reader, writer, operator or admin (required)")
	_ = cmd.MarkFlagRequired("role")
	_ = cmd.RegisterFlagCompletionFunc("role", cobra.FixedCompletions(grantRoles, cobra.ShellCompDirectiveNoFileComp))
	sc.add(cmd, "the role acts on")
	scopeRequired(cmd)
	return cmd
}

// roleBefore words the role a grant had before a change.
func roleBefore(r v1.Role) string {
	if r == v1.Role_ROLE_UNSPECIFIED {
		return "no grant"
	}
	return roleText(r)
}

// scopeRequired makes exactly one of --tenant and --installation required.
func scopeRequired(cmd *cobra.Command) {
	cmd.MarkFlagsMutuallyExclusive("tenant", "installation")
	cmd.MarkFlagsOneRequired("tenant", "installation")
}

// newGrantList builds "grant list".
func (a *app) newGrantList() *cobra.Command {
	var keyID string
	var sc scopeFlags
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list (--key-id ID | --tenant NAME | --installation)",
		Short: "List the grants of one key, or the grants on one scope",
		Long: `List the grants of one key on every scope (--key-id, which takes the key's public
id and never the key itself), or the grants of every key on one scope. Exactly
one of the three is required: there is no list of every grant at once.`,
		Example: `  nopacs grant list --key-id k8Zp2QvXn5Rt
  nopacs grant list --tenant lab1
  nopacs grant list --installation`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			if cmd.Flags().Changed("key-id") {
				if err := checkKeyID(keyID); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			grants, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Grant, string, error) {
				req := &v1.ListGrantsRequest{PageSize: size, PageToken: token}
				if cmd.Flags().Changed("key-id") {
					req.Filter = &v1.ListGrantsRequest_KeyId{KeyId: keyID}
				} else {
					req.Filter = &v1.ListGrantsRequest_Scope{Scope: sc.scope()}
				}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListGrantsResponse, error) {
					return c.Access.ListGrants(ctx, req)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetGrants(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "grants", grants, more, func() error {
				rows := make([][]string, 0, len(grants))
				for _, g := range grants {
					rows = append(rows, []string{g.GetKeyId(), scopeText(g.GetScope()), roleText(g.GetRole())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"KEY", "SCOPE", "ROLE"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "list the grants of the key with this id (the key's public id, never the key itself)")
	sc.add(cmd, "whose grants to list")
	cmd.MarkFlagsMutuallyExclusive("key-id", "tenant", "installation")
	cmd.MarkFlagsOneRequired("key-id", "tenant", "installation")
	l.add(cmd)
	return cmd
}

// newGrantRemove builds "grant remove".
func (a *app) newGrantRemove() *cobra.Command {
	var sc scopeFlags
	cmd := &cobra.Command{
		Use:   "remove <key-id> (--tenant NAME | --installation)",
		Short: "Take back a key's grant on a scope",
		Long: `Take back the grant a key holds on a tenant or on the installation. Removing a
grant the key does not hold changes nothing and succeeds, so a repeated command
does not fail.`,
		Example: `  nopacs grant remove k8Zp2QvXn5Rt --tenant lab1`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkKeyID(args[0]); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemoveGrantResponse, error) {
				return c.Access.RemoveGrant(ctx, &v1.RemoveGrantRequest{KeyId: args[0], Scope: sc.scope()})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				was := "it held none"
				if res.GetPreviousRole() != v1.Role_ROLE_UNSPECIFIED {
					was = "it was " + roleText(res.GetPreviousRole())
				}
				_, err := fmt.Fprintf(w, "key %s holds no grant on %s (%s)\n", output.Clean(args[0]), output.Clean(scopeText(sc.scope())), was)
				return err
			})
		}),
	}
	sc.add(cmd, "the grant is on")
	scopeRequired(cmd)
	return cmd
}
