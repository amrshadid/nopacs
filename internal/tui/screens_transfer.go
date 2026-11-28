package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// transferAction is the key of a catalog list that opens the transfer form for
// the row under the cursor; ref says the subject, then the session when it
// applies.
func transferAction(tenant string, ref func(any) []string) action {
	return action{key: "t", help: "transfer", needsRow: true, plain: true, do: func(c *ctx, _ screen, r any) tea.Cmd {
		return push(newTransferForm(c, tenant, strings.Join(ref(r), "/")), nil)
	}}
}

// newTransferList makes the list of a tenant's transfers, newest first; a
// transfer opens on its steps and outcomes, and n starts one.
func newTransferList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Transfers of "+clean(tenant), []column{{"ID", 8, 26}, {"TRANSFERS", 8, 40}, {"MODE", 4, 4}, {"TO", 6, 40}, {"STATE", 5, 8},
		{"STEP", 4, 10}, {"CONFIRMED", 9, 14}, {"MADE", 8, 19}},
		"no transfer yet; n: copy or move a subject or a session into another tenant")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return transfersPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		t, _ := r.ref.(*v1.Transfer)
		return push(newTransferDetail(c, tenant, t), nil)
	}
	if c.acc.onTenantPlain(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = []action{{key: "n", help: "new", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newTransferForm(c, tenant, ""), nil) }}}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newTransferDetail makes the screen of one transfer: where it goes, the step
// it has reached and how many instances are confirmed, copied, not yet copied
// and failed; r reads it again.
func newTransferDetail(c *ctx, tenant string, t *v1.Transfer) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Transfer " + clean(t.GetJob().GetId()), ref: t, pairs: transferLines}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		t, _ := ref.(*v1.Transfer)
		return getTransfer(c, id, tenant, t.GetJob().GetId())
	}
	d.actions = []action{
		{key: "r", help: "refresh", do: func(c *ctx, self screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Transfer)
			return getTransfer(c, self.sid(), tenant, t.GetJob().GetId())
		}},
		{key: "w", help: "job", need: v1.Role_ROLE_OPERATOR, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Transfer)
			s, cmd := newJobDetail(c, tenant, t.GetJob().GetId())
			return push(s, cmd)
		}},
	}
	d.actions = allowed(c, tenant, d.actions)
	return d
}

// newTransferForm asks which subject or session goes to which tenant, and
// whether it is copied or moved. Its answer is the preview of the transfer.
func newTransferForm(c *ctx, tenant, source string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Transfer data out of " + clean(tenant),
		intro: []string{
			"The archived bytes of a subject, or one session of it, are handed to a receiver of type job of another tenant, so its own routing, profile and checks apply.",
			"A copy leaves the source as it is. A move deletes the source, as an ordinary delete, once every instance is confirmed in the target.",
			"Needs admin on this tenant and writer on the target; never a masked key.",
		},
		fields: []formField{
			filled("Source", "subject or subject/session", source),
			{label: "To tenant", hint: "the tenant to transfer into"},
			{label: "Via receiver", hint: "optional: the target's receiver of type job; its only enabled one when empty"},
			{label: "Reason", hint: "why, kept with the job and with a move's delete"},
			{label: "Move", choices: []string{"no", "yes"}, hint: "yes: delete the source once every instance is confirmed in the target"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		parts := strings.Split(f.value(0), "/")
		if len(parts) > 2 {
			return nil, fault.New(fault.Invalid, "type a subject or subject/session")
		}
		for _, p := range parts {
			if _, err := labelOf(p, "the label"); err != nil {
				return nil, err
			}
		}
		if f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "name the tenant to transfer into")
		}
		if f.value(3) == "" {
			return nil, fault.New(fault.Invalid, "say why the data is transferred")
		}
		req := &v1.CreateTransferRequest{Tenant: tenant, SubjectLabel: parts[0], TargetTenant: f.value(1), Receiver: f.value(2), Reason: f.value(3), Move: f.value(4) == "yes"}
		if len(parts) > 1 {
			req.SessionLabel = parts[1]
		}
		return previewTransfer(c, f.id, req), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(transferPreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newTransferPreview(c, m), nil, true
	}
	return f
}

// newTransferPreview shows what a transfer would cover, with the key that goes
// on to the confirmation.
func newTransferPreview(c *ctx, m transferPreviewMsg) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Transfer " + clean(m.req.GetSubjectLabel()) + " to " + clean(m.req.GetTargetTenant()), ref: m, pairs: transferPreviewLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newTransferConfirm(c, m), nil) }}}
	return d
}

// newTransferConfirm starts the transfer: a copy on a word, a move, which
// deletes the source, on the source tenant's name typed.
func newTransferConfirm(c *ctx, m transferPreviewMsg) screen {
	tenant := m.req.GetTenant()
	f := formScreen{
		base:    base{c.newID()},
		heading: "Transfer " + clean(m.req.GetSubjectLabel()) + " to " + clean(m.req.GetTargetTenant()),
		pop:     2,
		verb:    "start",
		refusal: true,
	}
	if m.req.GetMove() {
		f.intro = []string{
			"This moves " + itoa(m.prev.GetInstances()) + " instances into " + clean(m.req.GetTargetTenant()) + " and deletes them from " + clean(tenant) + " once each is confirmed there.",
			"A hold or a retention rule that stops the delete ends the job partial, and the copy stays.",
		}
		f.fields, f.want, f.confirms = []formField{{label: "Type the tenant", hint: tenant}}, tenant, true
		f.verb = "move"
	} else {
		f.intro = []string{"This copies " + itoa(m.prev.GetInstances()) + " instances into " + clean(m.req.GetTargetTenant()) + ". The source is left as it is."}
		f.fields = []formField{{label: "Start the copy", choices: []string{"no", "yes"}}}
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if !m.req.GetMove() && f.value(0) != "yes" {
			return nil, fault.New(fault.Invalid, "choose yes to start the copy")
		}
		return startTransfer(c, f.id, m.req), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		started, ok := msg.(transferStartedMsg)
		if !ok {
			return nil, nil, false
		}
		note := "transfer " + started.t.GetJob().GetId() + " is " + transferWord(started.t.GetStep()) + ": " + itoa(m.prev.GetInstances()) + " instances to " + started.t.GetTargetTenant()
		return f, backThen(f.pop, note, push(newTransferDetail(c, tenant, started.t), nil)), true
	}
	return f
}
