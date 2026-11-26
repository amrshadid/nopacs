package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file holds the calls of the retention and hold screens.

// disposePreviewMsg is the answer of a disposal's preview: what it would
// delete and the token the disposal must carry, with the rule and reason it
// was asked for, which the confirmation sends again.
type disposePreviewMsg struct {
	res          *v1.DisposalPreview
	rule, reason string
}

// ruleRow is a retention rule as the cells of a row.
func ruleRow(r *v1.RetentionRule) row {
	return row{cells: []string{r.GetName(), output.RetentionSelector(r.GetSelector()), output.RetentionPeriod(r.GetKeepAtLeast()),
		output.RetentionPeriod(r.GetDisposeAfter()), output.RetentionAction(r.GetAction()), itoa(r.GetSessionsDue())}, ref: r}
}

// rulesPage asks for one page of a tenant's retention rules.
func rulesPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention rule list", func(ctx context.Context) (*v1.ListRetentionRulesResponse, error) {
			return cl.Retention.ListRetentionRules(ctx, &v1.ListRetentionRulesRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListRetentionRulesResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetRules()))
			for _, rule := range r.GetRules() {
				rows = append(rows, ruleRow(rule))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// addRule adds a retention rule.
func addRule(c *ctx, owner int, tenant string, rule *v1.RetentionRule) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention rule add", func(ctx context.Context) (*v1.AddRetentionRuleResponse, error) {
			return cl.Retention.AddRetentionRule(ctx, &v1.AddRetentionRuleRequest{Tenant: tenant, Rule: rule})
		})
		return answer(res, err, func(r *v1.AddRetentionRuleResponse) tea.Msg {
			return actionDoneMsg{note: "retention rule " + r.GetRule().GetName() + " added"}
		})
	})
}

// setRule changes the fields of a retention rule the mask names.
func setRule(c *ctx, owner int, tenant string, rule *v1.RetentionRule, paths []string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention rule set", func(ctx context.Context) (*v1.SetRetentionRuleResponse, error) {
			return cl.Retention.SetRetentionRule(ctx, &v1.SetRetentionRuleRequest{Tenant: tenant, Rule: rule, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		})
		return answer(res, err, func(r *v1.SetRetentionRuleResponse) tea.Msg {
			return actionDoneMsg{note: "retention rule " + r.GetRule().GetName() + " changed"}
		})
	})
}

// removeRule removes a retention rule.
func removeRule(c *ctx, owner int, tenant, name string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention rule remove", func(ctx context.Context) (*v1.RemoveRetentionRuleResponse, error) {
			return cl.Retention.RemoveRetentionRule(ctx, &v1.RemoveRetentionRuleRequest{Tenant: tenant, Name: name})
		})
		return answer(res, err, func(r *v1.RemoveRetentionRuleResponse) tea.Msg {
			return actionDoneMsg{note: "retention rule " + r.GetRule().GetName() + " removed"}
		})
	})
}

// dueRow is a due session as the cells of a row.
func dueRow(s *v1.DueSession, loc *time.Location) row {
	note := "-"
	switch {
	case s.GetHeld():
		note = "held"
	case s.GetUndone():
		note = "undone by a person"
	}
	return row{cells: []string{s.GetSubjectLabel() + "/" + s.GetSessionLabel(), s.GetRule(), output.RetentionAction(s.GetAction()),
		listDate(s.GetDueTime(), loc, "-"), itoa(s.GetInstances()), output.Human(s.GetBytes()), note}, ref: s}
}

// duePage asks for one page of a tenant's due sessions, oldest due first.
func duePage(c *ctx, owner, gen int, token, tenant, rule string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention due", func(ctx context.Context) (*v1.ListDueSessionsResponse, error) {
			return cl.Retention.ListDueSessions(ctx, &v1.ListDueSessionsRequest{Tenant: tenant, Rule: rule, IncludeHeld: true, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListDueSessionsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetSessions()))
			for _, s := range r.GetSessions() {
				rows = append(rows, dueRow(s, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// previewDispose asks what a disposal would delete and for the token it must
// carry; nothing is deleted.
func previewDispose(c *ctx, owner int, tenant, rule, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention dispose", func(ctx context.Context) (*v1.DisposeResponse, error) {
			return cl.Retention.Dispose(ctx, &v1.DisposeRequest{Tenant: tenant, Rule: rule, Reason: reason, ValidateOnly: true})
		})
		return answer(res, err, func(r *v1.DisposeResponse) tea.Msg {
			return disposePreviewMsg{res: r.GetPreview(), rule: rule, reason: reason}
		})
	})
}

