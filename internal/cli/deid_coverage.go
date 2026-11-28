package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// coveragePage is how many elements a request for the report asks for.
const coveragePage = 1000

// newDeidCoverage builds "deid coverage".
func (a *app) newDeidCoverage() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "coverage <tenant> [--refresh]",
		Short: "List the elements archived files held that no rule of their profile named",
		Long: `Show the tenant's coverage report: every standard element, and every private
element by its private creator and its element within the creator's block,
that files its receivers de-identified held as received and that no rule of
their profile named, so the profile's default decided it (a private element
is removed, any other kept or removed as the profile's unnamed says). For each:
its VR as the file gave it, how many instances held it and how many times in
all. It is what a profile never decided, and where to look for the private
elements a vendor writes.

It holds no value of any file. A private creator is a vendor's name written in
the file and is shown only when it is printable ASCII that held no value the
profile removed; any other is shown as "(withheld)", its elements counted
together. A key that is masked may read it.

The report is built by a job (kind deid_coverage) that reads each scan's
metadata file once; nothing is read from an instance's file. Without
--refresh the command shows the report of the latest job that ended, and says
so when none has. With --refresh it makes the job, waits for it to end, and
shows the new report: a scan whose metadata cannot be read is counted skipped
and the report is marked partial. An instance archived before the metadata
recorded this, or as received, is counted but contributes no element. It needs
the operator role on the tenant.`,
		Example: `  nopacs deid coverage lab1
  nopacs deid coverage lab1 --refresh
  nopacs deid coverage lab1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			if refresh {
				if err := a.refreshCoverage(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := a.coverageOf(ctx, c, cmd, args[0])
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.coverageText(w, args[0], res) })
		}),
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "build the report again first: make the job and wait for it to end")
	return cmd
}

// refreshCoverage makes the coverage job and waits for it to end, reading the
// job every watch pause. A job that failed or was canceled is an error: the
// report before it stands.
func (a *app) refreshCoverage(ctx context.Context, c *client.Client, cmd *cobra.Command, tenantName string) error {
	made, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RefreshCoverageResponse, error) {
		return c.Deid.RefreshCoverage(ctx, &v1.RefreshCoverageRequest{Tenant: tenantName})
	})
	if err != nil {
		return err
	}
	job := made.GetJob()
	for !output.JobEnded(job.GetState()) {
		select {
		case <-ctx.Done():
			return fault.New(fault.Internal, "stopped waiting; the job "+job.GetId()+" goes on")
		case <-time.After(a.watchPause()):
		}
		got, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetJobResponse, error) {
			return c.Jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: tenantName, Id: job.GetId()})
		})
		if err != nil {
			return err
		}
		job = got.GetJob()
	}
	switch job.GetState() {
	case v1.JobState_JOB_STATE_FAILED, v1.JobState_JOB_STATE_CANCELED:
		return fault.New(fault.Unavailable, "the coverage job "+job.GetId()+" ended "+strings.ToLower(strings.TrimPrefix(job.GetState().String(), "JOB_STATE_"))+
			" ("+output.Clean(job.GetReason())+"); the report before it stands")
	}
	return nil
}

// coverageOf reads the whole report, a page at a time, into one answer.
func (a *app) coverageOf(ctx context.Context, c *client.Client, cmd *cobra.Command, tenantName string) (*v1.GetCoverageResponse, error) {
	out := &v1.GetCoverageResponse{}
	token := ""
	for {
		page, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetCoverageResponse, error) {
			return c.Deid.GetCoverage(ctx, &v1.GetCoverageRequest{Tenant: tenantName, PageSize: coveragePage, PageToken: token})
		})
		if err != nil {
			return nil, err
		}
		if token == "" {
			out.Report, out.Refreshing = page.GetReport(), page.GetRefreshing()
		}
		out.Elements = append(out.Elements, page.GetElements()...)
		if page.GetNextPageToken() == "" {
			return out, nil
		}
		token = page.GetNextPageToken()
	}
}

// coverageText writes a report for a person.
func (a *app) coverageText(w io.Writer, tenantName string, r *v1.GetCoverageResponse) error {
	if j := r.GetRefreshing(); j != nil {
		if _, err := io.WriteString(w, "A refresh is running: job "+output.Clean(j.GetId())+" ("+output.JobProgressOf(j)+").\n"); err != nil {
			return err
		}
	}
	rep := r.GetReport()
	if rep == nil {
		_, err := io.WriteString(w, "No coverage report yet. Build one with: nopacs deid coverage "+output.Clean(tenantName)+" --refresh\n")
		return err
	}
	made := "complete"
	if rep.GetScansSkipped() > 0 {
		made = "partial: " + strconv.FormatInt(rep.GetScansSkipped(), 10) + " scan(s) could not be read"
	}
	pairs := []output.Pair{
		{Label: "Made", Value: a.when(rep.GetEndTime()) + ", " + made},
		{Label: "Job", Value: output.Clean(rep.GetJobId())},
		{Label: "Scans read", Value: strconv.FormatInt(rep.GetScansRead(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(rep.GetInstances(), 10) + ", " + strconv.FormatInt(rep.GetInstancesRecorded(), 10) + " with a record"},
		{Label: "Elements", Value: strconv.FormatInt(rep.GetDistinctElements(), 10)},
	}
	if rep.GetInstancesCapped() > 0 || rep.GetElementsNotListed() > 0 {
		pairs = append(pairs, output.Pair{Label: "Not listed", Value: strconv.FormatInt(rep.GetElementsNotListed(), 10) + " occurrences, from " +
			strconv.FormatInt(rep.GetInstancesCapped(), 10) + " instances with more distinct elements than a record holds"})
	}
	if err := output.Details(w, a.style(), pairs); err != nil {
		return err
	}
	if len(r.GetElements()) == 0 {
		_, err := io.WriteString(w, "\nNo element lacks a rule.\n")
		return err
	}
	rows := make([][]string, 0, len(r.GetElements()))
	for _, e := range r.GetElements() {
		creator := "-"
		switch {
		case e.GetCreatorWithheld():
			creator = "(withheld)"
		case e.GetPrivateCreator() != "":
			creator = output.Clean(e.GetPrivateCreator())
		}
		rows = append(rows, []string{e.GetTag(), e.GetVr(), creator, strconv.FormatInt(e.GetInstances(), 10), strconv.FormatInt(e.GetOccurrences(), 10)})
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return output.Table(w, a.style(), []string{"TAG", "VR", "PRIVATE CREATOR", "INSTANCES", "OCCURRENCES"}, rows)
}

// deidCoverageDetails returns the labeled values of a coverage job.
func deidCoverageDetails(d *v1.DeidCoverageJob) []output.Pair {
	pairs := []output.Pair{{Label: "Reads", Value: "the metadata of every scan, for the elements no rule named"}}
	if d.GetInstancesRead() > 0 {
		pairs = append(pairs, output.Pair{Label: "Instances read", Value: strconv.FormatInt(d.GetInstancesRead(), 10) + ", " +
			strconv.FormatInt(d.GetInstancesRecorded(), 10) + " with a record"})
	}
	if d.GetScansSkipped() > 0 {
		pairs = append(pairs, output.Pair{Label: "Scans skipped", Value: strconv.FormatInt(d.GetScansSkipped(), 10)})
	}
	return pairs
}
