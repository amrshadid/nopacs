package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// batchOutcomePrefix starts every name of a batch target's outcome.
const batchOutcomePrefix = "DELETE_BATCH_OUTCOME_"

// searchSet reads a --from-search query into the filters of "catalog
// scans": space-separated key=value pairs, each key once, among modality,
// description-prefix, receiver, archived-since and archived-until.
func searchSet(query string) (*downloadSet, error) {
	s := &downloadSet{}
	seen := map[string]bool{}
	for _, f := range strings.Fields(query) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || v == "" || seen[k] {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("--from-search: %q is not key=value, or its key is given twice", output.Clean(f)))
		}
		seen[k] = true
		switch k {
		case "modality":
			s.modality = v
		case "description-prefix":
			s.description = v
		case "receiver":
			s.receiver = v
		case "archived-since":
			s.archivedSince = v
		case "archived-until":
			s.archivedUntil = v
		default:
			return nil, fault.New(fault.Invalid, fmt.Sprintf("--from-search: %q is not a filter of catalog scans: use modality, description-prefix, receiver, archived-since or archived-until",
				output.Clean(k)))
		}
	}
	if !s.filtered() {
		return nil, fault.New(fault.Invalid, "--from-search names at least one filter, such as modality=MR")
	}
	return s, nil
}

