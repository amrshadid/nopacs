package api

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// catalogService is CatalogService: the subjects, sessions and scans a
// tenant holds, read from its database alone.
type catalogService struct{ c *core }

var _ nopacsv1connect.CatalogServiceHandler = catalogService{}

// orderOf returns the order a request asks for. An unspecified order is
// newest first; a number the contract does not define is invalid.
func orderOf(o v1.CatalogOrder) (tenant.CatalogOrder, error) {
	switch o {
	case v1.CatalogOrder_CATALOG_ORDER_UNSPECIFIED, v1.CatalogOrder_CATALOG_ORDER_NEWEST:
		return tenant.OrderNewest, nil
	case v1.CatalogOrder_CATALOG_ORDER_LABEL:
		return tenant.OrderLabel, nil
	case v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE:
		return tenant.OrderStudyDate, nil
	case v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION:
		return tenant.OrderSeriesDescription, nil
	}
	return tenant.OrderNewest, invalid("order", "is an order the contract defines")
}

// rangeOf returns the time range of a request, each bound checked.
func rangeOf(start, end *timestamppb.Timestamp) (tenant.TimeRange, error) {
	var r tenant.TimeRange
	var err error
	if r.Start, err = filterTime("start_time", start); err != nil {
		return r, err
	}
	if r.End, err = filterTime("end_time", end); err != nil {
		return r, err
	}
	if !r.Start.IsZero() && !r.End.IsZero() && !r.End.After(r.Start) {
		return r, invalid("end_time", "is after start_time")
	}
	return r, nil
}

// withFilter returns the port's Invalid error of a filter that no index
// serves or that holds a value that is not valid, as the request's: the
// message is the port's, which names what is served.
func withFilter(err error) error {
	if fault.KindOf(err) != fault.Invalid {
		return err
	}
	return fault.Wrap(fault.Invalid, err, "filters: "+fault.Message(err))
}

// subjectMessage returns a subject as the contract writes it.
func subjectMessage(s tenant.CatalogSubject) *v1.Subject {
	return &v1.Subject{
		Label: s.Label, SessionCount: s.Sessions, InstanceCount: s.Instances, SizeBytes: s.Bytes,
		ArchiveTime: timestamp(s.Created), UpdateTime: timestamp(s.Updated),
	}
}

// sessionMessage returns a session as the contract writes it.
func sessionMessage(s tenant.CatalogSession) *v1.Session {
	return &v1.Session{
		SubjectLabel: s.SubjectLabel, Label: s.Label, StudyInstanceUid: s.StudyUID, StudyDate: s.StudyDate,
		Modalities: append([]string(nil), s.Modalities...), ScanCount: s.Scans, InstanceCount: s.Instances,
		SizeBytes: s.Bytes, Storage: s.Storage, Receiver: s.Receiver,
		ArchiveTime: timestamp(s.Created), UpdateTime: timestamp(s.Updated),
	}
}

// scanMessage returns a scan as the contract writes it. A series number is
// answered only when the scan's files held one.
func scanMessage(s tenant.CatalogScan) *v1.Scan {
	m := &v1.Scan{
		SubjectLabel: s.SubjectLabel, SessionLabel: s.SessionLabel, Label: s.Label, SeriesInstanceUid: s.SeriesUID,
		Modality: s.Values.Modality, SeriesDescription: s.Values.SeriesDescription,
		InstanceCount: s.Instances, SizeBytes: s.Bytes, Storage: s.Storage,
		ManifestSha256: s.ManifestSHA256.String(), Receiver: s.Receiver,
		ArchiveTime: timestamp(s.Created), UpdateTime: timestamp(s.Updated),
	}
	if s.Values.HasSeriesNumber {
		n := s.Values.SeriesNumber
		m.SeriesNumber = &n
	}
	return m
}

