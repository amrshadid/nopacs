package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newCorrectionList makes the list of a tenant's corrections, newest first.
// Its keys delete, relabel and replace, and undo and purge the one under the
// cursor; every one asks for what it acts on to be typed, and a delete shows
// its preview first.
func newCorrectionList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Corrections of "+clean(tenant), []column{{"ID", 8, 26}, {"KIND", 4, 8}, {"STATE", 5, 10}, {"TARGET", 8, 50}, {"INSTANCES", 9, 10}, {"MADE", 8, 10}},
		"no correction yet; n: delete, l: relabel, m: replace")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return correctionsPage(c, l.id, l.gen, token, tenant)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		cr, _ := r.ref.(*v1.Correction)
		d := detailScreen{base: base{c.newID()}, heading: "Correction " + clean(cr.GetId()), ref: cr, pairs: correctionLines}
		return push(d, nil)
	}
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = []action{
			{key: "n", help: "delete", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newDeleteForm(c, tenant), nil) }},
			{key: "l", help: "relabel", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newRelabelForm(c, tenant), nil) }},
			{key: "m", help: "replace", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newReplaceForm(c, tenant), nil) }},
			{key: "u", help: "undo", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				cr, _ := ref.(*v1.Correction)
				return push(newUndoForm(c, tenant, cr.GetId()), nil)
			}},
			{key: "p", help: "purge", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				cr, _ := ref.(*v1.Correction)
				return push(newPurgeForm(c, tenant, cr.GetId()), nil)
			}},
		}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// parseTarget reads a path of labels a person typed: subject, subject/session
// or subject/session/scan. The server judges the labels themselves.
func parseTarget(s, instance string) (*v1.CorrectionTarget, error) {
	parts := strings.Split(s, "/")
	if len(parts) > 3 {
		return nil, fault.New(fault.Invalid, "type subject, subject/session or subject/session/scan")
	}
	for _, p := range parts {
		if p == "" {
			return nil, fault.New(fault.Invalid, "type subject, subject/session or subject/session/scan")
		}
	}
	t := &v1.CorrectionTarget{SubjectLabel: parts[0], InstanceId: instance}
	if len(parts) > 1 {
		t.SessionLabel = parts[1]
	}
	if len(parts) > 2 {
		t.ScanLabel = parts[2]
	}
	if instance != "" && t.ScanLabel == "" {
		return nil, fault.New(fault.Invalid, "an instance belongs to a scan: type subject/session/scan")
	}
	return t, nil
}

// newDeleteForm asks what to delete and why. Its answer is the preview of the
// delete, which says what would be removed and lets the person confirm it.
func newDeleteForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Delete data in " + clean(tenant),
		intro: []string{
			"The next screen shows what the delete would remove. Nothing is deleted until you confirm it there.",
			"A delete hides its data at once and keeps its bytes, unserved, for the tenant's grace period; undo shows it again until then.",
		},
		fields: []formField{
			{label: "Target", hint: "subject[/session[/scan]]"},
			{label: "Instance", hint: "optional: one instance of the scan, by its id"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Override retention", choices: []string{"no", "yes"}},
			{label: "Override reason", hint: "only with the override: an installation admin may lift a retention rule"},
		},
		verb: "preview",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		t, err := parseTarget(f.value(0), f.value(1))
		if err != nil {
			return nil, err
		}
		if f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the data is deleted")
		}
		override := f.value(3) == "yes"
		if override && f.value(4) == "" {
			return nil, fault.New(fault.Invalid, "say why the retention rule is lifted")
		}
		req := &v1.DeleteRequest{Tenant: tenant, Target: t, Reason: f.value(2), OverrideRetention: override}
		if override {
			req.OverrideReason = f.value(4)
		}
		return previewDelete(c, f.id, req), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(deletePreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newDeletePreview(c, m, f.value(0)), nil, true
	}
	return f
}

// newDeletePreview shows what a delete would remove, with the key that goes
// on to the confirmation.
func newDeletePreview(c *ctx, m deletePreviewMsg, typed string) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Delete " + clean(targetWord(m.req.GetTarget())), ref: m, pairs: previewLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		if m.prev.GetConfirm() == "" {
			return nil
		}
		return push(newDeleteConfirm(c, m, typed), nil)
	}}}
	return d
}

