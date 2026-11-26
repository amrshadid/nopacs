package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxRepairFindings is the most findings one repair names.
const maxRepairFindings = 1000

// repairKinds are the finding kinds a repair mends: an orphan is an object
// the catalog does not name, which a replica cannot put back.
var repairKinds = []v1.ScrubFindingKind{
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING,
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED,
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_DISAGREEING,
	v1.ScrubFindingKind_SCRUB_FINDING_KIND_MANIFEST_DAMAGED,
}

// newRepair builds "repair".
func (a *app) newRepair() *cobra.Command {
	var findings, instances []string
	var all, wait bool
	var from string
	cmd := &cobra.Command{
		Use:   "repair <tenant> <storage> (--finding ID... | --instance ID... | --all) [--from REPLICA] [--wait]",
		Short: "Put a primary's missing or damaged objects back from a replica",
		Long: `Queue a repair job that puts back on a primary storage the objects a scrub found
missing or damaged there. Each is read whole from a replica that is in step for
its scan and checked against the catalog's length and SHA-256; the damaged
object's size and found checksum are recorded on its finding first; exactly the
bytes the catalog names are put at the same key; they are read back; and the
finding is cleared in one step with an audit entry. An object that no replica
holds good is left as it is and counted refused ("no good copy"): "correction
replace" from a file sent again is then the way to mend it.

Name what to mend with --finding (an id from "job findings"; repeat for more, at
most 1000), --instance (an instance id; the command finds its findings on the
storage that hold now), or --all (every finding of a kind a repair mends that
holds on the storage now). --from names the replica to read from; without it any
replica in step is read, online ones first.

It needs the admin role on the tenant, since a primary is written to. A repair of
a replica is refused: the server copies a replica's objects again by itself.
A repair of the same storage, replica and findings already queued or running is
answered instead of starting another.

The answer is the job as queued. --wait follows it as "job watch" does, with the
same exit codes; interrupting the wait stops only the watching.`,
		Example: `  nopacs job findings lab1 --storage main --kind damaged
  nopacs repair lab1 main --finding 01jd0000000000000000000001
  nopacs repair lab1 main --instance 7a3c9d --from copy --wait
  nopacs repair lab1 main --all`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if len(findings) > maxRepairFindings {
				return fault.New(fault.Invalid, fmt.Sprintf("at most %d findings in one repair: use --all, or repair in parts", maxRepairFindings))
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			ids := findings
			if len(instances) > 0 {
				found, err := findingsOfInstances(ctx, c, cmd, args[0], args[1], instances)
				if err != nil {
					return err
				}
				ids = found
			}
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RepairResponse, error) {
				return c.Replicas.Repair(ctx, &v1.RepairRequest{Tenant: args[0], Storage: args[1], From: from, Findings: ids, All: all})
			})
			if err != nil {
				return err
			}
			if wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.startedJob(w, args[0], res.GetJob()) })
		}),
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&findings, "finding", nil, `the id of a finding to mend, as "job findings" lists it; repeat for more`)
	fl.StringArrayVar(&instances, "instance", nil, "the id of an instance whose findings on the storage to mend; repeat for more")
	fl.BoolVar(&all, "all", false, "mend every finding that holds on the storage now, of a kind a repair mends")
	fl.StringVar(&from, "from", "", "the replica to read the good bytes from; any replica in step when empty")
	fl.BoolVar(&wait, "wait", false, `follow the repair job until it ends, as "job watch" does, with its exit codes`)
	cmd.MarkFlagsMutuallyExclusive("finding", "instance", "all")
	cmd.MarkFlagsOneRequired("finding", "instance", "all")
	return cmd
}

// findingsOfInstances finds the ids of the findings on a storage that hold now
// for the given instances. An instance with none is an error that names it, so
// a repair never silently mends less than was asked.
func findingsOfInstances(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, storage string, instances []string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	token := ""
	for {
		res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListScrubFindingsResponse, error) {
			return c.Corrections.ListScrubFindings(ctx, &v1.ListScrubFindingsRequest{Tenant: tenant, Kinds: repairKinds, Storage: storage, PageSize: maxPageSize, PageToken: token})
		})
		if err != nil {
			return nil, err
		}
		for _, f := range res.GetFindings() {
			if slices.Contains(instances, f.GetInstanceId()) {
				ids = append(ids, f.GetId())
				seen[f.GetInstanceId()] = true
			}
		}
		if res.GetNextPageToken() == "" || res.GetNextPageToken() == token {
			break
		}
		token = res.GetNextPageToken()
	}
	for _, in := range instances {
		if !seen[in] {
			return nil, fault.New(fault.NotFound, fmt.Sprintf("storage %s has no finding that holds for instance %s", output.Clean(storage), output.Clean(in)))
		}
	}
	return ids, nil
}
