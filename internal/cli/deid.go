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
	"github.com/amrshadid/nopacs/internal/fault"
)

// newDeid builds the deid noun: the jobs that judge the archive against a
// de-identification profile version, changing nothing.
func (a *app) newDeid() *cobra.Command {
	return group("deid", "Check the archive against its profile versions, dry-run a version, or find a value's codes",
		`Judge archived instances against a de-identification profile version, changing
nothing: no catalog row, no stored byte, no lookup table. Each is a job of the
kind deid, which "job watch", "job show", "job failures", "job cancel" and "job
retry" work on as on any job; "job show" adds its counts per rule and per
reason. A target is a subject, a session of it (<subject>/<session>) or a scan
of that (<subject>/<session>/<scan>); with none, the whole tenant. Both need the
operator role on the tenant.

"deid code" is the forward lookup: from a value a person has (a Patient ID) to
the codes the archive gives it, such as the subject's label, for a withdrawal
of consent. It needs the admin role.`,
		a.newDeidCheck(), a.newDeidDryRun(), a.newDeidCode())
}

// parseTarget reads an optional target: none for the whole tenant, or a
// subject, a session or a scan by labels.
func parseTarget(args []string) (subject, session, scan string, err error) {
	if len(args) == 0 {
		return "", "", "", nil
	}
	parts, err := splitRef(args[0], 1, 3, "<subject>, <subject>/<session> or <subject>/<session>/<scan>")
	if err != nil {
		return "", "", "", err
	}
	parts = append(parts, "", "")
	return parts[0], parts[1], parts[2], nil
}

// createDeidJob makes the deid job and answers it, or follows it with
// wait, as "job create" does.
func (a *app) createDeidJob(ctx context.Context, cmd *cobra.Command, tenantName string, dj *v1.DeidJob, wait bool) error {
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	cursor := ""
	if wait {
		if cursor, err = a.feedCursor(ctx, c, cmd, tenantName); err != nil {
			return err
		}
	}
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateJobResponse, error) {
		return c.Jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: tenantName, Kind: &v1.CreateJobRequest_Deid{Deid: dj}})
	})
	if err != nil {
		return err
	}
	if wait {
		return a.followJob(ctx, c, cmd, tenantName, res.GetJob().GetId(), cursor)
	}
	return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
}

// deidLong is what the deid commands say of their answers and exit codes.
const deidLong = `
The answer is the job as queued. With --wait the command follows it to its end
as "job watch" does, then shows it with its counts per rule and per reason, and
its exit code is that of "job watch": 0 when every instance passed (some
perhaps with a note), 1 when some did not (the job ended partial), failed or was
canceled. "job failures <tenant> <id> --warnings" lists each instance that did
not pass, or passed with a note, and why; no value from a file is shown
anywhere, only names of rules, actions, reasons, element tags and SOP classes.`

// newDeidCheck builds "deid check".
func (a *app) newDeidCheck() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "check <tenant> [<subject>[/<session>[/<scan>]]] [--wait]",
		Short: "Verify archived instances against the profile version each records",
		Long: `Verify each archived instance of the target against the profile version its
catalog row records, as a job, by what that version would leave of each element
the archive holds: no element it removes (a private element it does not keep
among them), no value in an element it empties, nothing but what the IOD keeps
of an element it removes or empties unless required; Patient Identity Removed
(0012,0062) YES; De-identification Method (0012,0063) naming the version; and no
UID outside the standard's registry that a file the tenant received carried.
Only Study, Series and SOP Instance UIDs the catalog holds are recognized: an
original Frame of Reference UID, or a Referenced UID of an instance never
archived, is not. A rule whose condition the archive cannot answer (see
"deid dry-run") makes a note, check|undecidable, not a finding. An instance
archived as received records no version and passes with a note.` + deidLong,
		Example: `  nopacs deid check lab1 --wait
  nopacs deid check lab1 sub-0017/ses-2026-03-02 --json`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			sub, ses, scan, err := parseTarget(args[1:])
			if err != nil {
				return err
			}
			return a.createDeidJob(ctx, cmd, args[0], &v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_CHECK,
				SubjectLabel: sub, SessionLabel: ses, ScanLabel: scan}, wait)
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	return cmd
}

