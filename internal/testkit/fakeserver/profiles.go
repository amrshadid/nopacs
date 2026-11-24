package fakeserver

import (
	"context"
	"errors"
	"sort"
	"strconv"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// profiles is the fake's de-identification profiles: each tenant's
// versions in order, checked and expanded by the real profile language,
// and the answers a test sets for the trial runs and explanations, which
// the fake does not compute (it runs no engine).
type profiles struct {
	versions map[string][]*v1.ProfileVersion
	try      *v1.TryProfileResponse
	explain  *v1.ExplainProfileResponse
	tries    []*v1.TryProfileRequest
}

// ensure makes the table of versions.
func (p *profiles) ensure() {
	if p.versions == nil {
		p.versions = map[string][]*v1.ProfileVersion{}
	}
}

// profileHandler is the fake's ProfileService.
type profileHandler struct{ s *Server }

var _ nopacsv1connect.ProfileServiceHandler = profileHandler{}

// SetTrialAnswer makes TryProfile answer res, whatever it is asked.
func (s *Server) SetTrialAnswer(res *v1.TryProfileResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prof.try = res
}

// SetExplainAnswer makes ExplainProfile answer res, whatever it is asked.
func (s *Server) SetExplainAnswer(res *v1.ExplainProfileResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prof.explain = res
}

// TrialRequests returns the TryProfile requests received so far.
func (s *Server) TrialRequests() []*v1.TryProfileRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.TryProfileRequest(nil), s.prof.tries...)
}

// problemsRefusal is a document refused for its problems, with every
// problem as a ProfileProblems detail, as the server answers it.
func problemsRefusal(ps profile.Problems) error {
	ce := codes.ToConnect(fault.New(fault.Invalid, "document: "+ps[0].String()))
	list := &v1.ProfileProblems{}
	for _, p := range ps {
		list.Problems = append(list.Problems, p.Proto())
	}
	if d, err := connectproto.NewErrorDetail(list); err == nil {
		ce = ce.WithDetail(d)
	}
	return ce
}

// version returns a stored version: 0 for the latest.
func (s *Server) version(tenant, name string, n int32) (*v1.ProfileVersion, error) {
	vs := s.prof.versions[key(tenant, name)]
	if n == 0 && len(vs) > 0 {
		return vs[len(vs)-1], nil
	}
	if n < 1 || int(n) > len(vs) {
		return nil, fault.New(fault.NotFound, "the tenant has no such profile version")
	}
	return vs[n-1], nil
}

// shown returns a version as an answer carries it with the view.
func shown(v *v1.ProfileVersion, view v1.ProfileView, withDocument bool) *v1.ProfileVersion {
	out := proto.Clone(v).(*v1.ProfileVersion)
	if view != v1.ProfileView_PROFILE_VIEW_FULL {
		out.Expanded = nil
	}
	if !withDocument {
		out.Document, out.Expanded = nil, nil
	}
	return out
}

// CreateProfileVersion stores a valid document as the next version.
func (h profileHandler) CreateProfileVersion(_ context.Context, req *v1.CreateProfileVersionRequest) (*v1.CreateProfileVersionResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	if !profile.ValidName(req.GetName()) {
		return nil, fault.New(fault.Invalid, "name: is 1 to 63 of a-z, 0-9 and -, starting with a letter")
	}
	v, err := profile.New(req.GetName(), 1, req.GetDocument(), profile.Shipped())
	var ps profile.Problems
	if errors.As(err, &ps) && len(ps) > 0 {
		return nil, problemsRefusal(ps)
	}
	if err != nil {
		return nil, fault.New(fault.Invalid, "document: "+err.Error())
	}
	k := key(req.GetTenant(), req.GetName())
	vs := s.prof.versions[k]
	if n := len(vs); n > 0 && vs[n-1].GetChecksum() == v.Checksum() && proto.Equal(vs[n-1].GetDocument(), v.Document()) {
		return &v1.CreateProfileVersionResponse{Version: shown(vs[n-1], req.GetView(), true)}, nil
	}
	if req.ExpectedLatestVersion != nil && int(req.GetExpectedLatestVersion()) != len(vs) {
		return nil, fault.New(fault.Precondition, "the profile's latest version is not the one expected")
	}
	stored := &v1.ProfileVersion{Name: req.GetName(), Version: int32(len(vs) + 1), Checksum: v.Checksum(), Note: req.GetNote(), //nolint:gosec // G115: a fake's few versions.
		CreateTime: timestamppb.New(s.now()), Document: v.Document(), Expanded: v.Expanded()}
	s.prof.versions[k] = append(vs, stored)
	return &v1.CreateProfileVersionResponse{Version: shown(stored, req.GetView(), true), Created: true}, nil
}

// GetProfileVersion answers one version.
func (h profileHandler) GetProfileVersion(_ context.Context, req *v1.GetProfileVersionRequest) (*v1.GetProfileVersionResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	v, err := s.version(req.GetTenant(), req.GetName(), req.GetVersion())
	if err != nil {
		return nil, err
	}
	return &v1.GetProfileVersionResponse{Version: shown(v, req.GetView(), true)}, nil
}

