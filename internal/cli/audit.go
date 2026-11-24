package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newAudit builds the audit noun.
func (a *app) newAudit() *cobra.Command {
	return group("audit", "Read, verify and export the audit log",
		`Every request to the API writes one entry to an audit log, refused requests
included. What concerns the installation is in the installation's log; what
concerns a tenant is in that tenant's. Reading a log needs the admin role on its
scope, and the reading is itself recorded.

The server seals every log every audit.seal-interval: each entry gets its seq,
its place in a hash chain, and each pass ends in a checkpoint signed with a key
derived from the installation key file. verify checks the chain and the
signatures, checkpoints lists them, export writes a log with what checks it
outside the server, and public-key writes the key that checks the signatures,
to keep outside the installation.`,
		a.newAuditList(), a.newAuditVerify(), a.newAuditCheckpoints(), a.newAuditExport(), a.newAuditPublicKey())
}

// methodText shortens a method's full name to "TenantService/CreateTenant".
func methodText(m string) string { return output.Method(m) }

// whoText says who made a request.
func whoText(e *v1.AuditEntry) string { return output.Who(e) }

// newAuditList builds "audit list".
func (a *app) newAuditList() *cobra.Command {
	var tenant, keyID, outcome, since, until string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list [--tenant NAME] [--key-id ID] [--outcome done|refused|failed] [--since TIME] [--until TIME]",
		Short: "List audit entries, newest first",
		Long: `List the entries of the installation's audit log, or with --tenant of that
tenant's, newest first, optionally only those of one key (--key-id takes the key's
public id), one outcome, or a span of time.

A TIME is a date (2026-11-17, midnight in your zone), a time with its offset
(2026-11-17T09:30:00+03:00) or an age (24h, 7d: that long before now). --since
is inclusive and --until is exclusive. Times in the table are shown in your
zone with the offset.`,
		Example: `  nopacs audit list --since 24h
  nopacs audit list --outcome refused --since 2026-11-01
  nopacs audit list --tenant lab1 --key-id k8Zp2QvXn5Rt --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			req := &v1.ListAuditEntriesRequest{Scope: optionalScope(tenant), KeyId: keyID}
			if cmd.Flags().Changed("key-id") {
				if err := checkKeyID(keyID); err != nil {
					return err
				}
			}
			if outcome != "" {
				o, err := parseOutcome(outcome)
				if err != nil {
					return err
				}
				req.Outcome = o
			}
			if req.StartTime, req.EndTime, err = a.timeRange(since, until); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			entries, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.AuditEntry, string, error) {
				page := &v1.ListAuditEntriesRequest{
					Scope: req.GetScope(), KeyId: req.GetKeyId(), Outcome: req.GetOutcome(),
					StartTime: req.GetStartTime(), EndTime: req.GetEndTime(), PageSize: size, PageToken: token,
				}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListAuditEntriesResponse, error) {
					return c.Audit.ListAuditEntries(ctx, page)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetEntries(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "entries", entries, more, func() error {
				rows := make([][]string, 0, len(entries))
				for _, e := range entries {
					rows = append(rows, []string{a.when(e.GetTime()), whoText(e), methodText(e.GetMethod()), output.Dash(e.GetTenant()), output.Dash(e.GetObject()), outcomeText(e.GetOutcome()), e.GetRequestId(), output.Dash(e.GetReason())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"TIME", "WHO", "METHOD", "TENANT", "OBJECT", "OUTCOME", "REQUEST ID", "REASON"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "read this tenant's log (default: the installation's log)")
	cmd.Flags().StringVar(&keyID, "key-id", "", "only entries of requests that carried the key with this id (the key's public id, never the key itself)")
	cmd.Flags().StringVar(&outcome, "outcome", "", "only entries with this outcome: done, refused or failed")
	cmd.Flags().StringVar(&since, "since", "", "only entries at or after this time: a date, a time with its offset, or an age such as 24h")
	cmd.Flags().StringVar(&until, "until", "", "only entries before this time, written like --since")
	_ = cmd.RegisterFlagCompletionFunc("outcome", cobra.FixedCompletions([]string{"done", "refused", "failed"}, cobra.ShellCompDirectiveNoFileComp))
	l.add(cmd)
	return cmd
}
