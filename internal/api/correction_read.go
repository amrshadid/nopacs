package api

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxFilterValues is the most values one of a listing's filters names, the
// contract's.
const maxFilterValues = 16

// correctionFilter returns the filter a listing of corrections asks for,
// checked, and whether the origins it asks for keep any correction: every
// correction of this version is a person's, so a filter of origins that
// leaves out CORRECTION_ORIGIN_PERSON keeps none.
func correctionFilter(r *v1.ListCorrectionsRequest) (tenant.CorrectionFilter, bool, error) {
	var f tenant.CorrectionFilter
	if len(r.GetKinds()) > maxFilterValues || len(r.GetStates()) > maxFilterValues || len(r.GetOrigins()) > maxFilterValues {
		return f, false, invalid("kinds, states or origins", "name at most 16 values each")
	}
	for _, k := range r.GetKinds() {
		kind, ok := mapKey(correctionKinds, k)
		if !ok {
			return f, false, invalid("kinds", "are kinds the contract defines, none of them unspecified")
		}
		f.Kinds = append(f.Kinds, kind)
	}
	for _, s := range r.GetStates() {
		state, ok := mapKey(correctionStates, s)
		if !ok {
			return f, false, invalid("states", "are states the contract defines, none of them unspecified")
		}
		f.States = append(f.States, state)
	}
	person := len(r.GetOrigins()) == 0
	for _, o := range r.GetOrigins() {
		switch o {
		case v1.CorrectionOrigin_CORRECTION_ORIGIN_PERSON:
			person = true
		case v1.CorrectionOrigin_CORRECTION_ORIGIN_RETENTION:
		default:
			return f, false, invalid("origins", "are origins the contract defines, none of them unspecified")
		}
	}
	return f, person, nil
}

// ListCorrections answers a page of a tenant's corrections, newest first.
func (s correctionService) ListCorrections(ctx context.Context, req *v1.ListCorrectionsRequest) (*v1.ListCorrectionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CorrectionServiceListCorrectionsProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), correctionFilterDetails(req))
	if err != nil {
		return nil, err
	}
	filter, any, err := correctionFilter(req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	cs, err := corrections(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := &v1.ListCorrectionsResponse{Corrections: []*v1.Correction{}}
	if any {
		items, next, err := cs.ListCorrections(ctx, filter, page)
		if err != nil {
			return nil, c.failIn(d, st, listError(err, page.Cursor))
		}
		for _, it := range items {
			out.Corrections = append(out.Corrections, correctionMessage(it, false))
		}
		out.NextPageToken = next
	}
	c.done(d, st)
	return out, nil
}

// GetCorrection answers one correction, with how far it got.
func (s correctionService) GetCorrection(ctx context.Context, req *v1.GetCorrectionRequest) (*v1.GetCorrectionResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CorrectionServiceGetCorrectionProcedure)
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
	cs, err := corrections(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := cs.Correction(ctx, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, correctionError(err))
	}
	c.done(d, st)
	return &v1.GetCorrectionResponse{Correction: correctionMessage(out, true)}, nil
}

