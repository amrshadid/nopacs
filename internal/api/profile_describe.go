package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/trial"
)

// DescribeProfile says what a version or a document does to each class of
// element, the sentences that hold for all of it, and the record of the
// method its files carry.
func (s profileService) DescribeProfile(ctx context.Context, req *v1.DescribeProfileRequest) (*v1.DescribeProfileResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ProfileServiceDescribeProfileProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inProfiles(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if req.GetEffectiveReceiver() != "" {
		return s.describeEffective(ctx, d, st, req)
	}
	target, ps, err := resolveTrial(ctx, p, req.GetProfile())
	if err != nil {
		return nil, problemsError(c.failIn(d, st, err), ps)
	}
	out, err := describeTarget(target.expanded, target.method)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out.Version, out.Checksum = target.versionMessage(), target.checksum
	if d.Masked() {
		withholdChecksums(out.TextLists...)
		if target.stored != nil && len(target.expanded.GetTextLists()) > 0 {
			// The version's checksum covers its terms.
			out.Checksum = ""
			withholdLists(out.Version, true)
		}
	}
	c.record(d, st, master.OutcomeDone, "", access.Facts{Object: target.object})
	return out, nil
}

// describeEffective answers what decides each selector of a receiver's
// effective profile, its profile stacked over the tenant's base, and the
// layer each decision came from. A receiver that archives as received has no
// profile to describe.
func (s profileService) describeEffective(ctx context.Context, d access.Decision, st tenant.Store, req *v1.DescribeProfileRequest) (*v1.DescribeProfileResponse, error) {
	c := s.c
	p, err := c.pipeline(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetProfile() != nil {
		return nil, c.failIn(d, st, invalid("profile", "is not given with effective_receiver: the receiver's profile is described"))
	}
	if err := itemName("effective_receiver", req.GetEffectiveReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	rcv, err := lookupReceiver(ctx, p, req.GetEffectiveReceiver())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if !rcv.Deidentifies() {
		return nil, c.failIn(d, st, precondition("effective_receiver: the receiver archives what it receives as received; it has no profile"))
	}
	ver, err := loadedProfile(ctx, p, "effective_receiver", rcv.Profile)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	base, err := c.tenantBase(ctx, p)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	var eff *profile.Effective
	var problems profile.Problems
	if base == nil {
		eff, problems = profile.Stack(layerOf(profile.LayerReceiver, rcv.Profile, ver))
	} else {
		eff, problems = base.stack(profile.LayerReceiver, rcv.Profile, ver)
	}
	if len(problems) > 0 {
		// Checked when the receiver or the base was set, so only a change
		// that slipped past both would be here.
		return nil, problemsError(c.failIn(d, st, precondition("effective_receiver: the receiver's profile "+rcv.Profile.String()+
			" does not stack over the tenant's base profile: "+problems[0].String()+problemsMore(len(problems)))), problems)
	}
	out := &v1.DescribeProfileResponse{Version: &v1.ProfileVersion{Name: rcv.Profile.Name, Version: rcv.Profile.Version}}
	for _, r := range eff.Describe() {
		out.Effective = append(out.Effective, effectiveElement(r))
	}
	c.record(d, st, master.OutcomeDone, "", access.Facts{Object: profileObject(req.GetEffectiveReceiver())})
	return out, nil
}

// describeTarget describes an expanded profile whose files record method
// in De-identification Method (0012,0063): the classes, the notes and the
// method record, without the version and checksum, which the caller adds.
// The command line describes a document it holds with it, without a
// server. A profile this build cannot run is a precondition failure.
func describeTarget(e *v1.ExpandedProfile, method string) (*v1.DescribeProfileResponse, error) {
	keys, err := trial.ThrowawayKeys()
	if err != nil {
		return nil, err
	}
	// The keys decide no part of the record; the engine needs some to be
	// made, and these are thrown away with it.
	en, err := engine.New(e, keys, engine.Options{Lookups: trial.Lookups{Unassigned: new(int)}, Method: method, OneOff: true})
	if err != nil {
		return nil, precondition("profile: this server cannot run the profile: " + trial.ReceptionRefusal(err))
	}
	m, err := en.Method()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "api: the method record of a profile")
	}
	out := &v1.DescribeProfileResponse{
		Classes: profile.Describe(e),
		Notes:   profile.DescribeNotes(e),
		Method:  &v1.ProfileMethod{MethodValues: m.Values, IdentityRemoved: "YES", TemporalModified: m.Temporal},
		// Each list's name, version, count and checksum; never a term.
		TextLists: profile.DescribeTextLists(e),
	}
	for _, code := range m.Codes {
		scheme := code.Scheme
		if scheme == "" {
			scheme = "DCM"
		}
		out.Method.Codes = append(out.Method.Codes, &v1.MethodCode{Value: code.Value, Scheme: scheme, Meaning: code.Meaning})
	}
	if len(m.Codes) == 0 && len(profile.MethodCodes(e.GetBases())) > 0 {
		weakened, err := profile.KeepsBaseChange(e)
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "api: the warnings of a stored profile")
		}
		if weakened {
			out.Method.Withheld = "a rule of the profile's own keeps what a PS3.15 Annex E base changes or removes, " +
				"so its files are not what the codes would claim"
		}
	}
	return out, nil
}
