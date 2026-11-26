package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// hostile is text a network peer could send: a screen clear, a color, a title
// change, a bell and a line break.
const hostile = "x\x1b[2Jy\x1b[31mz\x1b]0;owned\ab\x07c\r\nd\x9b1m"

// hasControl reports whether a drawn line holds a character that would act on
// a terminal. The styler is off in these tests, so no escape of our own is
// drawn.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || (r >= 0x7f && r < 0xa0) })
}

// TestDetailDrawsNoControlCharacterFromLabelsOrValues guards the detail
// renderer: a label built from a name the server sent is cleaned like a value,
// whatever screen built it.
func TestDetailDrawsNoControlCharacterFromLabelsOrValues(t *testing.T) {
	c := &ctx{base: context.Background(), pending: map[int]*pendingCall{}, w: 80, h: 24}
	d := detailScreen{pairs: func(*ctx, any) []pair {
		return []pair{{hostile, hostile}, {"Receiver " + hostile, "ok"}, {"plain", hostile}}
	}}
	for _, l := range d.view(c) {
		if hasControl(l) {
			t.Errorf("a control character reached the drawn line %q", l)
		}
	}
	if got := strings.Join(d.view(c), "\n"); !strings.Contains(got, "plain:") {
		t.Errorf("the labels are not drawn at all:\n%s", got)
	}
}

// TestFormDrawsNoControlCharacterFromHintsLabelsOrIntro guards the form
// renderer: hints carry rule names, ids and storage names from the server, and
// are cleaned where they are drawn.
func TestFormDrawsNoControlCharacterFromHintsLabelsOrIntro(t *testing.T) {
	c := &ctx{base: context.Background(), pending: map[int]*pendingCall{}, w: 100, h: 24}
	f := formScreen{
		heading: "Form",
		intro:   []string{"about " + hostile},
		fields: []formField{
			{label: "Name " + hostile, hint: "type " + hostile},
			{label: "Pick", hint: hostile, choices: []string{"a", "b"}},
		},
		msg: hostile,
	}
	for _, l := range f.view(c) {
		if hasControl(l) {
			t.Errorf("a control character reached the drawn line %q", l)
		}
	}
}

// TestConfirmationWithNothingToTypeRefuses guards the typed check: a
// confirmation whose expected text is empty (the server left out the name or
// the id) can never be matched, so it refuses to submit instead of skipping the
// check and sending on one key.
func TestConfirmationWithNothingToTypeRefuses(t *testing.T) {
	c := &ctx{base: context.Background(), pending: map[int]*pendingCall{}, w: 80, h: 24}
	sent := 0
	f := formScreen{
		heading:  "Remove",
		fields:   []formField{{label: "Type the name"}},
		confirms: true,
		submit:   func(*ctx, formScreen) (tea.Cmd, error) { sent++; return nil, nil },
	}
	next, _ := f.send(c)
	got := next.(formScreen)
	if sent != 0 || got.busy || !strings.Contains(got.msg, "nothing to type") {
		t.Errorf("sent %d, busy %v, message %q", sent, got.busy, got.msg)
	}
	f.want = "lab1"
	f.fields = []formField{{label: "Type the name"}}
	if next, _ = f.send(c); sent != 0 || !strings.Contains(next.(formScreen).msg, "type lab1 exactly") {
		t.Errorf("an empty field matched: sent %d", sent)
	}
}