// ListSubjects answers a page of a tenant's subjects.
func (s catalogService) ListSubjects(ctx context.Context, req *v1.ListSubjectsRequest) (*v1.ListSubjectsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceListSubjectsProcedure)
	if err != nil {
		return nil, err
	}
	details := catalogFilterDetails(req.GetLabelPrefix(), req.GetOrder())
	timeDetails(details, req.GetStartTime().AsTime(), req.GetStartTime() != nil, req.GetEndTime().AsTime(), req.GetEndTime() != nil)
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	f := tenant.SubjectFilter{LabelPrefix: req.GetLabelPrefix()}
	if f.Order, err = orderOf(req.GetOrder()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if f.Range, err = rangeOf(req.GetStartTime(), req.GetEndTime()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := f.Validate(); err != nil {
		return nil, c.failIn(d, st, withFilter(err))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	got, err := p.ListSubjects(ctx, f, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListSubjectsResponse{Subjects: make([]*v1.Subject, 0, len(got.Items)), NextPageToken: got.Next, TotalCount: got.Total}
	for _, it := range got.Items {
		out.Subjects = append(out.Subjects, subjectMessage(it))
	}
	return out, nil
}

// GetSubject answers one subject.
func (s catalogService) GetSubject(ctx context.Context, req *v1.GetSubjectRequest) (*v1.GetSubjectResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceGetSubjectProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := tenant.ValidateLabel("subject", req.GetSubjectLabel()); err != nil {
		return nil, c.failIn(d, st, fieldOf("subject_label", err))
	}
	got, err := p.CatalogSubject(ctx, req.GetSubjectLabel())
	if err != nil {
		return nil, c.failIn(d, st, relabeled(ctx, st, label.Path{Subject: req.GetSubjectLabel()}, err))
	}
	c.done(d, st)
	return &v1.GetSubjectResponse{Subject: subjectMessage(got)}, nil
}

// ListSessions answers a page of a tenant's sessions.
func (s catalogService) ListSessions(ctx context.Context, req *v1.ListSessionsRequest) (*v1.ListSessionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceListSessionsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), sessionFilterDetails(req))
	if err != nil {
		return nil, err
	}
	f := tenant.SessionFilter{
		SubjectLabel: req.GetSubjectLabel(), LabelPrefix: req.GetLabelPrefix(),
		StudyDateStart: req.GetStudyDateStart(), StudyDateEnd: req.GetStudyDateEnd(),
		Modality: req.GetModality(), Receiver: req.GetReceiver(),
	}
	if f.Order, err = orderOf(req.GetOrder()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if f.Range, err = rangeOf(req.GetStartTime(), req.GetEndTime()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := f.Validate(); err != nil {
		return nil, c.failIn(d, st, withFilter(err))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	got, err := p.ListSessions(ctx, f, page)
	if err != nil {
		return nil, c.failIn(d, st, relabeled(ctx, st, filterPath(f.SubjectLabel, ""), listError(err, page.Cursor)))
	}
	c.done(d, st)
	out := &v1.ListSessionsResponse{Sessions: make([]*v1.Session, 0, len(got.Items)), NextPageToken: got.Next, TotalCount: got.Total}
	for _, it := range got.Items {
		out.Sessions = append(out.Sessions, sessionMessage(it))
	}
	return out, nil
}

// GetSession answers one session.
func (s catalogService) GetSession(ctx context.Context, req *v1.GetSessionRequest) (*v1.GetSessionResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceGetSessionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := tenant.ValidateLabel("subject", req.GetSubjectLabel()); err != nil {
		return nil, c.failIn(d, st, fieldOf("subject_label", err))
	}
	if err := tenant.ValidateLabel("session", req.GetSessionLabel()); err != nil {
		return nil, c.failIn(d, st, fieldOf("session_label", err))
	}
	got, err := p.CatalogSession(ctx, req.GetSubjectLabel(), req.GetSessionLabel())
	if err != nil {
		return nil, c.failIn(d, st, relabeled(ctx, st, label.Path{Subject: req.GetSubjectLabel(), Session: req.GetSessionLabel()}, err))
	}
	c.done(d, st)
	return &v1.GetSessionResponse{Session: sessionMessage(got)}, nil
}

// ListScans answers a page of a tenant's scans.
func (s catalogService) ListScans(ctx context.Context, req *v1.ListScansRequest) (*v1.ListScansResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceListScansProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), scanFilterDetails(req))
	if err != nil {
		return nil, err
	}
	f := tenant.ScanFilter{
		SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(), LabelPrefix: req.GetLabelPrefix(),
		Modality: req.GetModality(), DescriptionPrefix: tenant.FoldDescriptionPrefix(req.GetSeriesDescriptionPrefix()),
		Receiver: req.GetReceiver(),
	}
	if f.Order, err = orderOf(req.GetOrder()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if f.Range, err = rangeOf(req.GetStartTime(), req.GetEndTime()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := f.Validate(); err != nil {
		return nil, c.failIn(d, st, withFilter(err))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	got, err := p.ListScans(ctx, f, page)
	if err != nil {
		return nil, c.failIn(d, st, relabeled(ctx, st, filterPath(f.SubjectLabel, f.SessionLabel), listError(err, page.Cursor)))
	}
	c.done(d, st)
	out := &v1.ListScansResponse{Scans: make([]*v1.Scan, 0, len(got.Items)), NextPageToken: got.Next, TotalCount: got.Total}
	for _, it := range got.Items {
		out.Scans = append(out.Scans, scanMessage(it))
	}
	return out, nil
}

// GetScan answers one scan.
func (s catalogService) GetScan(ctx context.Context, req *v1.GetScanRequest) (*v1.GetScanResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CatalogServiceGetScanProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	path := label.Path{Subject: req.GetSubjectLabel(), Session: req.GetSessionLabel(), Scan: req.GetScanLabel()}
	if err := tenant.ValidateScanPath(path); err != nil {
		return nil, c.failIn(d, st, err)
	}
	got, err := p.CatalogScan(ctx, path)
	if err != nil {
		return nil, c.failIn(d, st, relabeled(ctx, st, path, err))
	}
	c.done(d, st)
	return &v1.GetScanResponse{Scan: scanMessage(got)}, nil
}
