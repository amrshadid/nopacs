package api

import (
	"context"
	"errors"
	"strconv"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoProfile answers a profile name, or a version, the tenant does
// not have.
const messageNoProfile = "the tenant has no such profile version"

// profileService is ProfileService: a tenant's de-identification profiles,
// each a list of versions that never change, kept in its store through the
// port; and the trial runs, explanations and comparisons that read them.
type profileService struct{ c *core }

var _ nopacsv1connect.ProfileServiceHandler = profileService{}

// profilesOf returns st as the port of a tenant's profiles. Every driver
// implements it, so a store that does not is the server's fault.
func profilesOf(st tenant.Store) (tenant.Deidentification, error) {
	p, ok := st.(tenant.Deidentification)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no profiles")
	}
	return p, nil
}

// inProfiles is inTenant for the profile methods: the tenant's store and
// its profiles' port, or the error already recorded.
func (c *core) inProfiles(ctx context.Context, d access.Decision, name string, details map[string]string) (tenant.Deidentification, tenant.Store, error) {
	_, st, err := c.inTenant(ctx, d, name, details)
	if err != nil {
		return nil, nil, err
	}
	p, err := profilesOf(st)
	if err != nil {
		return nil, nil, c.failIn(d, st, err)
	}
	return p, st, nil
}

// profileName returns an Invalid error naming field unless name is a
// profile's name.
func profileName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	if !profile.ValidName(name) {
		return invalid(field, "is 1 to 63 of a-z, 0-9 and -, starting with a letter")
	}
	return nil
}

// profileView returns an Invalid error unless v is a value of ProfileView,
// and whether it asks for the expanded form.
func profileView(field string, v v1.ProfileView) (full bool, err error) {
	if _, known := v1.ProfileView_name[int32(v)]; !known {
		return false, invalid(field, "is not a value of ProfileView")
	}
	return v == v1.ProfileView_PROFILE_VIEW_FULL, nil
}

// problemsError returns the error a document with problems is refused
// with: Invalid, naming the first problem's rule and field, and carrying
// every problem as one ProfileProblems detail. err is the API error the
// refusal was recorded as; with no problem it is returned as it is.
func problemsError(err error, ps profile.Problems) error {
	var ce *connect.Error
	if len(ps) == 0 || !errors.As(err, &ce) {
		return err
	}
	detail, derr := connectproto.NewErrorDetail(&v1.ProfileProblems{Problems: problemsProto(ps)})
	if derr != nil {
		return err
	}
	return ce.WithDetail(detail)
}

// problemsProto returns problems as the API answers them.
func problemsProto(ps profile.Problems) []*v1.ProfileProblem {
	out := make([]*v1.ProfileProblem, len(ps))
	for i, p := range ps {
		out[i] = p.Proto()
	}
	return out
}

// documentRefusal returns the Invalid error of a document with problems,
// field naming the document in the request.
func documentRefusal(field string, ps profile.Problems) error {
	return invalid(field, ps[0].String()+problemsMore(len(ps)))
}

// problemsMore says how many problems a message leaves unnamed.
func problemsMore(n int) string {
	if n <= 1 {
		return ""
	}
	return " (and " + strconv.Itoa(n-1) + " more, each in the error's ProfileProblems detail)"
}

// versionMessage returns a stored version as the API answers it: with its
// document when it holds its texts, and its expanded form with full. A
// version whose texts do not load (changed in the database after it was
// made) is the server's fault, never answered as it is.
func versionMessage(v tenant.ProfileVersion, full bool) (*v1.ProfileVersion, error) {
	out := &v1.ProfileVersion{
		Name: v.Name, Version: v.Version, Checksum: v.Checksum, CreatedByKeyId: v.CreatedByKeyID, Note: v.Note,
		CreateTime: timestamppb.New(v.Created),
	}
	if v.Source == nil {
		return out, nil
	}
	loaded, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "api: a stored profile version does not load")
	}
	out.Document = loaded.Document()
	if full {
		out.Expanded = loaded.Expanded()
	}
	return out, nil
}

// readVersion reads a stored version, answering one the tenant does not
// have as NotFound with the contract's message.
func readVersion(ctx context.Context, p tenant.Profiles, name string, version int32) (tenant.ProfileVersion, error) {
	v, err := p.ProfileVersion(ctx, name, version)
	if fault.KindOf(err) == fault.NotFound {
		return v, fault.Wrap(fault.NotFound, err, messageNoProfile)
	}
	return v, err
}

