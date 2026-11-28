package cli

import (
	"context"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newDeidRerun builds "deid rerun".
func (a *app) newDeidRerun() *cobra.Command {
	var profileRef, reason string
	var preview, wait bool
	cmd := &cobra.Command{
		Use:   "rerun <tenant> [<subject>[/<session>[/<scan>]]] --profile <name>[@<version>] --reason TEXT [--preview] [--wait]",
		Short: "Run a newer profile version over archived data, a new revision of each file it changes",
		Long: `Run a newer profile version over data already archived de-identified. The
archived file is the only original there is, and what the version that made it
decided is already done in it, so only what the newer version decides otherwise,
and only where it lets through less, is done: an element it removes, empties,
replaces by a constant, generalizes or cleans. Each file that changes becomes a
new revision, as "correction edit" makes one: the superseded revision is kept
until the purge, "correction undo" gives it back before then, and the check
after runs over each new revision. A file the check holds is not written. A file
the newer version changes nothing in is skipped.

Refused, with a fixed detail ("job failures" lists each): a version that lets
through more than the one that made the file (loosens_lower_layer: it would need
the original, which de-identified data does not keep); an element the newer
version would hash, remap or shift that the older one left
(deid_failed/rerun_needs_original); a version with pixel rules; a file a hold
covers (held_by_hold). An instance archived as received passes with a note: a
profile run over it is a reception's work.

--preview writes nothing and counts what would change by element and action, with
no value, in the job's tallies ("job show"). The version is fixed when the job
is made (NAME alone is the latest). The job is a deid job: "job watch", "job
pause", "job cancel" and "job retry" work on it, and it goes on after a
restart. It needs the admin role on the tenant, never a masked key; --reason is
recorded in the audit log.` + deidLong,
		Example: `  nopacs deid rerun lab1 --profile research@5 --reason "the site now removes KVP" --preview --wait
  nopacs deid rerun lab1 sub-0017 --profile research@5 --reason "the site now removes KVP" --wait`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if profileRef == "" {
				return fault.New(fault.Invalid, "name the profile version to run with --profile, such as research@5")
			}
			if reason == "" && !preview {
				return fault.New(fault.Invalid, "say why with --reason")
			}
			ref, err := parseVersionArg(profileRef)
			if err != nil {
				return err
			}
			dj := &v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_RERUN, Profile: ref.Name, ProfileVersion: ref.Version, Reason: reason}
			if preview {
				dj.Mode, dj.Reason = v1.DeidJobMode_DEID_JOB_MODE_RERUN_PREVIEW, ""
			}
			if dj.SubjectLabel, dj.SessionLabel, dj.ScanLabel, err = parseTarget(args[1:]); err != nil {
				return err
			}
			return a.createDeidJob(ctx, cmd, args[0], dj, wait)
		}),
	}
	cmd.Flags().StringVar(&profileRef, "profile", "", "the profile version to run, <name>[@<version>]")
	cmd.Flags().StringVar(&reason, "reason", "", "why the profile is re-run; required unless --preview")
	cmd.Flags().BoolVar(&preview, "preview", false, "count what would change, by element and action, and write nothing")
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	return cmd
}
