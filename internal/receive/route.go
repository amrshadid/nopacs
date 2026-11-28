package receive

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// routed is what a reception's routing decided in its identify step.
type routed struct {
	// choice is what is kept with the reception (tenant.ReceptionRoute).
	choice tenant.ReceptionRoute
	// storage is the storage a new session goes to.
	storage string
	// unrouted, when not nil, holds the study: the details of its records.
	unrouted map[string]string
}

// RouteSubject returns what a routing condition reads of one file, the
// file's bytes in r, size long, received from caller: its data set as
// received, its values read within headerLimit, and the AE titles. A file
// whose header cannot be read is an error; the caller holds such a file
// for that before any rule.
func RouteSubject(r io.ReaderAt, size, headerLimit int64, caller tenant.Caller, now time.Time) (query.Subject, error) {
	h, err := header.ReadWithin(r, size, headerLimit)
	if err != nil {
		return query.Subject{}, err
	}
	ds, _, little, err := h.Dataset(r)
	if err != nil {
		return query.Subject{}, err
	}
	return query.Subject{Dataset: ds, BigEndian: !little, Catalog: tenant.RouteCatalog(caller), Now: now}, nil
}

// routeSubjectOf returns what a routing condition reads of a received file
// in the incoming area.
func (p *Pipeline) routeSubjectOf(ctx context.Context, f tenant.ReceivedFile) (query.Subject, error) {
	file, size, err := p.area.Open(f.Incoming)
	if err != nil {
		return query.Subject{}, err
	}
	defer func() { _ = file.Close() }()
	s, err := RouteSubject(file, size, p.headerLimit(ctx), f.Caller, p.now())
	if err != nil {
		return query.Subject{}, fault.Wrap(fault.Internal, err, "a recorded file's header cannot be read")
	}
	return s, nil
}

// routeFile returns what a condition reads of the first file, in the order
// the naming rule reads them (the first file, by SOP Instance UID, of the
// first series, by UID, whose bytes are here), whose header can be read:
// the same file every run, so a run again chooses the same.
func (p *Pipeline) routeFile(ctx context.Context, groups []series) (query.Subject, tenant.Caller, error) {
	var lastErr error
	for _, g := range groups {
		for _, f := range g.files {
			s, err := p.routeSubjectOf(ctx, f)
			if err != nil {
				lastErr = err
				continue
			}
			return s, f.Caller, nil
		}
	}
	if lastErr == nil {
		lastErr = fault.New(fault.Internal, "a reception has no file")
	}
	return query.Subject{}, tenant.Caller{}, fault.Wrap(fault.KindOf(lastErr), lastErr, "no file of the reception could be read to route it")
}

// firstCaller returns the caller of the reception's first file, in the
// naming rule's order: the AE titles a new session keeps.
func firstCaller(groups []series) tenant.Caller {
	for _, g := range groups {
		for _, f := range g.files {
			return f.Caller
		}
	}
	return tenant.Caller{}
}

// route evaluates the archive phase of the receiver's rule set for the
// first reception of a new session, in its identify step. The archive rules
// run in order over the reception's first file (routeFile); the first that
// holds chooses its storage and profile; none, and the rule set's default
// decides: the receiver's storage and profile, or unrouted. A rule that
// cannot be evaluated holds the study as unrouted, whatever comes after it.
//
// A storage a release into the reception named chooses only the storage (a
// person decided where it goes): the profile is still the rule's, or the
// receiver's when none holds, and the default never holds it; a rule that
// cannot be evaluated still does, since which profile applies is not known.
func (p *Pipeline) route(ctx context.Context, r tenant.Reception, rcv tenant.Receiver, groups []series) (routed, error) {
	released := r.Route.ReleasedStorage
	caller := firstCaller(groups)
	out := routed{storage: rcv.Storage, choice: tenant.ReceptionRoute{CallingAE: caller.CallingAETitle, CalledAE: caller.CalledAETitle}}
	rs := rcv.Routing
	if len(rs.Archive) > 0 || rs.QuarantineUnmatched {
		s, _, err := p.routeFile(ctx, groups)
		if err != nil {
			return routed{}, err
		}
		ev := p.rules(rs).Evaluate(s)
		switch {
		case ev.Unusable != "":
			return routed{unrouted: map[string]string{"cause": "route_unusable", "route": ev.Unusable}}, nil
		case ev.Archive >= 0:
			out.choice = tenant.ChosenBy(rs.Version, rs.Archive[ev.Archive])
			out.choice.CallingAE, out.choice.CalledAE = caller.CallingAETitle, caller.CalledAETitle
			out.storage = rs.Archive[ev.Archive].Storage
		case rs.QuarantineUnmatched && released == "":
			return routed{unrouted: map[string]string{"cause": "no_match"}}, nil
		default:
			out.choice.RuleSet = rs.Version
		}
	}
	if released != "" {
		out.storage = released
	}
	return out, nil
}

