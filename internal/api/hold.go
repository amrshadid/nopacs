package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wholeTenantDetail is the target detail of a hold of the whole tenant,
// which names no label.
const wholeTenantDetail = "whole tenant"

// holdTargetOf returns the target a request gives, unchecked.
func holdTargetOf(t *v1.HoldTarget) tenant.HoldTarget {
	return tenant.HoldTarget{Subject: t.GetSubjectLabel(), Session: t.GetSessionLabel(), Scan: t.GetScanLabel(), WholeTenant: t.GetWholeTenant()}
}

// holdTargetDetail returns a hold's target as its entry's detail: its path
// of labels, or "whole tenant".
func holdTargetDetail(t *v1.HoldTarget) string {
	if t.GetWholeTenant() {
		return wholeTenantDetail
	}
	return labelPath(t.GetSubjectLabel(), t.GetSessionLabel(), t.GetScanLabel())
}

// holdWriter returns the hold writer of the tenant named (Config.Holds), or
// Unavailable for a server that writes no hold records.
func (c *core) holdWriter(ctx context.Context, name string) (*holds.Writer, error) {
	if c.holds == nil {
		return nil, fault.New(fault.Unavailable, "the server writes no hold records; a hold cannot be placed or released")
	}
	return c.holds(ctx, name)
}

// noSuchHold gives a store's NotFound for a hold the contract's message.
func noSuchHold(err error) error {
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.NotFound, err, messageNoHold)
	}
	return err
}

// PlaceHold places a hold through the tenant's hold writer: its record on
// every primary the target touches, then its row; the active hold with the
// same target and reason is answered instead.
func (s retentionService) PlaceHold(ctx context.Context, req *v1.PlaceHoldRequest) (*v1.PlaceHoldResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServicePlaceHoldProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailTarget: holdTargetDetail(req.GetTarget())}}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	target := holdTargetOf(req.GetTarget())
	switch {
	case req.GetTarget() == nil:
		return nil, c.failWith(d, st, facts, invalid("target", "is required"))
	case target.Validate() != nil:
		return nil, c.failWith(d, st, facts, fieldOf("target", target.Validate()))
	case tenant.ValidateUndoReason(req.GetReason()) != nil:
		return nil, c.failWith(d, st, facts, fieldOf("reason", tenant.ValidateUndoReason(req.GetReason())))
	}
	facts.Details[detailReason] = req.GetReason()
	w, err := c.holdWriter(ctx, req.GetTenant())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	h, _, err := w.Place(ctx, holds.Placing{Target: target, Reason: req.GetReason(), KeyID: callerKeyID(d), Audit: changeEntry(d, facts),
		Guard: c.roleGuard(d, master.RoleAdmin)})
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.PlaceHoldResponse{Hold: holdMessage(h)}, nil
}

// ReleaseHold releases a hold through the tenant's hold writer: its record
// rewritten released on every primary, then its row; a hold released
// already is answered as it stands. An id no hold can have is answered as
// a hold the tenant does not have.
func (s retentionService) ReleaseHold(ctx context.Context, req *v1.ReleaseHoldRequest) (*v1.ReleaseHoldResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceReleaseHoldProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Object: jobObject(req.GetId()), Details: map[string]string{}}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	switch {
	case tenant.ValidateHoldID(req.GetId()) != nil:
		return nil, c.failWith(d, st, facts, fault.New(fault.NotFound, messageNoHold))
	case tenant.ValidateUndoReason(req.GetReason()) != nil:
		return nil, c.failWith(d, st, facts, fieldOf("reason", tenant.ValidateUndoReason(req.GetReason())))
	}
	facts.Details[detailReason] = req.GetReason()
	w, err := c.holdWriter(ctx, req.GetTenant())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	h, err := w.Release(ctx, holds.Releasing{ID: req.GetId(), Reason: req.GetReason(), KeyID: callerKeyID(d), Audit: changeEntry(d, facts),
		Guard: c.roleGuard(d, master.RoleAdmin)})
	if err != nil {
		return nil, c.failWith(d, st, facts, noSuchHold(err))
	}
	return &v1.ReleaseHoldResponse{Hold: holdMessage(h)}, nil
}
