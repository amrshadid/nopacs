package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newJobPause builds "job pause".
func (a *app) newJobPause() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <tenant> <id>",
		Short: "Pause a batch delete job until it is resumed",
		Long: `Pause a queued or running job: it stays in its state, starts no more work,
and goes on from where it stopped once resumed ("job resume"). The target being
deleted when the pause lands finishes. Only a batch delete job pauses in this
version; another kind, or a job that ended, is refused (exit 5). A job already
paused is answered as it stands. Needs the operator role on the tenant.`,
		Example: `  nopacs job pause lab1 job_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.PauseJobResponse, error) {
				return c.Jobs.PauseJob(ctx, &v1.PauseJobRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return a.pausedOut(res, res.GetJob())
		}),
	}
}

// newJobResume builds "job resume".
func (a *app) newJobResume() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <tenant> <id>",
		Short: "Let a paused batch delete job go on",
		Long: `Let a paused job go on from where it stopped. A job not paused is answered as
it stands. A batch delete resumed deletes again, so it needs the admin role on
the tenant, and a masked key is refused.`,
		Example: `  nopacs job resume lab1 job_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ResumeJobResponse, error) {
				return c.Jobs.ResumeJob(ctx, &v1.ResumeJobRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return a.pausedOut(res, res.GetJob())
		}),
	}
}

// pausedOut writes a job paused or resumed: one line, then its details.
func (a *app) pausedOut(res proto.Message, j *v1.Job) error {
	return messageOut(a, res, func(w io.Writer) error {
		how := "running again"
		if j.GetPaused() {
			how = "paused"
		}
		if _, err := fmt.Fprintf(w, "job %s is %s (%s)\n\n", output.Clean(j.GetId()), output.JobState(j.GetState()), how); err != nil {
			return err
		}
		return output.Details(w, a.style(), a.jobDetails(j))
	})
}
