package api

import (
	"context"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errNoProfiles is a tenant database that keeps no profiles, which every
// driver of this version does.
var errNoProfiles = fault.New(fault.Internal, "the tenant's database keeps no profiles")

// loadedProfile returns a stored profile version loaded; one the tenant does
// not have is a precondition failure starting with field.
func loadedProfile(ctx context.Context, p tenant.Pipeline, field string, ref tenant.ProfileRef) (*profile.Version, error) {
	profiles, ok := p.(tenant.Profiles)
	if !ok {
		return nil, errNoProfiles
	}
	v, err := profiles.ProfileVersion(ctx, ref.Name, ref.Version)
	if fault.KindOf(err) == fault.NotFound {
		return nil, fault.Wrap(fault.Precondition, err, field+": the tenant has no profile version "+ref.String())
	}
	if err != nil {
		return nil, err
	}
	loaded, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, field+": the stored profile version "+ref.String()+" does not load")
	}
	return loaded, nil
}

// checkRuleSetProfiles checks each profile version a rule set the receiver
// rcv is to run names, as a receiver's own is checked (checkProfileVersion):
// one the tenant has, which can de-identify a receiver's files. A forward
// rule's destination profile is also stacked over each profile a study of
// the receiver may be archived under (the receiver's, and each archive
// rule's), as a destination layer over the layer below it
// (profile.Stack): it may only tighten what that profile did, so a copy
// never carries more of a file than the archive holds. A destination that
// would loosen one, or whose rule cannot be compared with the one below,
// is a precondition failure carrying every problem (problemsError).
func (c *core) checkRuleSetProfiles(ctx context.Context, p tenant.Pipeline, rcv tenant.Receiver, rs tenant.RuleSet) error {
	type lower struct {
		kind profile.LayerKind
		ref  tenant.ProfileRef
	}
	var lowers []lower
	loaded := map[tenant.ProfileRef]*profile.Version{}
	load := func(field string, ref tenant.ProfileRef) (*profile.Version, error) {
		if v, ok := loaded[ref]; ok {
			return v, nil
		}
		v, err := loadedProfile(ctx, p, field, ref)
		if err == nil {
			loaded[ref] = v
		}
		return v, err
	}
	if rcv.Deidentifies() {
		lowers = append(lowers, lower{profile.LayerReceiver, rcv.Profile})
	}
	for _, a := range rs.Archive {
		if a.Profile.IsZero() {
			continue
		}
		field := "archive rule " + a.Name
		if err := c.checkProfileVersion(ctx, p, field, a.Profile); err != nil {
			return err
		}
		lowers = append(lowers, lower{profile.LayerRoute, a.Profile})
		if !rcv.Deidentifies() {
			continue
		}
		// An archive rule's profile is a route layer over the receiver's: it
		// may only tighten what the receiver removes, or a study would be
		// archived with more of it than the receiver's profile allows.
		below, err := load(field, rcv.Profile)
		if err != nil {
			return err
		}
		upper, err := load(field, a.Profile)
		if err != nil {
			return err
		}
		if _, problems := profile.Stack(profile.Layer{Kind: profile.LayerReceiver, Name: rcv.Profile.String(), Expanded: below.Expanded()},
			profile.Layer{Kind: profile.LayerRoute, Name: a.Profile.String(), Expanded: upper.Expanded()}); len(problems) > 0 {
			return &layerRefusal{err: fault.New(fault.Precondition, field+": its profile "+a.Profile.String()+
				" would loosen what the receiver's profile "+rcv.Profile.String()+" archives: "+problems[0].String()+problemsMore(len(problems))), problems: problems}
		}
	}
	for _, f := range rs.Forward {
		if f.Profile.IsZero() {
			continue
		}
		field := "forward rule " + f.Name
		for _, name := range f.Senders {
			// A stow peer's answer names each instance by the UIDs it was
			// sent with, which a copy changes, so only a DIMSE sender sends
			// copies; a sender the tenant does not have is the store's to
			// refuse.
			if s, err := p.Sender(ctx, name); err == nil && s.Type == tenant.SenderSTOW {
				return fault.New(fault.Precondition, field+": a destination profile is applied by DIMSE senders only, and "+name+" is a stow sender")
			}
		}
		if err := c.checkProfileVersion(ctx, p, field, f.Profile); err != nil {
			return err
		}
		upper, err := load(field, f.Profile)
		if err != nil {
			return err
		}
		for _, l := range lowers {
			below, err := load(field, l.ref)
			if err != nil {
				return err
			}
			if _, problems := profile.Stack(profile.Layer{Kind: l.kind, Name: l.ref.String(), Expanded: below.Expanded()},
				profile.Layer{Kind: profile.LayerDestination, Name: f.Profile.String(), Expanded: upper.Expanded()}); len(problems) > 0 {
				return &layerRefusal{err: fault.New(fault.Precondition, field+": its destination profile "+f.Profile.String()+
					" would loosen what "+l.ref.String()+" archives: "+problems[0].String()+problemsMore(len(problems))), problems: problems}
			}
		}
	}
	// Each archive rule's and each destination's profile is also a layer over
	// the tenant's base, whatever the receiver does.
	base, err := c.tenantBase(ctx, p)
	if err != nil {
		return err
	}
	return checkRuleSetBase(base, rcv, rs, load)
}

// layerRefusal is a destination profile that loosens a layer below it: the
// precondition failure, and every problem, which the API answers as a
// ProfileProblems detail (problemsError).
type layerRefusal struct {
	err      error
	problems profile.Problems
}

// Error returns the failure's message.
func (r *layerRefusal) Error() string { return r.err.Error() }

// Unwrap returns the failure, so its kind is the fault's.
func (r *layerRefusal) Unwrap() error { return r.err }
