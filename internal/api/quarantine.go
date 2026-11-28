package api

import (
	"context"
	"errors"
	"maps"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoFile answers a quarantined file's id the tenant does not have.
const messageNoFile = "the tenant has no quarantined file with this id"

// quarantineService is QuarantineService: a tenant's quarantine records,
// kept in its store, and the files they hold, kept in the incoming area by
// the reception pipeline.
type quarantineService struct{ c *core }

var _ nopacsv1connect.QuarantineServiceHandler = quarantineService{}

// reasons pairs each reason the store keeps with the contract's value.
var reasons = map[tenant.QuarantineReason]v1.QuarantineReason{
	tenant.QuarantineNotDICOM:            v1.QuarantineReason_QUARANTINE_REASON_NOT_DICOM,
	tenant.QuarantineSOPClassNotAccepted: v1.QuarantineReason_QUARANTINE_REASON_SOP_CLASS_NOT_ACCEPTED,
	tenant.QuarantineTooLarge:            v1.QuarantineReason_QUARANTINE_REASON_TOO_LARGE,
	tenant.QuarantineBadUID:              v1.QuarantineReason_QUARANTINE_REASON_BAD_UID,
	tenant.QuarantineBadLabel:            v1.QuarantineReason_QUARANTINE_REASON_BAD_LABEL,
	tenant.QuarantineLabelConflict:       v1.QuarantineReason_QUARANTINE_REASON_LABEL_CONFLICT,
	tenant.QuarantineDifferentFile:       v1.QuarantineReason_QUARANTINE_REASON_DIFFERENT_FILE,
	tenant.QuarantineUIDElsewhere:        v1.QuarantineReason_QUARANTINE_REASON_UID_ELSEWHERE,
	tenant.QuarantineStorageRefused:      v1.QuarantineReason_QUARANTINE_REASON_STORAGE_REFUSED,
	tenant.QuarantineReadBackMismatch:    v1.QuarantineReason_QUARANTINE_REASON_READ_BACK_MISMATCH,
	tenant.QuarantineHeaderTooLarge:      v1.QuarantineReason_QUARANTINE_REASON_HEADER_TOO_LARGE,
	tenant.QuarantineIncomingLost:        v1.QuarantineReason_QUARANTINE_REASON_INCOMING_LOST,
	tenant.QuarantineUIDMismatch:         v1.QuarantineReason_QUARANTINE_REASON_UID_MISMATCH,
	tenant.QuarantineDeidFailed:          v1.QuarantineReason_QUARANTINE_REASON_DEID_FAILED,
	tenant.QuarantineResidualValue:       v1.QuarantineReason_QUARANTINE_REASON_RESIDUAL_VALUE,
	tenant.QuarantineProfileRefused:      v1.QuarantineReason_QUARANTINE_REASON_PROFILE_REFUSED,
	tenant.QuarantineUnrouted:            v1.QuarantineReason_QUARANTINE_REASON_UNROUTED,
	tenant.QuarantineBurnedInRisk:        v1.QuarantineReason_QUARANTINE_REASON_BURNED_IN_RISK,
	tenant.QuarantineRepairsDamaged:      v1.QuarantineReason_QUARANTINE_REASON_REPAIRS_DAMAGED,
	tenant.QuarantineWasDeleted:          v1.QuarantineReason_QUARANTINE_REASON_WAS_DELETED,
}

// quarantineStates pairs each state the store keeps with the contract's.
var quarantineStates = map[tenant.QuarantineState]v1.QuarantineState{
	tenant.QuarantineHeld:     v1.QuarantineState_QUARANTINE_STATE_HELD,
	tenant.QuarantineReleased: v1.QuarantineState_QUARANTINE_STATE_RELEASED,
	tenant.QuarantineRejected: v1.QuarantineState_QUARANTINE_STATE_REJECTED,
}

// fileMessage returns a quarantine record as the contract writes it. Where
// its bytes are in the incoming area is the server's, and is not answered.
func fileMessage(f tenant.QuarantinedFile) *v1.QuarantinedFile {
	m := &v1.QuarantinedFile{
		Id: f.ID, Reason: reasons[f.Reason], Details: maps.Clone(f.Details), State: quarantineStates[f.State],
		Receiver: f.Receiver, Reception: f.ReceptionID,
		Caller:    &v1.Caller{CallingAeTitle: f.Caller.CallingAETitle, CalledAeTitle: f.Caller.CalledAETitle, Address: f.Caller.Address},
		SizeBytes: f.Size, Sha256: f.SHA256.String(), KeptSha256: f.KeptSHA256.String(),
		SopClassUid: f.SOPClassUID, SopInstanceUid: f.SOPInstanceUID, StudyInstanceUid: f.StudyUID,
		SeriesInstanceUid: f.SeriesUID, TransferSyntaxUid: f.TransferSyntaxUID, ContentKept: f.ContentKept,
		QuarantineTime: timestamp(f.Quarantined), DecideTime: timestamp(f.Decided),
		ReleaseReception: f.ReleaseReception, RejectReason: f.RejectReason, ReleasedFrom: f.ReleasedFrom,
		ReleaseReceiver: f.ReleaseReceiver, ReleaseReason: f.ReleaseReason, ReleaseStorage: f.ReleaseStorage,
		ImportJob: f.ImportJob,
	}
	if !f.ReleaseProfile.IsZero() {
		m.ReleaseProfile = &v1.ProfileVersionRef{Name: f.ReleaseProfile.Name, Version: f.ReleaseProfile.Version}
	}
	if f.ReleaseMode == tenant.ReleaseAsIs {
		m.ReleaseMode = v1.ReleaseMode_RELEASE_MODE_AS_IS
	}
	return m
}

// quarantineFilterDetails returns the filters of a listing of quarantined
// files, as its entry's details record them: a UID only as present.
func quarantineFilterDetails(r *v1.ListQuarantinedFilesRequest) map[string]string {
	d := map[string]string{}
	if r.GetState() != v1.QuarantineState_QUARANTINE_STATE_UNSPECIFIED {
		d[detailState] = r.GetState().String()
	}
	if r.GetReason() != v1.QuarantineReason_QUARANTINE_REASON_UNSPECIFIED {
		d[detailReason] = r.GetReason().String()
	}
	if r.GetReceiver() != "" {
		d[detailReceiver] = clip(r.GetReceiver(), 64)
	}
	if r.GetReception() != "" {
		d[detailReception] = clip(r.GetReception(), 64)
	}
	if r.GetStudyInstanceUid() != "" {
		d[detailStudyUID] = presentValue
	}
	if r.GetSopInstanceUid() != "" {
		d[detailSOPUID] = presentValue
	}
	timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
	return d
}

// quarantineFilter returns the filter a listing asks for, checked.
func quarantineFilter(r *v1.ListQuarantinedFilesRequest) (tenant.QuarantineFilter, error) {
	f := tenant.QuarantineFilter{Receiver: r.GetReceiver(), ReceptionID: r.GetReception(), StudyUID: r.GetStudyInstanceUid(), SOPInstanceUID: r.GetSopInstanceUid()}
	if r.GetState() != v1.QuarantineState_QUARANTINE_STATE_UNSPECIFIED {
		for s, v := range quarantineStates {
			if v == r.GetState() {
				f.State = s
			}
		}
		if f.State == "" {
			return f, invalid("state", "is a state the contract defines, or unspecified for every state")
		}
	}
	if r.GetReason() != v1.QuarantineReason_QUARANTINE_REASON_UNSPECIFIED {
		for s, v := range reasons {
			if v == r.GetReason() {
				f.Reason = s
			}
		}
		if f.Reason == "" {
			return f, invalid("reason", "is a reason the contract defines, or unspecified for every reason")
		}
	}
	if f.Receiver != "" {
		if err := itemName("receiver", f.Receiver); err != nil {
			return f, err
		}
	}
	if f.ReceptionID != "" {
		if err := serverID("reception", f.ReceptionID); err != nil {
			return f, err
		}
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
		return f, fieldOf("study_instance_uid or sop_instance_uid", err)
	}
	return f, nil
}

// lookupFile reads a quarantine record, answering one the tenant does not
// have as NotFound with the contract's message.
func lookupFile(ctx context.Context, p tenant.Pipeline, id string) (tenant.QuarantinedFile, error) {
	f, err := p.QuarantinedFile(ctx, id)
	if fault.KindOf(err) == fault.NotFound {
		return f, fault.Wrap(fault.NotFound, err, messageNoFile)
	}
	return f, err
}

// ListQuarantinedFiles answers a page of a tenant's quarantine records,
// newest first.
func (s quarantineService) ListQuarantinedFiles(ctx context.Context, req *v1.ListQuarantinedFilesRequest) (*v1.ListQuarantinedFilesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.QuarantineServiceListQuarantinedFilesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), quarantineFilterDetails(req))
	if err != nil {
		return nil, err
	}
	filter, err := quarantineFilter(req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListQuarantined(ctx, filter, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListQuarantinedFilesResponse{Files: make([]*v1.QuarantinedFile, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Files = append(out.Files, recordFor(d, it))
	}
	return out, nil
}

// GetQuarantinedFile answers one quarantine record and, when asked and when
// one can be given, the held file's header.
func (s quarantineService) GetQuarantinedFile(ctx context.Context, req *v1.GetQuarantinedFileRequest) (*v1.GetQuarantinedFileResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.QuarantineServiceGetQuarantinedFileProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), headerDetails(req))
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetShowValues() && !req.GetIncludeHeader() {
		return nil, c.failIn(d, st, invalid("show_values", "is set only with include_header"))
	}
	f, err := lookupFile(ctx, p, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := &v1.GetQuarantinedFileResponse{File: recordFor(d, f)}
	if req.GetIncludeHeader() && f.ContentKept {
		text, tooLarge, err := c.headers.Header(ctx, req.GetTenant(), f, MaxHeaderBytes)
		if err == nil && !tooLarge && len(text) <= MaxHeaderBytes && !req.GetShowValues() {
			if text, err = headerWithoutValues(text); err != nil {
				err = ErrNoHeader
			}
		}
		switch {
		case errors.Is(err, ErrNoHeader):
			// The contract answers a header it cannot give by leaving it
			// out, without the too-large flag.
		case err != nil:
			return nil, c.failIn(d, st, err)
		case tooLarge:
			out.HeaderTooLarge = true
		case len(text) > MaxHeaderBytes:
			// A reader that broke its promise: left out, never cut.
			out.HeaderTooLarge = true
		default:
			out.HeaderJson = text
		}
	}
	c.done(d, st)
	return out, nil
}

// headerDetails returns GetQuarantinedFile's audit details: whether the
// header was asked for, and whether its values were.
func headerDetails(r *v1.GetQuarantinedFileRequest) map[string]string {
	if r.GetIncludeHeader() {
		return showValuesDetails(r.GetShowValues(), map[string]string{detailIncludeHeader: "true"})
	}
	return showValuesDetails(r.GetShowValues(), map[string]string{detailIncludeHeader: "false"})
}

// ReleaseQuarantinedFile runs a held file through the pipeline again, as it
// is; a file already released is answered as it stands.
func (s quarantineService) ReleaseQuarantinedFile(ctx context.Context, req *v1.ReleaseQuarantinedFileRequest) (*v1.ReleaseQuarantinedFileResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.QuarantineServiceReleaseQuarantinedFileProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), releaseDetails(req, ""))
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetMode() != v1.ReleaseMode_RELEASE_MODE_AS_IS {
		return nil, c.failIn(d, st, invalid("mode", "is RELEASE_MODE_AS_IS, the one mode of this version"))
	}
	if req.GetReceiver() != "" {
		if err := itemName("receiver", req.GetReceiver()); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	if req.GetReason() != "" {
		if err := tenant.ValidateDiscardReason(req.GetReason()); err != nil {
			return nil, c.failIn(d, st, fieldOf("reason", err))
		}
	}
	if req.GetStorage() != "" {
		if err := itemName("storage", req.GetStorage()); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	profile, err := releaseProfile(req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if !profile.IsZero() {
		// The version a receiver may de-identify with, checked as a
		// receiver's is; that the receiver the file runs by de-identifies
		// is the store's to check, in the release's transaction.
		if err := c.checkProfileVersion(ctx, p, "profile", profile); err != nil {
			return nil, c.failIn(d, st, err)
		}
		// A released file is de-identified as a receiver's would be, so the
		// version is a layer over the tenant's base too.
		if err := c.checkReceiverBase(ctx, p, "profile", profile); err != nil {
			return nil, c.layerFail(d, st, access.Facts{}, err)
		}
	}
	// Read first for the receiver the release runs through when the request
	// names none, which the entry records.
	before, err := lookupFile(ctx, p, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	receptionID := c.newID()
	if master.ValidateID("reception id", receptionID) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a reception id that is not valid"))
	}
	facts := access.Facts{Details: releaseDetails(req, before.Receiver)}
	f, err := c.incoming.Release(ctx, req.GetTenant(), p, tenant.Release{ID: req.GetId(), Mode: tenant.ReleaseAsIs, Receiver: req.GetReceiver(),
		ReceptionID: receptionID, Reason: req.GetReason(), Storage: req.GetStorage(), Profile: profile, Now: c.now()}, changeEntry(d, facts),
		c.roleGuard(d, releaseRole(req)))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoFile)
	}
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.ReleaseQuarantinedFileResponse{File: recordFor(d, f)}, nil
}

