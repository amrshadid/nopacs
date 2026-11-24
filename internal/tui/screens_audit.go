package tui

import (
	"sort"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// outcomeChoices are the words of the audit filter's outcome; "any" asks for
// every outcome.
var outcomeChoices = []string{"any", "done", "refused", "failed"}

// outcomeFromChoice is the outcome a choice word stands for.
func outcomeFromChoice(word string) v1.AuditOutcome {
	switch word {
	case "done":
		return v1.AuditOutcome_AUDIT_OUTCOME_DONE
	case "refused":
		return v1.AuditOutcome_AUDIT_OUTCOME_REFUSED
	case "failed":
		return v1.AuditOutcome_AUDIT_OUTCOME_FAILED
	}
	return v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED
}

// auditHeading words the heading of an audit list with its filter.
func auditHeading(f auditFilter) string {
	h := "Audit log of " + scopeName(f.scope)
	if f.outcome != v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED {
		h += ", " + output.Outcome(f.outcome) + " only"
	}
	if f.keyID != "" {
		h += ", key " + clean(f.keyID)
	}
	return h
}

// newAuditList makes the audit log of a scope, newest first. f filters it; the
// filter key opens a form that sends the list a filterSetMsg.
func newAuditList(c *ctx, scope *v1.Scope) (screen, tea.Cmd) {
	f := auditFilter{scope: scope}
	l := newList(c, auditHeading(f), []column{{"TIME", 19, 19}, {"OUTCOME", 7, 7}, {"KEY", 6, 14}, {"METHOD", 10, 30}, {"OBJECT", 8, 30}}, "no entry")
	l.params = f
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return auditPage(c, l.id, l.gen, token, l.params.(auditFilter))
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		e, _ := r.ref.(*v1.AuditEntry)
		return push(newAuditDetail(c, e), nil)
	}
	l.onMsg = func(c *ctx, l listScreen, msg tea.Msg) (listScreen, tea.Cmd, bool) {
		set, ok := msg.(filterSetMsg)
		if !ok {
			return l, nil, false
		}
		f := l.params.(auditFilter)
		f.outcome, f.keyID = set.outcome, set.keyID
		l.params, l.heading = f, auditHeading(f)
		nl, cmd := l.start(c)
		return nl, cmd, true
	}
	l.actions = []action{{key: "f", help: "filter by outcome and key", do: func(c *ctx, self screen, _ any) tea.Cmd {
		cur, _ := self.(listScreen)
		return push(newAuditFilter(c, cur.id, cur.params.(auditFilter)), nil)
	}}}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newAuditFilter makes the form that asks which outcome and which key's entries
// to show, and tells the list with the given id.
func newAuditFilter(c *ctx, listID int, cur auditFilter) screen {
	sel := 0
	for i, w := range outcomeChoices {
		if outcomeFromChoice(w) == cur.outcome {
			sel = i
		}
	}
	f := formScreen{
		base:    base{c.newID()},
		heading: "Filter the audit log",
		fields: []formField{
			{label: "Outcome", choices: outcomeChoices, sel: sel},
			{label: "Key id", hint: "leave empty for every key", in: field{}.set(cur.keyID), keyID: true},
		},
		verb: "filter",
	}
	f.submit = func(_ *ctx, f formScreen) (tea.Cmd, error) {
		keyID := f.value(1)
		if keyID != "" {
			if err := output.CheckKeyID(keyID); err != nil {
				return nil, err
			}
		}
		set := filterSetMsg{outcome: outcomeFromChoice(f.value(0)), keyID: keyID}
		return tea.Batch(back(false, ""), func() tea.Msg { return deliverMsg{to: listID, msg: set} }), nil
	}
	return f
}

// newAuditDetail makes the screen of one entry, whole: its reason, its details
// and the request id, which a person reports.
func newAuditDetail(c *ctx, e *v1.AuditEntry) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Audit entry " + clean(e.GetId()), ref: e}
	d.pairs = func(c *ctx, ref any) []pair {
		e, _ := ref.(*v1.AuditEntry)
		ps := []pair{
			{"ID", e.GetId()},
			{"Time", output.Time(e.GetTime(), c.loc)},
			{"Caller", output.Who(e)},
			{"From", e.GetClientAddress()},
			{"Method", output.Method(e.GetMethod())},
			{"Tenant", e.GetTenant()},
			{"Object", e.GetObject()},
			{"Outcome", output.Outcome(e.GetOutcome())},
			{"Reason", e.GetReason()},
			{"Request id", e.GetRequestId()},
		}
		names := make([]string, 0, len(e.GetDetails()))
		for k := range e.GetDetails() {
			names = append(names, k)
		}
		sort.Strings(names)
		for i, k := range names {
			label := ""
			if i == 0 {
				label = "Details"
			}
			ps = append(ps, pair{label, k + " = " + e.GetDetails()[k]})
		}
		for i := range ps {
			if ps[i].value == "" {
				ps[i].value = "-"
			}
		}
		return ps
	}
	return d
}
