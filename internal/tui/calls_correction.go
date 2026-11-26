package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file holds the calls of the correction screens.

// Every name of a correction's kind and state starts with one of these.
const (
	correctionKindPrefix  = "CORRECTION_KIND_"
	correctionStatePrefix = "CORRECTION_STATE_"
)

// deletePreviewMsg is the answer of a delete's preview: what it would remove
// and the token the delete must carry, with the request it answers, which the
// confirmation sends again.
type deletePreviewMsg struct {
	prev *v1.DeletePreview
	req  *v1.DeleteRequest
}

// correctionWord writes a correction's enum name as a word.
func correctionWord(name, prefix string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "_", "-")
}

// targetWord writes a correction's target as a path of labels.
func targetWord(t *v1.CorrectionTarget) string {
	parts := []string{t.GetSubjectLabel()}
	for _, p := range []string{t.GetSessionLabel(), t.GetScanLabel()} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	s := strings.Join(parts, "/")
	if t.GetInstanceId() != "" {
		s += " instance " + t.GetInstanceId()
	}
	return s
}

// correctionRow is a correction as the cells of a row.
func correctionRow(cr *v1.Correction, loc *time.Location) row {
	return row{cells: []string{cr.GetId(), correctionWord(cr.GetKind().String(), correctionKindPrefix), correctionWord(cr.GetState().String(), correctionStatePrefix),
		targetWord(cr.GetTarget()), itoa(cr.GetCounts().GetInstances()), listDate(cr.GetCreateTime(), loc, "-")}, ref: cr}
}

// correctionsPage asks for one page of a tenant's corrections, newest first.
func correctionsPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction list", func(ctx context.Context) (*v1.ListCorrectionsResponse, error) {
			return cl.Corrections.ListCorrections(ctx, &v1.ListCorrectionsRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListCorrectionsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetCorrections()))
			for _, cr := range r.GetCorrections() {
				rows = append(rows, correctionRow(cr, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// previewDelete asks what a delete would remove and for the token it must
// carry; nothing is deleted.
func previewDelete(c *ctx, owner int, req *v1.DeleteRequest) tea.Cmd {
	cl := c.client
	preview := &v1.DeleteRequest{Tenant: req.GetTenant(), Target: req.GetTarget(), Reason: req.GetReason(), ValidateOnly: true,
		OverrideRetention: req.GetOverrideRetention(), OverrideReason: req.GetOverrideReason()}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction delete", func(ctx context.Context) (*v1.DeleteResponse, error) {
			return cl.Corrections.Delete(ctx, preview)
		})
		return answer(res, err, func(r *v1.DeleteResponse) tea.Msg { return deletePreviewMsg{prev: r.GetPreview(), req: req} })
	})
}

// deleteData deletes what a preview listed, with the preview's token.
func deleteData(c *ctx, owner int, req *v1.DeleteRequest, token string) tea.Cmd {
	cl := c.client
	real := &v1.DeleteRequest{Tenant: req.GetTenant(), Target: req.GetTarget(), Reason: req.GetReason(), Confirm: token,
		OverrideRetention: req.GetOverrideRetention(), OverrideReason: req.GetOverrideReason()}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction delete", func(ctx context.Context) (*v1.DeleteResponse, error) {
			return cl.Corrections.Delete(ctx, real)
		})
		return answer(res, err, func(r *v1.DeleteResponse) tea.Msg {
			return actionDoneMsg{note: "deleted " + targetWord(req.GetTarget()) + ": correction " + r.GetCorrection().GetId() + "; undo it until its bytes are purged"}
		})
	})
}

// relabelData gives a subject, a session or a scan a new label.
func relabelData(c *ctx, owner int, tenant string, target *v1.CorrectionTarget, to, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction relabel", func(ctx context.Context) (*v1.RelabelResponse, error) {
			return cl.Corrections.Relabel(ctx, &v1.RelabelRequest{Tenant: tenant, Target: target, ToLabel: to, Reason: reason})
		})
		return answer(res, err, func(r *v1.RelabelResponse) tea.Msg {
			return actionDoneMsg{note: "relabel " + r.GetCorrection().GetId() + " is " + correctionWord(r.GetCorrection().GetState().String(), correctionStatePrefix)}
		})
	})
}

// replaceInstance puts a corrected quarantined file in place of the archived
// instance.
func replaceInstance(c *ctx, owner int, tenant, file, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction replace", func(ctx context.Context) (*v1.ReplaceResponse, error) {
			return cl.Corrections.Replace(ctx, &v1.ReplaceRequest{Tenant: tenant, QuarantinedFile: file, Reason: reason})
		})
		return answer(res, err, func(r *v1.ReplaceResponse) tea.Msg {
			return actionDoneMsg{note: "replace " + r.GetCorrection().GetId() + " is " + correctionWord(r.GetCorrection().GetState().String(), correctionStatePrefix)}
		})
	})
}

