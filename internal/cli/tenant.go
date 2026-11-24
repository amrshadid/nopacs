package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// when writes a timestamp in the zone times are shown in, with its offset.
func (a *app) when(ts *timestamppb.Timestamp) string {
	return output.Time(ts, a.env.Location)
}

// whenOr is when with a word for an absent time, such as "never".
func (a *app) whenOr(ts *timestamppb.Timestamp, absent string) string {
	if ts == nil {
		return absent
	}
	return a.when(ts)
}

// newTenant builds the tenant noun.
func (a *app) newTenant() *cobra.Command {
	return group("tenant", "Create, list, show, check, remove and hand off tenants",
		`A tenant is an independent archive inside an installation, with its own
database. These commands need the admin role on the installation, except list
and show, which need the reader role, and status, which needs the reader role
on the tenant. A tenant moves to another installation by export there and
import here; reclaim is the local operator's, and import is a command of
nopacs-server, run on the machine that holds the data directory.`,
		a.newTenantCreate(), a.newTenantList(), a.newTenantShow(), a.newTenantStatus(), a.newTenantRemove(),
		a.newTenantExport(), a.newTenantReclaim(), moved("import", "nopacs-server tenant import"))
}

// tenantDetails writes one tenant as labeled lines.
func (a *app) tenantDetails(w io.Writer, t *v1.Tenant) error {
	pairs := []output.Pair{
		{Label: "Name", Value: t.GetName()},
		{Label: "Database", Value: databaseText(t.GetDatabaseType())},
		{Label: "State", Value: stateText(t.GetState())},
	}
	if t.GetStateReason() != "" {
		pairs = append(pairs, output.Pair{Label: "Reason", Value: t.GetStateReason()})
	}
	if t.GetStateTime() != nil {
		// When the server's current view of the state began; absent while
		// the server holds no view of the tenant yet.
		pairs = append(pairs, output.Pair{Label: "Since", Value: a.when(t.GetStateTime())})
	}
	pairs = append(pairs, output.Pair{Label: "Created", Value: a.when(t.GetCreateTime())})
	return output.Details(w, a.style(), pairs)
}

// newTenantCreate builds "tenant create".
func (a *app) newTenantCreate() *cobra.Command {
	var dbEnv, dbFile string
	cmd := &cobra.Command{
		Use:   "create <name> [--db-env VAR | --db-file PATH]",
		Short: "Create a tenant",
		Long: `Create a tenant and make its tables in its database.

With neither --db-env nor --db-file the tenant is a SQLite database, a file the
server places in its data directory. For a Postgres tenant, name the
environment variable or the file that holds the database's address (a
connection URI with its login and password). The address is a secret, so it is
never an argument: it is read from the variable or the file, sent to the server
over the connection, and never printed. The database and its login must exist
before this command runs; NoPACs makes only the tables.

A tenant's name is 1 to 63 lowercase letters, digits and hyphens, starting with
a letter. It is used in paths on the server, so nothing else is accepted.`,
		Example: `  nopacs tenant create lab1
  export LAB2_DB="$(cat /etc/nopacs/lab2.dsn)"
  nopacs tenant create lab2 --db-env LAB2_DB
  nopacs tenant create lab3 --db-file /etc/nopacs/lab3.dsn`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			req := &v1.CreateTenantRequest{Name: args[0], DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE}
			addr, err := a.readAddress(dbEnv, dbFile)
			if err != nil {
				return err
			}
			if addr != "" {
				req.DatabaseType = v1.DatabaseType_DATABASE_TYPE_POSTGRES
				req.DatabaseAddress = addr
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateTenantResponse, error) {
				return c.Tenants.CreateTenant(ctx, req)
			})
			if err != nil {
				return err
			}
			// The response never holds the address, so it is safe to print
			// whole; the request, which does, is not printed.
			return messageOut(a, res, func(w io.Writer) error { return a.tenantDetails(w, res.GetTenant()) })
		}),
	}
	cmd.Flags().StringVar(&dbEnv, "db-env", "", "name of the environment variable that holds the Postgres address of the tenant's database")
	cmd.Flags().StringVar(&dbFile, "db-file", "", "path of a file that holds the Postgres address of the tenant's database")
	cmd.MarkFlagsMutuallyExclusive("db-env", "db-file")
	return cmd
}

// newTenantList builds "tenant list".
func (a *app) newTenantList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the tenants this caller may see",
		Long: `List tenants ordered by name: every tenant for a reader or more on the
installation and for the local operator, otherwise only the tenants the key
holds a grant on. A tenant whose database is down is listed with its state and
the reason.`,
		Example: `  nopacs tenant list
  nopacs tenant list --all --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			tenants, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Tenant, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListTenantsResponse, error) {
					return c.Tenants.ListTenants(ctx, &v1.ListTenantsRequest{PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetTenants(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "tenants", tenants, more, func() error {
				withReason := false
				for _, t := range tenants {
					withReason = withReason || t.GetStateReason() != ""
				}
				header := []string{"NAME", "DATABASE", "STATE", "CREATED"}
				if withReason {
					header = append(header, "REASON")
				}
				rows := make([][]string, 0, len(tenants))
				for _, t := range tenants {
					row := []string{t.GetName(), databaseText(t.GetDatabaseType()), stateText(t.GetState()), a.when(t.GetCreateTime())}
					if withReason {
						row = append(row, output.Dash(t.GetStateReason()))
					}
					rows = append(rows, row)
				}
				return a.write(func(w io.Writer) error { return output.Table(w, a.style(), header, rows) })
			})
		}),
	}
	l.add(cmd)
	return cmd
}

// newTenantShow builds "tenant show".
func (a *app) newTenantShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one tenant, whatever its state",
		Long: `Show one tenant from the master database: its database type, its state and,
when it is not ready, why. It answers even when the tenant's own database is
down.`,
		Example: `  nopacs tenant show lab1`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetTenantResponse, error) {
				return c.Tenants.GetTenant(ctx, &v1.GetTenantRequest{Name: args[0]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.tenantDetails(w, res.GetTenant()) })
		}),
	}
}

// newTenantRemove builds "tenant remove".
func (a *app) newTenantRemove() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name> --yes",
		Short: "Unregister a tenant (its data is left where it is)",
		Long: `Unregister a tenant. This removes the tenant's registration and nothing else:
its SQLite file or its Postgres tables stay where they are, and the command
says where, so you can keep them, move them or delete them by hand. Deleting
data is a separate, later feature.

Without --yes nothing is sent: the command says what it would do and exits 2.`,
		Example: `  nopacs tenant remove lab1          # says what would happen, exits 2
  nopacs tenant remove lab1 --yes`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if !yes {
				return fault.New(fault.Invalid, fmt.Sprintf("nothing was removed: this would unregister tenant %q; its database and files stay where they are; run it again with --yes to do it", output.Clean(args[0])))
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The name is sent twice, as the contract requires, so a name
			// that was altered on the way refuses instead of removing
			// another tenant.
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemoveTenantResponse, error) {
				return c.Tenants.RemoveTenant(ctx, &v1.RemoveTenantRequest{Name: args[0], ConfirmName: args[0]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				return output.Details(w, a.style(), []output.Pair{
					{Label: "Tenant", Value: args[0] + " unregistered; no data was deleted"},
					{Label: "Database", Value: databaseText(res.GetDatabaseType())},
					{Label: "Data left at", Value: res.GetDataLocation()},
				})
			})
		}),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "really unregister the tenant; without it the command only says what it would do")
	return cmd
}