// ListScrubFindings answers a page of what scrubs found in a tenant.
func (s correctionService) ListScrubFindings(ctx context.Context, req *v1.ListScrubFindingsRequest) (*v1.ListScrubFindingsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CorrectionServiceListScrubFindingsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), findingFilterDetails(req))
	if err != nil {
		return nil, err
	}
	filter := tenant.ScrubFindingFilter{Storage: req.GetStorage(), IncludeCleared: req.GetIncludeCleared()}
	if len(req.GetKinds()) > maxFilterValues {
		return nil, c.failIn(d, st, invalid("kinds", "name at most 16 values"))
	}
	for _, k := range req.GetKinds() {
		kind, ok := mapKey(findingKinds, k)
		if !ok {
			return nil, c.failIn(d, st, invalid("kinds", "are kinds the contract defines, none of them unspecified"))
		}
		filter.Kinds = append(filter.Kinds, kind)
	}
	if filter.Storage != "" {
		if err := itemName("storage", filter.Storage); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListScrubFindings(ctx, filter, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListScrubFindingsResponse{Findings: make([]*v1.ScrubFinding, 0, len(items)), NextPageToken: next}
	for _, f := range items {
		out.Findings = append(out.Findings, findingMessage(f))
	}
	return out, nil
}

// ResolveInstances answers what became of each (instance, revision) pair.
func (s correctionService) ResolveInstances(ctx context.Context, req *v1.ResolveInstancesRequest) (*v1.ResolveInstancesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CorrectionServiceResolveInstancesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	members := req.GetMembers()
	if len(members) == 0 || len(members) > tenant.MaxResolve {
		return nil, c.failIn(d, st, invalid("members", "are 1 to 1000 pairs"))
	}
	refs := make([]tenant.InstanceRef, 0, len(members))
	for _, m := range members {
		if err := tenant.ValidateInstanceUID(m.GetInstanceId()); err != nil {
			return nil, c.failIn(d, st, fieldOf("members.instance_id", err))
		}
		if m.GetRevision() < 1 {
			return nil, c.failIn(d, st, invalid("members.revision", "is 1 or more"))
		}
		rev := int(m.GetRevision())
		if rev > layout.MaxRevision {
			// Above any revision a key can name: no instance reached it.
			rev = layout.MaxRevision + 1
		}
		refs = append(refs, tenant.InstanceRef{UID: m.GetInstanceId(), Revision: rev})
	}
	cs, err := corrections(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	answers, err := resolve(ctx, cs, refs)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := &v1.ResolveInstancesResponse{Instances: make([]*v1.ResolvedInstance, 0, len(answers))}
	tally := map[v1.InstanceDisposition]int{}
	for i, a := range answers {
		m := resolvedMessage(a)
		m.Member.Revision = members[i].GetRevision()
		if a.Disposition == tenant.ResolvedSuperseded && a.Path.Subject != "" {
			// The checksum of the revision served now, which the store
			// answers only for the current one.
			if in, err := p.HeldInstance(ctx, a.Path, a.Ref.UID); err == nil {
				m.Sha256 = archivedSum(in).String()
			} else if fault.KindOf(err) != fault.NotFound {
				return nil, c.failIn(d, st, err)
			}
		}
		tally[m.GetDisposition()]++
		out.Instances = append(out.Instances, m)
	}
	c.recordResolve(d, st, len(members), tally)
	return out, nil
}

// resolve answers each pair, those above any revision unknown without
// asking the store, which refuses them.
func resolve(ctx context.Context, cs tenant.Corrections, refs []tenant.InstanceRef) ([]tenant.ResolvedInstance, error) {
	out := make([]tenant.ResolvedInstance, len(refs))
	var ask []tenant.InstanceRef
	var at []int
	for i, r := range refs {
		if r.Revision > layout.MaxRevision {
			out[i] = tenant.ResolvedInstance{Ref: r, Disposition: tenant.ResolvedUnknown}
			continue
		}
		ask, at = append(ask, r), append(at, i)
	}
	if len(ask) == 0 {
		return out, nil
	}
	got, err := cs.ResolveInstances(ctx, ask)
	if err != nil {
		return nil, err
	}
	if len(got) != len(ask) {
		return nil, fault.New(fault.Internal, "api: the store answered another number of pairs than asked")
	}
	for j, g := range got {
		out[at[j]] = g
	}
	return out, nil
}

// recordResolve records a resolve's entry: how many pairs were asked and how
// many of each disposition were answered, never an instance id.
func (c *core) recordResolve(d access.Decision, st tenant.Store, asked int, tally map[v1.InstanceDisposition]int) {
	details := map[string]string{detailMembers: strconv.Itoa(asked)}
	for k, n := range tally {
		details[dispositionDetail(k)] = strconv.Itoa(n)
	}
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: details})
}

// dispositionDetail is the key a disposition's count is recorded under.
func dispositionDetail(k v1.InstanceDisposition) string {
	switch k {
	case v1.InstanceDisposition_INSTANCE_DISPOSITION_CURRENT:
		return "current"
	case v1.InstanceDisposition_INSTANCE_DISPOSITION_SUPERSEDED:
		return "superseded"
	case v1.InstanceDisposition_INSTANCE_DISPOSITION_DELETED:
		return "deleted"
	}
	return "unknown"
}
