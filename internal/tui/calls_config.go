package tui

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// This file holds the calls of the maintenance, profile, route and audit
// seal screens.

// createScrubJob queues a scrub: every stored object read again and compared
// with the catalog, on one storage or on every one.
func createScrubJob(c *ctx, owner int, tenant, storage string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job scrub", func(ctx context.Context) (*v1.CreateJobResponse, error) {
			return cl.Jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: tenant, Kind: &v1.CreateJobRequest_Scrub{Scrub: &v1.ScrubJob{Storage: storage}}})
		})
		return answer(res, err, func(r *v1.CreateJobResponse) tea.Msg {
			return actionDoneMsg{note: "scrub job " + r.GetJob().GetId() + " queued"}
		})
	})
}

// createRebuildJob queues a rebuild of the catalog from the storages.
func createRebuildJob(c *ctx, owner int, tenant string, mode v1.RebuildJobMode, from string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "job rebuild", func(ctx context.Context) (*v1.CreateJobResponse, error) {
			return cl.Jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: tenant, Kind: &v1.CreateJobRequest_Rebuild{Rebuild: &v1.RebuildJob{Mode: mode, FromStorage: from}}})
		})
		return answer(res, err, func(r *v1.CreateJobResponse) tea.Msg {
			return actionDoneMsg{note: "rebuild job " + r.GetJob().GetId() + " queued"}
		})
	})
}

// profilesPage asks for one page of a tenant's de-identification profiles.
func profilesPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "profile list", func(ctx context.Context) (*v1.ListProfilesResponse, error) {
			return cl.Profiles.ListProfiles(ctx, &v1.ListProfilesRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListProfilesResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetProfiles()))
			for _, p := range r.GetProfiles() {
				rows = append(rows, row{cells: []string{p.GetName(), strconv.Itoa(int(p.GetLatestVersion())), listTime(p.GetLatestCreateTime(), loc)}, ref: p})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// routeRow is one route of a receiver as the cells of a row.
func routeRow(i int, rt *v1.Route) row {
	profile := "-"
	if p := rt.GetProfile(); p != nil {
		profile = p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))
	}
	when := "every study"
	if n := len(rt.GetConditions()); n > 0 {
		when = strconv.Itoa(n) + " condition(s)"
	}
	return row{cells: []string{strconv.Itoa(i + 1), rt.GetName(), when, rt.GetStorage(), profile}, ref: rt}
}

// routesPage asks for a receiver and lists its routes in order. The routes
// are part of the receiver, so there is one page.
func routesPage(c *ctx, owner, gen int, tenant, receiver string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "receiver route list", func(ctx context.Context) (*v1.GetReceiverResponse, error) {
			return cl.Receivers.GetReceiver(ctx, &v1.GetReceiverRequest{Tenant: tenant, Name: receiver})
		})
		return answer(res, err, func(r *v1.GetReceiverResponse) tea.Msg {
			routes := r.GetReceiver().GetRouting().GetRoutes()
			rows := make([]row, 0, len(routes))
			for i, rt := range routes {
				rows = append(rows, routeRow(i, rt))
			}
			return pageMsg{gen: gen, rows: rows}
		})
	})
}

// sealVerifiedMsg is the answer of a verification of an audit log.
type sealVerifiedMsg struct{ res *v1.VerifyAuditResponse }

// verifySeal verifies a scope's audit log from its first entry, page after
// page, and answers what it found in one piece.
func verifySeal(c *ctx, owner int, scope *v1.Scope) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		req := &v1.VerifyAuditRequest{Scope: scope}
		total := &v1.VerifyAuditResponse{Intact: true}
		for {
			res, err := client.Call(ctx, cl, "audit verify", func(ctx context.Context) (*v1.VerifyAuditResponse, error) {
				return cl.AuditSeal.VerifyAudit(ctx, req)
			})
			if err != nil {
				return failMsg{err}
			}
			total.Intact = total.Intact && res.GetIntact()
			total.EntriesChecked += res.GetEntriesChecked()
			total.CheckpointsChecked += res.GetCheckpointsChecked()
			total.UnsealedEntries, total.LastCheckpoint = res.GetUnsealedEntries(), res.GetLastCheckpoint()
			if len(total.Problems) < 100 {
				total.Problems = append(total.Problems, res.GetProblems()...)
			}
			next := res.GetNextSeq()
			if next == 0 {
				return sealVerifiedMsg{res: total}
			}
			if next <= req.GetFromSeq() {
				return failMsg{fault.New(fault.Internal, "the server answered a next seq that does not go forward")}
			}
			req.FromSeq = next
		}
	})
}

// sealLines are the labeled lines of a verification.
func sealLines(c *ctx, ref any) []pair {
	r, _ := ref.(*v1.VerifyAuditResponse)
	intact := "yes"
	if !r.GetIntact() {
		intact = "NO: the log was changed or cut"
	}
	ps := []pair{
		{"Intact", intact},
		{"Checked", itoa(r.GetEntriesChecked()) + " entries, " + itoa(r.GetCheckpointsChecked()) + " checkpoints"},
		{"Unsealed", itoa(r.GetUnsealedEntries()) + " (sealed at the next pass; no verification covers them)"},
	}
	if lc := r.GetLastCheckpoint(); lc != nil {
		ps = append(ps, pair{"Last checkpoint", "seq " + itoa(lc.GetFromSeq()) + " to " + itoa(lc.GetToSeq()) + ", " + output.Time(lc.GetTime(), c.loc)},
			pair{"Its digest", hex.EncodeToString(lc.GetDigest())})
	} else {
		ps = append(ps, pair{"Last checkpoint", "none: the log was never sealed"})
	}
	for _, p := range r.GetProblems() {
		at := ""
		if p.GetSeq() > 0 {
			at = " at seq " + itoa(p.GetSeq())
		}
		ps = append(ps, pair{"Problem", output.AuditProblem(p.GetKind()) + at + ": " + strings.TrimSpace(p.GetDetail())})
	}
	return ps
}
