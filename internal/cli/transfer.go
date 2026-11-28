package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newTransfer builds the transfer noun.
func (a *app) newTransfer() *cobra.Command {
	return group("transfer", "Copy or move a subject or a session into another tenant",
		`A transfer reads the archived bytes of a subject or a session of one tenant
and hands them to a receiver of type job of another, so the target's own
routing, de-identification profile, naming rule and checks apply and its labels
are its own. It is a job of the source tenant, in four steps the database
saves, so a restart goes on where it stopped and copies nothing twice: listing,
copying (each byte checked against the catalog's SHA-256), confirming (each
instance found in the target and its received checksum compared with the
source's) and, for a move, deleting: the source's ordinary delete with the
reason "moved to <target>", made only when every instance is confirmed and the
source is unchanged. A hold or a retention rule that stops the delete ends the
job partial, and the copy stays.

An instance archived as received is copied only into a receiver that
de-identifies; any other is failed, as a send refuses it. The target records,
for each instance, the source tenant, its labels there (left out for an
instance archived as received), its revision there and the job; never a UID of
the source and never an original identifier. A transfer needs admin on the
source and writer on the target, and never a masked key.`,
		a.newTransferCreate(), a.newTransferShow(), a.newTransferList())
}

// transferStepText returns a step's name in lowercase.
func transferStepText(s v1.TransferStep) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "TRANSFER_STEP_"))
}

// transferDetails lists one transfer as labeled values.
func (a *app) transferDetails(t *v1.Transfer) []output.Pair {
	j := t.GetJob()
	what := t.GetSubjectLabel()
	if t.GetSessionLabel() != "" {
		what += "/" + t.GetSessionLabel()
	}
	mode := "copy"
	if t.GetMove() {
		mode = "move"
	}
	c, it := j.GetCounts(), t.GetItems()
	pairs := []output.Pair{
		{Label: "ID", Value: output.Clean(j.GetId())},
		{Label: "Transfers", Value: output.Clean(what) + " as a " + mode + " to " + output.Clean(t.GetTargetTenant()) + " through " + output.Clean(t.GetReceiver())},
		{Label: "State", Value: output.JobState(j.GetState())},
		{Label: "Step", Value: transferStepText(t.GetStep())},
		{Label: "Instances", Value: fmt.Sprintf("%d listed, %d bytes", c.GetInstances(), c.GetBytes())},
		{Label: "Confirmed in the target", Value: strconv.FormatInt(it.GetConfirmed(), 10)},
		{Label: "Copied, not yet found", Value: strconv.FormatInt(it.GetCopied(), 10)},
		{Label: "Not yet copied", Value: strconv.FormatInt(it.GetListed(), 10)},
		{Label: "Failed", Value: strconv.FormatInt(it.GetFailed(), 10)},
		{Label: "Reason", Value: output.Dash(output.Clean(t.GetReason()))},
		{Label: "Ended because", Value: output.Dash(output.Clean(j.GetReason()))},
		{Label: "Made by", Value: output.Dash(output.Clean(j.GetCreatedByKeyId()))},
		{Label: "Made", Value: a.when(j.GetCreateTime())},
		{Label: "Ended", Value: a.whenOr(j.GetEndTime(), "-")},
	}
	if t.GetMove() {
		pairs = append(pairs, output.Pair{Label: "Delete of the source", Value: output.Dash(output.Clean(t.GetDeleteCorrectionId()))})
	}
	return pairs
}

