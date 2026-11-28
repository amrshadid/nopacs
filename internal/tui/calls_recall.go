package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file holds the calls of the recall and the sample of a replica's
// screens.

// recallPreviewMsg is the answer of a recall's preview: what it would ask the
// archive tier for, and the token the recall must carry, with the request it
// answers.
type recallPreviewMsg struct {
	res *v1.RecallResponse
	req *v1.RecallRequest
	// target is the subject, session or scan as a person typed it.
	target string
}

// recallRequest is a recall's request: with validate_only, the preview's, or
// with the token it gave, the recall.
func recallRequest(base *v1.RecallRequest, validate bool, confirm string) *v1.RecallRequest {
	return &v1.RecallRequest{Tenant: base.GetTenant(), SubjectLabel: base.GetSubjectLabel(), SessionLabel: base.GetSessionLabel(),
		ScanLabel: base.GetScanLabel(), Storage: base.GetStorage(), Days: base.GetDays(), ValidateOnly: validate, Confirm: confirm}
}

// previewRecall asks what a recall would ask the archive tier for; nothing is
// asked of the store.
func previewRecall(c *ctx, owner int, req *v1.RecallRequest, target string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "recall", func(ctx context.Context) (*v1.RecallResponse, error) {
			return cl.Replicas.Recall(ctx, recallRequest(req, true, ""))
		})
		return answer(res, err, func(r *v1.RecallResponse) tea.Msg { return recallPreviewMsg{res: r, req: req, target: target} })
	})
}

// recallObjects asks the archive tier to bring a target's objects back, with
// the token a preview gave.
func recallObjects(c *ctx, owner int, m recallPreviewMsg) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "recall", func(ctx context.Context) (*v1.RecallResponse, error) {
			return cl.Replicas.Recall(ctx, recallRequest(m.req, false, m.res.GetConfirm()))
		})
		return answer(res, err, func(r *v1.RecallResponse) tea.Msg {
			return actionDoneMsg{note: "asked the store to recall " + itoa(r.GetObjectsRequested()) + " objects (" + output.Bytes(r.GetBytesRequested()) + ") of " + m.target}
		})
	})
}

// recallToAsk is how many objects a recall would ask for now: those the
// storages hold that are neither online nor being recalled already.
func recallToAsk(r *v1.RecallResponse) int64 {
	return r.GetObjects() - r.GetObjectsOnline() - r.GetObjectsRecalling()
}

// recallLines are the labeled lines of a recall's preview.
func recallLines(c *ctx, ref any) []pair {
	m, _ := ref.(recallPreviewMsg)
	r := m.res
	return []pair{
		{"Target", m.target}, {"Storages", output.Dash(strings.Join(r.GetStorages(), ", "))},
		{"Objects", itoa(r.GetObjects()) + " (" + output.Bytes(r.GetBytes()) + ")"},
		{"Online already", itoa(r.GetObjectsOnline())}, {"Being recalled", itoa(r.GetObjectsRecalling())},
		{"To ask for", itoa(recallToAsk(r)) + " (" + output.Bytes(r.GetBytesRequested()) + "), at the store's charge"},
		{"Confirm token", r.GetConfirm()},
	}
}

// samplePage asks for a random sample of a storage's objects, in one page, each
// with its key, size and SHA-256 as the catalog holds them, for a tool outside
// the server to check. The server reads no object itself.
func samplePage(c *ctx, owner, gen int, tenant, storage string, count int32, seed *int64) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "replica verify", func(ctx context.Context) (*v1.VerifySampleResponse, error) {
			return cl.Replicas.VerifySample(ctx, &v1.VerifySampleRequest{Tenant: tenant, Storage: storage, Count: count, Seed: seed})
		})
		return answer(res, err, func(r *v1.VerifySampleResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetObjects()))
			for _, o := range r.GetObjects() {
				rows = append(rows, row{cells: []string{o.GetKey(), itoa(o.GetSizeBytes()), o.GetSha256(), itoa(int64(o.GetRevision())),
					o.GetSubjectLabel() + "/" + o.GetSessionLabel() + "/" + o.GetScanLabel()}, ref: o})
			}
			return sampleMsg{page: pageMsg{gen: gen, rows: rows}, population: r.GetPopulation()}
		})
	})
}

// sampleMsg is a sample of a storage's objects, and how many objects the
// sample was drawn from.
type sampleMsg struct {
	page       pageMsg
	population int64
}
