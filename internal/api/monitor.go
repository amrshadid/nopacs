package api

import (
	"context"
	"errors"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// monitorService is MonitorService: a tenant's numbers now, read from its
// database in one snapshot and from what the server holds in memory, and
// the stream of its events.
type monitorService struct{ c *core }

var _ nopacsv1connect.MonitorServiceHandler = monitorService{}

// GetStatus answers the status of one tenant, or a page of every tenant's.
// A tenant whose database does not answer is answered with its state and
// what is held in memory rather than refused: that is when its status
// matters most. A fenced tenant, which serves no data, is answered with
// its state alone. The request is recorded in the log of its scope, or for
// a tenant whose database does not answer or is fenced, in the
// installation's log (with the reason, for the first).
func (s monitorService) GetStatus(ctx context.Context, req *v1.GetStatusRequest) (*v1.GetStatusResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.MonitorServiceGetStatusProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	out := &v1.GetStatusResponse{}
	if scope.IsInstallation() {
		page, err := pageOf(req.GetPageSize(), req.GetPageToken())
		if err != nil {
			return nil, c.fail(d, err)
		}
		rows, next, err := c.master.ListTenants(ctx, page)
		if err != nil {
			return nil, c.fail(d, listError(err, page.Cursor))
		}
		for _, t := range rows {
			// A fenced tenant serves no data: its status is its state alone.
			st, _ := c.reg.DataReader(t.Name)
			out.Tenants = append(out.Tenants, c.tenantStatus(ctx, t.Name, st))
		}
		out.NextPageToken = next
		out.ExpiringKeys = c.expiringKeys(ctx)
		out.Time = timestamp(c.now())
		c.done(d, nil)
		return out, nil
	}
	if req.GetPageSize() != 0 {
		return nil, c.fail(d, invalid("page_size", "is zero with a tenant"))
	}
	if req.GetPageToken() != "" {
		return nil, c.fail(d, invalid("page_token", "is empty with a tenant"))
	}
	st, err := c.tenantStore(ctx, d, scope.Tenant)
	switch {
	case err == nil:
		out.Tenants = []*v1.TenantStatus{c.tenantStatus(ctx, scope.Tenant, st)}
		out.Time = timestamp(c.now())
		c.done(d, st)
	case fault.KindOf(err) == fault.Unavailable:
		// The tenant is there and its database does not answer: its status
		// is what the server holds, recorded where a log can be written.
		out.Tenants = []*v1.TenantStatus{c.tenantStatus(ctx, scope.Tenant, nil)}
		out.Time = timestamp(c.now())
		c.record(d, nil, master.OutcomeDone, "", access.Facts{TenantName: scope.Tenant,
			Details: map[string]string{detailScope: scope.String(), access.DetailRerouted: reroutedValue}})
	case errors.Is(err, master.ErrFencedRead):
		// A fenced tenant serves no data, but its state stays visible: its
		// status is that state alone, recorded in the installation's log.
		out.Tenants = []*v1.TenantStatus{c.tenantStatus(ctx, scope.Tenant, nil)}
		out.Time = timestamp(c.now())
		c.record(d, nil, master.OutcomeDone, "", access.Facts{TenantName: scope.Tenant, Details: map[string]string{detailScope: scope.String()}})
	default:
		return nil, c.failWith(d, nil, access.Facts{TenantName: scope.Tenant}, err)
	}
	return out, nil
}

// tenantStatus returns the status of the tenant named name: its state as
// the registry sees it; from st, when its database answers, the snapshot of
// its counts and feed, its storages and its incoming limit; and from
// memory, its receivers and throughput. A part that cannot be read is left
// out, never answered as an error: the rest still says what can be said.
func (c *core) tenantStatus(ctx context.Context, name string, st tenant.Store) *v1.TenantStatus {
	ts := &v1.TenantStatus{Tenant: name}
	if state, ok := c.reg.State(name); ok {
		ts.DatabaseState = tenantState(state.State)
		ts.DatabaseStateTime = timestamp(state.Since)
	}
	live, _ := c.live.TenantLive(name)
	ts.Throughput = &v1.Throughput{
		FilesReceivedPerSecond: live.Throughput.FilesReceived, BytesReceivedPerSecond: live.Throughput.BytesReceived,
		FilesArchivedPerSecond: live.Throughput.FilesArchived, BytesArchivedPerSecond: live.Throughput.BytesArchived,
	}
	receivers, senders := live.Receivers, live.Senders
	if st != nil {
		var snap tenant.StatusSnapshot
		feed, ok := st.(tenant.Events)
		snapped := false
		if ok {
			if s, err := feed.StatusSnapshot(ctx); err == nil {
				snap, snapped = s, true
				ts.EventCursor, ts.EventHistory = s.EventCursor, s.History
				ts.Counts = countsMessage(s)
			}
		}
		if p, ok := st.(tenant.Pipeline); ok {
			if snapped {
				ts.Storages = storageStatuses(ctx, p, snap, live)
			}
			if receivers == nil {
				// Nothing runs the tenant's receivers yet: they are read
				// from the database, each with the health that says so.
				receivers, _ = allReceivers(ctx, p)
			}
			if senders == nil {
				senders, _ = allSenders(ctx, p)
			}
		}
		if limit, err := c.values().OnTenant(st).IncomingLimit(ctx); err == nil {
			ts.IncomingLimitBytes = limit
		}
	}
	for _, r := range receivers {
		rs := &v1.ReceiverStatus{Receiver: r.Name, Health: healthMessage(c.receivers.Health(name, r))}
		if r.Type == tenant.ReceiverDIMSE {
			rs.AnyCaller = r.DIMSE.AnyCaller
		}
		ts.Receivers = append(ts.Receivers, rs)
	}
	for _, sn := range senders {
		h := c.senders.SenderHealth(name, sn)
		ts.Senders = append(ts.Senders, &v1.SenderStatus{
			Sender: sn.Name, Health: senderHealthMessage(h), OpenAssociations: int32(h.OpenAssociations), //nolint:gosec // G115: at most 1024.
		})
	}
	return ts
}

// countsMessage returns the counts of a snapshot as the contract writes
// them.
func countsMessage(s tenant.StatusSnapshot) *v1.TenantCounts {
	m := &v1.TenantCounts{
		Subjects: s.Catalog.Subjects, Sessions: s.Catalog.Sessions, Scans: s.Catalog.Scans, Instances: s.Catalog.Instances,
		QuarantinedFiles: s.QuarantinedFiles, QuarantinedBytes: s.QuarantinedBytes, IncomingBytes: s.IncomingBytes,
	}
	for _, sc := range s.InProgress {
		m.ReceptionsInProgress = append(m.ReceptionsInProgress, &v1.ReceptionStepCount{Step: stepOf(sc.Step), Receptions: sc.Receptions})
	}
	for _, jc := range s.JobsInProgress {
		m.JobsInProgress = append(m.JobsInProgress, &v1.JobStateCount{State: enumOf[v1.JobState](v1.JobState_value, "JOB_STATE_", string(jc.State)), Jobs: jc.Jobs})
	}
	return m
}

// storageStatuses returns each of the tenant's storages, ordered by name:
// its used bytes from the snapshot; its free bytes, the smaller of what was
// last measured and what is left under its capacity, absent when neither
// is known; and whether it is below its reserve, as last measured.
func storageStatuses(ctx context.Context, p tenant.Pipeline, snap tenant.StatusSnapshot, live TenantLive) []*v1.StorageStatus {
	rows, err := allStorages(ctx, p)
	if err != nil {
		return nil
	}
	out := make([]*v1.StorageStatus, 0, len(rows))
	for _, r := range rows {
		used := snap.StorageBytes[r.Name]
		m := &v1.StorageStatus{Storage: r.Name, UsedBytes: used, CapacityBytes: r.CapacityBytes, Tier: v1.StorageTier_STORAGE_TIER_ONLINE}
		measured, known := live.Storages[r.Name]
		var free int64
		switch {
		case known && r.CapacityBytes > 0:
			free = min(measured.FreeBytes, max(r.CapacityBytes-used, 0))
		case known:
			free = measured.FreeBytes
		case r.CapacityBytes > 0:
			free, known = max(r.CapacityBytes-used, 0), true
		}
		if known {
			m.FreeBytes = &free
		}
		m.NearlyFull = measured.NearlyFull
		out = append(out, m)
	}
	return out
}

// allStorages reads every storage of the tenant, page after page.
func allStorages(ctx context.Context, p tenant.Pipeline) ([]tenant.Storage, error) {
	return wholeList(ctx, "storages", p.ListStorages)
}

// allReceivers reads every receiver of the tenant, page after page.
func allReceivers(ctx context.Context, p tenant.Pipeline) ([]tenant.Receiver, error) {
	return wholeList(ctx, "receivers", p.ListReceivers)
}

// allSenders reads every sender of the tenant, page after page, ordered by
// name.
func allSenders(ctx context.Context, p tenant.Pipeline) ([]tenant.Sender, error) {
	return wholeList(ctx, "senders", p.ListSenders)
}
