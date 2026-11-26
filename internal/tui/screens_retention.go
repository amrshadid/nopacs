package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newRuleList makes the list of a tenant's retention rules. Its keys add,
// change and remove a rule, list the sessions due, dispose of them and show
// the tenant's retention state and clock; each change asks first.
func newRuleList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Retention rules of "+clean(tenant), []column{{"NAME", 8, 40}, {"MATCHES", 8, 40}, {"KEEP", 4, 10}, {"DUE AFTER", 5, 12}, {"ACTION", 6, 8}, {"DUE NOW", 4, 8}},
		"no rule yet; a: add one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return rulesPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rule, _ := r.ref.(*v1.RetentionRule)
		d := detailScreen{base: base{c.newID()}, heading: "Rule " + clean(rule.GetName()), ref: rule, pairs: rulePairs}
		return push(d, nil)
	}
	l.actions = []action{
		{key: "u", help: "due", do: func(c *ctx, _ screen, ref any) tea.Cmd {
			rule := ""
			if r, ok := ref.(*v1.RetentionRule); ok {
				rule = r.GetName()
			}
			s, cmd := newDueList(c, tenant, rule)
			return push(s, cmd)
		}},
		{key: "t", help: "state", do: func(c *ctx, _ screen, _ any) tea.Cmd {
			s, cmd := newRetentionState(c, tenant)
			return push(s, cmd)
		}},
	}
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = append(l.actions,
			action{key: "a", help: "add", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newRuleForm(c, tenant, nil), nil) }},
			action{key: "e", help: "change", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rule, _ := ref.(*v1.RetentionRule)
				return push(newRuleForm(c, tenant, rule), nil)
			}},
			action{key: "d", help: "remove", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rule, _ := ref.(*v1.RetentionRule)
				return push(newRemoveRule(c, tenant, rule.GetName()), nil)
			}},
			action{key: "x", help: "dispose", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				rule, _ := ref.(*v1.RetentionRule)
				return push(newDisposeReason(c, tenant, rule.GetName()), nil)
			}})
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// rulePairs are the lines of a rule.
func rulePairs(c *ctx, ref any) []pair {
	r, _ := ref.(*v1.RetentionRule)
	ps := []pair{
		{"Name", r.GetName()}, {"Matches", output.RetentionSelector(r.GetSelector())},
		{"Keep at least", output.RetentionPeriod(r.GetKeepAtLeast())}, {"Due after", output.RetentionPeriod(r.GetDisposeAfter())},
		{"Action", output.RetentionAction(r.GetAction())}, {"Sessions due now", itoa(r.GetSessionsDue())},
		{"Created", output.Time(r.GetCreateTime(), c.loc)}, {"Changed", output.Time(r.GetUpdateTime(), c.loc)},
	}
	if r.GetNoticeUntil() != nil {
		ps = append(ps, pair{"Change held back until", output.Time(r.GetNoticeUntil(), c.loc)})
	}
	return ps
}

// periodField reads a period typed in a form: empty or "none" for none.
func periodField(label, s string) (*v1.RetentionPeriod, error) {
	if s == "" || strings.EqualFold(s, "none") {
		return nil, nil
	}
	p, err := output.ParseRetentionPeriod(s)
	if err != nil {
		return nil, fault.New(fault.Invalid, label+" is not a period: "+err.Error())
	}
	return p, nil
}