// transferPreviewOut writes a transfer's preview.
func (a *app) transferPreviewOut(w io.Writer, p *v1.TransferPreview) error {
	refused := "not counted (more than 100000 instances)"
	if p.GetRefusalsCounted() {
		refused = strconv.FormatInt(p.GetRefusedInstances(), 10)
	}
	deid := "no: archived as received, so only de-identified instances are copied"
	if p.GetReceiverDeidentifies() {
		deid = "yes"
	}
	return output.Details(w, a.style(), []output.Pair{
		{Label: "Sessions", Value: strconv.FormatInt(p.GetSessions(), 10)},
		{Label: "Scans", Value: strconv.FormatInt(p.GetScans(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(p.GetInstances(), 10)},
		{Label: "Bytes", Value: output.Bytes(p.GetBytes())},
		{Label: "Through the receiver", Value: output.Clean(p.GetReceiver())},
		{Label: "The receiver de-identifies", Value: deid},
		{Label: "Instances it would refuse", Value: refused},
	})
}

// newTransferCreate builds "transfer create".
func (a *app) newTransferCreate() *cobra.Command {
	var to, via, reason string
	var move, preview, yes bool
	cmd := &cobra.Command{
		Use:   "create <tenant> <subject>[/<session>] --to TENANT --reason TEXT [--move] [--via RECEIVER] [--preview] [--yes]",
		Short: "Copy or move a subject or a session into another tenant, as a job",
		Long: `Copy a subject, or one of its sessions, of the tenant into the tenant named by
--to, or with --move move it: the source is deleted once every instance is
confirmed in the target. The target's receiver is its only enabled receiver of
type job, or the one --via names.

The command first asks the server for the preview and shows it: what the
transfer covers, the receiver it goes through, whether that receiver
de-identifies and how many instances the rule of what may leave would fail.
--preview shows it and stops. A copy then starts at once. A move first asks, on
a terminal, for the tenant's name to be typed, or takes --yes; with no terminal
and no --yes it moves nothing and exits 2.

The transfer is a job (kind transfer) of the source tenant: "transfer show"
and "job watch" follow it, "job failures" lists each instance that failed and
why, and "job cancel" stops it (what was copied stays in the target).`,
		Example: `  nopacs transfer create lab1 sub-0017/ses-1 --to lab2 --reason "study belongs to the trial in lab2" --preview
  nopacs transfer create lab1 sub-0017 --to lab2 --reason "consent moved to lab2" --move --yes
  nopacs transfer create lab1 sub-0017/ses-1 --to lab2 --via from-lab1 --reason "second reading"`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			if to == "" {
				return fault.New(fault.Invalid, "--to names the tenant to transfer into")
			}
			parts, err := splitRef(args[1], 1, 2, "<subject> or <subject>/<session>")
			if err != nil {
				return err
			}
			req := &v1.CreateTransferRequest{Tenant: args[0], SubjectLabel: parts[0], TargetTenant: to, Receiver: via, Move: move, Reason: reason}
			if len(parts) > 1 {
				req.SessionLabel = parts[1]
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			prev := proto.Clone(req).(*v1.CreateTransferRequest)
			prev.ValidateOnly = true
			pv, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateTransferResponse, error) {
				return c.Transfers.CreateTransfer(ctx, prev)
			})
			if err != nil {
				return err
			}
			show := func(w io.Writer) error { return a.transferPreviewOut(w, pv.GetPreview()) }
			if preview {
				return messageOut(a, pv, show)
			}
			if move {
				if !yes && !a.opts.json {
					if err := a.write(show); err != nil {
						return err
					}
				}
				if err := a.confirmTyped(yes, args[0], "moved", fmt.Sprintf("move %d instances of %s from tenant %s into tenant %s and delete them from %s once each is confirmed there",
					pv.GetPreview().GetInstances(), args[1], args[0], to, args[0])); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateTransferResponse, error) {
				return c.Transfers.CreateTransfer(ctx, req)
			})
			if err != nil {
				return err
			}
			t := res.GetTransfer()
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "transfer %s is %s: %d instances to tenant %s\nfollow it with: nopacs transfer show %s %s\n", output.Clean(t.GetJob().GetId()),
					output.JobState(t.GetJob().GetState()), pv.GetPreview().GetInstances(), output.Clean(t.GetTargetTenant()), output.Clean(args[0]), output.Clean(t.GetJob().GetId()))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the tenant to transfer into (required)")
	cmd.Flags().StringVar(&via, "via", "", "the target's receiver of type job to go through; default its only enabled one")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the job and with a move's delete (required)")
	cmd.Flags().BoolVar(&move, "move", false, "delete the source once every instance is confirmed in the target")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the transfer would do and stop")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the tenant's name before a move")
	return cmd
}

// newTransferShow builds "transfer show".
func (a *app) newTransferShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <id>",
		Short: "Show one transfer: where it goes, its step and how many instances are in each state",
		Long: `Show one transfer of the tenant: what it transfers and where, its job's state,
the step it has reached, and how many of its instances are confirmed in the
target, copied and not yet found there, not yet copied, and failed. "job
failures" lists the failed ones with the reason of each.`,
		Example: `  nopacs transfer show lab1 job_3Tq9`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetTransferResponse, error) {
				return c.Transfers.GetTransfer(ctx, &v1.GetTransferRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return output.Details(w, a.style(), a.transferDetails(res.GetTransfer())) })
		}),
	}
}

// newTransferList builds "transfer list".
func (a *app) newTransferList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:     "list <tenant>",
		Short:   "List a tenant's transfers, newest first",
		Long:    `List a tenant's transfers, newest first, each with what it transfers, where it goes, its state and step, and its instances confirmed of those listed.`,
		Example: "  nopacs transfer list lab1",
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			ts, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Transfer, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListTransfersResponse, error) {
					return c.Transfers.ListTransfers(ctx, &v1.ListTransfersRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetTransfers(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "transfers", ts, more, func() error {
				rows := make([][]string, 0, len(ts))
				for _, t := range ts {
					what := t.GetSubjectLabel()
					if t.GetSessionLabel() != "" {
						what += "/" + t.GetSessionLabel()
					}
					mode := "copy"
					if t.GetMove() {
						mode = "move"
					}
					rows = append(rows, []string{output.Clean(t.GetJob().GetId()), output.Clean(what), mode, output.Clean(t.GetTargetTenant()),
						output.JobState(t.GetJob().GetState()), transferStepText(t.GetStep()),
						fmt.Sprintf("%d/%d", t.GetItems().GetConfirmed(), t.GetJob().GetCounts().GetInstances()), a.when(t.GetJob().GetCreateTime())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "TRANSFERS", "MODE", "TO", "STATE", "STEP", "CONFIRMED", "MADE"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}
