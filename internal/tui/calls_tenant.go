package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// eventSeenMsg is the answer of a stream that saw an event of its tenant:
// the event's cursor, which the next stream starts after.
type eventSeenMsg struct{ cursor string }

// streamEndedMsg is the answer of a stream that ended without an event: the
// server stopped it, or it failed. The screen follows again after a wait.
type streamEndedMsg struct{ err error }

// getStatus asks for one tenant's status.
func getStatus(c *ctx, owner int, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "status", func(ctx context.Context) (*v1.GetStatusResponse, error) {
			return cl.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: tenantScope(tenant)})
		})
		return answer(res, err, func(r *v1.GetStatusResponse) tea.Msg {
			if len(r.GetTenants()) == 0 {
				return detailMsg{ref: &v1.TenantStatus{Tenant: tenant}}
			}
			return detailMsg{ref: r.GetTenants()[0]}
		})
	})
}

// followEvents follows a tenant's events after cursor until one comes, in a
// call the screen does not wait for (ctx.follow): leaving the screen stops
// it. Keep-alives are passed over.
func followEvents(c *ctx, owner int, tenant, cursor string) tea.Cmd {
	cl := c.client
	return c.follow(owner, func(ctx context.Context) tea.Msg {
		seen := ""
		err := cl.Watch(ctx, &v1.WatchRequest{Scope: tenantScope(tenant), Cursor: cursor}, func(m *v1.WatchResponse) bool {
			if e := m.GetEvent(); e != nil {
				seen = e.GetCursor()
				return false
			}
			return true
		})
		if seen != "" {
			return eventSeenMsg{cursor: seen}
		}
		return streamEndedMsg{err: err}
	})
}

// followAgainAfter is how long a status waits before it follows its
// tenant's events again after a stream ended.
const followAgainAfter = 5 * time.Second

// storagesPage asks for one page of a tenant's storages.
func storagesPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "storage list", func(ctx context.Context) (*v1.ListStoragesResponse, error) {
			return cl.Storages.ListStorages(ctx, &v1.ListStoragesRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListStoragesResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetStorages()))
			for _, s := range r.GetStorages() {
				capacity := "no limit"
				if s.GetCapacityBytes() > 0 {
					capacity = output.Bytes(s.GetCapacityBytes())
				}
				rows = append(rows, row{cells: []string{s.GetName(), output.StorageType(s), output.StorageWhere(s), capacity}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// receiversPage asks for one page of a tenant's receivers.
func receiversPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "receiver list", func(ctx context.Context) (*v1.ListReceiversResponse, error) {
			return cl.Receivers.ListReceivers(ctx, &v1.ListReceiversRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListReceiversResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetReceivers()))
			for _, rc := range r.GetReceivers() {
				port, aet := "-", "-"
				if d := rc.GetDimse(); d != nil {
					port, aet = itoa(int64(d.GetPort())), d.GetAeTitle()
				}
				rows = append(rows, row{cells: []string{rc.GetName(), port, aet, rc.GetStorage(), output.ReceiverHealth(rc.GetHealth())}, ref: rc})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// receptionsPage asks for one page of a tenant's receptions, newest first.
func receptionsPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "reception list", func(ctx context.Context) (*v1.ListReceptionsResponse, error) {
			return cl.Receptions.ListReceptions(ctx, &v1.ListReceptionsRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListReceptionsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetReceptions()))
			for _, rc := range r.GetReceptions() {
				n := rc.GetCounts()
				rows = append(rows, row{cells: []string{rc.GetId(), rc.GetReceiver(), output.ReceptionStep(rc.GetStep()),
					itoa(n.GetFilesReceived()), itoa(n.GetFilesArchived()), itoa(n.GetFilesQuarantined()), listTime(rc.GetStartTime(), loc)}, ref: rc})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// quarantinePage asks for one page of a tenant's quarantined files held
// now, newest first.
func quarantinePage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "quarantine list", func(ctx context.Context) (*v1.ListQuarantinedFilesResponse, error) {
			return cl.Quarantine.ListQuarantinedFiles(ctx, &v1.ListQuarantinedFilesRequest{Tenant: tenant,
				State: v1.QuarantineState_QUARANTINE_STATE_HELD, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListQuarantinedFilesResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetFiles()))
			for _, f := range r.GetFiles() {
				rows = append(rows, row{cells: []string{f.GetId(), output.QuarantineReason(f.GetReason()), f.GetReceiver(),
					output.Bytes(f.GetSizeBytes()), listTime(f.GetQuarantineTime(), loc)}, ref: f})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// releaseFile releases a quarantined file as it is, with the reason given.
func releaseFile(c *ctx, owner int, tenant, id, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "quarantine release", func(ctx context.Context) (*v1.ReleaseQuarantinedFileResponse, error) {
			return cl.Quarantine.ReleaseQuarantinedFile(ctx, &v1.ReleaseQuarantinedFileRequest{Tenant: tenant, Id: id,
				Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Reason: strings.TrimSpace(reason)})
		})
		return answer(res, err, func(r *v1.ReleaseQuarantinedFileResponse) tea.Msg {
			return actionDoneMsg{note: "released " + id + " into reception " + r.GetFile().GetReleaseReception()}
		})
	})
}

// rejectFile rejects a quarantined file, removing its bytes for good, with
// the reason given.
func rejectFile(c *ctx, owner int, tenant, id, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "quarantine reject", func(ctx context.Context) (*v1.RejectQuarantinedFileResponse, error) {
			return cl.Quarantine.RejectQuarantinedFile(ctx, &v1.RejectQuarantinedFileRequest{Tenant: tenant, Id: id, Reason: strings.TrimSpace(reason)})
		})
		return answer(res, err, func(*v1.RejectQuarantinedFileResponse) tea.Msg { return actionDoneMsg{note: "rejected " + id} })
	})
}
