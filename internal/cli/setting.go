package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newSetting builds the setting noun.
func (a *app) newSetting() *cobra.Command {
	return group("setting", "Read and change settings",
		`The server knows a fixed list of settings, each with a built-in default. The
installation may set its own value of any of them, and a tenant may override
those the setting allows. The value that applies is the tenant's override when
it has one, else the installation's value, else the built-in default, and each
answer says which. Without --tenant a command acts on the installation. A
setting never holds a secret: its value is an ordinary argument and is not
recorded in the audit log. Reading needs the reader role on the scope; changing
needs the admin role.`,
		a.newSettingGet(), a.newSettingSet(), a.newSettingUnset(), a.newSettingList())
}

// settingWhere says where a setting's value comes from, in words.
func settingWhere(s *v1.Setting) string { return output.SettingFrom(s) }

// settingDetails writes one setting as labeled lines.
func (a *app) settingDetails(w io.Writer, s *v1.Setting) error {
	return output.Details(w, a.style(), []output.Pair{
		{Label: "Name", Value: s.GetName()},
		{Label: "Value", Value: s.GetValue()},
		{Label: "From", Value: settingWhere(s)},
	})
}

// newSettingGet builds "setting get".
func (a *app) newSettingGet() *cobra.Command {
	var tenant string
	cmd := &cobra.Command{
		Use:   "get <name> [--tenant NAME]",
		Short: "Show the value of a setting as it applies to a scope",
		Long: `Show a setting's value on the installation, or on a tenant: the tenant's
override when it has one, else the installation's value, else the built-in
default. The answer says which of the three the value came from.`,
		Example: `  nopacs setting get key.default-lifetime
  nopacs setting get key.default-lifetime --tenant lab1`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetSettingResponse, error) {
				return c.Settings.GetSetting(ctx, &v1.GetSettingRequest{Name: args[0], Scope: optionalScope(tenant)})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.settingDetails(w, res.GetSetting()) })
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "resolve the setting on this tenant (default: the installation)")
	return cmd
}

// newSettingSet builds "setting set".
func (a *app) newSettingSet() *cobra.Command {
	var tenant string
	cmd := &cobra.Command{
		Use:   "set <name> <value> [--tenant NAME]",
		Short: "Set the installation's value of a setting, or a tenant's override",
		Long: `Set a setting's value on the installation (for every tenant that does not
override it), or, with --tenant, that tenant's override, for a setting a tenant
may override. The server refuses a name it does not know, a value the setting
does not accept, and --tenant for a setting set on the installation only.`,
		Example: `  nopacs setting set key.default-lifetime 30d`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetSettingResponse, error) {
				return c.Settings.SetSetting(ctx, &v1.SetSettingRequest{Setting: &v1.Setting{Name: args[0], Value: args[1], Scope: optionalScope(tenant)}})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.settingDetails(w, res.GetSetting()) })
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "set an override for this tenant (default: the installation's value)")
	return cmd
}

// newSettingUnset builds "setting unset".
func (a *app) newSettingUnset() *cobra.Command {
	var tenant string
	cmd := &cobra.Command{
		Use:   "unset <name> [--tenant NAME]",
		Short: "Remove the installation's value of a setting, or a tenant's override",
		Long: `Remove the value set on a scope: on the installation, its value, so the
built-in default applies again; with --tenant, that tenant's override, so what
applies on the installation applies to the tenant again. Removing a value that
is not set changes nothing and succeeds.`,
		Example: `  nopacs setting unset key.default-lifetime`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.UnsetSettingResponse, error) {
				return c.Settings.UnsetSetting(ctx, &v1.UnsetSettingRequest{Name: args[0], Scope: optionalScope(tenant)})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				who := "the installation has no value"
				if tenant != "" {
					who = "tenant " + output.Clean(tenant) + " has no override"
				}
				_, err := fmt.Fprintf(w, "%s of %s; now applies: %s (%s)\n",
					who, output.Clean(args[0]), output.Clean(res.GetSetting().GetValue()), settingWhere(res.GetSetting()))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "remove this tenant's override (default: the installation's value)")
	return cmd
}

// newSettingList builds "setting list".
func (a *app) newSettingList() *cobra.Command {
	var tenant string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list [--tenant NAME]",
		Short: "List the installation's settings, or a tenant's overrides",
		Long: `List settings of one scope, ordered by name: on the installation, every setting
the server knows with the value that applies there and where it comes from; on
a tenant, only that tenant's overrides.`,
		Example: `  nopacs setting list
  nopacs setting list --tenant lab1 --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			settings, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Setting, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListSettingsResponse, error) {
					return c.Settings.ListSettings(ctx, &v1.ListSettingsRequest{Scope: optionalScope(tenant), PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetSettings(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "settings", settings, more, func() error {
				rows := make([][]string, 0, len(settings))
				for _, s := range settings {
					rows = append(rows, []string{s.GetName(), s.GetValue(), settingWhere(s)})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"NAME", "VALUE", "FROM"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "list this tenant's overrides (default: the installation's settings)")
	l.add(cmd)
	return cmd
}
