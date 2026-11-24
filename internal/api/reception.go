package api

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoReception answers a reception id the tenant does not have.
const messageNoReception = "the tenant has no reception with this id"

// receptionService is ReceptionService: a tenant's receptions, which the
// server makes and the tenant's store keeps; a person only reads them, and
// retries or discards one that failed.
type receptionService struct{ c *core }

var _ nopacsv1connect.ReceptionServiceHandler = receptionService{}

// steps pairs each state of the core's reception machine with the
// contract's step of the same name.
var steps = map[reception.State]v1.ReceptionStep{
	reception.Receiving:   v1.ReceptionStep_RECEPTION_STEP_RECEIVING,
	reception.Complete:    v1.ReceptionStep_RECEPTION_STEP_COMPLETE,
	reception.Identified:  v1.ReceptionStep_RECEPTION_STEP_IDENTIFIED,
	reception.Archived:    v1.ReceptionStep_RECEPTION_STEP_ARCHIVED,
	reception.Verified:    v1.ReceptionStep_RECEPTION_STEP_VERIFIED,
	reception.Done:        v1.ReceptionStep_RECEPTION_STEP_DONE,
	reception.Quarantined: v1.ReceptionStep_RECEPTION_STEP_QUARANTINED,
	reception.Failed:      v1.ReceptionStep_RECEPTION_STEP_FAILED,
}

// stepOf returns the contract's step of a state, UNSPECIFIED for a state it
// has no step for.
func stepOf(s reception.State) v1.ReceptionStep { return steps[s] }

// stateOf returns the state of a step a request names, and false for
// UNSPECIFIED or a number the contract does not define.
func stateOf(step v1.ReceptionStep) (reception.State, bool) {
	for s, st := range steps {
		if st == step {
			return s, true
		}
	}
	return "", false
}

// receptionMessage returns a reception as the contract writes it.
func receptionMessage(r tenant.Reception) *v1.Reception {
	c := r.Counts
	return &v1.Reception{
		Id: r.ID, Receiver: r.Receiver, StudyInstanceUid: r.StudyUID, Arrival: int32(r.Arrival), //nolint:gosec // G115: an arrival number, far below 2^31.
		Step: stepOf(r.State),
		Counts: &v1.ReceptionCounts{
			FilesReceived: c.FilesReceived, BytesReceived: c.BytesReceived, FilesArchived: c.FilesArchived,
			BytesArchived: c.BytesArchived, FilesDuplicate: c.FilesDuplicate, BytesDuplicate: c.BytesDuplicate,
			FilesQuarantined: c.FilesQuarantined, BytesQuarantined: c.BytesQuarantined, FilesReleased: c.FilesReleased,
		},
		SubjectLabel: r.SubjectLabel, SessionLabel: r.SessionLabel, Storage: r.Storage, Failure: r.Failure,
		StartTime: timestamp(r.Started), LastFileTime: timestamp(r.LastFile), StepTime: timestamp(r.StepTime),
		EndTime: timestamp(r.Ended), Associations: r.Associations, CallingAeTitles: r.CallingAETitles,
		CallingAeTitlesTruncated: r.CallingAETitlesTruncated, RetryOf: r.RetryOf, RetryReception: r.RetryReception,
		DiscardReason: r.DiscardReason, DiscardTime: timestamp(r.DiscardTime),
	}
}

// receptionFilterDetails returns the filters of a listing of receptions, as
// its entry's details record them: a Study Instance UID only as present.
func receptionFilterDetails(r *v1.ListReceptionsRequest) map[string]string {
	d := map[string]string{}
	if r.GetReceiver() != "" {
		d[detailReceiver] = clip(r.GetReceiver(), 64)
	}
	if r.GetStudyInstanceUid() != "" {
		d[detailStudyUID] = presentValue
	}
	if len(r.GetSteps()) > 0 {
		names := make([]string, 0, len(r.GetSteps()))
		for i, s := range r.GetSteps() {
			if i == tenant.MaxStepFilter {
				break
			}
			names = append(names, s.String())
		}
		d[detailSteps] = clip(joinDetail(names), master.MaxAuditFieldLength-8)
	}
	if r.GetInProgress() {
		d[detailInProgress] = "true"
	}
	timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
	return d
}