// dispose queues the job that deletes exactly the sessions a preview listed.
func dispose(c *ctx, owner int, tenant, rule, reason, token string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention dispose", func(ctx context.Context) (*v1.DisposeResponse, error) {
			return cl.Retention.Dispose(ctx, &v1.DisposeRequest{Tenant: tenant, Rule: rule, Reason: reason, Confirm: token})
		})
		return answer(res, err, func(r *v1.DisposeResponse) tea.Msg {
			return actionDoneMsg{note: "disposal queued as job " + r.GetJob().GetId()}
		})
	})
}

// getRetention asks for a tenant's retention state, from its status.
func getRetention(c *ctx, owner int, tenant string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "retention clock", func(ctx context.Context) (*v1.GetStatusResponse, error) {
			return cl.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: tenantScope(tenant)})
		})
		return answer(res, err, func(r *v1.GetStatusResponse) tea.Msg {
			if len(r.GetTenants()) == 0 || r.GetTenants()[0].GetRetention() == nil {
				return detailMsg{ref: &v1.RetentionStatus{}}
			}
			return detailMsg{ref: r.GetTenants()[0].GetRetention()}
		})
	})
}

// confirmClock ends a pause for a clock jump with the time the person read.
func confirmClock(c *ctx, owner int, tenant string, t time.Time, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		_, err := client.Call(ctx, cl, "retention clock", func(ctx context.Context) (*v1.ConfirmClockResponse, error) {
			return cl.Retention.ConfirmClock(ctx, &v1.ConfirmClockRequest{Scope: tenantScope(tenant), ObservedTime: timestamppb.New(t), Reason: reason})
		})
		if err != nil {
			return failMsg{err}
		}
		return actionDoneMsg{note: "time confirmed; retention resumes"}
	})
}

// holdRow is a hold as the cells of a row.
func holdRow(h *v1.Hold, loc *time.Location) row {
	return row{cells: []string{h.GetId(), output.HoldTarget(h.GetTarget()), output.HoldState(h.GetState()), listDate(h.GetPlaceTime(), loc, "-"), h.GetReason()}, ref: h}
}

// holdsPage asks for one page of a tenant's holds, newest first.
func holdsPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "hold list", func(ctx context.Context) (*v1.ListHoldsResponse, error) {
			return cl.Retention.ListHolds(ctx, &v1.ListHoldsRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListHoldsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetHolds()))
			for _, h := range r.GetHolds() {
				rows = append(rows, holdRow(h, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// placeHold places a hold. A server that does not serve it yet says so in one
// plain line, and nothing is held.
func placeHold(c *ctx, owner int, tenant string, target *v1.HoldTarget, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "hold place", func(ctx context.Context) (*v1.PlaceHoldResponse, error) {
			return cl.Retention.PlaceHold(ctx, &v1.PlaceHoldRequest{Tenant: tenant, Target: target, Reason: reason})
		})
		if err != nil {
			return failMsg{client.OrNotServed("hold place", err)}
		}
		return actionDoneMsg{note: "hold " + res.GetHold().GetId() + " placed on " + output.HoldTarget(res.GetHold().GetTarget())}
	})
}

// releaseHold releases a hold.
func releaseHold(c *ctx, owner int, tenant, id, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "hold release", func(ctx context.Context) (*v1.ReleaseHoldResponse, error) {
			return cl.Retention.ReleaseHold(ctx, &v1.ReleaseHoldRequest{Tenant: tenant, Id: id, Reason: reason})
		})
		if err != nil {
			return failMsg{client.OrNotServed("hold release", err)}
		}
		return actionDoneMsg{note: "hold " + res.GetHold().GetId() + " released"}
	})
}