// newRuleForm adds a rule, or with rule given changes it. A change names the
// whole rule as the form shows it, so what is on the screen is what is kept.
func newRuleForm(c *ctx, tenant string, rule *v1.RetentionRule) screen {
	action := 0
	if rule.GetAction() == v1.RetentionAction_RETENTION_ACTION_DELETE {
		action = 1
	}
	text := func(label, hint, value string) formField {
		return formField{label: label, hint: hint, in: field{}.set(value)}
	}
	period := func(p *v1.RetentionPeriod) string {
		if s := output.RetentionPeriod(p); s != "-" {
			return strings.ReplaceAll(s, " ", "")
		}
		return ""
	}
	heading, verb := "Add a retention rule", "add"
	if rule != nil {
		heading, verb = "Change rule "+clean(rule.GetName()), "change"
	}
	f := formScreen{
		base:    base{c.newID()},
		heading: heading,
		intro: []string{
			"A period is whole years and days from the session's start: 7y, 90d or 2y90d; empty or none for no period.",
			"What a change makes due sooner is held back for the tenant's notice; what it keeps longer applies at once.",
		},
		fields: []formField{
			text("Name", "1 to 63 lowercase letters, digits and hyphens", rule.GetName()),
			text("Receiver", "empty for every session", rule.GetSelector().GetReceiver()),
			text("Route", "a route of that receiver", rule.GetSelector().GetRoute()),
			text("Storage", "a primary storage, instead of a receiver", rule.GetSelector().GetStorage()),
			text("Keep at least", "until then a delete is refused", period(rule.GetKeepAtLeast())),
			text("Due after", "not shorter than the keep period", period(rule.GetDisposeAfter())),
			{label: "Action", choices: []string{"review", "delete"}, sel: action},
		},
		verb: verb,
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		keep, err := periodField("Keep at least", f.value(4))
		if err != nil {
			return nil, err
		}
		due, err := periodField("Due after", f.value(5))
		if err != nil {
			return nil, err
		}
		act := v1.RetentionAction_RETENTION_ACTION_REVIEW
		if f.value(6) == "delete" {
			act = v1.RetentionAction_RETENTION_ACTION_DELETE
		}
		var sel *v1.RetentionSelector
		if f.value(1) != "" || f.value(2) != "" || f.value(3) != "" {
			sel = &v1.RetentionSelector{Receiver: f.value(1), Route: f.value(2), Storage: f.value(3)}
		}
		r := &v1.RetentionRule{Name: f.value(0), Selector: sel, KeepAtLeast: keep, DisposeAfter: due, Action: act}
		if r.GetName() == "" {
			return nil, fault.New(fault.Invalid, "name the rule")
		}
		if rule != nil {
			r.Name = rule.GetName()
			return setRule(c, f.id, tenant, r, []string{"selector", "keep_at_least", "dispose_after", "action"}), nil
		}
		return addRule(c, f.id, tenant, r), nil
	}
	return f
}

// newRemoveRule asks for the rule's name to be typed before it is removed.
func newRemoveRule(c *ctx, tenant, name string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Remove rule " + clean(name),
		intro: []string{
			"The rule is removed at once. What it kept stays kept until the tenant's notice ends, so a removal never makes data due in a moment.",
			"Corrections and jobs the rule made keep its name.",
		},
		fields:   []formField{{label: "Type the name", hint: name}},
		want:     name,
		confirms: true,
		verb:     "remove",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return removeRule(c, f.id, tenant, name), nil }
	return f
}

// newDueList lists the sessions due now, held ones marked; rule limits it to
// one rule when given. x disposes of a rule's due sessions.
func newDueList(c *ctx, tenant, rule string) (screen, tea.Cmd) {
	heading := "Sessions due in " + clean(tenant)
	if rule != "" {
		heading += " under " + clean(rule)
	}
	l := newList(c, heading, []column{{"SESSION", 8, 50}, {"RULE", 6, 40}, {"ACTION", 6, 8}, {"DUE", 8, 10}, {"INSTANCES", 9, 10}, {"SIZE", 6, 10}, {"NOTE", 4, 20}},
		"no session is due")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return duePage(c, l.id, l.gen, token, tenant, rule) }
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = []action{{key: "x", help: "dispose", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			d, _ := ref.(*v1.DueSession)
			return push(newDisposeReason(c, tenant, d.GetRule()), nil)
		}}}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newDisposeReason is the first step of a disposal: why. The server's preview
// needs the reason as the disposal does, and shows what would be deleted.
func newDisposeReason(c *ctx, tenant, rule string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Dispose of what is due under " + clean(rule),
		intro: []string{
			"The next screen lists the sessions that would be deleted, the oldest due first and none held, and what that deletes in all.",
			"Nothing is deleted until you confirm it there. Each delete carries your reason and can be undone in its grace period.",
		},
		fields: []formField{{label: "Reason", hint: "why, kept with every delete"}},
		verb:   "preview",
		pop:    1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(0) == "" {
			return nil, fault.New(fault.Invalid, "say why the sessions are disposed of")
		}
		return previewDispose(c, f.id, tenant, rule, f.value(0)), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(disposePreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newDisposePreview(c, tenant, m), nil, true
	}
	return f
}

// disposePreviewScreen shows what a disposal would delete.
type disposePreviewScreen struct {
	detailScreen
}

// newDisposePreview shows a preview: the counts, the sessions listed and the
// token; c goes on to the confirmation.
func newDisposePreview(c *ctx, tenant string, m disposePreviewMsg) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Dispose under " + clean(m.rule), ref: m, pairs: disposePairs}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, ref any) tea.Cmd {
		p, _ := ref.(disposePreviewMsg)
		if p.res.GetConfirm() == "" || len(p.res.GetSessions()) == 0 {
			return nil
		}
		return push(newDisposeConfirm(c, tenant, p), nil)
	}}}
	return disposePreviewScreen{detailScreen: d}
}