// releaseProfile returns the profile version a release names, the zero
// version for none, or an Invalid error naming the field.
func releaseProfile(req *v1.ReleaseQuarantinedFileRequest) (tenant.ProfileRef, error) {
	pv := req.GetProfile()
	if pv == nil {
		return tenant.ProfileRef{}, nil
	}
	ref := tenant.ProfileRef{Name: pv.GetName(), Version: pv.GetVersion()}
	if err := ref.Validate(); err != nil {
		return tenant.ProfileRef{}, fieldOf("profile", err)
	}
	return ref, nil
}

// releaseRole is the role a release needs on the tenant: operator, or admin
// when it names a profile version, which decides what leaves a file as a
// receiver's version does, and a receiver's choice of version needs admin.
func releaseRole(req *v1.ReleaseQuarantinedFileRequest) master.Role {
	if req.GetProfile() != nil {
		return master.RoleAdmin
	}
	return master.RoleOperator
}

// releaseDetails returns a release's audit details: its mode, the storage
// and the profile version it names when it names them, and the receiver it
// runs through, the one the request names or else the one the file came to
// (first, when it is known).
func releaseDetails(req *v1.ReleaseQuarantinedFileRequest, first string) map[string]string {
	d := map[string]string{detailMode: req.GetMode().String()}
	if req.GetStorage() != "" {
		d[detailStorage] = clip(req.GetStorage(), 64)
	}
	if pv := req.GetProfile(); pv != nil {
		d[detailProfile] = clip(tenant.ProfileRef{Name: pv.GetName(), Version: pv.GetVersion()}.String(), 80)
	}
	switch {
	case req.GetReceiver() != "":
		d[detailReceiver] = clip(req.GetReceiver(), 64)
	case first != "":
		d[detailReceiver] = first
	}
	return d
}

// RejectQuarantinedFile records a held file as rejected with its reason,
// then hands the removal of its bytes to the reception pipeline; a file
// already rejected is answered as it stands, with its first reason.
func (s quarantineService) RejectQuarantinedFile(ctx context.Context, req *v1.RejectQuarantinedFileRequest) (*v1.RejectQuarantinedFileResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.QuarantineServiceRejectQuarantinedFileProcedure)
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
	// Read first for where its bytes are, which the record no longer says
	// once it is rejected.
	before, err := lookupFile(ctx, p, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	f, err := p.RejectQuarantined(ctx, req.GetId(), req.GetReason(), c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleOperator))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoFile)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if before.State == tenant.QuarantineHeld && before.Incoming != "" {
		if err := c.incoming.RemoveRejected(ctx, req.GetTenant(), before.ID, before.Incoming); err != nil {
			e := d.Entry(master.OutcomeDone, "", access.Facts{})
			c.log.Warn("api: a rejected file's bytes were not removed yet", "request_id", e.RequestID, "file", f.ID, "error", fault.Message(err))
		}
	}
	return &v1.RejectQuarantinedFileResponse{File: recordFor(d, f)}, nil
}
