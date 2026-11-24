package receive

import (
	"bytes"
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// routeTags are the elements a route's header subjects are (PS3.6), read
// from the data set's top level as the naming rule reads them.
var routeTags = map[tenant.RouteSubject]naming.Tag{
	tenant.SubjectModality:         naming.NewTag(0x0008, 0x0060),
	tenant.SubjectSOPClassUID:      naming.NewTag(0x0008, 0x0016),
	tenant.SubjectManufacturer:     naming.NewTag(0x0008, 0x0070),
	tenant.SubjectModelName:        naming.NewTag(0x0008, 0x1090),
	tenant.SubjectSoftwareVersions: naming.NewTag(0x0018, 0x1020),
}

// routeFacts are what a route reads of one received file: its header, as
// received, and the AE titles of the association it came in.
type routeFacts struct {
	h      *header.Header
	caller tenant.Caller
}

// Value implements tenant.RouteFacts: a header value decoded under the
// file's character set with its padding removed (header.Header.Lookup), an
// AE title as recorded, without padding (PS3.5 6.2).
func (f routeFacts) Value(s tenant.RouteSubject) (string, bool) {
	switch s {
	case tenant.SubjectCallingAETitle:
		return f.caller.CallingAETitle, f.caller.CallingAETitle != ""
	case tenant.SubjectCalledAETitle:
		return f.caller.CalledAETitle, f.caller.CalledAETitle != ""
	}
	t, ok := routeTags[s]
	if !ok {
		return "", false
	}
	return f.h.Lookup(t)
}

// routed is what a reception's routing decided in its identify step.
type routed struct {
	// choice is what is kept with the reception (tenant.ReceptionRoute).
	choice tenant.ReceptionRoute
	// storage is the storage a new session goes to.
	storage string
	// unrouted, when not nil, holds the study: the details of its records.
	unrouted map[string]string
}

// route evaluates the receiver's routing for the first reception of a new
// session, in its identify step. The routes run in order over the file the
// naming rule reads first (the first file, by SOP Instance UID, of the
// first series, by UID, whose bytes are here), the same file every run, so
// a run again chooses the same. The first route whose conditions all hold
// chooses its storage, profile and senders; none, and the routing's default
// decides: the receiver's storage, profile and senders, or unrouted. A
// route that cannot be evaluated holds the study as unrouted, whatever
// comes after it.
//
// A storage a release into the reception named chooses only the storage
// (a person decided where it goes): the profile and the senders are still
// the route's, or the receiver's when none holds, and the default never
// holds it; a route that cannot be evaluated still does, since which
// profile applies is not known.
func (p *Pipeline) route(ctx context.Context, r tenant.Reception, rcv tenant.Receiver, groups []series) (routed, error) {
	released := r.Route.ReleasedStorage
	out := routed{storage: rcv.Storage}
	if !rcv.Routing.IsZero() {
		facts, err := p.routeFile(ctx, groups)
		if err != nil {
			return routed{}, err
		}
		rt, err := rcv.Routing.Choose(facts)
		switch {
		case errors.Is(err, tenant.ErrRouteUnusable):
			return routed{unrouted: map[string]string{"cause": "route_unusable", "route": rt.Name}}, nil
		case err != nil:
			return routed{}, err
		case rt != nil:
			out = routed{choice: tenant.ChosenBy(*rt), storage: rt.Storage}
		case rcv.Routing.QuarantineUnmatched && released == "":
			return routed{unrouted: map[string]string{"cause": "no_match"}}, nil
		}
	}
	if released != "" {
		out.storage = released
	}
	return out, nil
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

// routeFile returns the facts of the first file, in the order the naming
// rule reads them, whose header can be read.
func (p *Pipeline) routeFile(ctx context.Context, groups []series) (routeFacts, error) {
	var lastErr error
	for _, g := range groups {
		for _, f := range g.files {
			h, err := p.readHeader(ctx, f.Incoming)
			if err != nil {
				lastErr = err
				continue
			}
			return routeFacts{h: h, caller: f.Caller}, nil
		}
	}
	if lastErr == nil {
		lastErr = fault.New(fault.Internal, "a reception has no file")
	}
	return routeFacts{}, fault.Wrap(fault.KindOf(lastErr), lastErr, "no file of the reception could be read to route it")
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
// with the profile version its route chose in place of the receiver's
// (tenant.ReceptionRoute.ProfileFor); the zero receiver, which names no
// profile, when it was removed.
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

// TrialRoute is what a receiver's routing would decide for one file a
// trial tries, as the first file of a new session: the route chosen, or
// nil for none; or, when the routing would not route it, the details of
// the unrouted hold.
type TrialRoute struct {
	// Route is the route chosen; nil when none was, or when the file is
	// unrouted.
	Route *tenant.Route
	// Unrouted, when not nil, holds the file: the details of its record.
	Unrouted map[string]string
}

// RouteTrial evaluates rcv's routing over file, received in an association
// with caller's AE titles, as route does for the first reception of a new
// session. ok is false when the file's header cannot be read within
// headerLimit: the trial holds such a file for that, before any route.
func RouteTrial(rcv tenant.Receiver, file []byte, headerLimit int64, caller tenant.Caller) (out TrialRoute, ok bool) {
	if rcv.Routing.IsZero() {
		return TrialRoute{}, true
	}
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), headerLimit)
	if err != nil {
		return TrialRoute{}, false
	}
	rt, err := rcv.Routing.Choose(routeFacts{h: h, caller: caller})
	switch {
	case err != nil:
		name := ""
		if rt != nil {
			name = rt.Name
		}
		return TrialRoute{Unrouted: map[string]string{"cause": "route_unusable", "route": name}}, true
	case rt != nil:
		return TrialRoute{Route: rt}, true
	case rcv.Routing.QuarantineUnmatched:
		return TrialRoute{Unrouted: map[string]string{"cause": "no_match"}}, true
	}
	return TrialRoute{}, true
}