// forwards evaluates the forward phase of the receiver's rule set for a
// reception about to be archived: every forward rule whose condition holds
// over the reception's first file, and, for a rule with a series filter,
// the series whose first file (by SOP Instance UID) it holds for, one
// header read per series. A rule that cannot be evaluated, or a series
// filter that cannot, forwards nothing of what it could not judge: a copy
// is sent only where a rule says so.
func (p *Pipeline) forwards(ctx context.Context, rcv tenant.Receiver, groups []series) (int32, []tenant.ForwardChoice, error) {
	rs := rcv.Routing
	if len(rs.Forward) == 0 {
		return 0, nil, nil
	}
	s, _, err := p.routeFile(ctx, groups)
	if err != nil {
		return 0, nil, err
	}
	c := p.rules(rs)
	ev := c.Evaluate(s)
	var out []tenant.ForwardChoice
	firsts := map[string]query.Subject{}
	for _, i := range ev.Forward {
		f := rs.Forward[i]
		choice := tenant.ForwardChoice{Rule: f.Name, Senders: append([]string(nil), f.Senders...), Profile: f.Profile}
		if c.SeriesFiltered(i) {
			choice.Filtered = true
			for _, g := range groups {
				if len(g.files) == 0 {
					continue
				}
				gs, ok := firsts[g.uid]
				if !ok {
					if gs, err = p.routeSubjectOf(ctx, g.files[0]); err != nil {
						// A series whose first file cannot be read is not
						// judged, and not forwarded by a filter.
						continue
					}
					firsts[g.uid] = gs
				}
				if o := c.Series(i, gs); o.Unusable == "" && o.Truth == query.True {
					choice.Series = append(choice.Series, g.uid)
				}
			}
			if len(choice.Series) == 0 || len(choice.Series) > tenant.MaxForwardSeries {
				continue
			}
		}
		out = append(out, choice)
	}
	if len(out) == 0 {
		return 0, nil, nil
	}
	return rs.Version, out, nil
}

// rules returns rs compiled, from the pipeline's cache: a version is never
// changed, so one compilation serves every reception it routes.
func (p *Pipeline) rules(rs tenant.RuleSet) *tenant.CompiledRules {
	return p.ruleCache.get(rs)
}

// errStudyIdentifying is an identify step that must wait: another
// reception of the study, which arrived first, is being identified, and
// what is chosen for it is chosen for the session. It is tried again after
// the unit's retry delay.
var errStudyIdentifying = fault.New(fault.Unavailable, "another reception of this study is being identified; the step is tried again later")

