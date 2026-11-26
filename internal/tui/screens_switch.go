package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// switchTokenChars is how many characters of the token a person types to
// confirm a switch, when the token is at least that long.
const switchTokenChars = 8

// switchPreviewScreen is the first step of a switch: what the server says the
// switch would do now, what stands in its way and the token the switch must
// carry. Nothing has changed when it is shown.
type switchPreviewScreen struct {
	detailScreen
	tenant, primary, replica string
}

// newSwitchPreview asks the server whether the switch could start and shows
// the answer; c goes on to the confirmation.
func newSwitchPreview(c *ctx, tenant, primary, replica string) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Switch " + clean(primary) + " to " + clean(replica), ref: &v1.SwitchStorageResponse{}, loading: true}
	d.pairs = switchPairs
	d.refetch = func(c *ctx, id int, _ any) tea.Cmd { return previewSwitch(c, id, tenant, primary, replica) }
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, self screen, ref any) tea.Cmd {
		p, _ := ref.(*v1.SwitchStorageResponse)
		if p.GetConfirm() == "" {
			return nil
		}
		return push(newSwitchConfirm(c, tenant, primary, replica, p), nil)
	}}}
	s := switchPreviewScreen{detailScreen: d, tenant: tenant, primary: primary, replica: replica}
	return s, previewSwitch(c, s.id, tenant, primary, replica)
}

// update applies the preview the server answered; the rest is the detail
// screen's.
func (s switchPreviewScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if m, ok := msg.(switchPreviewMsg); ok {
		s.ref, s.loading = m.res, false
		return s, nil
	}
	next, cmd := s.detailScreen.update(c, msg)
	if d, ok := next.(detailScreen); ok {
		s.detailScreen = d
		return s, cmd
	}
	return next, cmd
}

// switchPairs are the lines of a switch's preview.
func switchPairs(c *ctx, ref any) []pair {
	p, _ := ref.(*v1.SwitchStorageResponse)
	if p.GetConfirm() == "" && len(p.GetBlockers()) == 0 {
		return []pair{{"Switch", "asking the server what the switch would do"}}
	}
	ready := "no: something stands in its way"
	if p.GetReady() {
		ready = "yes"
	}
	ps := []pair{{"Ready", ready}}
	for i, b := range p.GetBlockers() {
		label := ""
		if i == 0 {
			label = "Blocked by"
		}
		ps = append(ps, pair{label, b})
	}
	return append(ps,
		pair{"Scans not in step", itoa(p.GetScansPending())},
		pair{"Writes pause for", output.Dash(strings.Join(p.GetReceivers(), ", ")) + " (each sender tries again)"},
		pair{"Generation", itoa(p.GetSwitchGeneration())},
		pair{"Confirm token", p.GetConfirm()})
}

// newSwitchConfirm asks for the confirmation of a switch: the first characters
// of the token the preview showed, or the primary's name, typed. A switch that
// something blocks goes ahead only when its checks are passed over with a
// reason, which the audit log keeps.
func newSwitchConfirm(c *ctx, tenant, primary, replica string, p *v1.SwitchStorageResponse) screen {
	token := p.GetConfirm()
	n := min(switchTokenChars, len(token))
	f := formScreen{
		base:    base{c.newID()},
		heading: "Switch " + clean(primary) + " to " + clean(replica),
		intro: []string{
			"Every write to " + primary + " pauses while the copy drains and the two storages swap places; senders are told to try again, reads go on.",
			"If the copy does not drain in time the switch is refused and every write resumes on the old place.",
			"Once accepted it runs to its end whatever becomes of this screen.",
		},
		fields: []formField{
			{label: "Type to confirm", hint: itoa(int64(n)) + " of the token: " + token[:n] + " or: " + primary},
			{label: "Pass over checks", choices: []string{"no", "yes"}},
			{label: "Reason", hint: "only with the checks passed over: why, kept in the audit log"},
		},
		verb: "switch",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		typed := f.fields[0].in.value()
		prefix := len(typed) >= n && n > 0 && strings.HasPrefix(token, typed)
		if typed != primary && !prefix {
			return nil, fault.New(fault.Invalid, "not done: type the first "+itoa(int64(n))+" characters of the token, or "+primary+", exactly, to go ahead")
		}
		skip := f.value(1) == "yes"
		if skip && f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the checks are passed over")
		}
		if !skip && len(p.GetBlockers()) > 0 {
			return nil, fault.New(fault.Invalid, "the preview shows blockers: pass over the checks with a reason, or fix them first")
		}
		reason := ""
		if skip {
			reason = f.value(2)
		}
		ps := newSwitchProgress(c, tenant, primary, replica, token, skip, reason)
		return func() tea.Msg { return replaceMsg{s: ps, cmd: ps.start(c)} }, nil
	}
	return f
}

