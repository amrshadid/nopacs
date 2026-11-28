package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// detailAttribute is the audit detail that names a search attribute by
// keyword.
const detailAttribute = "attribute"

// searchBackfills returns the store's search backfills, or an Internal fault
// for one that keeps none: a mistake of the code, since every driver does.
func searchBackfills(st tenant.Store) (tenant.SearchBackfills, error) {
	sb, ok := st.(tenant.SearchBackfills)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no search backfills")
	}
	return sb, nil
}

// BackfillAttribute makes the job that fills the search tables for the scans
// archived before an attribute was added. The job's runner is the server's:
// it reads each scan's metadata file and writes the scan's search row.
func (s searchService) BackfillAttribute(ctx context.Context, req *v1.BackfillAttributeRequest) (*v1.BackfillAttributeResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SearchServiceBackfillAttributeProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailAttribute: clip(req.GetAttribute(), 128)}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	sb, err := searchBackfills(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	facts.Object = id
	facts.Details[detailJob] = id
	job, err := sb.QueueSearchBackfill(ctx, tenant.SearchBackfillQueue{ID: id, Field: req.GetAttribute(), KeyID: callerKeyID(d), Now: c.now()},
		changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.BackfillAttributeResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, job)}, nil
}
