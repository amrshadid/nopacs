package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// previewDetails lists what a delete would remove as labeled values.
func (a *app) previewDetails(t *v1.CorrectionTarget, p *v1.DeletePreview) []output.Pair {
	c := p.GetCounts()
	pairs := []output.Pair{
		{Label: "Target", Value: targetText(t)},
		{Label: "Subjects", Value: strconv.FormatInt(c.GetSubjects(), 10)},
		{Label: "Sessions", Value: strconv.FormatInt(c.GetSessions(), 10)},
		{Label: "Scans", Value: strconv.FormatInt(c.GetScans(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(c.GetInstances(), 10)},
		{Label: "Bytes", Value: output.Bytes(c.GetBytes())},
		{Label: "Storages", Value: output.Dash(strings.Join(p.GetStorages(), ", "))},
		{Label: "Purged after", Value: a.whenOr(p.GetPurgeTime(), "-")},
	}
	for _, sp := range p.GetStoragePurges() {
		pairs = append(pairs, output.Pair{Label: "  " + output.Clean(sp.GetStorage()),
			Value: enumWord(sp.GetExpected().String(), "OBJECT_DISPOSITION_") + " (" + output.Clean(sp.GetWhy()) + ")"})
	}
	pairs = append(pairs,
		output.Pair{Label: "Quarantined files cleared", Value: strconv.FormatInt(p.GetQuarantined(), 10)},
		output.Pair{Label: "Receptions cleared", Value: strconv.FormatInt(p.GetReceptions(), 10)},
		output.Pair{Label: "Held after purge", Value: output.Dash(output.Clean(strings.Join(p.GetHeldAfterPurge(), ", ")))})
	for _, s := range p.GetSentTo() {
		pairs = append(pairs, output.Pair{Label: "Sent to " + output.Clean(s.GetSender()),
			Value: strconv.FormatInt(s.GetInstances(), 10) + " instances (out of reach: ask the peer to delete them)"})
	}
	jobs := strconv.Itoa(len(p.GetJobs()) + int(p.GetJobsNotListed()))
	pairs = append(pairs, output.Pair{Label: "Jobs that meet it", Value: jobs + " (of " + strconv.FormatInt(p.GetTenantJobs(), 10) + " not ended in the tenant)"})
	return pairs
}

// newCorrectionDelete builds "correction delete".
func (a *app) newCorrectionDelete() *cobra.Command {
	var reason, instance string
	var yes, preview bool
	cmd := &cobra.Command{
		Use:   "delete <tenant> <subject>[/<session>[/<scan>]] [--instance UID] --reason TEXT [--preview] [--yes]",
		Short: "Delete a subject, a session, a scan or one instance, after a preview",
		Long: `Delete archived data: a subject, one session of it, one scan, or with
--instance one instance of a scan, by its id as the catalog names it.

The command first asks the server what the delete would remove and shows it:
the counts and bytes, the storages that hold it and what each will do with a
removed object, the senders it was sent to (whose copies are out of reach), the
jobs that would meet it, the quarantined files and receptions its purge clears,
the labels arrivals stay held under, and when its bytes would be purged. Then,
without --yes, it asks for the target to be typed again on a terminal; with no
terminal it deletes nothing, shows the preview and exits 2. --preview shows the
preview and stops.

The delete carries the preview's token, so it acts on what was shown: when
anything changed since (data arrived under the target, a job was made), the
server refuses it (exit 5) and the command is run again. The data is hidden at
once; its bytes stay, unserved, until the tenant's grace period ends
(delete.grace), and "correction undo" shows it again until then.`,
		Example: `  nopacs correction delete lab1 sub-3f9a --reason "consent withdrawn" --preview
  nopacs correction delete lab1 sub-3f9a/ses-81c2 --reason "wrong patient at the scanner"
  nopacs correction delete lab1 sub-3f9a --reason "consent withdrawn" --yes --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			target, err := correctionTarget(args[1], instance)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			prev, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DeleteResponse, error) {
				return c.Corrections.Delete(ctx, &v1.DeleteRequest{Tenant: args[0], Target: target, Reason: reason, ValidateOnly: true})
			})
			if err != nil {
				return err
			}
			showPreview := func(w io.Writer) error {
				return output.Details(w, a.style(), a.previewDetails(target, prev.GetPreview()))
			}
			if preview {
				return messageOut(a, prev, showPreview)
			}
			if !yes && !a.opts.json {
				if err := a.write(showPreview); err != nil {
					return err
				}
			}
			if err := a.confirmTyped(yes, args[1], "deleted", fmt.Sprintf("delete %s (%d instances) from every read; its bytes are purged after the grace period",
				targetText(target), prev.GetPreview().GetCounts().GetInstances())); err != nil {
				return err
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DeleteResponse, error) {
				return c.Corrections.Delete(ctx, &v1.DeleteRequest{Tenant: args[0], Target: target, Reason: reason, Confirm: prev.GetPreview().GetConfirm()})
			})
			if err != nil {
				return err
			}
			cor := res.GetCorrection()
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "deleted %s: correction %s; its bytes are purged after %s; undo with: nopacs correction undo %s %s --reason TEXT\n",
					targetText(target), cor.GetId(), a.whenOr(cor.GetPurgeTime(), "-"), output.Clean(args[0]), cor.GetId())
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	cmd.Flags().StringVar(&instance, "instance", "", "delete only this instance of the scan, by its id")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the delete would remove, and delete nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the target to be typed again")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