// receptionFilter returns the filter a listing asks for, checked.
func receptionFilter(r *v1.ListReceptionsRequest) (tenant.ReceptionFilter, error) {
	f := tenant.ReceptionFilter{Receiver: r.GetReceiver(), StudyUID: r.GetStudyInstanceUid(), InProgress: r.GetInProgress()}
	if f.Receiver != "" {
		if err := itemName("receiver", f.Receiver); err != nil {
			return f, err
		}
	}
	if len(r.GetSteps()) > tenant.MaxStepFilter {
		return f, invalid("steps", "names at most "+strconv.Itoa(tenant.MaxStepFilter)+" steps")
	}
	for _, s := range r.GetSteps() {
		st, ok := stateOf(s)
		if !ok {
			return f, invalid("steps", "names only steps the contract defines, never RECEPTION_STEP_UNSPECIFIED")
		}
		f.States = append(f.States, st)
	}
	var err error
	if f.Since, err = filterTime("start_time", r.GetStartTime()); err != nil {
		return f, err
	}
	if f.Until, err = filterTime("end_time", r.GetEndTime()); err != nil {
		return f, err
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return f, invalid("end_time", "is after start_time")
	}
	if err := f.Validate(); err != nil {
		if f.StudyUID != "" {
			return f, fieldOf("study_instance_uid", err)
		}
		return f, err
	}
	return f, nil
}

// lookupReception reads a reception and its study's arrivals, answering
// one the tenant does not have as NotFound with the contract's message.
func lookupReception(ctx context.Context, p tenant.Pipeline, id string) (tenant.Reception, []tenant.Arrival, error) {
	r, arrivals, err := p.Reception(ctx, id)
	if fault.KindOf(err) == fault.NotFound {
		return r, nil, fault.Wrap(fault.NotFound, err, messageNoReception)
	}
	return r, arrivals, err
}

// ListReceptions answers a page of a tenant's receptions, newest first.
func (s receptionService) ListReceptions(ctx context.Context, req *v1.ListReceptionsRequest) (*v1.ListReceptionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReceptionServiceListReceptionsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), receptionFilterDetails(req))
	if err != nil {
		return nil, err
	}
	filter, err := receptionFilter(req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListReceptions(ctx, filter, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListReceptionsResponse{Receptions: make([]*v1.Reception, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Receptions = append(out.Receptions, receptionMessage(it))
	}
	return out, nil
}

// GetReception answers one reception, the scans it archived into and the
// arrivals of its study.
func (s receptionService) GetReception(ctx context.Context, req *v1.GetReceptionRequest) (*v1.GetReceptionResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReceptionServiceGetReceptionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	r, arrivals, err := lookupReception(ctx, p, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	scans, more, err := p.ReceptionScans(ctx, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	out := &v1.GetReceptionResponse{Reception: receptionMessage(r), ScansTruncated: more}
	for _, sc := range scans {
		out.Scans = append(out.Scans, &v1.ReceptionScan{Label: sc.Label, SeriesInstanceUid: sc.SeriesUID, Files: sc.Instances, Bytes: sc.Bytes})
	}
	for _, a := range arrivals {
		out.Arrivals = append(out.Arrivals, &v1.ReceptionArrival{Id: a.ID, Arrival: int32(a.Arrival), Step: stepOf(a.State), StartTime: timestamp(a.Started)}) //nolint:gosec // G115: an arrival number, far below 2^31.
	}
	return out, nil
}

// RetryReception runs a failed reception's files again in a new reception;
// a reception already retried is answered as it stands.
func (s receptionService) RetryReception(ctx context.Context, req *v1.RetryReceptionRequest) (*v1.RetryReceptionResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceptionServiceRetryReceptionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetReceiver() != "" {
		if err := itemName("receiver", req.GetReceiver()); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	newID := c.newID()
	if master.ValidateID("reception id", newID) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a reception id that is not valid"))
	}
	failed, retry, err := c.incoming.Retry(ctx, req.GetTenant(), p, req.GetId(), newID, req.GetReceiver(), c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleOperator))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoReception)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	return &v1.RetryReceptionResponse{Reception: receptionMessage(failed), Retry: receptionMessage(retry)}, nil
}

// DiscardReception records that a failed reception's files left in the
// incoming area are removed, with its reason, and then hands their removal
// to the reception pipeline; nothing on a storage is touched.
func (s receptionService) DiscardReception(ctx context.Context, req *v1.DiscardReceptionRequest) (*v1.DiscardReceptionResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceptionServiceDiscardReceptionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := tenant.ValidateDiscardReason(req.GetReason()); err != nil {
		return nil, c.failIn(d, st, fieldOf("reason", err))
	}
	r, err := p.DiscardReception(ctx, req.GetId(), req.GetReason(), c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleOperator))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoReception)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	// The discard is recorded; the bytes are the pipeline's to remove, and
	// a removal that fails here is done again when it next starts.
	if err := c.incoming.RemoveDiscarded(ctx, req.GetTenant(), r.ID); err != nil {
		e := d.Entry(master.OutcomeDone, "", access.Facts{})
		c.log.Warn("api: the files of a discarded reception were not removed yet", "request_id", e.RequestID, "reception", r.ID, "error", fault.Message(err))
	}
	return &v1.DiscardReceptionResponse{Reception: receptionMessage(r)}, nil
}
