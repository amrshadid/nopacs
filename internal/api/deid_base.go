package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageBaseMasked refuses a change of the tenant's base profile by a masked
// key.
const messageBaseMasked = "the tenant's base profile (deid.base-profile) is set by a person, never by a masked key"

// baseLayer is a tenant's base profile, loaded: the lowest layer of its
// de-identification, which every receiver's profile, archive rule's profile
// and destination's profile must be at least as strict as.
type baseLayer struct {
	ref tenant.ProfileRef
	v   *profile.Version
}

// tenantBase returns the tenant's base profile, loaded, or nil when it has
// none. A base the tenant no longer has (a version is never removed, so none
// is missing) is the server's fault.
func (c *core) tenantBase(ctx context.Context, p tenant.Pipeline) (*baseLayer, error) {
	name, version, ok, err := c.values().OnTenant(p).BaseProfile(ctx)
	if err != nil || !ok {
		return nil, err
	}
	ref := tenant.ProfileRef{Name: name, Version: version}
	v, err := loadedProfile(ctx, p, settings.DeidBaseProfile, ref)
	if err != nil {
		if fault.KindOf(err) == fault.Precondition {
			return nil, fault.Wrap(fault.Internal, err, "api: the tenant's base profile "+ref.String()+" is gone")
		}
		return nil, err
	}
	return &baseLayer{ref: ref, v: v}, nil
}

// layerOf returns v as a layer of the kind: without its pixel rules, which
// the stack of layers leaves to the first layer. A profile runs alone for a
// reception, so its pixel rules decide its images whatever the base says,
// and the base's elements are what the layers judge.
func layerOf(kind profile.LayerKind, ref tenant.ProfileRef, v *profile.Version) profile.Layer {
	e := v.Expanded()
	e.PixelRules = nil
	return profile.Layer{Kind: kind, Name: ref.String(), Expanded: e}
}

// stack stacks the profile upper, of the kind of layer given, over the base:
// the effective profile, or every problem that makes it looser than the base
// or not comparable with it.
func (b *baseLayer) stack(kind profile.LayerKind, ref tenant.ProfileRef, upper *profile.Version) (*profile.Effective, profile.Problems) {
	return profile.Stack(layerOf(profile.LayerTenant, b.ref, b.v), layerOf(kind, ref, upper))
}

// check refuses, as a precondition failure carrying every problem
// (layerRefusal), a profile that would loosen the base. field names what
// points at the profile, in the message. A profile that is the base is not
// judged against itself.
func (b *baseLayer) check(field string, kind profile.LayerKind, ref tenant.ProfileRef, upper *profile.Version) error {
	if b == nil || ref == b.ref {
		return nil
	}
	if _, problems := b.stack(kind, ref, upper); len(problems) > 0 {
		return &layerRefusal{err: fault.New(fault.Precondition, field+": its profile "+ref.String()+" would loosen the tenant's base profile "+b.ref.String()+
			": "+problems[0].String()+problemsMore(len(problems))), problems: problems}
	}
	return nil
}

// checkReceiverBase checks the profile of a receiver that de-identifies
// against the tenant's base. A receiver that archives as received has none.
func (c *core) checkReceiverBase(ctx context.Context, p tenant.Pipeline, field string, ref tenant.ProfileRef) error {
	base, err := c.tenantBase(ctx, p)
	if err != nil || base == nil {
		return err
	}
	v, err := loadedProfile(ctx, p, field, ref)
	if err != nil {
		return err
	}
	return base.check(field, profile.LayerReceiver, ref, v)
}

