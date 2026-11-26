package cli

import (
	"context"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// rebuildModePrefix is what every name of a rebuild job's mode starts with.
const rebuildModePrefix = "REBUILD_JOB_MODE_"

// newJobScrub builds "job scrub".
func (a *app) newJobScrub() *cobra.Command {
	var storage string
	var wait bool
	cmd := &cobra.Command{
		Use:   "scrub <tenant> [--storage NAME] [--wait]",
		Short: "Read every stored object again and compare it with the catalog",
		Long: `Make a scrub job: it reads each stored object of the tenant, or of one of its
storages, in full and compares its length and SHA-256 with what the catalog
and the scan's manifest say it should be, and lists the objects no scan names.
Every difference is read a second time before it is believed, then recorded as
a finding ("job findings"). A damaged or missing instance is marked, so a read
of it answers data loss at once instead of streaming bytes that are not what
was archived; a later scrub that reads it good clears the finding and the mark.

A scrub reports and marks only: it never deletes, moves, rewrites or repairs a
stored object. It reads at most the tenant's setting scrub.rate per storage,
behind reception. The server also makes one of each storage by itself, every
scrub.interval. A scrub of a storage is refused (exit 5) while another scrub of
it is queued or running.

The job's counts read: sent, the instances read good; refused, those with a
finding. "job show" adds its tallies: the findings by kind, those cleared, the
objects not online (never a finding) and the storages that could not be listed.
With --wait the command follows the job, as "job watch" does, with its exit
codes. It needs the operator role on the tenant.`,
		Example: `  nopacs job scrub lab1
  nopacs job scrub lab1 --storage main --wait
  nopacs job scrub lab1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			return a.createJob(ctx, cmd, args[0], &v1.CreateJobRequest{
				Tenant: args[0], Kind: &v1.CreateJobRequest_Scrub{Scrub: &v1.ScrubJob{Storage: storage}}}, wait)
		}),
	}
	cmd.Flags().StringVar(&storage, "storage", "", "scrub only the tenant's storage of this name (every storage when left out)")
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	return cmd
}

// newJobRebuild builds "job rebuild".
func (a *app) newJobRebuild() *cobra.Command {
	var mode, fromStorage, scratch string
	var yes, wait bool
	cmd := &cobra.Command{
		Use:   "rebuild <tenant> --mode compare|replace [--from-storage NAME] [--scratch-schema NAME] [--yes] [--wait]",
		Short: "Build the catalog again from the storages: compare it, or replace the one in use",
		Long: `Make a rebuild job: the server builds a shadow catalog from what the tenant's
storages hold (their correction records, manifests and metadata; no DICOM file
is opened), without touching the catalog in use, which keeps answering reads
and sends.

--mode compare tallies each difference between the shadow and the catalog in
use, then drops the shadow. It changes nothing and is the way to see what a
replace would do. --from-storage builds the shadow from one storage alone, as
if the tenant had no other, and compares it with that storage's scans (or, for
a replica, its primary's): the check before a switch, which must find no
difference.

--mode replace makes the shadow the catalog in use, in one transaction, once it
is complete. It needs the admin role on the tenant and --yes. Until the job
ends, however it ends, the tenant's receivers refuse new data (a sender tries
again; cancel the job to lift it at once), corrections and purges are refused,
and the build starts only once the receptions and corrections in flight have
ended. A scan the build leaves out (its metadata names a profile version the
tenant does not have, a committed delete hides it, its manifest cannot be read)
is removed from the catalog in use; its files stay on the storage. Run a
compare first and read its tallies. Without --yes the command asks on a
terminal for the tenant's name to be typed again, and otherwise refuses with
exit 2 and sends nothing.

On a tenant whose database is Postgres, --scratch-schema names the empty schema
the database's administrator made for the shadow; it is emptied again when the
job ends. On SQLite it must be left out.

The job's counts read: sent, the instances cataloged (or compared); refused,
those left out. "job show" adds its tallies, each counting scans, such as
left_out|deleted, differs|<table>|<added|removed|changed> and activated. Only
one rebuild of a tenant can be queued or running (exit 5). With --wait the
command follows the job, as "job watch" does, with its exit codes.`,
		Example: `  nopacs job rebuild lab1 --mode compare --wait
  nopacs job rebuild lab1 --mode compare --from-storage main
  nopacs job rebuild lab1 --mode replace --yes --wait
  nopacs job rebuild lab1 --mode compare --scratch-schema lab1_shadow --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			m, err := parseEnum[v1.RebuildJobMode]("mode", mode, v1.RebuildJobMode_name, rebuildModePrefix)
			if err != nil {
				return err
			}
			if m == v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE {
				if fromStorage != "" {
					return fault.New(fault.Invalid, "--from-storage is for --mode compare: a replace reads every storage")
				}
				if err := a.confirmTyped(yes, args[0], "started", "replace the catalog of tenant "+args[0]+" with one built from its storages: "+
					"its receivers refuse new data until the job ends; corrections and purges are refused meanwhile; "+
					"scans the build leaves out are removed from the catalog in use, their files staying on the storage; "+
					"run a compare first (--mode compare)"); err != nil {
					return err
				}
			}
			return a.createJob(ctx, cmd, args[0], &v1.CreateJobRequest{Tenant: args[0], Kind: &v1.CreateJobRequest_Rebuild{
				Rebuild: &v1.RebuildJob{Mode: m, FromStorage: fromStorage, ScratchSchema: scratch}}}, wait)
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&mode, "mode", "", "what to do with the catalog built: "+strings.Join(enumNames(v1.RebuildJobMode_name, rebuildModePrefix), " or ")+" (required)")
	fl.StringVar(&fromStorage, "from-storage", "", "compare: build the shadow from the one storage of this name, as if the tenant had no other")
	fl.StringVar(&scratch, "scratch-schema", "", "Postgres: the empty schema to build the shadow in; leave out on SQLite")
	fl.BoolVar(&yes, "yes", false, "confirm a replace without being asked")
	fl.BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	_ = cmd.MarkFlagRequired("mode")
	enumCompletion(cmd, "mode", v1.RebuildJobMode_name, rebuildModePrefix)
	return cmd
}