// CreateProfileVersion checks and expands a document and stores it as the
// profile's next version, or answers the latest when the document equals
// it.
func (s profileService) CreateProfileVersion(ctx context.Context, req *v1.CreateProfileVersionRequest) (*v1.CreateProfileVersionResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ProfileServiceCreateProfileVersionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := profileName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	full, err := profileView("view", req.GetView())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetDocument() == nil {
		return nil, c.failIn(d, st, invalid("document", "is required"))
	}
	if req.ExpectedLatestVersion != nil && req.GetExpectedLatestVersion() < 0 {
		return nil, c.failIn(d, st, invalid("expected_latest_version", "is 0 or more"))
	}
	// The version's number here only lets the language make its texts;
	// the store decides the number it is stored under.
	v, err := profile.New(req.GetName(), 1, req.GetDocument(), profile.Shipped())
	var ps profile.Problems
	if errors.As(err, &ps) && len(ps) > 0 {
		return nil, problemsError(c.failIn(d, st, documentRefusal("document", ps)), ps)
	}
	if err != nil {
		return nil, c.failIn(d, st, fault.Wrap(fault.Invalid, err, "document: "+err.Error()))
	}
	warnings, err := profile.Warnings(v.Expanded())
	if err != nil {
		return nil, c.failIn(d, st, fault.Wrap(fault.Internal, err, "api: the warnings of a valid document"))
	}
	in := tenant.NewProfileVersion{Name: req.GetName(), Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum(), Note: req.GetNote()}
	if req.ExpectedLatestVersion != nil {
		want := req.GetExpectedLatestVersion()
		in.ExpectLatest = &want
	}
	stored, created, err := p.CreateProfileVersion(ctx, in, c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("document", err))
	}
	msg, err := versionMessage(stored, full)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.CreateProfileVersionResponse{
		Version: msg, Created: created, Warnings: problemsProto(warnings),
		NoteDiffers: !created && stored.Note != req.GetNote(),
	}, nil
}

// GetProfileVersion answers one stored version: its document, which is the
// profile's export, and its expanded form with the full view.
func (s profileService) GetProfileVersion(ctx context.Context, req *v1.GetProfileVersionRequest) (*v1.GetProfileVersionResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceGetProfileVersionProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := profileName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetVersion() < 0 {
		return nil, c.failIn(d, st, invalid("version", "is 0 (the latest) or more"))
	}
	full, err := profileView("view", req.GetView())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	v, err := readVersion(ctx, p, req.GetName(), req.GetVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	msg, err := versionMessage(v, full)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetProfileVersionResponse{Version: msg}, nil
}

// ListProfiles answers a page of the tenant's profiles by name, each with
// its latest version.
func (s profileService) ListProfiles(ctx context.Context, req *v1.ListProfilesRequest) (*v1.ListProfilesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceListProfilesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListProfiles(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListProfilesResponse{Profiles: make([]*v1.ProfileSummary, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Profiles = append(out.Profiles, &v1.ProfileSummary{Name: it.Name, LatestVersion: it.LatestVersion, LatestCreateTime: timestamppb.New(it.LatestCreated)})
	}
	return out, nil
}

// ListProfileVersions answers a page of one profile's versions, newest
// first, without their texts.
func (s profileService) ListProfileVersions(ctx context.Context, req *v1.ListProfileVersionsRequest) (*v1.ListProfileVersionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceListProfileVersionsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := profileName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListProfileVersions(ctx, req.GetName(), page)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, "the tenant has no profile with this name")
	}
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListProfileVersionsResponse{Versions: make([]*v1.ProfileVersion, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		it.Source, it.Expanded = nil, nil
		msg, _ := versionMessage(it, false)
		out.Versions = append(out.Versions, msg)
	}
	return out, nil
}

// ValidateProfile checks a document as CreateProfileVersion would and
// answers every problem, or its checksum, warnings and (full view) its
// expanded form. A document with problems is a valid call.
func (s profileService) ValidateProfile(ctx context.Context, req *v1.ValidateProfileRequest) (*v1.ValidateProfileResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceValidateProfileProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	full, err := profileView("view", req.GetView())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetDocument() == nil {
		return nil, c.failIn(d, st, invalid("document", "is required"))
	}
	out := &v1.ValidateProfileResponse{}
	e, ps := profile.Expand(req.GetDocument(), profile.Shipped())
	if len(ps) == 0 {
		_, sum, err := profile.Checksum(e)
		if err != nil {
			return nil, c.failIn(d, st, fault.Wrap(fault.Internal, err, "api: the checksum of a valid document"))
		}
		warnings, err := profile.Warnings(e)
		if err != nil {
			return nil, c.failIn(d, st, fault.Wrap(fault.Internal, err, "api: the warnings of a valid document"))
		}
		out.Checksum, out.Warnings = sum, problemsProto(warnings)
		if full {
			out.Expanded = e
		}
	}
	out.Problems = problemsProto(ps)
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: map[string]string{detailProblems: strconv.Itoa(len(ps))}})
	return out, nil
}

// detailProblems is the key of the number of problems a validated
// document has, in the details of ValidateProfile's entry.
const detailProblems = "problems"

// ListProfileBases answers every base the server ships.
func (s profileService) ListProfileBases(ctx context.Context, _ *v1.ListProfileBasesRequest) (*v1.ListProfileBasesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceListProfileBasesProcedure)
	if err != nil {
		return nil, err
	}
	c.done(d, nil)
	return &v1.ListProfileBasesResponse{Bases: profile.Shipped().Proto()}, nil
}