// checkBaseSetting checks a base profile about to be set (value is its
// "name@version", "" to unset, which no receiver can object to) against the
// profiles the tenant's receivers de-identify with now and those of the rule
// sets they run: each must be at least as strict as the new base. The base
// itself must be a version the tenant has. The caller holds core.places for
// writing, so no receiver or rule set is added or changed meanwhile.
func (c *core) checkBaseSetting(ctx context.Context, p tenant.Pipeline, value string) error {
	name, version, err := settings.ParseBaseProfile(value)
	if err != nil {
		return invalid("setting.value", err.Error())
	}
	if name == "" {
		return nil
	}
	ref := tenant.ProfileRef{Name: name, Version: version}
	v, err := loadedProfile(ctx, p, "setting.value", ref)
	if err != nil {
		return err
	}
	base := &baseLayer{ref: ref, v: v}
	receivers, err := allReceivers(ctx, p)
	if err != nil {
		return err
	}
	loaded := map[tenant.ProfileRef]*profile.Version{}
	load := func(field string, r tenant.ProfileRef) (*profile.Version, error) {
		if v, ok := loaded[r]; ok {
			return v, nil
		}
		v, err := loadedProfile(ctx, p, field, r)
		if err == nil {
			loaded[r] = v
		}
		return v, err
	}
	for _, rcv := range receivers {
		if rcv.Deidentifies() {
			field := "the receiver " + rcv.Name
			upper, err := load(field, rcv.Profile)
			if err != nil {
				return err
			}
			if err := base.check(field, profile.LayerReceiver, rcv.Profile, upper); err != nil {
				return err
			}
		}
		if rcv.Routing.Version == 0 {
			continue
		}
		rs, err := p.RuleSet(ctx, rcv.Name, rcv.Routing.Version)
		if err != nil {
			return err
		}
		if err := checkRuleSetBase(base, rcv, rs, load); err != nil {
			return err
		}
	}
	return nil
}

// checkRuleSetBase checks each archive rule's profile and each forward
// rule's destination profile of a rule set against the base.
func checkRuleSetBase(base *baseLayer, rcv tenant.Receiver, rs tenant.RuleSet, load func(string, tenant.ProfileRef) (*profile.Version, error)) error {
	if base == nil {
		return nil
	}
	for _, a := range rs.Archive {
		if a.Profile.IsZero() {
			continue
		}
		field := "the receiver " + rcv.Name + "'s archive rule " + a.Name
		upper, err := load(field, a.Profile)
		if err != nil {
			return err
		}
		if err := base.check(field, profile.LayerRoute, a.Profile, upper); err != nil {
			return err
		}
	}
	for _, f := range rs.Forward {
		if f.Profile.IsZero() {
			continue
		}
		field := "the receiver " + rcv.Name + "'s forward rule " + f.Name
		upper, err := load(field, f.Profile)
		if err != nil {
			return err
		}
		if err := base.check(field, profile.LayerDestination, f.Profile, upper); err != nil {
			return err
		}
	}
	return nil
}

// effectiveElement returns one element of an effective description as the
// contract writes it.
func effectiveElement(r profile.EffectiveRule) *v1.EffectiveElement {
	return &v1.EffectiveElement{
		What: r.What, Class: r.Class, Action: r.Action, Conditional: r.Conditional,
		Layer: r.Layer.String(), LayerProfile: r.LayerName, Rule: int32(r.Rule), RuleName: r.RuleName, Origin: r.Origin, //nolint:gosec // G115: a position within 10,000 rules.
	}
}

// trialOverBase returns t as a receiver of the tenant would run it: stacked
// over the tenant's base profile when it has one (profile.Reception), so a
// trial or an explanation as a receiver shows what a reception does. A
// profile that loosens the base is refused with its problems, as it would be
// set on a receiver; a masked key is refused a base that holds text lists,
// whose terms a trial would answer by membership.
func (c *core) trialOverBase(ctx context.Context, p tenant.Pipeline, t trialTarget, masked bool) (trialTarget, error) {
	base, err := c.tenantBase(ctx, p)
	if err != nil || base == nil {
		return t, err
	}
	be := base.v.Expanded()
	if masked && len(be.GetTextLists()) > 0 {
		return t, maskedListsError()
	}
	out, problems := profile.Reception(base.ref.String(), be, t.method, t.expanded)
	if len(problems) > 0 {
		return t, problemsErr{fault.New(fault.Precondition, "the profile would loosen the tenant's base profile "+base.ref.String()+": "+
			problems[0].String()+problemsMore(len(problems))), problems}
	}
	t.expanded = out
	return t, nil
}