// update is the detail screen's.
func (s disposePreviewScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	next, cmd := s.detailScreen.update(c, msg)
	if d, ok := next.(detailScreen); ok {
		s.detailScreen = d
		return s, cmd
	}
	return next, cmd
}

// disposePairs are the lines of a disposal's preview.
func disposePairs(c *ctx, ref any) []pair {
	m, _ := ref.(disposePreviewMsg)
	p := m.res
	n := p.GetCounts()
	ps := []pair{
		{"Rule", p.GetRule()},
		{"Sessions", itoa(int64(len(p.GetSessions()))) + " listed, " + itoa(p.GetSessionsNotListed()) + " not listed, " + itoa(p.GetSessionsHeld()) + " held and skipped"},
		{"Deletes", itoa(n.GetInstances()) + " instances, " + output.Bytes(n.GetBytes())},
		{"Confirm token", p.GetConfirm()},
	}
	for i, s := range p.GetSessions() {
		label := ""
		if i == 0 {
			label = "Sessions listed"
		}
		ps = append(ps, pair{label, s.GetSubjectLabel() + "/" + s.GetSessionLabel() + ", due " + listDate(s.GetDueTime(), c.loc, "-")})
	}
	return ps
}

// newDisposeConfirm asks for the rule's name to be typed, then queues the
// deletion of exactly the sessions the preview listed.
func newDisposeConfirm(c *ctx, tenant string, m disposePreviewMsg) screen {
	n := int64(len(m.res.GetSessions()))
	f := formScreen{
		base:    base{c.newID()},
		heading: "Delete " + itoa(n) + " sessions under " + clean(m.rule),
		intro: []string{
			"This queues a job that deletes exactly the sessions the preview listed, each as an ordinary delete with your reason.",
			"Each can be undone during its grace period; after it the bytes are purged from every copy.",
			"If data fell due or a hold was placed since the preview, the server refuses, and you preview again.",
		},
		fields:   []formField{{label: "Type the rule name", hint: m.rule}},
		want:     m.rule,
		confirms: true,
		verb:     "delete",
		pop:      2,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return dispose(c, f.id, tenant, m.rule, m.reason, m.res.GetConfirm()), nil
	}
	return f
}

// newRetentionState shows a tenant's retention state: whether it is paused
// for a clock jump, and the key that confirms the time.
func newRetentionState(c *ctx, tenant string) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Retention of " + clean(tenant), ref: &v1.RetentionStatus{}, loading: true}
	d.pairs = func(c *ctx, ref any) []pair {
		rs, _ := ref.(*v1.RetentionStatus)
		paused := "no"
		if rs.GetPaused() {
			paused = "yes: " + rs.GetPauseReason() + "; nothing becomes due until an admin confirms the time (c)"
		}
		step := "-"
		if rs.GetClockStep() != nil {
			step = output.Span(rs.GetClockStep().AsDuration())
		}
		return []pair{
			{"Rules", itoa(rs.GetRules())},
			{"Sessions due", itoa(rs.GetSessionsDue()) + " (" + itoa(rs.GetSessionsDueReview()) + " for review, " + itoa(rs.GetSessionsDueHeld()) + " held)"},
			{"Next due", output.Time(rs.GetNextDueTime(), c.loc)}, {"Last daily pass", output.Time(rs.GetLastPassTime(), c.loc)},
			{"Paused", paused}, {"Clock step seen", step},
			{"Active holds", itoa(rs.GetActiveHolds())}, {"Hold records owed", itoa(rs.GetHoldRecordsOwed())},
		}
	}
	d.refetch = func(c *ctx, id int, _ any) tea.Cmd { return getRetention(c, id, tenant) }
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		d.actions = []action{{key: "c", help: "confirm clock", do: func(c *ctx, _ screen, _ any) tea.Cmd {
			return push(newConfirmClock(c, tenant), nil)
		}}}
	}
	return d, getRetention(c, d.id, tenant)
}

