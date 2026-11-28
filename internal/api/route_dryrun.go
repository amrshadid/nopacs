package api

import (
	"context"
	"slices"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The bounds of a dry run: the sessions asked for by default, and the
// metadata files one run reads at most (one per scan), so what one request
// costs is bounded whatever the sessions hold. A session past the bound is
// answered unreadable, never judged in part.
const (
	DefaultDryRunSessions = 100
	MaxDryRunScans        = 20000
)

// DryRunRoutes answers what a rule set would decide for the receiver's
// latest sessions beside what routed them.
func (s routeService) DryRunRoutes(ctx context.Context, req *v1.DryRunRoutesRequest) (*v1.DryRunRoutesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RouteServiceDryRunRoutesProcedure)
	if err != nil {
		return nil, err
	}
	n := int(req.GetSessions())
	if n == 0 {
		n = DefaultDryRunSessions
	}
	details := map[string]string{detailVersion: strconv.Itoa(int(req.GetVersion())), detailSessions: strconv.Itoa(n)}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: details}
	fail := func(err error) error { return c.failWith(d, st, facts, err) }
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, fail(err)
	}
	if err := ruleVersion("version", req.GetVersion()); err != nil {
		return nil, fail(err)
	}
	if n < 1 || n > tenant.MaxDryRunSessions {
		return nil, fail(invalid("sessions", "is from 1 to "+strconv.Itoa(tenant.MaxDryRunSessions)+"; 0 asks for "+strconv.Itoa(DefaultDryRunSessions)))
	}
	if _, err := lookupReceiver(ctx, p, req.GetReceiver()); err != nil {
		return nil, fail(err)
	}
	rs, err := p.RuleSet(ctx, req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, fail(err)
	}
	sessions, err := p.RecentSessions(ctx, req.GetReceiver(), n)
	if err != nil {
		return nil, fail(err)
	}
	stores := c.newReadStores(p, req.GetTenant())
	defer stores.Close()
	run := dryRun{c: c, p: p, d: d, tenant: req.GetTenant(), rs: rs, rules: rs.Compile(), stores: stores, budget: MaxDryRunScans,
		removed: map[tenant.ProfileRef]func(query.Field) bool{}}
	out := &v1.DryRunRoutesResponse{Version: rs.Version}
	for _, sess := range sessions {
		m, err := run.session(ctx, sess)
		if err != nil {
			return nil, fail(err)
		}
		switch m.GetOutcome() {
		case v1.DryRunOutcome_DRY_RUN_OUTCOME_SAME:
			out.Same++
		case v1.DryRunOutcome_DRY_RUN_OUTCOME_CHANGED:
			out.Changed++
		default:
			out.Unknown++
		}
		out.Sessions = append(out.Sessions, m)
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	return out, nil
}

// dryRun is one dry run's state: the rules tried, the storages it reads,
// the reads it has left, and the profiles' judgments it made.
type dryRun struct {
	c       *core
	d       access.Decision
	p       tenant.Pipeline
	tenant  string
	rs      tenant.RuleSet
	rules   *tenant.CompiledRules
	stores  *checkedread.Stores
	budget  int
	removed map[tenant.ProfileRef]func(query.Field) bool
}

// readable reports whether err is a failure of one session's files (its
// metadata file missing, damaged or not online), which makes that session
// unreadable rather than the whole run a failure.
func readable(err error) bool {
	switch fault.KindOf(err) {
	case fault.DataLoss, fault.NotFound:
		return false
	}
	return true
}

