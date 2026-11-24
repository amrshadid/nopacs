package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// newTenantExport builds "tenant export".
func (a *app) newTenantExport() *cobra.Command {
	var out, keyOut string
	cmd := &cobra.Command{
		Use:   "export <name> --out FILE --key-out FILE",
		Short: "Hand a tenant to another installation: write its export and its transfer key",
		Long: `Hand the tenant to another installation. The server makes the tenant moved
(read only: its reads are served, every change is refused, its receivers
refuse), lets its writers finish, then streams the tenant's database without
its catalog, encrypted under a transfer key made for this export. The file is
written to --out and the transfer key, once, to --key-out, a file readable by
you alone: give both to whoever runs the other installation, apart, and load
them there with nopacs-server tenant import. The two installations' key files never
meet.

Once the export is whole the tenant stays moved here, whether or not this
command received the end of it. If the import never happens, the local
operator takes it back with nopacs tenant reclaim. An export that fails before
its end gives the tenant back at once. Neither file is ever written over one
that is there.

Stop the tenant's senders' work first if it must not be cut: a job's unit in
flight stops with its place kept.

Needs the admin role on the installation.`,
		Example: `  nopacs tenant export lab1 --out lab1.export --key-out lab1.key`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if out == "" || keyOut == "" {
				return fault.New(fault.Invalid, "--out and --key-out are required: the files the export and its transfer key are written to")
			}
			if out == keyOut {
				return fault.New(fault.Invalid, "--out and --key-out name the same file")
			}
			for _, p := range []string{out, keyOut} {
				if err := absent(p, "export"); err != nil {
					return err
				}
			}
			file, err := createPart(out, "export")
			if err != nil {
				return err
			}
			part := file.Name()
			defer func() {
				if file != nil {
					_ = file.Close()
				}
				if part != "" {
					_ = os.Remove(part)
				}
			}()
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			sum, key, err := exportTenantTo(ctx, c, args[0], file)
			defer clear(key)
			if err != nil {
				return err
			}
			if err := file.Sync(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
			}
			if err := file.Close(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
			}
			file = nil
			// The key first: an export kept without its key opens to nothing,
			// and the tenant is moved already, so the key must not be lost to
			// a file that appeared at --out meanwhile.
			if err := secret.WriteTransferKeyFile(keyOut, key); err != nil {
				return fault.Wrap(fault.KindOf(err), err, "the tenant "+output.Clean(args[0])+" is moved, and its transfer key could not be written: "+
					fault.Message(err)+"; take it back with nopacs tenant reclaim and export it again")
			}
			if err := publish(part, out, "export"); err != nil {
				return err
			}
			part = ""
			if a.opts.json {
				return a.write(func(w io.Writer) error { return output.JSONMessage(w, sum) })
			}
			return a.write(func(w io.Writer) error {
				return output.Details(w, a.style(), []output.Pair{
					{Label: "Tenant", Value: args[0] + " moved: read only here until an import takes it over, or nopacs tenant reclaim takes it back"},
					{Label: "Export", Value: fmt.Sprintf("%s: %d bytes, SHA-256 %s", out, sum.GetSizeBytes(), hex.EncodeToString(sum.GetSha256()))},
					{Label: "Transfer key", Value: keyOut + " (readable by you alone; send it apart from the export)"},
					{Label: "Import with", Value: "nopacs-server tenant import " + out + " --transfer-key " + keyOut + " --data-dir <the other installation's>"},
				})
			})
		}),
	}
	cmd.Flags().StringVar(&out, "out", "", "the file to write the export to, never replacing one")
	cmd.Flags().StringVar(&keyOut, "key-out", "", "the file to write the transfer key to, readable by you alone, never replacing one")
	_ = cmd.MarkFlagFilename("out")
	_ = cmd.MarkFlagFilename("key-out")
	return cmd
}

// exportTenantTo streams an export of the tenant name to w and returns its
// summary and transfer key. The stream has no deadline of its own: a large
// tenant takes as long as it takes. A stream that ends before its summary is
// an error, so a cut export is never taken for a whole one.
func exportTenantTo(ctx context.Context, c *client.Client, name string, w io.Writer) (*v1.BackupSummary, []byte, error) {
	st, err := c.Backups.ExportTenant(ctx, &v1.ExportTenantRequest{Tenant: name})
	if err != nil {
		return nil, nil, c.StreamError(ctx, "tenant export", err)
	}
	defer func() { _ = st.Close() }()
	var sum *v1.TenantExportSummary
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, c.StreamError(ctx, "tenant export", err)
		}
		switch k := m.GetKind().(type) {
		case *v1.ExportTenantResponse_Data:
			if sum != nil {
				return nil, nil, fault.New(fault.Internal, "the server sent bytes after the export's summary")
			}
			if _, err := w.Write(k.Data); err != nil {
				return nil, nil, fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
			}
		case *v1.ExportTenantResponse_Summary:
			sum = k.Summary
		}
	}
	if sum == nil || len(sum.GetTransferKey()) != secret.TransferKeySize {
		return nil, nil, fault.New(fault.Unavailable, "the export ended before its summary; the server gave the tenant back unless it wrote the summary, "+
			"in which case the tenant is moved: take it back with nopacs tenant reclaim")
	}
	return sum.GetSummary(), sum.GetTransferKey(), nil
}

// newTenantReclaim builds "tenant reclaim".
func (a *app) newTenantReclaim() *cobra.Command {
	return &cobra.Command{
		Use:   "reclaim <name>",
		Short: "Take back a tenant whose export no import followed",
		Long: `Take back a moved tenant: it becomes ready here again, and writes again. Only
while the owner record on each of its storages still names this installation
and the generation the tenant holds: once another installation imported it,
the record names that one, and the tenant is not taken back.

Only the local operator may run it, on this machine's socket (--data-dir): a
key is refused whatever its grants, since whether the import went ahead is
known to whoever runs this host.`,
		Example: `  nopacs tenant reclaim lab1 --data-dir /var/lib/nopacs`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ReclaimTenantResponse, error) {
				return c.Tenants.ReclaimTenant(ctx, &v1.ReclaimTenantRequest{Name: args[0]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.tenantDetails(w, res.GetTenant()) })
		}),
	}
}
