package tui

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// This file holds the calls of the routing screens: a receiver's rule set
// versions, and what a version decides.

// maxExplainFile is the largest file an explanation sends: the server's bound.
const maxExplainFile = 64 << 20

// explainMsg is the answer of an explanation, with whether values were asked
// for: they are drawn only then, whatever the answer holds.
type explainMsg struct {
	res  *v1.ExplainRouteResponse
	show bool
}

// dryRunMsg is the answer of a dry run.
type dryRunMsg struct{ res *v1.DryRunRoutesResponse }

// routeTruthWord names a condition's result.
func routeTruthWord(t v1.RouteTruth) string {
	return strings.ToLower(strings.TrimPrefix(t.String(), "ROUTE_TRUTH_"))
}

// rulePhaseWord names a rule's phase.
func rulePhaseWord(p v1.RulePhase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "RULE_PHASE_"))
}

// profileRefText writes a profile version reference, or what stands for none.
func profileRefText(p *v1.ProfileVersionRef, none string) string {
	if p == nil {
		return none
	}
	return p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))
}

// whenOr writes a condition, or what an empty one holds for.
func whenOr(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

// ruleSetRow is one version of a receiver's rule sets as the cells of a row.
func ruleSetRow(rs *v1.RuleSet, loc *time.Location) row {
	running := ""
	if rs.GetActive() {
		running = "yes"
	}
	return row{cells: []string{strconv.Itoa(int(rs.GetVersion())), running, listTime(rs.GetCreateTime(), loc), output.Dash(rs.GetCreatedByKey()),
		strconv.Itoa(len(rs.GetArchiveRules())), strconv.Itoa(len(rs.GetForwardRules())), rs.GetComment()}, ref: rs}
}

// ruleSetsPage asks for one page of a receiver's rule set versions, the newest
// first.
func ruleSetsPage(c *ctx, owner, gen int, token, tenant, receiver string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "route list", func(ctx context.Context) (*v1.ListRuleSetsResponse, error) {
			return cl.Routes.ListRuleSets(ctx, &v1.ListRuleSetsRequest{Tenant: tenant, Receiver: receiver, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListRuleSetsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetRuleSets()))
			for _, rs := range r.GetRuleSets() {
				rows = append(rows, ruleSetRow(rs, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// getRuleSet reads one version of a receiver's rule sets.
func getRuleSet(c *ctx, owner int, tenant, receiver string, version int32) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "route get", func(ctx context.Context) (*v1.GetRuleSetResponse, error) {
			return cl.Routes.GetRuleSet(ctx, &v1.GetRuleSetRequest{Tenant: tenant, Receiver: receiver, Version: version})
		})
		return answer(res, err, func(r *v1.GetRuleSetResponse) tea.Msg { return detailMsg{ref: r.GetRuleSet()} })
	})
}

// activateRuleSet makes a receiver run one of its versions, or none (0).
func activateRuleSet(c *ctx, owner int, tenant, receiver string, version int32) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "route activate", func(ctx context.Context) (*v1.ActivateRuleSetResponse, error) {
			return cl.Routes.ActivateRuleSet(ctx, &v1.ActivateRuleSetRequest{Tenant: tenant, Receiver: receiver, Version: version})
		})
		return answer(res, err, func(r *v1.ActivateRuleSetResponse) tea.Msg {
			if r.GetRuleSet() == nil {
				return actionDoneMsg{note: "receiver " + receiver + " runs no rule set: every study goes as the receiver says"}
			}
			return actionDoneMsg{note: "receiver " + receiver + " runs rule set version " + strconv.Itoa(int(r.GetRuleSet().GetVersion()))}
		})
	})
}