// createJob makes a job from a request and answers it, or follows it with
// wait, as "job create" does.
func (a *app) createJob(ctx context.Context, cmd *cobra.Command, tenantName string, req *v1.CreateJobRequest, wait bool) error {
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	// With --wait the feed's position is read before the job is made, so
	// none of its events can come before it.
	cursor := ""
	if wait {
		if cursor, err = a.feedCursor(ctx, c, cmd, tenantName); err != nil {
			return err
		}
	}
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateJobResponse, error) {
		return c.Jobs.CreateJob(ctx, req)
	})
	if err != nil {
		return err
	}
	if wait {
		return a.followJob(ctx, c, cmd, tenantName, res.GetJob().GetId(), cursor)
	}
	return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
}

// scrubJobDetails returns the labeled values of a scrub job.
func scrubJobDetails(s *v1.ScrubJob) []output.Pair {
	storage := s.GetStorage()
	if storage == "" {
		storage = "every storage"
	} else {
		storage = "storage " + storage
	}
	by := "a person"
	if s.GetScheduled() {
		by = "the server, on its schedule"
	}
	return []output.Pair{{Label: "Scrubs", Value: storage}, {Label: "Made by", Value: by}}
}

// rebuildJobDetails returns the labeled values of a rebuild job.
func rebuildJobDetails(r *v1.RebuildJob) []output.Pair {
	pairs := []output.Pair{{Label: "Rebuilds", Value: "the catalog, " + rebuildMode(r.GetMode())}}
	if r.GetFromStorage() != "" {
		pairs = append(pairs, output.Pair{Label: "From storage", Value: r.GetFromStorage()})
	}
	if r.GetScratchSchema() != "" {
		pairs = append(pairs, output.Pair{Label: "Scratch schema", Value: r.GetScratchSchema()})
	}
	return pairs
}

// rebuildMode writes a rebuild job's mode as a person reads it.
func rebuildMode(m v1.RebuildJobMode) string {
	switch m {
	case v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE:
		return "compare"
	case v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE:
		return "replace"
	}
	return "unknown"
}

// countLabels names the first two counts of a job as its kind means them: a
// send's sent and refused, a scrub's read good and with a finding, a
// rebuild's cataloged and left out.
func countLabels(j *v1.Job) (sent, refused string) {
	switch j.GetKind().(type) {
	case *v1.Job_Scrub:
		return "Read good", "With a finding"
	case *v1.Job_Rebuild:
		return "Cataloged", "Left out"
	case *v1.Job_Replicate:
		return "Copied", "Failed"
	case *v1.Job_Repair:
		return "Repaired", "Refused"
	case *v1.Job_Dispose:
		return "Deleted", "Failed"
	}
	return "Sent", "Refused"
}
