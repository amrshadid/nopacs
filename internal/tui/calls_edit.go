package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/fault"
)

// This file holds the calls of the header edit screens.

// editPreviewMsg is the answer of an edit's preview: what it would change and
// the token the edit must carry, with the request it answers, which the
// confirmation sends again.
type editPreviewMsg struct {
	prev *v1.EditPreview
	req  *v1.EditRequest
}

// editDoneMsg is the answer of an edit that was made.
type editDoneMsg struct{ cor *v1.Correction }

// editChanges reads what a person typed as an edit's changes: set is Tag=Value
// pairs and remove is tags, each list separated by semicolons, each set first.
// A value holds no semicolon: the command line takes one in a repeated flag.
func editChanges(set, remove string) ([]*v1.EditChange, error) {
	var out []*v1.EditChange
	for _, s := range strings.Split(set, ";") {
		if strings.TrimSpace(s) == "" {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not Tag=Value", clean(strings.TrimSpace(s))))
		}
		out = append(out, &v1.EditChange{Attribute: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	for _, k := range strings.Split(remove, ";") {
		if strings.TrimSpace(k) != "" {
			out = append(out, &v1.EditChange{Attribute: strings.TrimSpace(k), Remove: true})
		}
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Invalid, "name what to change: Tag=Value to set, or a Tag to remove")
	}
	return out, nil
}

// previewEdit asks what an edit would change and for the token it must carry;
// nothing is changed. Values are asked for only when req says so.
func previewEdit(c *ctx, owner int, req *v1.EditRequest) tea.Cmd {
	cl := c.client
	preview := &v1.EditRequest{Tenant: req.GetTenant(), Target: req.GetTarget(), Changes: req.GetChanges(), Reason: req.GetReason(),
		ValidateOnly: true, ShowValues: req.GetShowValues()}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction edit", func(ctx context.Context) (*v1.EditResponse, error) {
			return cl.Corrections.Edit(ctx, preview)
		})
		return answer(res, err, func(r *v1.EditResponse) tea.Msg { return editPreviewMsg{prev: r.GetPreview(), req: req} })
	})
}

// editData makes the edit a preview was given the token for.
func editData(c *ctx, owner int, req *v1.EditRequest, token string) tea.Cmd {
	cl := c.client
	real := &v1.EditRequest{Tenant: req.GetTenant(), Target: req.GetTarget(), Changes: req.GetChanges(), Reason: req.GetReason(), Confirm: token}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction edit", func(ctx context.Context) (*v1.EditResponse, error) {
			return cl.Corrections.Edit(ctx, real)
		})
		return answer(res, err, func(r *v1.EditResponse) tea.Msg { return editDoneMsg{cor: r.GetCorrection()} })
	})
}

// editAttributeText words what one change does to one instance: the value now
// only when values were asked for, never otherwise.
func editAttributeText(a *v1.EditPreviewAttribute, show bool) string {
	now := "absent"
	switch {
	case !a.GetKnown():
		now = "not known"
	case a.GetPresent() && show:
		now = strconv.Quote(clean(a.GetOldValue()))
	case a.GetPresent():
		now = "present"
	}
	if a.GetRemove() {
		return now + " -> removed"
	}
	return now + " -> " + strconv.Quote(clean(a.GetNewValue()))
}

// maxPreviewInstances is how many instances of an edit's preview are drawn;
// the rest are counted.
const maxPreviewInstances = 200

// editPreviewLines are the labeled lines of an edit's preview.
func editPreviewLines(c *ctx, ref any) []pair {
	m, _ := ref.(editPreviewMsg)
	p := m.prev
	values := "hidden: only whether an attribute is present"
	if m.req.GetShowValues() {
		values = "shown, and recorded in the audit log"
	}
	ps := []pair{
		{"Target", targetWord(m.req.GetTarget())},
		{"Instances", strconv.Itoa(len(p.GetInstances()))},
		{"Storages", strings.Join(p.GetStorages(), ", ")},
		{"Values", values},
		{"Confirm token", p.GetConfirm()},
	}
	for i, in := range p.GetInstances() {
		if i == maxPreviewInstances {
			ps = append(ps, pair{"More", strconv.Itoa(len(p.GetInstances())-i) + " instances not drawn here"})
			break
		}
		where := in.GetSubjectLabel() + "/" + in.GetSessionLabel() + "/" + in.GetScanLabel() + " " + in.GetInstanceId()
		ps = append(ps, pair{where, "revision " + strconv.Itoa(int(in.GetRevision()))})
		for _, a := range in.GetAttributes() {
			ps = append(ps, pair{"  " + a.GetAttribute(), editAttributeText(a, m.req.GetShowValues())})
		}
	}
	return ps
}