// newDeleteConfirm asks for the target to be typed again, then deletes with
// the preview's token.
func newDeleteConfirm(c *ctx, m deletePreviewMsg, typed string) screen {
	n := m.prev.GetCounts().GetInstances()
	f := formScreen{
		base:    base{c.newID()},
		heading: "Delete " + clean(targetWord(m.req.GetTarget())),
		intro: []string{
			"This hides " + itoa(n) + " instances from every read. Their bytes are purged from every storage when the grace period ends.",
			"If anything changed since the preview, the server refuses and you preview again.",
		},
		fields:   []formField{{label: "Type the target", hint: typed}},
		want:     typed,
		confirms: true,
		verb:     "delete",
		pop:      2,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return deleteData(c, f.id, m.req, m.prev.GetConfirm()), nil
	}
	return f
}

// newRelabelForm asks which subject, session or scan gets which new label, and
// for the target to be typed again.
func newRelabelForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Relabel data in " + clean(tenant),
		intro: []string{
			"The data is copied under the new label, then switched; nothing is lost, and undo stops a relabel still copying.",
			"A hold on the data follows it to the new label.",
		},
		fields: []formField{
			{label: "Target", hint: "subject[/session[/scan]]"},
			{label: "New label", hint: "the label that replaces the last part of the target"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Type the target", hint: "again, exactly, to go ahead"},
		},
		verb: "relabel",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		t, err := parseTarget(f.value(0), "")
		if err != nil {
			return nil, err
		}
		if f.value(3) != f.value(0) {
			return nil, fault.New(fault.Invalid, "not done: type "+f.value(0)+" again, exactly, to go ahead")
		}
		if f.value(1) == "" || f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "give the new label and say why")
		}
		return relabelData(c, f.id, tenant, t, f.value(1), f.value(2)), nil
	}
	return f
}

// newReplaceForm asks which quarantined file replaces its archived instance.
func newReplaceForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Replace an instance in " + clean(tenant),
		intro: []string{
			"A held quarantined file is de-identified as its receiver says, then put in place of the archived instance with its SOP Instance UID.",
			"The old revision stays, unserved, until the correction is purged; the way back is another replace from the old file.",
		},
		fields: []formField{
			{label: "Quarantined file", hint: "its id, from the quarantine list"},
			{label: "Reason", hint: "why, kept with the correction"},
			{label: "Type the id", hint: "again, exactly, to go ahead"},
		},
		verb: "replace",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(0) == "" || f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "name the file and say why")
		}
		if f.value(2) != f.value(0) {
			return nil, fault.New(fault.Invalid, "not done: type "+f.value(0)+" again, exactly, to go ahead")
		}
		return replaceInstance(c, f.id, tenant, f.value(0), f.value(1)), nil
	}
	return f
}

// newUndoForm asks for the correction's id typed and a reason.
func newUndoForm(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Undo correction " + clean(id),
		intro: []string{
			"A committed delete shows its data again as it was; a relabel still copying stops and its copies are removed.",
			"A delete whose purge has started cannot be undone.",
		},
		fields:   []formField{{label: "Type the id", hint: id}, {label: "Reason", hint: "why, kept with the correction"}},
		want:     id,
		confirms: true,
		verb:     "undo",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "say why it is undone")
		}
		return undoCorrection(c, f.id, tenant, id, f.value(1)), nil
	}
	return f
}

// newPurgeForm asks for the correction's id typed before its bytes are
// removed for good.
func newPurgeForm(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Purge correction " + clean(id),
		intro: []string{
			"This removes for good the bytes the correction hid or superseded, from every storage, before its grace period ends.",
			"Once a purge has started nothing is undone.",
		},
		fields:   []formField{{label: "Type the id", hint: id}},
		want:     id,
		confirms: true,
		verb:     "purge",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return purgeCorrection(c, f.id, tenant, id), nil }
	return f
}
