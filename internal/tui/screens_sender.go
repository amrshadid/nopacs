package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
)

// allowIdentifiedText words a sender's mark: whether it may also send what was
// not de-identified.
func allowIdentifiedText(allowed bool) string {
	if allowed {
		return "yes: also sends instances archived as received, without their profile recorded, or with an accepted pixel risk"
	}
	return "no: sends only instances archived de-identified"
}

// newAllowIdentifiedForm asks to set or clear a sender's mark, with the sender's
// name typed. Setting it lets the sender send what was not de-identified, so it
// is an installation admin's to do, never a masked key's, and the server audits
// each change; a refusal shows on the form.
func newAllowIdentifiedForm(c *ctx, tenant string, s *v1.Sender) screen {
	name := s.GetName()
	current := 0
	if s.GetAllowIdentified() {
		current = 1
	}
	f := formScreen{
		base:    base{c.newID()},
		heading: "Allow identified on sender " + clean(name),
		intro: []string{
			"A sender with this mark also sends instances archived as received, without their profile version recorded whole, or with an accepted pixel risk: data that still identifies a person.",
			"Only an admin on the installation sets or clears it, never a masked key; each change is audited and reported as an event.",
		},
		fields:   []formField{{label: "Type the name", hint: name}, {label: "Allow identified", choices: []string{"no", "yes"}, sel: current}},
		want:     name,
		confirms: true,
		verb:     "set",
		pop:      1,
		refusal:  true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return setAllowIdentified(c, f.id, tenant, name, f.value(1) == "yes"), nil
	}
	return f
}

// setAllowIdentified sets or clears a sender's allow_identified mark.
func setAllowIdentified(c *ctx, owner int, tenant, name string, allow bool) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "sender set", func(ctx context.Context) (*v1.SetSenderResponse, error) {
			return cl.Senders.SetSender(ctx, &v1.SetSenderRequest{Tenant: tenant, Sender: &v1.Sender{Name: name, AllowIdentified: allow},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"allow_identified"}}})
		})
		return answer(res, err, func(r *v1.SetSenderResponse) tea.Msg {
			if r.GetSender().GetAllowIdentified() {
				return actionDoneMsg{note: "sender " + name + " may now send instances that are not de-identified"}
			}
			return actionDoneMsg{note: "sender " + name + " sends only instances archived de-identified"}
		})
	})
}
