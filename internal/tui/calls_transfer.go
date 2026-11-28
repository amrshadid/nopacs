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

// This file holds the calls of the transfer screens.

// transferPreviewMsg is the answer of a transfer's preview: what it covers and
// what the target's receiver would refuse, with the request it answers.
type transferPreviewMsg struct {
	prev *v1.TransferPreview
	req  *v1.CreateTransferRequest
}

// transferStartedMsg is the answer of a transfer that was started.
type transferStartedMsg struct{ t *v1.Transfer }

// transferWord writes a step's name in lowercase.
func transferWord(s v1.TransferStep) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "TRANSFER_STEP_"))
}

// transferWhat is what a transfer carries: a subject, or one session of it.
func transferWhat(t *v1.Transfer) string {
	if t.GetSessionLabel() != "" {
		return t.GetSubjectLabel() + "/" + t.GetSessionLabel()
	}
	return t.GetSubjectLabel()
}

// transferMode is "move" or "copy".
func transferMode(move bool) string {
	if move {
		return "move"
	}
	return "copy"
}

// transferRow is a transfer as the cells of a row.
func transferRow(t *v1.Transfer, loc *time.Location) row {
	return row{cells: []string{t.GetJob().GetId(), transferWhat(t), transferMode(t.GetMove()), t.GetTargetTenant(), output.JobState(t.GetJob().GetState()),
		transferWord(t.GetStep()), itoa(t.GetItems().GetConfirmed()) + "/" + itoa(t.GetJob().GetCounts().GetInstances()),
		listTime(t.GetJob().GetCreateTime(), loc)}, ref: t}
}

// transfersPage asks for one page of a tenant's transfers, newest first.
func transfersPage(c *ctx, owner, gen int, token, tenant string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "transfer list", func(ctx context.Context) (*v1.ListTransfersResponse, error) {
			return cl.Transfers.ListTransfers(ctx, &v1.ListTransfersRequest{Tenant: tenant, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListTransfersResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetTransfers()))
			for _, t := range r.GetTransfers() {
				rows = append(rows, transferRow(t, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// getTransfer fetches one transfer again.
func getTransfer(c *ctx, owner int, tenant, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "transfer show", func(ctx context.Context) (*v1.GetTransferResponse, error) {
			return cl.Transfers.GetTransfer(ctx, &v1.GetTransferRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.GetTransferResponse) tea.Msg { return detailMsg{ref: r.GetTransfer()} })
	})
}

// previewTransfer asks what a transfer would cover; nothing is copied.
func previewTransfer(c *ctx, owner int, req *v1.CreateTransferRequest) tea.Cmd {
	cl := c.client
	preview := &v1.CreateTransferRequest{Tenant: req.GetTenant(), SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(),
		TargetTenant: req.GetTargetTenant(), Receiver: req.GetReceiver(), Move: req.GetMove(), Reason: req.GetReason(), ValidateOnly: true}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "transfer create", func(ctx context.Context) (*v1.CreateTransferResponse, error) {
			return cl.Transfers.CreateTransfer(ctx, preview)
		})
		return answer(res, err, func(r *v1.CreateTransferResponse) tea.Msg { return transferPreviewMsg{prev: r.GetPreview(), req: req} })
	})
}

// startTransfer makes the transfer a preview showed.
func startTransfer(c *ctx, owner int, req *v1.CreateTransferRequest) tea.Cmd {
	cl := c.client
	real := &v1.CreateTransferRequest{Tenant: req.GetTenant(), SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(),
		TargetTenant: req.GetTargetTenant(), Receiver: req.GetReceiver(), Move: req.GetMove(), Reason: req.GetReason()}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "transfer create", func(ctx context.Context) (*v1.CreateTransferResponse, error) {
			return cl.Transfers.CreateTransfer(ctx, real)
		})
		return answer(res, err, func(r *v1.CreateTransferResponse) tea.Msg { return transferStartedMsg{t: r.GetTransfer()} })
	})
}

// transferPreviewLines are the labeled lines of a transfer's preview.
func transferPreviewLines(c *ctx, ref any) []pair {
	m, _ := ref.(transferPreviewMsg)
	p := m.prev
	refused := "not counted (more than 100000 instances)"
	if p.GetRefusalsCounted() {
		refused = itoa(p.GetRefusedInstances())
	}
	deid := "no: archived as received, so only de-identified instances are copied"
	if p.GetReceiverDeidentifies() {
		deid = "yes"
	}
	what := m.req.GetSubjectLabel()
	if m.req.GetSessionLabel() != "" {
		what += "/" + m.req.GetSessionLabel()
	}
	return []pair{
		{"Transfers", what + " as a " + transferMode(m.req.GetMove()) + " to " + m.req.GetTargetTenant()},
		{"Sessions", itoa(p.GetSessions())}, {"Scans", itoa(p.GetScans())}, {"Instances", itoa(p.GetInstances())}, {"Bytes", output.Bytes(p.GetBytes())},
		{"Through the receiver", p.GetReceiver()}, {"The receiver de-identifies", deid}, {"Instances it would refuse", refused},
	}
}

// transferLines are the labeled lines of one transfer: where it goes, its job's
// state, the step it has reached and how many instances are in each state.
func transferLines(c *ctx, ref any) []pair {
	t, _ := ref.(*v1.Transfer)
	j, n, it := t.GetJob(), t.GetJob().GetCounts(), t.GetItems()
	ps := []pair{
		{"Id", j.GetId()},
		{"Transfers", transferWhat(t) + " as a " + transferMode(t.GetMove()) + " to " + t.GetTargetTenant() + " through " + t.GetReceiver()},
		{"State", output.JobState(j.GetState())}, {"Step", transferWord(t.GetStep())},
		{"Instances", itoa(n.GetInstances()) + " listed, " + output.Bytes(n.GetBytes())},
		{"Confirmed in the target", itoa(it.GetConfirmed())}, {"Copied, not yet found", itoa(it.GetCopied())},
		{"Not yet copied", itoa(it.GetListed())}, {"Failed", itoa(it.GetFailed())},
		{"Reason", output.Dash(t.GetReason())}, {"Ended because", output.Dash(j.GetReason())}, {"Made by", output.Dash(j.GetCreatedByKeyId())},
		{"Made", output.Time(j.GetCreateTime(), c.loc)}, {"Ended", output.Time(j.GetEndTime(), c.loc)},
	}
	if t.GetMove() {
		ps = append(ps, pair{"Delete of the source", output.Dash(t.GetDeleteCorrectionId())})
	}
	return ps
}