// explainRoute asks what a version decides for one file or one archived
// instance, rule by rule. The file is read here, in the command, never in the
// screen's update, and at most maxExplainFile bytes of it.
func explainRoute(c *ctx, owner int, req *v1.ExplainRouteRequest, path string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		if path != "" {
			info, err := os.Stat(path)
			if err != nil {
				return failMsg{fault.Wrap(fault.Invalid, err, "cannot read "+clean(path))}
			}
			if !info.Mode().IsRegular() || info.Size() > maxExplainFile {
				return failMsg{fault.New(fault.Invalid, clean(path)+" is not a file of at most "+strconv.FormatInt(maxExplainFile>>20, 10)+"MiB")}
			}
			b, err := os.ReadFile(path) // #nosec G304 -- the file the person named
			if err != nil {
				return failMsg{fault.Wrap(fault.Invalid, err, "cannot read "+clean(path))}
			}
			req.Source = &v1.ExplainRouteRequest_File{File: b}
		}
		res, err := client.Call(ctx, cl, "route explain", func(ctx context.Context) (*v1.ExplainRouteResponse, error) {
			return cl.Routes.ExplainRoute(ctx, req)
		})
		return answer(res, err, func(r *v1.ExplainRouteResponse) tea.Msg { return explainMsg{res: r, show: req.GetShowValues()} })
	})
}

// dryRunRoutes tries a version on a receiver's latest sessions.
func dryRunRoutes(c *ctx, owner int, tenant, receiver string, version, sessions int32) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "route dry-run", func(ctx context.Context) (*v1.DryRunRoutesResponse, error) {
			return cl.Routes.DryRunRoutes(ctx, &v1.DryRunRoutesRequest{Tenant: tenant, Receiver: receiver, Version: version, Sessions: sessions})
		})
		return answer(res, err, func(r *v1.DryRunRoutesResponse) tea.Msg { return dryRunMsg{res: r} })
	})
}

// routeHitsPage asks for the receptions each rule routed, in one page.
func routeHitsPage(c *ctx, owner, gen int, tenant, receiver string, version int32) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "route hits", func(ctx context.Context) (*v1.GetRouteHitsResponse, error) {
			return cl.Routes.GetRouteHits(ctx, &v1.GetRouteHitsRequest{Tenant: tenant, Receiver: receiver, Version: version})
		})
		return answer(res, err, func(r *v1.GetRouteHitsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetHits()))
			for _, h := range r.GetHits() {
				rule := whenOr(h.GetRule(), "(default)")
				rows = append(rows, row{cells: []string{strconv.Itoa(int(h.GetVersion())), rulePhaseWord(h.GetPhase()), rule, itoa(h.GetReceptions()), itoa(h.GetSeries()),
					listTime(h.GetLastTime(), loc)}, ref: h})
			}
			return pageMsg{gen: gen, rows: rows}
		})
	})
}

// ruleSetLines are the labeled lines of one rule set version: its archive
// rules in the order tried, what happens to a study none chooses, and its
// forward rules.
func ruleSetLines(c *ctx, ref any) []pair {
	rs, _ := ref.(*v1.RuleSet)
	running := "no"
	if rs.GetActive() {
		running = "yes: the receiver runs it"
	}
	ps := []pair{{"Receiver", rs.GetReceiver()}, {"Version", strconv.Itoa(int(rs.GetVersion()))}, {"Running", running},
		{"Made", output.Time(rs.GetCreateTime(), c.loc)}, {"Made by", output.Dash(rs.GetCreatedByKey())}}
	if rs.GetComment() != "" {
		ps = append(ps, pair{"Comment", rs.GetComment()})
	}
	for i, r := range rs.GetArchiveRules() {
		ps = append(ps, pair{"Archive rule " + strconv.Itoa(i+1), r.GetName() + ": when " + whenOr(r.GetWhen(), "every study") + ", to " + r.GetStorage() +
			", profile " + profileRefText(r.GetProfile(), "receiver's")})
	}
	otherwise := "the receiver's storage and profile"
	if rs.GetQuarantineUnmatched() {
		otherwise = "held as unrouted"
	}
	ps = append(ps, pair{"No archive rule", otherwise})
	for _, r := range rs.GetForwardRules() {
		ps = append(ps, pair{"Forward rule", r.GetName() + ": when " + whenOr(r.GetWhen(), "every reception") + ", series " + whenOr(r.GetSeries(), "every series") +
			", to " + strings.Join(r.GetSenders(), ", ") + ", copy profile " + profileRefText(r.GetDestinationProfile(), "as archived")})
	}
	return ps
}

