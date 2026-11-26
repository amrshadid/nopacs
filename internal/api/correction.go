package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReplaceSources makes the file a replace puts from a held quarantined
// file, through the tenant's running reception pipeline, which owns the
// incoming area and de-identifies as a release would
// (receive.Pipeline.ReplaceSource).
type ReplaceSources interface {
	// ReplaceSource makes the file a replace from q puts; the caller calls
	// its Done once the replace has answered.
	ReplaceSource(ctx context.Context, tenantName string, q tenant.QuarantinedFile) (receive.ReplaceFile, error)
}

// noReplaceSources answers every replace unavailable, as a server that runs
// no reception.
type noReplaceSources struct{}

// ReplaceSource answers unavailable.
func (noReplaceSources) ReplaceSource(context.Context, string, tenant.QuarantinedFile) (receive.ReplaceFile, error) {
	return receive.ReplaceFile{}, fault.New(fault.Unavailable, "the tenant's reception is not running now; try again shortly")
}

// messageNoCorrection answers a correction's id the tenant does not have.
const messageNoCorrection = "the tenant has no correction with this id"

// messageOverrideNotAdmin answers a delete that sets override_retention
// from a caller who is not the installation's admin or the local operator.
const messageOverrideNotAdmin = "override_retention: only an admin on the installation, or the local operator, lifts a retention rule's keep_at_least; " +
	"the person who wants a delete is not the one who lifts the rule that refuses it"

// correctionService is CorrectionService: the corrections a tenant's
// store keeps, made through the tenant's corrections engine
// (Config.Corrections).
type correctionService struct{ c *core }

var _ nopacsv1connect.CorrectionServiceHandler = correctionService{}

// corrections returns the tenant's correction port of st.
func corrections(st tenant.Store) (tenant.Corrections, error) {
	cs, ok := st.(tenant.Corrections)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no corrections")
	}
	return cs, nil
}

// engine returns the tenant's corrections engine for one request, and its
// release.
func (c *core) engine(ctx context.Context, name string, st tenant.Store) (*correct.Engine, func(), error) {
	if c.corrections == nil {
		return nil, nil, fault.New(fault.Unavailable, "the server runs no corrections")
	}
	return c.corrections.Use(ctx, name, st)
}

// background runs work on the tenant's engine after the request, when the
// server runs corrections.
func (c *core) background(name string, st tenant.Store, what string, work func(ctx context.Context, e *correct.Engine) error) {
	if c.corrections != nil {
		c.corrections.Go(name, st, what, work)
	}
}

// correctionID returns an Invalid error unless id can name a correction:
// required and at most 64 bytes; one of another form is answered as one
// the tenant does not have (notCorrection).
func correctionID(id string) error {
	if id == "" {
		return invalid("id", "is required")
	}
	if len(id) > MaxServerIDLength {
		return invalid("id", "is at most 64 bytes")
	}
	return nil
}

// noSuchCorrection answers an id of a form no correction has as NotFound,
// with the contract's message, and passes nil through.
func noSuchCorrection(id string) error {
	if tenant.ValidateCorrectionID(id) != nil {
		return fault.New(fault.NotFound, messageNoCorrection)
	}
	return nil
}

// correctionError gives a store's NotFound for a correction the contract's
// message.
func correctionError(err error) error {
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.NotFound, err, messageNoCorrection)
	}
	return err
}

// callerKeyID returns the id of the key that asks, empty for the local operator.
func callerKeyID(d access.Decision) string {
	if k, ok := d.Principal().Key(); ok {
		return k.ID
	}
	return ""
}

// checkDelete returns the Invalid error of a delete request, or nil.
func checkDelete(req *v1.DeleteRequest, target tenant.CorrectionTarget) error {
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return fieldOf("reason", err)
	}
	if err := target.Validate(); err != nil {
		return fieldOf("target", err)
	}
	switch {
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return invalid("confirm", "is the confirm token of a preview (validate_only) of the same target")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return invalid("confirm", "is at most 128 bytes")
	case req.GetOverrideRetention() && req.GetOverrideReason() == "":
		return invalid("override_reason", "is required with override_retention")
	case !req.GetOverrideRetention() && req.GetOverrideReason() != "":
		return invalid("override_reason", "is empty without override_retention")
	}
	if req.GetOverrideRetention() {
		if err := tenant.ValidateUndoReason(req.GetOverrideReason()); err != nil {
			return fieldOf("override_reason", err)
		}
	}
	return nil
}