// session tries the rules on one session: its first scan's first instance,
// by Series and SOP Instance UID, as live routing reads a reception's first
// file, and each scan's first instance for a series filter.
func (r *dryRun) session(ctx context.Context, sess tenant.RoutedSession) (*v1.DryRunSession, error) {
	m := &v1.DryRunSession{SubjectLabel: sess.SubjectLabel, SessionLabel: sess.SessionLabel, RecordedVersion: sess.Route.RuleSet,
		RecordedArchiveRule: sess.Route.Route}
	for _, x := range sess.Matches {
		if x.Phase == tenant.PhaseForward {
			m.RecordedForwardRules = append(m.RecordedForwardRules, x.Rule)
		}
	}
	slices.Sort(m.RecordedForwardRules)
	unreadable := func(why string) (*v1.DryRunSession, error) {
		m.Unreadable, m.Outcome = why, v1.DryRunOutcome_DRY_RUN_OUTCOME_UNKNOWN
		return m, nil
	}
	scans, err := r.scans(ctx, sess)
	if err != nil {
		return nil, err
	}
	if len(scans) == 0 {
		return unreadable("the session holds no scan")
	}
	if len(scans) > r.budget {
		return unreadable("the run's bound of " + strconv.Itoa(MaxDryRunScans) + " metadata files is reached")
	}
	caller := tenant.Caller{CallingAETitle: sess.Route.CallingAE, CalledAETitle: sess.Route.CalledAE}
	firsts := make([]*query.Subject, len(scans))
	subject := func(i int) (*query.Subject, error) {
		if firsts[i] != nil {
			return firsts[i], nil
		}
		r.budget--
		path := label.Path{Subject: sess.SubjectLabel, Session: sess.SessionLabel, Scan: scans[i].Label}
		h, err := r.c.archivedFirst(ctx, r.p, r.tenant, path, "", r.stores)
		if err != nil {
			return nil, err
		}
		// The header read is the original when the session was archived as
		// received, which a masked key may not learn through the answer.
		if err := refuseAsReceived(r.d, h.held); err != nil {
			return nil, err
		}
		removed, err := removedBy(ctx, r.p, h.held.Profile, r.removed)
		if err != nil {
			return nil, err
		}
		s := query.Subject{Dataset: h.ds, Catalog: tenant.RouteCatalog(caller), Removed: removed, Now: r.c.now()}
		firsts[i] = &s
		return &s, nil
	}
	first, err := subject(0)
	if err != nil {
		if readable(err) {
			return nil, err
		}
		return unreadable("its first scan's metadata file cannot be read")
	}
	ev := r.rules.Evaluate(*first)
	switch {
	case ev.Unusable != "":
		return unreadable("archive rule " + ev.Unusable + " cannot be evaluated over its header")
	case ev.Archive >= 0:
		m.ArchiveRule = r.rs.Archive[ev.Archive].Name
	}
	m.ArchiveUnknown = ev.ArchiveUnknown
	for _, i := range ev.ForwardUnknown {
		m.ForwardUnknown = append(m.ForwardUnknown, r.rs.Forward[i].Name)
	}
	for _, i := range ev.Forward {
		name := r.rs.Forward[i].Name
		if !r.rules.SeriesFiltered(i) {
			m.ForwardRules = append(m.ForwardRules, name)
			continue
		}
		held, unknown := false, false
		for j := range scans {
			s, err := subject(j)
			if err != nil {
				if readable(err) {
					return nil, err
				}
				unknown = true
				continue
			}
			o := r.rules.Series(i, *s)
			switch {
			case o.Unusable != "" || o.Truth == query.Unknown:
				unknown = true
			case o.Truth == query.True:
				held = true
			}
			if held {
				break
			}
		}
		switch {
		case held:
			m.ForwardRules = append(m.ForwardRules, name)
		case unknown:
			m.ForwardUnknown = append(m.ForwardUnknown, name)
		}
	}
	slices.Sort(m.ForwardRules)
	switch {
	case m.ArchiveUnknown || len(m.ForwardUnknown) > 0:
		m.Outcome = v1.DryRunOutcome_DRY_RUN_OUTCOME_UNKNOWN
	case m.ArchiveRule == m.RecordedArchiveRule && slices.Equal(m.ForwardRules, m.RecordedForwardRules):
		m.Outcome = v1.DryRunOutcome_DRY_RUN_OUTCOME_SAME
	default:
		m.Outcome = v1.DryRunOutcome_DRY_RUN_OUTCOME_CHANGED
	}
	return m, nil
}

// scans returns the session's scans in order of Series Instance UID, the
// order a reception's series are routed in, at most the run's reads left
// and one more (so a session past the bound is seen to be).
func (r *dryRun) scans(ctx context.Context, sess tenant.RoutedSession) ([]tenant.CatalogScan, error) {
	var out []tenant.CatalogScan
	page := master.Page{Limit: MaxPageSize}
	for {
		got, err := r.p.ListScans(ctx, tenant.ScanFilter{SubjectLabel: sess.SubjectLabel, SessionLabel: sess.SessionLabel, Order: tenant.OrderLabel}, page)
		if fault.KindOf(err) == fault.NotFound {
			// The session was deleted or relabeled since it was listed.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, got.Items...)
		if got.Next == "" || len(out) > r.budget {
			break
		}
		page.Cursor = got.Next
	}
	slices.SortFunc(out, func(a, b tenant.CatalogScan) int {
		switch {
		case a.SeriesUID < b.SeriesUID:
			return -1
		case a.SeriesUID > b.SeriesUID:
			return 1
		}
		return 0
	})
	return out, nil
}
