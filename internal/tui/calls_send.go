package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// jobLoadedMsg is a job read together with the position of its tenant's event
// feed it was read at, so the stream that follows starts from there and no
// change between the two is missed.
type jobLoadedMsg struct {
	job    *v1.Job
	cursor string
}

// jobEventMsg is the answer of a stream that saw an event of a job: the
// event's cursor, which the next stream starts after, and the job's state and
// counts as the event reports them; job is nil for an event that is not about
// a job's progress or step.
type jobEventMsg struct {
	cursor string
	id     string
	state  v1.JobState
	counts *v1.JobCounts
	step   bool
}

// sendersPage asks for one page of a tenant's senders.
func sendersPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "sender list", func(ctx context.Context) (*v1.ListSendersResponse, error) {
			return cl.Senders.ListSenders(ctx, &v1.ListSendersRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListSendersResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetSenders()))
			for _, s := range r.GetSenders() {
				rows = append(rows, row{cells: []string{s.GetName(), output.SenderType(s), output.SenderWhere(s), yesNoWord(s.GetEnabled()), output.SenderHealth(s.GetHealth())}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// getSender fetches one sender again.
func getSender(c *ctx, owner int, tenant, name string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "sender show", func(ctx context.Context) (*v1.GetSenderResponse, error) {
			return cl.Senders.GetSender(ctx, &v1.GetSenderRequest{Tenant: tenant, Name: name})
		})
		return answer(res, err, func(r *v1.GetSenderResponse) tea.Msg { return detailMsg{ref: r.GetSender()} })
	})
}

// testSender tries a sender's peer; the answer is the result a screen shows.
func testSender(c *ctx, owner int, tenant, name string, dimse bool) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "sender test", func(ctx context.Context) (*v1.TestSenderResponse, error) {
			return cl.Senders.TestSender(ctx, &v1.TestSenderRequest{Tenant: tenant, Name: name})
		})
		return answer(res, err, func(r *v1.TestSenderResponse) tea.Msg { return detailMsg{ref: senderTest{res: r, dimse: dimse}} })
	})
}

// senderTest is the answer of a test with what the sender is, which words its
// last line.
type senderTest struct {
	res   *v1.TestSenderResponse
	dimse bool
}

// jobRow is a job as the cells of a row, its progress drawn as a bar.
func jobRow(j *v1.Job, when string) row {
	cn := j.GetCounts()
	by, what := j.GetSend().GetSender(), output.JobTarget(j.GetSend())
	switch k := j.GetKind().(type) {
	case *v1.Job_ImportJob:
		// The path can name a person; the list names the receiver only.
		by, what = "import "+k.ImportJob.GetReceiver(), "a directory"
	case *v1.Job_Scrub:
		by, what = "scrub", "storage "+output.Dash(k.Scrub.GetStorage())
	case *v1.Job_Rebuild:
		by, what = "rebuild", "the catalog"
	case *v1.Job_Replicate:
		by, what = "replicate", k.Replicate.GetPrimary()+" to "+k.Replicate.GetReplica()
	case *v1.Job_Repair:
		by, what = "repair", "storage "+k.Repair.GetStorage()
	case *v1.Job_Dispose:
		by, what = "dispose", "rule "+k.Dispose.GetRule()
	case *v1.Job_DeleteBatch:
		by, what = "delete batch", itoa(k.DeleteBatch.GetTargets())+" targets"
	}
	return row{cells: []string{j.GetId(), by, what, output.JobState(j.GetState()),
		output.JobBar(cn, 22) + " " + itoa(int64(output.JobPercent(cn))) + "%", itoa(cn.GetInstancesSent() + cn.GetInstancesWarned()), itoa(cn.GetInstancesRefused()), when}, ref: j}
}