// Delete answers a delete's preview, or makes the delete its confirm token
// was given for.
func (s correctionService) Delete(ctx context.Context, req *v1.DeleteRequest) (*v1.DeleteResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceDeleteProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: deleteDetails(req, nil)}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	target := targetOf(req.GetTarget())
	guard := c.roleGuard(d, master.RoleAdmin)
	if req.GetOverrideRetention() {
		facts.Details[detailReason] = req.GetReason()
		facts.Details[detailOverrideReason] = req.GetOverrideReason()
		// A keep-at-least is often a legal duty: lifting it is rare, and
		// kept to the installation's admin, so a tenant's admin who wants a
		// delete cannot also lift the rule that refuses it. The role is
		// judged before the request is validated, so a caller without it
		// learns nothing from the shape of its request.
		// A refusal is written to the installation's log, as the check's
		// own are (audit.proto).
		if !d.Principal().Role(master.Installation).Includes(master.RoleAdmin) {
			facts.TenantName = req.GetTenant()
			return nil, c.failWith(d, nil, facts, fault.New(fault.Denied, messageOverrideNotAdmin))
		}
		guard = c.overrideGuard(d)
	}
	if err := checkDelete(req, target); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	defer release()
	prev, err := e.Preview(ctx, target)
	switch {
	case err == nil:
		countDetails(facts.Details, prev.Counts)
	case req.GetValidateOnly() || fault.KindOf(err) != fault.NotFound:
		return nil, c.failWith(d, st, facts, err)
	default:
		// A delete whose token made one already answers it, though its
		// target no longer answers a preview: the delete hid it.
		prev = correct.Preview{}
	}
	if req.GetValidateOnly() {
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.DeleteResponse{Preview: previewMessage(prev)}, nil
	}
	made, err := e.Delete(ctx, correct.DeleteRequest{Target: target, Reason: req.GetReason(), Confirm: req.GetConfirm(), KeyID: callerKeyID(d),
		OverrideRetention: req.GetOverrideRetention(), OverrideReason: req.GetOverrideReason(),
		Audit: changeEntry(d, facts), Guard: guard})
	if err != nil {
		if made.ID != "" {
			// Recorded, then abandoned: its entry was written with it.
			return nil, c.toAPI(d, err)
		}
		return nil, c.failWith(d, st, facts, err)
	}
	prev.Confirm = ""
	return &v1.DeleteResponse{Preview: previewMessage(prev), Correction: correctionMessage(made, true)}, nil
}

// overrideGuard returns the check a delete that lifts retention makes of
// its caller inside the delete's transaction: d's key still admin on the
// tenant and still admin on the installation (or the local operator), so a
// key that loses its installation grant after the request was let in
// cannot still lift a keep-at-least.
func (c *core) overrideGuard(d access.Decision) tenant.Guard {
	onTenant := c.roleGuard(d, master.RoleAdmin)
	onInstallation := d.Actor(master.Installation, master.RoleAdmin)
	return func(ctx context.Context) error {
		if c.seams.guarding != nil {
			c.seams.guarding()
		}
		if err := c.master.CheckActor(ctx, onInstallation); err != nil {
			return err
		}
		return onTenant(ctx)
	}
}