// switchProgress is the screen of a switch under way and after it: the call
// that makes the switch, and beside it the tenant's status, which shows the
// storage switching and how many scans the replica still waits for. A call
// that the deadline or esc ended stops nothing in the server; the status says
// when the switch is over.
type switchProgress struct {
	base
	tenant, primary, replica, token string
	skip                            bool
	reason                          string
	// state is "switching", "done", "refused" or "unknown".
	state  string
	res    *v1.SwitchStorageResponse
	why    string
	status *v1.TenantStatus
}

// newSwitchProgress makes the screen of a switch about to start.
func newSwitchProgress(c *ctx, tenant, primary, replica, token string, skip bool, reason string) switchProgress {
	return switchProgress{base: base{c.newID()}, tenant: tenant, primary: primary, replica: replica, token: token, skip: skip, reason: reason, state: "switching"}
}

// start makes the switch and reads the status once.
func (s switchProgress) start(c *ctx) tea.Cmd {
	cmds := []tea.Cmd{switchStorage(c, s.id, s.tenant, s.primary, s.replica, s.token, s.skip, s.reason), getStatus(c, s.id, s.tenant)}
	if c.refresh > 0 {
		cmds = append(cmds, tick(c, s.id))
	}
	return tea.Batch(cmds...)
}

// update applies a message.
func (s switchProgress) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	n := s
	switch msg := msg.(type) {
	case switchDoneMsg:
		n.state, n.res = "done", msg.res
		return n, getStatus(c, n.id, n.tenant)
	case failMsg:
		n.why = fault.Message(msg.err)
		// A call the client's own deadline ended may have left the switch
		// running: say that, rather than that it failed. An answer the server
		// sent is final even when it is "unavailable" (the drain ran out, another
		// switch runs): that is a refusal, with its reason.
		if client.NoAnswer(msg.err) && n.state == "switching" {
			n.state = "unknown"
		} else if n.state == "switching" {
			n.state = "refused"
		}
		return n, nil
	case canceledMsg:
		if n.state == "switching" {
			n.state, n.why = "unknown", "the wait was canceled"
		}
		return n, nil
	case detailMsg:
		if st, ok := msg.ref.(*v1.TenantStatus); ok {
			n.status = st
		}
		return n, nil
	case tickMsg:
		if n.state != "switching" && n.state != "unknown" || c.refresh <= 0 {
			return n, nil
		}
		return n, tea.Batch(getStatus(c, n.id, n.tenant), tick(c, n.id))
	case tea.KeyPressMsg:
		switch msg.String() {
		case "r":
			return n, getStatus(c, n.id, n.tenant)
		case "esc":
			return n, back(true, "")
		}
	}
	return n, nil
}

// view draws the state of the switch.
func (s switchProgress) view(c *ctx) []string {
	lines := []string{""}
	add := func(label, value string) {
		lines = append(lines, trunc("  "+c.st.bold(padRight(label+":", 20))+" "+clean(value), c.w))
	}
	add("Switch", s.replica+" becomes the primary "+s.primary)
	switch s.state {
	case "done":
		add("Result", "made")
		add("Switch time", output.Time(s.res.GetSwitchTime(), c.loc))
		pause := "-"
		if s.res.GetPause() != nil {
			pause = s.res.GetPause().AsDuration().Round(time.Millisecond).String()
		}
		add("Writes paused for", pause)
		add("Scans drained", itoa(s.res.GetScansPending()))
		add("Associations refused", itoa(s.res.GetAssociationsRefused())+" (each is tried again by its sender)")
		add("Generation", itoa(s.res.GetSwitchGeneration()))
	case "refused":
		add("Result", "refused: every write resumes on the old place")
		add("Why", s.why)
	case "unknown":
		add("Result", "not known: the call ended, and the switch goes on in the server")
		add("Why", s.why)
		add("Next", "r reads the status again")
	default:
		add("Result", "under way: writes to "+s.primary+" are paused, senders try again")
	}
	if s.status != nil {
		for _, st := range s.status.GetStorages() {
			if st.GetStorage() == s.primary {
				add("Switching now", yesNoWord(st.GetSwitching()))
				add("Last switch", output.Time(st.GetLastSwitchTime(), c.loc))
				add("Generation seen", itoa(st.GetSwitchGeneration()))
			}
		}
		for _, r := range s.status.GetReplicas() {
			if r.GetReplica() == s.replica {
				add("Replica waits for", itoa(r.GetScansPending())+" scans")
			}
		}
	}
	return lines
}

// title is the heading.
func (s switchProgress) title(*ctx) string {
	return "Switching " + clean(s.primary) + " to " + clean(s.replica)
}

// editing is false: nothing is typed here.
func (switchProgress) editing() bool { return false }

// keys lists the keys of the screen.
func (switchProgress) keys() []keyHelp { return []keyHelp{{"r", "read status"}, {"esc", "back"}} }