// newConfirmClock asks for the time the person read from a clock they trust,
// typed in, never filled from the server, and a reason.
func newConfirmClock(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Confirm the time for " + clean(tenant),
		intro: []string{
			"Retention paused because the server's clock jumped. Read the time from a clock you trust and type it with its offset.",
			"The server takes its own clock as right from then, and refuses a time more than five minutes from its own.",
		},
		fields: []formField{{label: "The time you read", hint: "2026-11-26T09:30:00+03:00"}, {label: "Reason", hint: "why that clock is right"}},
		verb:   "confirm",
		pop:    1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		t, err := time.Parse(time.RFC3339, f.value(0))
		if err != nil {
			return nil, fault.New(fault.Invalid, "type a time with its offset, such as 2026-11-26T09:30:00+03:00")
		}
		if f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "say why the time is right")
		}
		return confirmClock(c, f.id, tenant, t, f.value(1)), nil
	}
	return f
}

// newHoldList makes the list of a tenant's holds, newest first. p places a
// hold and r releases the one under the cursor, each after asking; a server
// that does not serve either yet says so.
func newHoldList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Holds of "+clean(tenant), []column{{"ID", 8, 26}, {"COVERS", 8, 50}, {"STATE", 5, 8}, {"PLACED", 8, 10}, {"REASON", 8, 60}},
		"no hold; p: place one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return holdsPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		h, _ := r.ref.(*v1.Hold)
		d := detailScreen{base: base{c.newID()}, heading: "Hold " + clean(h.GetId()), ref: h, pairs: holdPairs}
		return push(d, nil)
	}
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = []action{
			{key: "p", help: "place", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newPlaceHold(c, tenant), nil) }},
			{key: "r", help: "release", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				h, _ := ref.(*v1.Hold)
				return push(newReleaseHold(c, tenant, h.GetId()), nil)
			}},
		}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// holdPairs are the lines of a hold.
func holdPairs(c *ctx, ref any) []pair {
	h, _ := ref.(*v1.Hold)
	ps := []pair{
		{"Id", h.GetId()}, {"Covers", output.HoldTarget(h.GetTarget())}, {"State", output.HoldState(h.GetState())},
		{"Reason", output.Dash(h.GetReason())}, {"Placed by", output.Dash(h.GetPlacedByKeyId())}, {"Placed", output.Time(h.GetPlaceTime(), c.loc)},
	}
	if h.GetState() == v1.HoldState_HOLD_STATE_RELEASED {
		ps = append(ps, pair{"Released by", output.Dash(h.GetReleasedByKeyId())}, pair{"Released", output.Time(h.GetReleaseTime(), c.loc)},
			pair{"Release reason", output.Dash(h.GetReleaseReason())})
	}
	return append(ps, pair{"Record on", output.Dash(strings.Join(h.GetStorages(), ", "))}, pair{"Record owed to", output.Dash(strings.Join(h.GetStoragesPending(), ", "))})
}

// newPlaceHold asks what to hold and why.
func newPlaceHold(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Place a hold in " + clean(tenant),
		intro: []string{
			"A hold stops a delete, a purge and a retention disposal of what it covers, now and for what arrives under it later, until it is released.",
			"Name a subject, a session (subject/session) or a scan (subject/session/scan), or choose the whole tenant.",
		},
		fields: []formField{
			{label: "Covers", choices: []string{"labels", "whole tenant"}},
			{label: "Labels", hint: "subject[/session[/scan]]"},
			{label: "Reason", hint: "why, kept with the hold"},
		},
		verb: "place",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the hold is placed")
		}
		t := &v1.HoldTarget{}
		if f.value(0) == "whole tenant" {
			t.WholeTenant = true
		} else {
			parts := strings.Split(f.value(1), "/")
			for _, p := range parts {
				if p == "" || len(parts) > 3 {
					return nil, fault.New(fault.Invalid, "type subject, subject/session or subject/session/scan")
				}
			}
			t.SubjectLabel = parts[0]
			if len(parts) > 1 {
				t.SessionLabel = parts[1]
			}
			if len(parts) > 2 {
				t.ScanLabel = parts[2]
			}
		}
		return placeHold(c, f.id, tenant, t, f.value(2)), nil
	}
	return f
}

// newReleaseHold asks for the hold's id to be typed and a reason: releasing
// lets what it held be deleted and purged again.
func newReleaseHold(c *ctx, tenant, id string) screen {
	f := formScreen{
		base:     base{c.newID()},
		heading:  "Release hold " + clean(id),
		intro:    []string{"What the hold covers may then be deleted, purged and disposed of again, as other holds allow."},
		fields:   []formField{{label: "Type the id", hint: id}, {label: "Reason", hint: "why, kept in the audit log"}},
		want:     id,
		confirms: true,
		verb:     "release",
		pop:      1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(1) == "" {
			return nil, fault.New(fault.Invalid, "say why the hold is released")
		}
		return releaseHold(c, f.id, tenant, id, f.value(1)), nil
	}
	return f
}