// Relabel records a relabel and leaves its copy and its publish to the
// background.
func (s correctionService) Relabel(ctx context.Context, req *v1.RelabelRequest) (*v1.RelabelResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceRelabelProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: relabelDetails(req)}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	target := targetOf(req.GetTarget())
	switch {
	case tenant.ValidateUndoReason(req.GetReason()) != nil:
		return nil, c.failWith(d, st, facts, fieldOf("reason", tenant.ValidateUndoReason(req.GetReason())))
	case target.Validate() != nil:
		return nil, c.failWith(d, st, facts, fieldOf("target", target.Validate()))
	case target.Instance != "":
		return nil, c.failWith(d, st, facts, invalid("target.instance_id", "is empty: a relabel names a subject, a session or a scan"))
	case label.Check(req.GetToLabel()) != nil:
		return nil, c.failWith(d, st, facts, invalid("to_label", label.Check(req.GetToLabel()).Error()))
	}
	facts.Details[detailReason] = req.GetReason()
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	made, err := e.Relabel(ctx, correct.RelabelRequest{Target: target, ToLabel: req.GetToLabel(), Reason: req.GetReason(), KeyID: callerKeyID(d),
		Audit: changeEntry(d, facts), Guard: c.roleGuard(d, master.RoleAdmin)})
	release()
	if err != nil {
		if made.ID != "" {
			return nil, c.toAPI(d, err)
		}
		return nil, c.failWith(d, st, facts, err)
	}
	if made.State == record.StatePrepared {
		id := made.ID
		c.background(req.GetTenant(), st, "relabel", func(ctx context.Context, e *correct.Engine) error {
			_, err := e.ContinueRelabel(ctx, id)
			return err
		})
	}
	return &v1.RelabelResponse{Correction: correctionMessage(made, true)}, nil
}

// Replace puts a held quarantined file in place of the archived instance
// with its SOP Instance UID.
func (s correctionService) Replace(ctx context.Context, req *v1.ReplaceRequest) (*v1.ReplaceResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceReplaceProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailQuarantined: jobObject(req.GetQuarantinedFile())}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := serverID("quarantined_file", req.GetQuarantinedFile()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("reason", err))
	}
	facts.Details[detailReason] = req.GetReason()
	q, err := lookupFile(ctx, p, req.GetQuarantinedFile())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if q.State == tenant.QuarantineReleased && q.ReleaseMode != tenant.ReleaseAsIs {
		// Used by a replace already: that replace is the answer.
		if made, ok, err := replaceOf(ctx, st, q.ID); err != nil || ok {
			if err != nil {
				return nil, c.failWith(d, st, facts, err)
			}
			c.record(d, st, master.OutcomeDone, "", access.Facts{Object: made.ID, Details: facts.Details})
			return &v1.ReplaceResponse{Correction: correctionMessage(made, true)}, nil
		}
	}
	src, err := c.replaceSources.ReplaceSource(ctx, req.GetTenant(), q)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	defer src.Done()
	old, err := p.HeldInstance(ctx, src.Path, q.SOPInstanceUID)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.Precondition, err, "the tenant no longer holds the instance the file would replace")
	}
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailTarget] = labelPath(src.Path.Subject, src.Path.Session, src.Path.Scan)
	facts.Details[detailOldSHA256] = archivedSum(old).String()
	facts.Details[detailNewSHA256] = src.SHA256.String()
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	defer release()
	made, err := e.Replace(ctx, correct.ReplaceRequest{Quarantine: q.ID, Storage: src.Storage, Path: src.Path, UID: q.SOPInstanceUID,
		Open: src.Open, Line: src.Line, Reason: req.GetReason(), KeyID: callerKeyID(d), Audit: changeEntry(d, facts),
		Guard: c.roleGuard(d, master.RoleAdmin)})
	switch {
	case err != nil && made.State == record.StateAbandoned:
		// Recorded, then abandoned with its reason: the contract answers
		// the correction so, its entry written with it.
		c.log.Warn("api: a replace was abandoned", "correction", made.ID, "reason", made.StateReason, "error", fault.Message(err))
	case err != nil && made.ID != "":
		return nil, c.toAPI(d, err)
	case err != nil:
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.ReplaceResponse{Correction: correctionMessage(made, true)}, nil
}

// archivedSum returns the checksum of an instance's archived bytes.
func archivedSum(in tenant.ScanInstance) tenant.SHA256 {
	if !in.ArchivedSHA256.IsZero() {
		return in.ArchivedSHA256
	}
	return in.SHA256
}

