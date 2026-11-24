package api

import (
	"context"
	"slices"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// trialRouting applies a receiver's routes to the files a trial tries, so a
// trial agrees with what the receiver would do: a file its routing would
// not route is held as unrouted, and a file whose route names a profile
// version other than the one tried is tried with that version, under keys
// of its own made for the request, as every trial's are. A held file of a
// study the catalog already has a session for goes by the session's
// recorded choice, as its reception would (a study is routed once, for its
// session), never by a fresh routing.
type trialRouting struct {
	p          tenant.Profiles
	catalog    trialCatalog
	target     trialTarget
	en         *engine.Engine
	keys       engine.Keys
	unassigned *int
	limit      int64
	// routed are the engines of the route's versions, made once each.
	routed map[tenant.ProfileRef]routedEngine
}

// trialCatalog is where a trial finds the session a held file's study
// already has: tenant.Pipeline is one.
type trialCatalog interface {
	CatalogLabels(ctx context.Context, studyUID string, seriesUIDs []string) (tenant.CatalogLabels, error)
}

// routedEngine is the engine of a version a route names, with its keys.
type routedEngine struct {
	en   *engine.Engine
	keys engine.Keys
}

// trialFile is how one file is tried after its receiver's routing.
type trialFile struct {
	en        *engine.Engine
	keys      engine.Keys
	routeName string
	profile   *tenant.ProfileRef
	unrouted  map[string]string
}

// of returns how in is tried: with the profile tried, or the version its
// route names; and what its routing decided. A held file whose study has a
// session takes the session's choice (its route and profile; never
// unrouted); any other file tried at a receiver is routed by the
// receiver's routes. A route's profile replaces the one tried only while
// the receiver de-identifies (tenant.ReceptionRoute.ProfileFor): a file
// landed at a receiver that archives as received is never de-identified by
// a route.
func (tr *trialRouting) of(ctx context.Context, in trialInput) (trialFile, error) {
	out := trialFile{en: tr.en, keys: tr.keys}
	if in.receiver == nil {
		return out, nil
	}
	var ref tenant.ProfileRef
	session, err := tr.session(ctx, in)
	switch {
	case err != nil:
		return out, err
	case session != nil:
		out.routeName, ref = session.Route.Route, session.Route.Profile
	case in.receiver.Routing.IsZero():
		return out, nil
	default:
		route, ok := receive.RouteTrial(*in.receiver, in.file, tr.limit,
			tenant.Caller{CallingAETitle: in.received.CallingAETitle, CalledAETitle: in.received.CalledAETitle})
		if !ok {
			return out, nil
		}
		out.unrouted = route.Unrouted
		switch {
		case route.Route != nil:
			out.routeName, ref = route.Route.Name, route.Route.Profile
		case route.Unrouted != nil:
			out.routeName = route.Unrouted["route"]
		}
	}
	if ref.IsZero() || !in.receiver.Deidentifies() {
		return out, nil
	}
	if s := tr.target.stored; s != nil && s.Ref() == ref {
		return out, nil
	}
	re, ok := tr.routed[ref]
	if !ok {
		target, _, err := resolveTrial(ctx, tr.p, &v1.TrialProfile{Kind: &v1.TrialProfile_Version{
			Version: &v1.ProfileVersionName{Name: ref.Name, Version: ref.Version}}})
		if err != nil {
			return out, err
		}
		if re.en, re.keys, err = trialEngine(target, tr.unassigned); err != nil {
			return out, err
		}
		tr.routed[ref] = re
	}
	out.en, out.keys, out.profile = re.en, re.keys, &ref
	return out, nil
}

// session returns the session the catalog has for a held file's study, as
// its record names the study (remapped when it was made de-identified, as
// the catalog then holds it), or nil: for a file sent with the request, for
// a record with no study, and for a study with no session yet.
func (tr *trialRouting) session(ctx context.Context, in trialInput) (*tenant.SessionLabels, error) {
	if in.study == "" || tr.catalog == nil {
		return nil, nil
	}
	labels, err := tr.catalog.CatalogLabels(ctx, in.study, nil)
	if err != nil {
		return nil, err
	}
	return labels.Session, nil
}

// hold makes t held as unrouted when the routing would not route its file,
// unless a check that runs before the routing at reception (the receiving
// checks, the labels) holds it already: those decide first, and
// de-identification's holds come after it.
func (f trialFile) hold(t *receive.Trial) {
	if f.unrouted == nil {
		return
	}
	if h := t.Hold; h != nil && slices.Contains(beforeRouting, h.Reason) {
		return
	}
	t.Hold = &receive.TrialHold{Reason: tenant.QuarantineUnrouted, Details: f.unrouted}
}

// beforeRouting are the reasons a file is held for before its study is
// routed at reception: when it lands, and when its labels are made.
var beforeRouting = []tenant.QuarantineReason{tenant.QuarantineNotDICOM, tenant.QuarantineHeaderTooLarge,
	tenant.QuarantineBadUID, tenant.QuarantineSOPClassNotAccepted, tenant.QuarantineBadLabel}

// mark writes on r the route chosen and the version the file was tried
// with when it is not the one asked for.
func (f trialFile) mark(r *v1.TrialResult) {
	r.Route = f.routeName
	if f.profile != nil {
		r.RoutedProfile = &v1.ProfileVersionName{Name: f.profile.Name, Version: f.profile.Version}
	}
}