// explainLines are the labeled lines of an explanation: the decision, then each
// rule's result and the comparison that decided it. The values the comparison
// saw are drawn only when they were asked for.
func explainLines(c *ctx, ref any) []pair {
	m, _ := ref.(explainMsg)
	d := m.res.GetDecision()
	var verdict string
	switch {
	case d.GetUnrouted() != "":
		verdict = "held as unrouted (" + d.GetUnrouted() + ")"
		if d.GetArchiveRule() != "" {
			verdict += ", at rule " + d.GetArchiveRule()
		}
	case d.GetArchiveUnknown():
		verdict = "unknown: a rule before any that holds reads a value the header does not keep"
	case d.GetArchiveRule() != "":
		verdict = "archive rule " + d.GetArchiveRule() + " to " + d.GetStorage() + ", profile " + profileRefText(d.GetProfile(), "receiver's")
	default:
		verdict = "no archive rule: the receiver's storage " + output.Dash(d.GetStorage())
	}
	forward := output.Dash(strings.Join(d.GetForwardRules(), ", "))
	if len(d.GetForwardUnknown()) > 0 {
		forward += " (unknown: " + strings.Join(d.GetForwardUnknown(), ", ") + ")"
	}
	ps := []pair{{"Version", strconv.Itoa(int(m.res.GetVersion()))}, {"Decision", verdict}, {"Forward rules", forward}}
	for _, r := range m.res.GetRules() {
		dc := r.GetDecidedBy()
		by := "no condition"
		switch {
		case r.GetUnusable() != "":
			by = r.GetUnusable()
		case dc != nil:
			by = dc.GetComparison()
			present := "absent"
			if dc.GetPresent() {
				present = "present"
			}
			by += " (tag " + output.Dash(dc.GetTag()) + ", VR " + output.Dash(dc.GetVr()) + ", " + present + ")"
			if m.show {
				by += " saw " + strings.Join(dc.GetValues(), `\`)
			}
		}
		ps = append(ps, pair{rulePhaseWord(r.GetPhase()) + " " + r.GetRule(), routeTruthWord(r.GetTruth()) + ", decided by " + by})
	}
	if m.show {
		return append(ps, pair{"Values", "shown, and recorded in the audit log"})
	}
	return append(ps, pair{"Values", "hidden; ask for them on the form (audited, refused to a masked key)"})
}

// maxDryRunSessions is how many sessions of a dry run are drawn; the rest are
// counted in its totals.
const maxDryRunSessions = 300

// dryRunLines are the labeled lines of a dry run: its totals, then each
// session with the rules that routed it beside those the version would choose.
func dryRunLines(c *ctx, ref any) []pair {
	m, _ := ref.(dryRunMsg)
	r := m.res
	ps := []pair{{"Version", strconv.Itoa(int(r.GetVersion()))},
		{"Result", itoa(int64(r.GetSame())) + " the same, " + itoa(int64(r.GetChanged())) + " changed, " + itoa(int64(r.GetUnknown())) + " unknown"}}
	for i, s := range r.GetSessions() {
		if i == maxDryRunSessions {
			ps = append(ps, pair{"More", itoa(int64(len(r.GetSessions())-i)) + " sessions not drawn here"})
			break
		}
		would := output.Dash(s.GetArchiveRule())
		if s.GetArchiveUnknown() {
			would = "unknown"
		}
		fwd := strings.Join(s.GetForwardRules(), ", ")
		if len(s.GetForwardUnknown()) > 0 {
			fwd += " ?" + strings.Join(s.GetForwardUnknown(), ", ?")
		}
		outcome := strings.ToLower(strings.TrimPrefix(s.GetOutcome().String(), "DRY_RUN_OUTCOME_"))
		if s.GetUnreadable() != "" {
			outcome += ": " + s.GetUnreadable()
		}
		ps = append(ps, pair{s.GetSubjectLabel() + "/" + s.GetSessionLabel(), outcome + "; went by " + output.Dash(s.GetRecordedArchiveRule()) + " @" +
			strconv.Itoa(int(s.GetRecordedVersion())) + ", would go by " + would + ", would forward by " + output.Dash(fwd)})
	}
	return ps
}