// replaceOf returns the replace not abandoned that used the quarantined
// file id, newest first, and whether there is one.
func replaceOf(ctx context.Context, st tenant.Store, id string) (tenant.Correction, bool, error) {
	cs, err := corrections(st)
	if err != nil {
		return tenant.Correction{}, false, err
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		items, next, err := cs.ListCorrections(ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindReplace}}, page)
		if err != nil {
			return tenant.Correction{}, false, err
		}
		for _, it := range items {
			if it.Quarantine == id && it.State != record.StateAbandoned {
				full, err := cs.Correction(ctx, it.ID)
				return full, err == nil, err
			}
		}
		if next == "" {
			return tenant.Correction{}, false, nil
		}
		page.Cursor = next
	}
}

// UndoCorrection undoes a committed delete, or stops a prepared relabel.
func (s correctionService) UndoCorrection(ctx context.Context, req *v1.UndoCorrectionRequest) (*v1.UndoCorrectionResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceUndoCorrectionProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{}}
	if err := correctionID(req.GetId()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("reason", err))
	}
	facts.Details[detailReason] = req.GetReason()
	if err := noSuchCorrection(req.GetId()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	defer release()
	out, err := e.Undo(ctx, req.GetId(), req.GetReason(), changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, correctionError(err))
	}
	return &v1.UndoCorrectionResponse{Correction: correctionMessage(out, true)}, nil
}

// PurgeCorrection starts a committed correction's purge now and leaves the
// purge to the background.
func (s correctionService) PurgeCorrection(ctx context.Context, req *v1.PurgeCorrectionRequest) (*v1.PurgeCorrectionResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServicePurgeCorrectionProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := correctionID(req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := noSuchCorrection(req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := e.StartPurge(ctx, req.GetId(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleAdmin))
	release()
	if err != nil {
		return nil, c.failIn(d, st, correctionError(err))
	}
	if out.State == record.StatePurging {
		id := out.ID
		c.background(req.GetTenant(), st, "purge", func(ctx context.Context, e *correct.Engine) error {
			_, err := e.ContinuePurge(ctx, id)
			return err
		})
	}
	return &v1.PurgeCorrectionResponse{Correction: correctionMessage(out, true)}, nil
}

// heldValueOf returns the value AllowArrivals names, checked.
func heldValueOf(v *v1.HeldValue) (tenant.HeldValue, error) {
	if v == nil {
		return tenant.HeldValue{}, invalid("value", "is required: one value of the delete's holds")
	}
	kind, ok := mapKey(heldKinds, v.GetKind())
	if !ok {
		return tenant.HeldValue{}, invalid("value.kind", "is subject, study, series or instance")
	}
	out := tenant.HeldValue{Kind: kind, Value: v.GetValue()}
	if err := out.Validate(); err != nil {
		return out, fieldOf("value.value", err)
	}
	return out, nil
}

// AllowArrivals lets one value a purged delete holds arrive again.
func (s correctionService) AllowArrivals(ctx context.Context, req *v1.AllowArrivalsRequest) (*v1.AllowArrivalsResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceAllowArrivalsProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: allowDetails(req)}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := correctionID(req.GetId()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("reason", err))
	}
	value, err := heldValueOf(req.GetValue())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	if err := noSuchCorrection(req.GetId()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	cs, err := corrections(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	cur, err := cs.Correction(ctx, req.GetId())
	if err != nil {
		return nil, c.failWith(d, st, facts, correctionError(err))
	}
	if cur.Kind == record.KindDelete && cur.State == record.StatePurged && cur.ArrivalsAllowed {
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.AllowArrivalsResponse{Correction: correctionMessage(cur, true)}, nil
	}
	out, err := cs.AllowArrivals(ctx, req.GetId(), value, req.GetReason(), c.now(), changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, correctionError(err))
	}
	return &v1.AllowArrivalsResponse{Correction: correctionMessage(out, true)}, nil
}