// jobsPage asks for one page of a tenant's jobs, newest first.
func jobsPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job list", func(ctx context.Context) (*v1.ListJobsResponse, error) {
			return cl.Jobs.ListJobs(ctx, &v1.ListJobsRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListJobsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetJobs()))
			for _, j := range r.GetJobs() {
				rows = append(rows, jobRow(j, listTime(j.GetCreateTime(), loc)))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// loadJob reads a job and the position of its tenant's event feed, the
// position first, so an event that happened between the two is read again and
// never missed.
func loadJob(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		st, err := client.Call(ctx, cl, "status", func(ctx context.Context) (*v1.GetStatusResponse, error) {
			return cl.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: tenantScope(tenant)})
		})
		if err != nil {
			return failMsg{err}
		}
		cursor := ""
		if len(st.GetTenants()) > 0 {
			cursor = st.GetTenants()[0].GetEventCursor()
		}
		res, err := client.Call(ctx, cl, "job get", func(ctx context.Context) (*v1.GetJobResponse, error) {
			return cl.Jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.GetJobResponse) tea.Msg { return jobLoadedMsg{job: r.GetJob(), cursor: cursor} })
	})
}

// getJob fetches one job again.
func getJob(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job get", func(ctx context.Context) (*v1.GetJobResponse, error) {
			return cl.Jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.GetJobResponse) tea.Msg { return detailMsg{ref: r.GetJob()} })
	})
}

// followJobs follows a tenant's job events after cursor until one comes, in a
// call the screen does not wait for (ctx.follow): leaving the screen stops it.
// The stream asks only for job events; keep-alives are passed over.
func followJobs(c *ctx, owner int, tenant, cursor string) tea.Cmd {
	cl := c.client
	return c.follow(owner, func(ctx context.Context) tea.Msg {
		var seen *jobEventMsg
		err := cl.Watch(ctx, &v1.WatchRequest{
			Scope: tenantScope(tenant), Cursor: cursor,
			Types: []v1.EventType{v1.EventType_EVENT_TYPE_JOB_STEP, v1.EventType_EVENT_TYPE_JOB_PROGRESS},
		}, func(m *v1.WatchResponse) bool {
			e := m.GetEvent()
			if e == nil {
				return true
			}
			seen = &jobEventMsg{cursor: e.GetCursor()}
			switch p := e.GetPayload().(type) {
			case *v1.Event_JobStep:
				seen.id, seen.state, seen.counts, seen.step = p.JobStep.GetJob(), p.JobStep.GetState(), p.JobStep.GetCounts(), true
			case *v1.Event_JobProgress:
				seen.id, seen.counts = p.JobProgress.GetJob(), p.JobProgress.GetCounts()
			}
			return false
		})
		if seen != nil {
			return *seen
		}
		return streamEndedMsg{err: err}
	})
}

// jobFailuresPage asks for one page of a job's failures, with the warnings.
func jobFailuresPage(c *ctx, owner, gen int, token, tenant, id string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job failures", func(ctx context.Context) (*v1.ListJobFailuresResponse, error) {
			return cl.Jobs.ListJobFailures(ctx, &v1.ListJobFailuresRequest{Tenant: tenant, Id: id, IncludeWarnings: true, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListJobFailuresResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetFailures()))
			for _, f := range r.GetFailures() {
				status := "-"
				if f.GetPeerStatus() != 0 {
					status = fmt.Sprintf("%04X", f.GetPeerStatus())
				}
				rows = append(rows, row{cells: []string{f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel(), f.GetInstanceId(),
					output.JobFailureCode(f.GetCode()), status, output.Dash(f.GetDetail()), listTime(f.GetTime(), loc)}, ref: f})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// cancelJob cancels a job that is in progress.
func cancelJob(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job cancel", func(ctx context.Context) (*v1.CancelJobResponse, error) {
			return cl.Jobs.CancelJob(ctx, &v1.CancelJobRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.CancelJobResponse) tea.Msg { return actionDoneMsg{note: "canceled " + r.GetJob().GetId()} })
	})
}

// retryJob makes the job that sends what an ended job did not send.
func retryJob(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job retry", func(ctx context.Context) (*v1.RetryJobResponse, error) {
			return cl.Jobs.RetryJob(ctx, &v1.RetryJobRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.RetryJobResponse) tea.Msg {
			return actionDoneMsg{note: "retried " + r.GetJob().GetId() + " as " + r.GetRetry().GetId()}
		})
	})
}