// routedInFlight returns another reception of r's study that is identified
// and has not ended, the first by arrival, whose choice is the session's
// before the session is in the catalog; or errStudyIdentifying when one
// that arrived before r is complete and not yet identified. One still
// receiving is not waited for: it takes r's choice when it comes to be
// identified.
func (p *Pipeline) routedInFlight(ctx context.Context, r tenant.Reception, arrivals []tenant.Arrival) (tenant.Reception, bool, error) {
	for _, a := range arrivals {
		if a.ID == r.ID {
			continue
		}
		switch a.State {
		case reception.Identified, reception.Archived, reception.Verified:
			o, _, err := p.store.Reception(ctx, a.ID)
			if err != nil {
				return tenant.Reception{}, false, err
			}
			if !o.State.Ended() && o.Storage != "" {
				return o, true, nil
			}
		case reception.Complete:
			if a.Arrival < r.Arrival {
				return tenant.Reception{}, false, errStudyIdentifying
			}
		}
	}
	return tenant.Reception{}, false, nil
}

// unroutedBefore returns the details of a file of the reception a run
// before held as unrouted, or nil when none is.
func (p *Pipeline) unroutedBefore(ctx context.Context, receptionID string) (map[string]string, error) {
	qs, _, err := p.store.ListQuarantined(ctx, tenant.QuarantineFilter{ReceptionID: receptionID, Reason: tenant.QuarantineUnrouted},
		master.Page{Limit: 1})
	if err != nil || len(qs) == 0 {
		return nil, err
	}
	return qs[0].Details, nil
}

// holdUnrouted holds every file of the reception as unrouted, with the
// details of the routing's decision, then ends it as quarantined.
func (p *Pipeline) holdUnrouted(ctx context.Context, w tenant.Work, r tenant.Reception, groups []series, details map[string]string) error {
	var records []tenant.QuarantinedFile
	for _, g := range groups {
		for _, f := range g.files {
			records = append(records, p.holdRecord(r, f, tenant.QuarantineUnrouted, details))
		}
	}
	p.at("identify.before-unrouted")
	if err := p.hold(ctx, w, records); err != nil {
		return err
	}
	return p.quarantineAll(ctx, w, r)
}

// receiverFor returns the receiver a reception's files are archived by,
// with the profile version its archive rule chose in place of the
// receiver's (tenant.ReceptionRoute.ProfileFor); the zero receiver, which
// names no profile, when it was removed.
func (p *Pipeline) receiverFor(ctx context.Context, r tenant.Reception) (tenant.Receiver, error) {
	rcv, err := p.receivers.get(ctx, r.Receiver)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return tenant.Receiver{}, nil
	case err != nil:
		return tenant.Receiver{}, err
	}
	rcv.Profile = r.Route.ProfileFor(rcv)
	return rcv, nil
}

// TrialRoute is what a receiver's rule set would decide for one file a
// trial tries, as the first file of a new session: the archive rule chosen,
// or nil for none; or, when the rules would not route it, the details of
// the unrouted hold.
type TrialRoute struct {
	// Route is the archive rule chosen; nil when none was, or when the file
	// is unrouted.
	Route *tenant.ArchiveRule
	// Unrouted, when not nil, holds the file: the details of its record.
	Unrouted map[string]string
}

// RouteTrial evaluates rcv's archive rules over file, received in an
// association with caller's AE titles, as route does for the first
// reception of a new session. ok is false when the file's header cannot be
// read within headerLimit: the trial holds such a file for that, before any
// rule.
func RouteTrial(rcv tenant.Receiver, file []byte, headerLimit int64, caller tenant.Caller) (out TrialRoute, ok bool) {
	rs := rcv.Routing
	if len(rs.Archive) == 0 && !rs.QuarantineUnmatched {
		return TrialRoute{}, true
	}
	s, err := RouteSubject(bytes.NewReader(file), int64(len(file)), headerLimit, caller, time.Now())
	if err != nil {
		return TrialRoute{}, false
	}
	ev := rs.Compile().Evaluate(s)
	switch {
	case ev.Unusable != "":
		return TrialRoute{Unrouted: map[string]string{"cause": "route_unusable", "route": ev.Unusable}}, true
	case ev.Archive >= 0:
		a := rs.Archive[ev.Archive]
		return TrialRoute{Route: &a}, true
	case rs.QuarantineUnmatched:
		return TrialRoute{Unrouted: map[string]string{"cause": "no_match"}}, true
	}
	return TrialRoute{}, true
}
