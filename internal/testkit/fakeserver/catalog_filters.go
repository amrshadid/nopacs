package fakeserver

import (
	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The three functions below build the port's filter from a request and call
// its Validate, so the fake refuses exactly the combinations the real
// server does (an order that does not serve a filter, two narrowing filters
// at once) and a command tested against it cannot rely on a looser rule.

// filterOrder returns the port's order of a request's.
func filterOrder(o v1.CatalogOrder) (tenant.CatalogOrder, error) {
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
	return tenant.OrderNewest, fault.New(fault.Invalid, "order: is an order the contract defines")
}

// filterRange returns the time range of a request.
func filterRange(start, end *timestamppb.Timestamp) tenant.TimeRange {
	var r tenant.TimeRange
	if start != nil {
		r.Start = start.AsTime()
	}
	if end != nil {
		r.End = end.AsTime()
	}
	return r
}

// validated returns the port's Invalid error of a filter as the request's,
// worded as the real server words it.
func validated(err error) error {
	if err == nil || fault.KindOf(err) != fault.Invalid {
		return err
	}
	return fault.Wrap(fault.Invalid, err, "filters: "+fault.Message(err))
}

// checkSubjectFilter validates ListSubjects' filters.
func checkSubjectFilter(req *v1.ListSubjectsRequest) error {
	order, err := filterOrder(req.GetOrder())
	if err != nil {
		return err
	}
	f := tenant.SubjectFilter{LabelPrefix: req.GetLabelPrefix(), Order: order, Range: filterRange(req.GetStartTime(), req.GetEndTime())}
	return validated(f.Validate())
}

// checkSessionFilter validates ListSessions' filters.
func checkSessionFilter(req *v1.ListSessionsRequest) error {
	order, err := filterOrder(req.GetOrder())
	if err != nil {
		return err
	}
	f := tenant.SessionFilter{
		SubjectLabel: req.GetSubjectLabel(), LabelPrefix: req.GetLabelPrefix(),
		StudyDateStart: req.GetStudyDateStart(), StudyDateEnd: req.GetStudyDateEnd(),
		Modality: req.GetModality(), Receiver: req.GetReceiver(),
		Order: order, Range: filterRange(req.GetStartTime(), req.GetEndTime()),
	}
	return validated(f.Validate())
}

// checkScanFilter validates ListScans' filters.
func checkScanFilter(req *v1.ListScansRequest) error {
	order, err := filterOrder(req.GetOrder())
	if err != nil {
		return err
	}
	f := tenant.ScanFilter{
		SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(), LabelPrefix: req.GetLabelPrefix(),
		Modality: req.GetModality(), DescriptionPrefix: tenant.FoldDescriptionPrefix(req.GetSeriesDescriptionPrefix()),
		Receiver: req.GetReceiver(),
		Order:    order, Range: filterRange(req.GetStartTime(), req.GetEndTime()),
	}
	return validated(f.Validate())
}
