package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file holds the calls of the move and merge screens.

// moveAsk is what a move or a merge asks of the server: the request, which a
// preview sends with validate_only and the token it gave, and the words that
// describe it on the screens.
type moveAsk struct {
	tenant string
	// what moves, and under which subject.
	what, to string
	rewrite  bool
	reason   string
	// merge says what moves is a whole subject; otherwise a session.
	merge bool
}

// movePreviewMsg is the answer of a move's or a merge's preview: what moves
// and what refuses it, and the token it must carry, with what was asked.
type movePreviewMsg struct {
	prev *v1.MovePreview
	ask  moveAsk
}

// moveDoneMsg is the answer of a move or a merge that was made.
type moveDoneMsg struct {
	cor *v1.Correction
	ask moveAsk
}

// moveCall makes the request of a move or a merge and sends it.
func moveCall(ctx context.Context, cl *client.Client, ask moveAsk, confirm string) (*v1.MovePreview, *v1.Correction, error) {
	if ask.merge {
		res, err := client.Call(ctx, cl, "correction merge", func(ctx context.Context) (*v1.MergeResponse, error) {
			return cl.Corrections.Merge(ctx, &v1.MergeRequest{Tenant: ask.tenant, SubjectLabel: ask.what, IntoSubjectLabel: ask.to, Reason: ask.reason,
				ValidateOnly: confirm == "", Confirm: confirm, RewriteIdentity: ask.rewrite})
		})
		return res.GetPreview(), res.GetCorrection(), err
	}
	subject, session, _ := strings.Cut(ask.what, "/")
	res, err := client.Call(ctx, cl, "correction move", func(ctx context.Context) (*v1.MoveResponse, error) {
		return cl.Corrections.Move(ctx, &v1.MoveRequest{Tenant: ask.tenant, SubjectLabel: subject, SessionLabel: session, ToSubjectLabel: ask.to, Reason: ask.reason,
			ValidateOnly: confirm == "", Confirm: confirm, RewriteIdentity: ask.rewrite})
	})
	return res.GetPreview(), res.GetCorrection(), err
}

// previewMove asks what a move or a merge would do and for the token it must
// carry; nothing is changed.
func previewMove(c *ctx, owner int, ask moveAsk) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		prev, _, err := moveCall(ctx, cl, ask, "")
		if err != nil {
			return failMsg{err}
		}
		return movePreviewMsg{prev: prev, ask: ask}
	})
}

// makeMove makes the move or the merge a preview gave its token for.
func makeMove(c *ctx, owner int, ask moveAsk, token string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		_, cor, err := moveCall(ctx, cl, ask, token)
		if err != nil {
			return failMsg{err}
		}
		return moveDoneMsg{cor: cor, ask: ask}
	})
}

// movePreviewLines are the labeled lines of a move's or a merge's preview.
func movePreviewLines(c *ctx, ref any) []pair {
	m, _ := ref.(movePreviewMsg)
	p, n := m.prev, m.prev.GetCounts()
	rewrite := "no"
	if m.ask.rewrite {
		rewrite = "yes: once the move is published, the moved instances are given the other subject's archived Patient ID and issuer by an edit of their own"
	}
	return []pair{
		{"Moves", m.ask.what}, {"Under", m.ask.to},
		{"Sessions", output.Dash(strings.Join(p.GetSessions(), ", "))},
		{"Scans", itoa(n.GetScans())}, {"Instances", itoa(n.GetInstances())}, {"Bytes", output.Bytes(n.GetBytes())},
		{"Storages", output.Dash(strings.Join(p.GetStorages(), ", "))},
		{"Holds", output.Dash(strings.Join(p.GetHolds(), ", "))},
		{"Labels held", output.Dash(strings.Join(p.GetClashes(), ", "))},
		{"Rewrite identity", rewrite},
		{"Old objects purged", output.Time(p.GetPurgeTime(), c.loc)},
		{"Confirm token", p.GetConfirm()},
	}
}

// moveResult is a move or a merge as the screen that follows it shows it: the
// correction it made, and whether its identity rewrite was asked.
type moveResult struct {
	cor     *v1.Correction
	rewrite bool
}

// moveResultLines are the lines of a correction a move or a merge made, with
// the state of its identity rewrite: a refusal is the server's, for good; one
// asked and not refused is made in the background or still owed.
func moveResultLines(c *ctx, ref any) []pair {
	r, _ := ref.(moveResult)
	ps := correctionLines(c, r.cor)
	if r.rewrite && r.cor.GetIdentityRewriteRefused() == "" {
		ps = append(ps, pair{"Identity rewrite", "asked: made in the background once the move is published, as an edit that is a correction of its own; a refusal shows here when this correction is opened again"})
	}
	if !r.rewrite {
		ps = append(ps, pair{"Identity rewrite", "not asked: the moved instances keep the identity they have"})
	}
	return ps
}
