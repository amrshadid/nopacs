package tui

import (
	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// filled is a text field that starts with a value, which the person may
// change: what a catalog row stands for, when a key was pressed on it.
func filled(label, hint, value string) formField {
	return formField{label: label, hint: hint, in: field{}.set(value)}
}

// valuesChoices are the words of the field that asks for values in a preview:
// a masked key is never answered one, so it is offered only "no".
func valuesChoices(c *ctx) []string {
	if c.acc.maskedKey() {
		return []string{"no"}
	}
	return []string{"no", "yes"}
}

// newEditForm asks which subject, session, scan or instance to edit, what to
// set or remove and why. Its answer is the preview of the edit, which says what
// would change and lets the person confirm it. Values are hidden in it unless
// they are asked for, and asking is audited.
func newEditForm(c *ctx, tenant, target, instance string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Edit headers in " + clean(tenant),
		intro: []string{
			"The next screen shows every instance and attribute the edit would change. Nothing is changed until you confirm it there.",
			"Each instance gets a later revision; the old one stays, unserved, until the correction is purged, and undo takes the edit back until then.",
			"An attribute is a keyword (SeriesDescription) or gggg,eeee. Separate several with semicolons: a value holds none. The UIDs, pixel data, private attributes and what the tenant's profile removes are refused.",
		},
		fields: []formField{
			filled("Target", "subject[/session[/scan]]", target),
			filled("Instance", "optional: one instance of the scan, by its id", instance),
			{label: "Set", hint: "Tag=Value; Tag=Value (a value is printable ASCII, several values joined by a backslash)"},
			{label: "Remove", hint: "Tag; Tag"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Show values", choices: valuesChoices(c), hint: "now: shown in the preview and recorded in the audit log; a masked key is refused"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		t, err := parseTarget(f.value(0), f.value(1))
		if err != nil {
			return nil, err
		}
		changes, err := editChanges(f.value(2), f.value(3))
		if err != nil {
			return nil, err
		}
		if f.value(4) == "" {
			return nil, fault.New(fault.Invalid, "say why the headers are edited")
		}
		req := &v1.EditRequest{Tenant: tenant, Target: t, Changes: changes, Reason: f.value(4), ShowValues: f.value(5) == "yes"}
		return previewEdit(c, f.id, req), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(editPreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newEditPreview(c, m, f.value(0)), nil, true
	}
	return f
}

// newEditPreview shows what an edit would change, with the key that goes on to
// the confirmation.
func newEditPreview(c *ctx, m editPreviewMsg, typed string) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Edit " + clean(targetWord(m.req.GetTarget())), ref: m, pairs: editPreviewLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		if m.prev.GetConfirm() == "" {
			return nil
		}
		return push(newEditConfirm(c, m, typed), nil)
	}}}
	return d
}

// newEditConfirm asks for the target to be typed again, then edits with the
// preview's token and shows the correction it made.
func newEditConfirm(c *ctx, m editPreviewMsg, typed string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Edit " + clean(targetWord(m.req.GetTarget())),
		intro: []string{
			"This writes a new revision of " + itoa(int64(len(m.prev.GetInstances()))) + " instances with the changes the preview showed.",
			"If anything changed since the preview, or the tenant's profile refuses a new revision, the server refuses and you preview again.",
		},
		fields:   []formField{{label: "Type the target", hint: typed}},
		want:     typed,
		confirms: true,
		verb:     "edit",
		pop:      2,
		refusal:  true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return editData(c, f.id, m.req, m.prev.GetConfirm()), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		done, ok := msg.(editDoneMsg)
		if !ok {
			return nil, nil, false
		}
		note := "edit " + done.cor.GetId() + " is " + correctionWord(done.cor.GetState().String(), correctionStatePrefix)
		return f, backThen(f.pop, note, push(newCorrectionDetail(c, done.cor), nil)), true
	}
	return f
}
