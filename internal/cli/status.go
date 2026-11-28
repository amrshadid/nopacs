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

// newTenantStatus builds "tenant status".
func (a *app) newTenantStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status <tenant>",
		Short: "Show a tenant's state now: receivers, storages, jobs and senders",
		Long: `Show one tenant as it stands now, in one call: whether its database answers,
what its catalog holds, the receptions and jobs in progress by step and state,
each receiver with its health, each storage with its space, and each sender
with its health and the associations it holds open. The counts come from one
snapshot of the tenant's database; the receivers' and senders' health from the
server's memory, so they are shown also when the database does not answer.

It needs the reader role on the tenant. With --json the answer is the status
as the API gives it, the position of the tenant's event feed included, from
which "nopacs watch" can follow what changes next.`,
		Example: `  nopacs tenant status lab1
  nopacs tenant status lab1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStatusResponse, error) {
				return c.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: optionalScope(args[0])})
			})
			if err != nil {
				return err
			}
			if len(res.GetTenants()) == 0 {
				return fmt.Errorf("the server answered no status for tenant %s", output.Clean(args[0]))
			}
			return messageOut(a, res, func(w io.Writer) error { return a.tenantStatusText(w, res.GetTenants()[0]) })
		}),
	}
}

// tenantStatusText writes one tenant's status: its labeled lines, then a
// table each of its receivers, storages and senders.
func (a *app) tenantStatusText(w io.Writer, st *v1.TenantStatus) error {
	pairs := []output.Pair{
		{Label: "Tenant", Value: st.GetTenant()},
		{Label: "Database", Value: stateText(st.GetDatabaseState()) + " since " + a.when(st.GetDatabaseStateTime())},
	}
	if cn := st.GetCounts(); cn != nil {
		receptions := make([]string, 0, len(cn.GetReceptionsInProgress()))
		for _, r := range cn.GetReceptionsInProgress() {
			receptions = append(receptions, strconv.FormatInt(r.GetReceptions(), 10)+" "+output.ReceptionStep(r.GetStep()))
		}
		jobs := make([]string, 0, len(cn.GetJobsInProgress()))
		for _, j := range cn.GetJobsInProgress() {
			jobs = append(jobs, strconv.FormatInt(j.GetJobs(), 10)+" "+output.JobState(j.GetState()))
		}
		incoming := output.Human(cn.GetIncomingBytes())
		if st.GetIncomingLimitBytes() > 0 {
			incoming += " of " + output.Human(st.GetIncomingLimitBytes()) + " allowed"
		}
		pairs = append(pairs,
			output.Pair{Label: "Catalog", Value: fmt.Sprintf("%d subjects, %d sessions, %d scans, %d instances",
				cn.GetSubjects(), cn.GetSessions(), cn.GetScans(), cn.GetInstances())},
			output.Pair{Label: "Receptions", Value: orNone(receptions, "none in progress")},
			output.Pair{Label: "Jobs", Value: orNone(jobs, "none in progress")},
			output.Pair{Label: "Quarantined", Value: fmt.Sprintf("%d files, %s", cn.GetQuarantinedFiles(), output.Human(cn.GetQuarantinedBytes()))},
			output.Pair{Label: "Incoming", Value: incoming})
	} else {
		pairs = append(pairs, output.Pair{Label: "Counts", Value: "unknown: the tenant's database did not answer"})
	}
	if warn := output.QuarantineWarning(st.GetQuarantine()); warn != "" {
		pairs = append(pairs, output.Pair{Label: "Warning", Value: warn})
	}
	pairs = append(pairs, quotaPairs(st.GetQuota())...)
	if err := output.Details(w, a.style(), pairs); err != nil {
		return err
	}
	var receivers, storages, senders [][]string
	for _, r := range st.GetReceivers() {
		callers := "listed"
		if r.GetAnyCaller() {
			callers = "any"
		}
		receivers = append(receivers, []string{r.GetReceiver(), output.ReceiverHealth(r.GetHealth()), callers})
	}
	for _, s := range st.GetStorages() {
		free := "unknown"
		if s.FreeBytes != nil {
			free = output.Human(s.GetFreeBytes())
		}
		capacity := "-"
		if s.GetCapacityBytes() > 0 {
			capacity = output.Human(s.GetCapacityBytes())
		}
		storages = append(storages, []string{s.GetStorage(), output.Human(s.GetUsedBytes()), free, capacity, yesNo(s.GetNearlyFull())})
	}
	for _, s := range st.GetSenders() {
		senders = append(senders, []string{s.GetSender(), output.SenderHealth(s.GetHealth()), strconv.Itoa(int(s.GetOpenAssociations()))})
	}
	for _, t := range []struct {
		header []string
		rows   [][]string
		none   string
	}{
		{[]string{"RECEIVER", "HEALTH", "CALLERS"}, receivers, "no receiver"},
		{[]string{"STORAGE", "USED", "FREE", "CAPACITY", "NEARLY FULL"}, storages, "no storage"},
		{[]string{"SENDER", "HEALTH", "OPEN"}, senders, "no sender"},
	} {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if len(t.rows) == 0 {
			if _, err := fmt.Fprintln(w, t.none); err != nil {
				return err
			}
			continue
		}
		if err := output.Table(w, a.style(), t.header, t.rows); err != nil {
			return err
		}
	}
	return nil
}

// orNone joins words with commas, or says none.
func orNone(words []string, none string) string {
	if len(words) == 0 {
		return none
	}
	return strings.Join(words, ", ")
}
