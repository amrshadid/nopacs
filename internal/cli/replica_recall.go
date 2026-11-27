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

// recallDetails lists what a recall would ask for, or asked for, as
// labeled values.
func recallDetails(target string, r *v1.RecallResponse, asked bool) []output.Pair {
	pairs := []output.Pair{
		{Label: "Target", Value: output.Clean(target)},
		{Label: "Storages", Value: output.Dash(output.Clean(strings.Join(r.GetStorages(), ", ")))},
		{Label: "Objects", Value: strconv.FormatInt(r.GetObjects(), 10) + " (" + output.Bytes(r.GetBytes()) + ")"},
		{Label: "Online already", Value: strconv.FormatInt(r.GetObjectsOnline(), 10)},
		{Label: "Being recalled", Value: strconv.FormatInt(r.GetObjectsRecalling(), 10)},
	}
	ask := r.GetObjects() - r.GetObjectsOnline() - r.GetObjectsRecalling()
	if asked {
		ask = r.GetObjectsRequested()
	}
	label := "To ask for"
	if asked {
		label = "Asked for"
	}
	return append(pairs, output.Pair{Label: label, Value: strconv.FormatInt(ask, 10) + " (" + output.Bytes(r.GetBytesRequested()) + ")"})
}

// newRecall builds "recall".
func (a *app) newRecall() *cobra.Command {
	var storage string
	var days int
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "recall <tenant> <subject>[/<session>[/<scan>]] [--storage NAME] [--days N] [--dry-run] [--yes]",
		Short: "Bring a subject's, session's or scan's objects back from an archive tier",
		Long: `Ask the archive-tier storages that hold a subject, a session of it or a scan of
that to bring its objects back, so that later reads and jobs find them online.
A read of an object that is not online answers "not online" and starts no
recall: recalling costs what the store charges, so a person asks for it here.

The command first asks the server what the recall would ask for and shows it:
the storages, the target's objects and bytes there, those online already, those
being recalled already (a recall is shared: one per object, however many ask),
and those it would ask for. Then, without --yes, it asks for the target to be
typed again on a terminal; with no terminal it asks for nothing, shows the
preview and exits 2. --dry-run shows the preview and stops.

The recall carries the preview's token, so it asks for what was shown: when the
objects to ask for changed since, the server refuses it (exit 5) and the command
is run again. --storage names one archive-tier storage; without it every one
that holds the target is asked. --days is how long the objects stay readable,
1 to 365; without it each storage's own recall days. Days above a storage's own
need the admin role on the tenant; the operator role does the rest.`,
		Example: `  nopacs recall lab1 sub-3f9a --dry-run
  nopacs recall lab1 sub-3f9a/ses-81c2 --days 7
  nopacs recall lab1 sub-3f9a/ses-81c2/t1w --storage cold --yes --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			target, err := correctionTarget(args[1], "")
			if err != nil {
				return err
			}
			var d int32
			if cmd.Flags().Changed("days") {
				if d, err = boundedInt32("days", days, 1, 365); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			req := func(validate bool, confirm string) *v1.RecallRequest {
				return &v1.RecallRequest{Tenant: args[0], SubjectLabel: target.GetSubjectLabel(), SessionLabel: target.GetSessionLabel(),
					ScanLabel: target.GetScanLabel(), Storage: storage, Days: d, ValidateOnly: validate, Confirm: confirm}
			}
			prev, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RecallResponse, error) {
				return c.Replicas.Recall(ctx, req(true, ""))
			})
			if err != nil {
				return err
			}
			show := func(r *v1.RecallResponse, asked bool) func(w io.Writer) error {
				return func(w io.Writer) error { return output.Details(w, a.style(), recallDetails(args[1], r, asked)) }
			}
			if dryRun {
				return messageOut(a, prev, show(prev, false))
			}
			if !yes && !a.opts.json {
				if err := a.write(show(prev, false)); err != nil {
					return err
				}
			}
			ask := prev.GetObjects() - prev.GetObjectsOnline() - prev.GetObjectsRecalling()
			if ask > 0 {
				if err := a.confirmTyped(yes, args[1], "recalled", fmt.Sprintf("ask the store to recall %d objects (%s) of %s, at its charge",
					ask, output.Bytes(prev.GetBytesRequested()), output.Clean(args[1]))); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RecallResponse, error) {
				return c.Replicas.Recall(ctx, req(false, prev.GetConfirm()))
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, show(res, true))
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&storage, "storage", "", "the archive-tier storage to recall from; every one that holds the target when empty")
	fl.IntVar(&days, "days", 0, "how many days the objects stay readable, 1 to 365; each storage's own recall days when not given")
	fl.BoolVar(&dryRun, "dry-run", false, "show what the recall would ask for, and ask for nothing")
	fl.BoolVar(&yes, "yes", false, "do not ask for the target to be typed again")
	return cmd
}

// newReplicaVerify builds "replica verify".
func (a *app) newReplicaVerify() *cobra.Command {
	var count int
	var seed int64
	cmd := &cobra.Command{
		Use:   "verify <tenant> <storage> [--count N] [--seed S]",
		Short: "List a random sample of a storage's objects to check with tools outside the server",
		Long: `Answer a random sample of the instances a storage holds, each with its key on the
storage, its size and its SHA-256 as the catalog holds them, so a tool outside
the server can read those objects straight from the storage (under the storage's
directory, or its bucket and prefix), hash and decode them, and compare. For a
replica, only the objects of scans in step on it are sampled. The server reads
no object itself.

Each run draws a new sample unless --seed is given: the same seed over the same
storage answers the same sample, so a check can be repeated. --count is 1 to
10000 (200 when not given). The table, and --json, hold the keys, which hold
UIDs as received: keep the output with the care the archive itself needs. The
operator role on the tenant is needed.`,
		Example: `  nopacs replica verify lab1 copy --count 200 --seed 7
  nopacs replica verify lab1 copy --count 50 --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			n, err := boundedInt32("count", count, 1, 10000)
			if err != nil {
				return err
			}
			req := &v1.VerifySampleRequest{Tenant: args[0], Storage: args[1], Count: n}
			if cmd.Flags().Changed("seed") {
				req.Seed = &seed
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.VerifySampleResponse, error) {
				return c.Replicas.VerifySample(ctx, req)
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				// Tab-separated, one object a line, for a script to read.
				if _, err := fmt.Fprintf(w, "# %d of %d objects of %s\nkey\tsize\tsha256\trevision\tsubject\tsession\tscan\n",
					len(res.GetObjects()), res.GetPopulation(), output.Clean(args[1])); err != nil {
					return err
				}
				for _, o := range res.GetObjects() {
					if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%s\t%s\n", output.Clean(o.GetKey()), o.GetSizeBytes(), output.Clean(o.GetSha256()),
						o.GetRevision(), output.Clean(o.GetSubjectLabel()), output.Clean(o.GetSessionLabel()), output.Clean(o.GetScanLabel())); err != nil {
						return err
					}
				}
				return nil
			})
		}),
	}
	cmd.Flags().IntVar(&count, "count", 200, "how many instances to sample, 1 to 10000")
	cmd.Flags().Int64Var(&seed, "seed", 0, "a seed for the draw, so the same sample can be drawn again")
	return cmd
}