// undoCorrection shows a deleted subject again, or stops a relabel copying.
func undoCorrection(c *ctx, owner int, tenant, id, reason string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction undo", func(ctx context.Context) (*v1.UndoCorrectionResponse, error) {
			return cl.Corrections.UndoCorrection(ctx, &v1.UndoCorrectionRequest{Tenant: tenant, Id: id, Reason: reason})
		})
		return answer(res, err, func(r *v1.UndoCorrectionResponse) tea.Msg {
			return actionDoneMsg{note: "correction " + r.GetCorrection().GetId() + " is " + correctionWord(r.GetCorrection().GetState().String(), correctionStatePrefix)}
		})
	})
}

// purgeCorrection purges a correction's hidden or superseded bytes now.
func purgeCorrection(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction purge", func(ctx context.Context) (*v1.PurgeCorrectionResponse, error) {
			return cl.Corrections.PurgeCorrection(ctx, &v1.PurgeCorrectionRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.PurgeCorrectionResponse) tea.Msg {
			return actionDoneMsg{note: "correction " + r.GetCorrection().GetId() + " is " + correctionWord(r.GetCorrection().GetState().String(), correctionStatePrefix)}
		})
	})
}

// correctionLines are the labeled lines of a correction: its dispositions
// per storage and the copies a detach abandoned, never worded more strongly
// than the storages said.
func correctionLines(c *ctx, ref any) []pair {
	cr, _ := ref.(*v1.Correction)
	n := cr.GetCounts()
	ps := []pair{
		{"Id", cr.GetId()}, {"Kind", correctionWord(cr.GetKind().String(), correctionKindPrefix)},
		{"State", correctionWord(cr.GetState().String(), correctionStatePrefix)}, {"Target", targetWord(cr.GetTarget())},
		{"Reason", cr.GetReason()}, {"Made by", output.Dash(cr.GetCreatedByKeyId())}, {"Made", output.Time(cr.GetCreateTime(), c.loc)},
		{"Purge after", output.Time(cr.GetPurgeTime(), c.loc)},
	}
	if cr.GetStateReason() != "" {
		ps = append(ps, pair{"Why", cr.GetStateReason()})
	}
	ps = append(ps, pair{"Instances", itoa(n.GetInstances()) + ", " + output.Bytes(n.GetBytes())},
		pair{"Objects removed", itoa(n.GetObjectsRemoved()) + " (" + output.Bytes(n.GetBytesRemoved()) + ")"})
	for _, d := range cr.GetDispositions() {
		label := "On " + d.GetStorage()
		if d.GetReplicaOf() != "" {
			label += " (replica of " + d.GetReplicaOf() + ")"
		}
		ps = append(ps, pair{label, output.PurgeClaim(d)})
	}
	if len(cr.GetCopiesAbandoned()) > 0 {
		ps = append(ps, pair{"Copies abandoned", strings.Join(cr.GetCopiesAbandoned(), ", ") + ": detached before the purge reached them; their objects are unknown and may still hold the bytes"})
	}
	if cr.GetOverrideRetention() {
		ps = append(ps, pair{"Retention overridden", cr.GetOverrideReason()})
	}
	if cr.GetOrigin() == v1.CorrectionOrigin_CORRECTION_ORIGIN_RETENTION {
		ps = append(ps, pair{"Retention rule", cr.GetRetentionRule()})
	}
	return ps
}

// previewLines are the labeled lines of a delete's preview.
func previewLines(c *ctx, ref any) []pair {
	m, _ := ref.(deletePreviewMsg)
	p, n := m.prev, m.prev.GetCounts()
	ps := []pair{
		{"Target", targetWord(m.req.GetTarget())},
		{"Removes", itoa(n.GetSubjects()) + " subjects, " + itoa(n.GetSessions()) + " sessions, " + itoa(n.GetScans()) + " scans, " + itoa(n.GetInstances()) + " instances, " + output.Bytes(n.GetBytes())},
		{"Storages", output.Dash(strings.Join(p.GetStorages(), ", "))},
		{"Purged after", output.Time(p.GetPurgeTime(), c.loc)},
	}
	for _, sp := range p.GetStoragePurges() {
		ps = append(ps, pair{"On " + sp.GetStorage(), correctionWord(sp.GetExpected().String(), "OBJECT_DISPOSITION_") + " (" + sp.GetWhy() + ")"})
	}
	for _, s := range p.GetSentTo() {
		ps = append(ps, pair{"Sent to " + s.GetSender(), itoa(s.GetInstances()) + " instances, out of reach: ask the peer to delete them"})
	}
	if m.req.GetOverrideRetention() {
		ps = append(ps, pair{"Retention", "overridden: " + m.req.GetOverrideReason()})
	}
	return append(ps, pair{"Confirm token", p.GetConfirm()})
}