// newDeidDryRun builds "deid dry-run".
func (a *app) newDeidDryRun() *cobra.Command {
	var wait, own bool
	cmd := &cobra.Command{
		Use:   "dry-run <tenant> (<profile>[@<version>] | --own) [<subject>[/<session>[/<scan>]]] [--wait]",
		Short: "Run a profile version over archived instances and say what it would hold",
		Long: `Run a profile version over each archived instance of the target in memory, as
"profile try" runs it over sample files, and say what it would archive and what
it would hold and why. The version is fixed when the job is made (NAME alone is
the latest). The run uses keys made for it, never the tenant's, and reads no
lookup table, so a value a lookup rule would replace makes a note, not a
verdict. It judges de-identification alone: no receiver's checks run. With
--own, each instance is run under the version its catalog row records, and one
archived as received passes with a note.

Conditions read the instance's reception: its receiver, its calling AE title
when its files came with exactly one (otherwise none), and the called AE title
of its receiver as configured now. A condition on Modality, SOP Class UID,
Manufacturer, Model Name or Software Versions reads the archived file, which
says what the file said as received only where the version that archived it
kept that element; on de-identified data such a condition may decide
otherwise than at reception. A file larger than 64 MiB is not read (a note),
and an image a pixel rule decodes is bounded by the lower of deid.pixel-limit
and 256 MiB.` + deidLong,
		Example: `  nopacs deid dry-run lab1 research@4 sub-0017 --wait
  nopacs deid dry-run lab1 research --json
  nopacs deid dry-run lab1 --own sub-0017/ses-2026-03-02`,
		Args: rangeArgs(1, 3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			dj := &v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN}
			rest := args[1:]
			if !own {
				if len(rest) == 0 {
					return fault.New(fault.Invalid, "name the profile version to run, such as research@4, or --own")
				}
				ref, err := parseVersionArg(rest[0])
				if err != nil {
					return err
				}
				dj.Profile, dj.ProfileVersion, rest = ref.Name, ref.Version, rest[1:]
			}
			if len(rest) > 1 {
				return fault.New(fault.Invalid, "at most one target follows the profile")
			}
			var err error
			if dj.SubjectLabel, dj.SessionLabel, dj.ScanLabel, err = parseTarget(rest); err != nil {
				return err
			}
			return a.createDeidJob(ctx, cmd, args[0], dj, wait)
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	cmd.Flags().BoolVar(&own, "own", false, "run each instance under the version its catalog row records")
	return cmd
}

// deidMode writes a deid job's mode as a person reads it.
func deidMode(m v1.DeidJobMode) string {
	switch m {
	case v1.DeidJobMode_DEID_JOB_MODE_CHECK:
		return "check"
	case v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN:
		return "dry run"
	}
	return "unknown"
}

// deidTarget writes what a deid job judges: its labels, or the whole
// tenant.
func deidTarget(d *v1.DeidJob) string {
	if d.GetSubjectLabel() == "" {
		return "the whole tenant"
	}
	return output.JobTarget(&v1.SendJob{SubjectLabel: d.GetSubjectLabel(), SessionLabel: d.GetSessionLabel(), ScanLabel: d.GetScanLabel()})
}

// deidVersion writes the version a deid job runs.
func deidVersion(d *v1.DeidJob) string {
	if d.GetProfile() == "" {
		return "each instance's own"
	}
	return d.GetProfile() + "@" + strconv.Itoa(int(d.GetProfileVersion()))
}

// jobKindText writes a job's kind and what it acts on, for a list's two
// columns: its sender or mode, and its target.
func jobKindText(j *v1.Job) (string, string) {
	if d := j.GetDeid(); d != nil {
		return "deid " + deidMode(d.GetMode()), deidTarget(d)
	}
	if ij := j.GetImportJob(); ij != nil {
		// The path can name a person and is shown by "job show" alone.
		return "import " + ij.GetReceiver(), "a directory"
	}
	if sj := j.GetScrub(); sj != nil {
		if sj.GetStorage() == "" {
			return "scrub", "every storage"
		}
		return "scrub", "storage " + sj.GetStorage()
	}
	if rj := j.GetRebuild(); rj != nil {
		return "rebuild " + rebuildMode(rj.GetMode()), "the catalog"
	}
	return j.GetSend().GetSender(), output.JobTarget(j.GetSend())
}

// jobOut writes one job as labeled values and, for a deid job, its tallies
// as a table: the summary of what it found.
func (a *app) jobOut(w io.Writer, j *v1.Job) error {
	if err := output.Details(w, a.style(), a.jobDetails(j)); err != nil {
		return err
	}
	if len(j.GetTallies()) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(j.GetTallies()))
	for _, t := range j.GetTallies() {
		kind, rest, _ := strings.Cut(t.GetKey(), "|")
		rows = append(rows, []string{kind, strings.ReplaceAll(rest, "|", "  "), strconv.FormatInt(t.GetCount(), 10)})
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return output.Table(w, a.style(), []string{"KIND", "WHAT", "COUNT"}, rows)
}