// batchTargets resolves the targets of a batch delete: the references of
// the file, or the scans the search selects, each checked as a label path.
func (a *app) batchTargets(ctx context.Context, cmd *cobra.Command, c *client.Client, tenant, file, search string) ([]*v1.CorrectionTarget, error) {
	var refs [][]string
	switch {
	case (file == "") == (search == ""):
		return nil, fault.New(fault.Invalid, "name the targets with --file LIST or with --from-search QUERY, one of the two")
	case file != "":
		r, err := a.readRefsFile(file)
		if err != nil {
			return nil, err
		}
		refs = r
	default:
		s, err := searchSet(search)
		if err != nil {
			return nil, err
		}
		if refs, err = a.filteredScans(ctx, cmd, c, tenant, s); err != nil {
			return nil, err
		}
	}
	out := make([]*v1.CorrectionTarget, 0, len(refs))
	for _, ref := range refs {
		t, err := correctionTarget(strings.Join(ref, "/"), "")
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// batchPreviewOut writes a batch delete's preview: its totals, then one row
// per target with what keeps it from being deleted.
func (a *app) batchPreviewOut(w io.Writer, p *v1.DeleteBatchPreview) error {
	c := p.GetCounts()
	if err := output.Details(w, a.style(), []output.Pair{
		{Label: "Targets", Value: strconv.Itoa(len(p.GetItems()))},
		{Label: "Held (skipped)", Value: strconv.FormatInt(p.GetTargetsHeld(), 10)},
		{Label: "Kept by a rule (skipped)", Value: strconv.FormatInt(p.GetTargetsKept(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(c.GetInstances(), 10)},
		{Label: "Bytes", Value: output.Bytes(c.GetBytes())},
		{Label: "Purged after", Value: a.whenOr(p.GetPurgeTime(), "-") + " (each from its own delete)"},
	}); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	rows := make([][]string, 0, len(p.GetItems()))
	for _, it := range p.GetItems() {
		kept := "-"
		if it.GetSessionsUnderMinimum() > 0 {
			kept = a.whenOr(it.GetKeepUntil(), "-")
		}
		rows = append(rows, []string{targetText(it.GetTarget()), strconv.FormatInt(it.GetCounts().GetInstances(), 10), output.Bytes(it.GetCounts().GetBytes()),
			output.Dash(output.Clean(strings.Join(it.GetHolds(), ", "))), kept, output.Dash(output.Clean(strings.Join(it.GetReplicas(), ", "))),
			output.Dash(output.Clean(strings.Join(it.GetSentTo(), ", ")))})
	}
	return output.Table(w, a.style(), []string{"TARGET", "INSTANCES", "BYTES", "HOLDS", "KEPT UNTIL", "REPLICAS", "SENT TO"}, rows)
}

// newCorrectionDeleteBatch builds "correction delete-batch".
func (a *app) newCorrectionDeleteBatch() *cobra.Command {
	var reason, file, search string
	var yes, preview bool
	cmd := &cobra.Command{
		Use:   "delete-batch <tenant> (--file LIST | --from-search QUERY) --reason TEXT [--preview] [--yes]",
		Short: "Delete many subjects, sessions or scans with one reason, as a job",
		Long: `Delete many targets with one reason. Name them with --file, a file of
references, one <subject>[/<session>[/<scan>]] per line (blank lines and lines
starting with # ignored; - reads standard input), or with --from-search, the
filters of "catalog scans" as key=value pairs separated by spaces (modality,
description-prefix, receiver, archived-since, archived-until), each scan found
a target. A batch names at most the tenant's correction.batch-max targets
(10000 by default), each once and none inside another.

The command first asks the server for the preview and shows it: each target's
instances and bytes, the holds that cover it, the time a retention rule keeps
it until, the replicas its purge reaches and the senders it was sent to. Then,
without --yes, it asks for the tenant's name to be typed on a terminal; with no
terminal it deletes nothing and exits 2. --preview shows the preview and stops.

Confirmed, the batch is a job (kind delete_batch) that deletes each target in
turn by an ordinary delete with the reason given, so each is undone, purged and
listed as any delete ("correction list", "correction undo"). A target a hold
covers, one a rule keeps, one that changed since the preview, one gone, or one
another correction holds is skipped with that reason, never forced. "job watch"
follows the job, "job pause", "job resume" and "job cancel" stop and start it,
and "correction batch-targets" lists what became of each target. One batch runs
per tenant at a time.`,
		Example: `  nopacs correction delete-batch lab1 --file withdrawn.txt --reason "consent withdrawn" --preview
  nopacs correction delete-batch lab1 --from-search "modality=SR receiver=old-gateway" --reason "test data sent by mistake"
  nopacs correction delete-batch lab1 --file withdrawn.txt --reason "consent withdrawn" --yes --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			targets, err := a.batchTargets(ctx, cmd, c, args[0], file, search)
			if err != nil {
				return err
			}
			prev, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DeleteBatchResponse, error) {
				return c.Corrections.DeleteBatch(ctx, &v1.DeleteBatchRequest{Tenant: args[0], Targets: targets, Reason: reason, ValidateOnly: true})
			})
			if err != nil {
				return err
			}
			show := func(w io.Writer) error { return a.batchPreviewOut(w, prev.GetPreview()) }
			if preview {
				return messageOut(a, prev, show)
			}
			if !yes && !a.opts.json {
				if err := a.write(show); err != nil {
					return err
				}
			}
			if err := a.confirmTyped(yes, args[0], "deleted", fmt.Sprintf("delete %d targets of tenant %s (%d instances), each its bytes purged after the grace period",
				len(targets), args[0], prev.GetPreview().GetCounts().GetInstances())); err != nil {
				return err
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DeleteBatchResponse, error) {
				return c.Corrections.DeleteBatch(ctx, &v1.DeleteBatchRequest{Tenant: args[0], Targets: targets, Reason: reason,
					Confirm: prev.GetPreview().GetConfirm()})
			})
			if err != nil {
				return err
			}
			j := res.GetJob()
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "batch job %s is %s: %d targets, deleted one at a time\nfollow it with: nopacs job watch %s %s\nlist its targets with: nopacs correction batch-targets %s %s\n",
					output.Clean(j.GetId()), output.JobState(j.GetState()), j.GetDeleteBatch().GetTargets(), output.Clean(args[0]), output.Clean(j.GetId()),
					output.Clean(args[0]), output.Clean(j.GetId()))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with each delete (required)")
	cmd.Flags().StringVar(&file, "file", "", "a file of targets, one <subject>[/<session>[/<scan>]] per line; - reads standard input")
	cmd.Flags().StringVar(&search, "from-search", "", `the scans the filters of "catalog scans" select, as key=value pairs, such as "modality=MR archived-until=2024-01-01"`)
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the batch would delete, and delete nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the tenant's name to be typed")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newCorrectionBatchTargets builds "correction batch-targets".
func (a *app) newCorrectionBatchTargets() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:   "batch-targets <tenant> <job>",
		Short: "List a batch delete job's targets and what became of each",
		Long: `List the targets of a batch delete job, in the order given, each with its
outcome (pending, deleted, or skipped and why: held, kept, changed, gone,
overlap, refused) and the delete made for it, which "correction undo" undoes as
any delete.`,
		Example: `  nopacs correction batch-targets lab1 job_8Qm2
  nopacs correction batch-targets lab1 job_8Qm2 --all --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			items, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.DeleteBatchTarget, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListDeleteBatchTargetsResponse, error) {
					return c.Corrections.ListDeleteBatchTargets(ctx, &v1.ListDeleteBatchTargetsRequest{Tenant: args[0], JobId: args[1], PageSize: size,
						PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetTargets(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "targets", items, more, func() error {
				rows := make([][]string, 0, len(items))
				for _, t := range items {
					rows = append(rows, []string{strconv.FormatInt(t.GetPosition(), 10), targetText(t.GetTarget()),
						enumWord(t.GetOutcome().String(), batchOutcomePrefix), output.Dash(t.GetCorrectionId()),
						strconv.FormatInt(t.GetInstances(), 10)})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"#", "TARGET", "OUTCOME", "CORRECTION", "INSTANCES"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}
