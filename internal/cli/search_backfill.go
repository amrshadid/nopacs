package cli

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newSearchBackfill builds "search backfill".
func (a *app) newSearchBackfill() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "backfill <tenant> <attribute> [--wait]",
		Short: "Fill a search attribute added after scans were archived",
		Long: `Make a search backfill job. An attribute added to a tenant's search tables
after scans were archived starts empty for those scans, and a search that names
it is refused until they are filled (the API's ListSearchAttributes says which
attributes are ready). The job reads each archived scan's metadata file, writes the scan's search row
as the archive writes it, then makes every attribute still being filled ready
to search and analyzes the table.

<attribute> is the DICOM keyword of an attribute being filled. The job
resumes where it stood after a restart, one runs at a time (exit 5 while
another has not ended), and "job cancel" stops it, the attributes staying
unready. A scan whose metadata cannot be read keeps no value for the
attribute and is counted refused: the job then ends partial. It needs the admin
role on the tenant. With --wait the command follows the job, as "job watch"
does, with its exit codes.`,
		Example: `  nopacs search backfill lab1 EchoTime
  nopacs search backfill lab1 EchoTime --wait --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// With --wait the feed's position is read before the job is made,
			// so none of its events can come before it.
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.BackfillAttributeResponse, error) {
				return c.Search.BackfillAttribute(ctx, &v1.BackfillAttributeRequest{Tenant: args[0], Attribute: args[1]})
			})
			if err != nil {
				return err
			}
			if wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	return cmd
}

// searchBackfillDetails returns the labeled values of a search backfill job.
func searchBackfillDetails(s *v1.SearchBackfillJob) []output.Pair {
	pairs := []output.Pair{{Label: "Fills", Value: "search attributes " + output.Clean(strings.Join(s.GetFields(), ", "))}}
	if s.GetScansSkipped() > 0 {
		pairs = append(pairs, output.Pair{Label: "Scans skipped", Value: strconv.FormatInt(s.GetScansSkipped(), 10)})
	}
	return pairs
}
