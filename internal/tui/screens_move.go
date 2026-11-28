package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// labelOf returns text as a subject's or a session's label, or why it is not
// one; the server judges the labels again.
func labelOf(text, what string) (string, error) {
	if err := label.Check(text); err != nil {
		return "", fault.New(fault.Invalid, fmt.Sprintf("%s %q is not a label: %s", what, clean(text), fault.Message(err)))
	}
	return text, nil
}

// newMoveForm asks which session goes under which subject, and why. Its answer
// is the preview of the move, which says what moves and what refuses it.
func newMoveForm(c *ctx, tenant, session string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Move a session in " + clean(tenant),
		intro: []string{
			"The session, with its scans, is filed under another subject the tenant holds: a study filed under the wrong patient.",
			"Every object is copied to its key under the other subject and checked; the session switches at once and the old objects stay until the correction is purged. Undo files it back until then.",
			"Refused: a session label the other subject holds, a hold, a correction not ended, a reception in progress, a subject's last session (merge it) and a masked key.",
		},
		fields: []formField{
			filled("Session", "subject/session", session),
			{label: "To subject", hint: "the label of the subject the session goes under"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Rewrite identity", choices: []string{"no", "yes"}, hint: "yes: give the moved instances the other subject's archived Patient ID and issuer"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		parts := strings.Split(f.value(0), "/")
		if len(parts) != 2 {
			return nil, fault.New(fault.Invalid, "a session is subject/session")
		}
		for _, p := range parts {
			if _, err := labelOf(p, "the label"); err != nil {
				return nil, err
			}
		}
		to, err := labelOf(f.value(1), "the subject")
		if err != nil {
			return nil, err
		}
		if f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the session is moved")
		}
		return previewMove(c, f.id, moveAsk{tenant: tenant, what: f.value(0), to: to, reason: f.value(2), rewrite: f.value(3) == "yes"}), nil
	}
	f.result = moveResultHandler
	return f
}

// newMergeForm asks which subject merges into which, and why. Its answer is
// the preview of the merge.
func newMergeForm(c *ctx, tenant, subject string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Merge a subject in " + clean(tenant),
		intro: []string{
			"A whole subject is merged into another the tenant holds: one patient archived under two subjects. Every session moves as a move moves one, in one correction.",
			"The emptied subject is hidden and its label is held for arrivals until the merge is purged; undo makes it again with every session until then.",
			"Refused as a move is: a session label the other subject holds, a hold, a correction not ended, a reception in progress and a masked key.",
		},
		fields: []formField{
			filled("Subject", "the subject that is merged", subject),
			{label: "Into subject", hint: "the label of the subject it merges into"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Rewrite identity", choices: []string{"no", "yes"}, hint: "yes: give the moved instances the other subject's archived Patient ID and issuer"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		from, err := labelOf(f.value(0), "the subject")
		if err != nil {
			return nil, err
		}
		into, err := labelOf(f.value(1), "the subject")
		if err != nil {
			return nil, err
		}
		if f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the subject is merged")
		}
		return previewMove(c, f.id, moveAsk{tenant: tenant, what: from, to: into, reason: f.value(2), rewrite: f.value(3) == "yes", merge: true}), nil
	}
	f.result = moveResultHandler
	return f
}

// moveResultHandler takes the preview a move's or a merge's form asked for.
func moveResultHandler(c *ctx, _ formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
	m, ok := msg.(movePreviewMsg)
	if !ok {
		return nil, nil, false
	}
	return newMovePreview(c, m), nil, true
}

// newMovePreview shows what a move or a merge would do, with the key that goes
// on to the confirmation.
func newMovePreview(c *ctx, m movePreviewMsg) screen {
	word := "Move"
	if m.ask.merge {
		word = "Merge"
	}
	d := detailScreen{base: base{c.newID()}, heading: word + " " + clean(m.ask.what) + " under " + clean(m.ask.to), ref: m, pairs: movePreviewLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		if m.prev.GetConfirm() == "" {
			return nil
		}
		return push(newMoveConfirm(c, m), nil)
	}}}
	return d
}

// newMoveConfirm asks for what moves to be typed again, then makes the move or
// the merge with the preview's token and shows the correction it made, with the
// state of its identity rewrite.
func newMoveConfirm(c *ctx, m movePreviewMsg) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Move " + clean(m.ask.what) + " under " + clean(m.ask.to),
		intro: []string{
			"This files " + itoa(m.prev.GetCounts().GetInstances()) + " instances under " + clean(m.ask.to) + ". The copies are made in the background; the old objects stay until the purge.",
			"If anything changed since the preview, the server refuses and you preview again.",
		},
		fields:   []formField{{label: "Type the name", hint: m.ask.what}},
		want:     m.ask.what,
		confirms: true,
		verb:     "move",
		pop:      2,
		refusal:  true,
	}
	if m.ask.merge {
		f.heading = "Merge " + clean(m.ask.what) + " into " + clean(m.ask.to)
		f.verb = "merge"
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return makeMove(c, f.id, m.ask, m.prev.GetConfirm()), nil }
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		done, ok := msg.(moveDoneMsg)
		if !ok {
			return nil, nil, false
		}
		d := detailScreen{base: base{c.newID()}, heading: "Correction " + clean(done.cor.GetId()), ref: moveResult{cor: done.cor, rewrite: done.ask.rewrite}, pairs: moveResultLines}
		note := correctionWord(done.cor.GetKind().String(), correctionKindPrefix) + " " + done.cor.GetId() + " is " + correctionWord(done.cor.GetState().String(), correctionStatePrefix)
		return f, backThen(f.pop, note, push(d, nil)), true
	}
	return f
}