// ListProfiles answers the tenant's profiles by name.
func (h profileHandler) ListProfiles(_ context.Context, req *v1.ListProfilesRequest) (*v1.ListProfilesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	keys := forTenant(s.prof.versions, req.GetTenant())
	sort.Strings(keys)
	page, next, err := pageOfKeys(keys, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListProfilesResponse{NextPageToken: next}
	for _, k := range page {
		vs := s.prof.versions[k]
		last := vs[len(vs)-1]
		out.Profiles = append(out.Profiles, &v1.ProfileSummary{Name: last.GetName(), LatestVersion: last.GetVersion(), LatestCreateTime: last.GetCreateTime()})
	}
	return out, nil
}

// ListProfileVersions answers a profile's versions, newest first.
func (h profileHandler) ListProfileVersions(_ context.Context, req *v1.ListProfileVersionsRequest) (*v1.ListProfileVersionsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	vs := s.prof.versions[key(req.GetTenant(), req.GetName())]
	if len(vs) == 0 {
		return nil, fault.New(fault.NotFound, "the tenant has no profile with this name")
	}
	out := &v1.ListProfileVersionsResponse{}
	for i := len(vs) - 1; i >= 0; i-- {
		out.Versions = append(out.Versions, shown(vs[i], 0, false))
	}
	return out, nil
}

// ValidateProfile checks a document with the real language.
func (h profileHandler) ValidateProfile(_ context.Context, req *v1.ValidateProfileRequest) (*v1.ValidateProfileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	e, ps := profile.Expand(req.GetDocument(), profile.Shipped())
	out := &v1.ValidateProfileResponse{}
	for _, p := range ps {
		out.Problems = append(out.Problems, p.Proto())
	}
	if len(ps) == 0 {
		_, out.Checksum, _ = profile.Checksum(e)
		if req.GetView() == v1.ProfileView_PROFILE_VIEW_FULL {
			out.Expanded = e
		}
	}
	return out, nil
}

// ListProfileBases answers the bases the binary ships.
func (h profileHandler) ListProfileBases(context.Context, *v1.ListProfileBasesRequest) (*v1.ListProfileBasesResponse, error) {
	return &v1.ListProfileBasesResponse{Bases: profile.Shipped().Proto()}, nil
}

// TryProfile records the request and answers what the test set.
func (h profileHandler) TryProfile(_ context.Context, req *v1.TryProfileRequest) (*v1.TryProfileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	s.prof.tries = append(s.prof.tries, req)
	if s.prof.try == nil {
		return nil, fault.New(fault.Unavailable, "the fake has no trial answer set")
	}
	return s.prof.try, nil
}

// ExplainProfile answers what the test set.
func (h profileHandler) ExplainProfile(_ context.Context, req *v1.ExplainProfileRequest) (*v1.ExplainProfileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	if s.prof.explain == nil {
		return nil, fault.New(fault.Unavailable, "the fake has no explanation set")
	}
	return s.prof.explain, nil
}

// DiffProfileVersions compares two stored versions with the real language.
func (h profileHandler) DiffProfileVersions(_ context.Context, req *v1.DiffProfileVersionsRequest) (*v1.DiffProfileVersionsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	toName := req.GetToName()
	if toName == "" {
		toName = req.GetName()
	}
	from, err := s.version(req.GetTenant(), req.GetName(), req.GetFromVersion())
	if err != nil {
		return nil, err
	}
	to, err := s.version(req.GetTenant(), toName, req.GetToVersion())
	if err != nil {
		return nil, err
	}
	out := profile.Diff(from.GetExpanded(), to.GetExpanded())
	out.From, out.To = shown(from, 0, false), shown(to, 0, false)
	return out, nil
}

// passThrough reports whether err is an API error the fake answers as it
// is, details included: a refused document's ProfileProblems.
func passThrough(err error) (*connect.Error, bool) {
	var ce *connect.Error
	if errors.As(err, &ce) && len(ce.Details()) > 0 {
		return ce, true
	}
	return nil, false
}

// DescribeProfile describes a stored version or a document with the real
// language; the method record holds the codes the version's bases claim,
// with no software versions (the fake runs no engine).
func (h profileHandler) DescribeProfile(_ context.Context, req *v1.DescribeProfileRequest) (*v1.DescribeProfileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.prof.ensure()
	out := &v1.DescribeProfileResponse{}
	var e *v1.ExpandedProfile
	method := ""
	switch k := req.GetProfile().GetKind().(type) {
	case *v1.TrialProfile_Version:
		v, err := s.version(req.GetTenant(), k.Version.GetName(), k.Version.GetVersion())
		if err != nil {
			return nil, err
		}
		e, out.Version, out.Checksum = v.GetExpanded(), shown(v, 0, false), v.GetChecksum()
		method = v.GetName() + "@" + strconv.Itoa(int(v.GetVersion()))
	case *v1.TrialProfile_Document:
		var ps profile.Problems
		if e, ps = profile.Expand(k.Document, profile.Shipped()); len(ps) > 0 {
			return nil, problemsRefusal(ps)
		}
		_, out.Checksum, _ = profile.Checksum(e)
		method = "unsaved " + out.Checksum[:16]
	default:
		return nil, fault.New(fault.Invalid, "profile: is required: a stored version or a document")
	}
	out.Classes, out.Notes = profile.Describe(e), profile.DescribeNotes(e)
	out.Method = &v1.ProfileMethod{IdentityRemoved: "YES"}
	for _, c := range profile.MethodCodes(e.GetBases()) {
		out.Method.Codes = append(out.Method.Codes, &v1.MethodCode{Value: c.Value, Scheme: "DCM", Meaning: c.Meaning})
		out.Method.MethodValues = append(out.Method.MethodValues, c.Meaning)
	}
	out.Method.MethodValues = append(out.Method.MethodValues, method)
	return out, nil
}
