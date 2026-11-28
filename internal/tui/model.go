package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The size a screen is drawn at before the terminal has said its own, and the
// least size at which the screens can be drawn at all.
const (
	defaultWidth  = 80
	defaultHeight = 24
	minWidth      = 40
	minHeight     = 10
)

// chrome is the number of lines the root draws around a screen's body: the
// header, the screen's title, the status line and the key hints.
const chrome = 4

// keyHelp is one key of a screen and what it does, for the hint line and the
// help screen.
type keyHelp struct{ key, what string }

// screen is one screen of the program: a value, with an update function from
// the screen and a message to the new screen and a command, and a view function
// from the screen to the lines of its body. It never calls the API itself.
type screen interface {
	// sid is the screen's id, which its calls carry so that their answers go
	// to it and to nothing else.
	sid() int
	// title is the line under the header: what the screen is, and its count.
	title(c *ctx) string
	// update applies a message.
	update(c *ctx, msg tea.Msg) (screen, tea.Cmd)
	// view draws the body: at most c.bodyHeight() lines, each at most c.w
	// cells wide.
	view(c *ctx) []string
	// keys lists the screen's own keys for the hint line and for help.
	keys() []keyHelp
	// editing says that a text field has the keyboard, so letters are text and
	// not commands.
	editing() bool
}

// cursorer is a screen that wants the terminal's cursor shown at a position in
// its body, which a screen with a text field does.
type cursorer interface {
	cursorAt(c *ctx) (x, y int, ok bool)
}

// guard is a screen that asks before it may be left, and so handles the quit
// keys itself: the one that shows a new key.
type guard interface {
	guarded() bool
}

// base is what every screen holds: its id.
type base struct{ id int }

// sid returns the id.
func (b base) sid() int { return b.id }

// pendingCall is a call in flight: who asked for it and how to stop it.
type pendingCall struct {
	owner    int
	cancel   context.CancelFunc
	canceled bool
	// background marks a call the screen does not wait for, such as a
	// stream it follows: esc leaves the screen rather than stopping it,
	// the bottom line does not say the program is waiting, and leaving the
	// screen stops it without a note.
	background bool
}

// ctx is what the screens share: the client to make calls with, the size of
// the terminal, how things are worded and drawn, and the calls in flight. It
// is held by pointer so the screens, which are values, need not carry it, and
// it is only ever used from the program's one update loop; a command reads
// from it before it starts, never while it runs.
type ctx struct {
	base    context.Context
	client  *client.Client
	where   string
	st      styler
	now     func() time.Time
	loc     *time.Location
	refresh time.Duration
	// after makes the command that sends a screen a tickMsg when the refresh
	// interval has passed; nil means tea.Tick. A test replaces it.
	after func(to int) tea.Cmd

	w, h     int
	nextID   int
	nextCall int
	pending  map[int]*pendingCall
	topID    int
	// acc is what the caller may do, from who-am-I; nil until known.
	acc *access
	// marked holds the catalog rows marked for a batch delete.
	marked basket
}

// newID gives the next screen id.
func (c *ctx) newID() int {
	c.nextID++
	return c.nextID
}

// bodyHeight is the number of lines a screen may draw.
func (c *ctx) bodyHeight() int { return max(c.h-chrome, 1) }

// call makes the command for one API call owned by a screen. The function runs
// beside the interface, in a command, with a context that esc (or leaving the
// screen) cancels; client.Call gives it the same deadline as a command's. Its
// message comes back inside a doneMsg, and an error becomes a failMsg, which
// the root draws on the bottom line.
func (c *ctx) call(owner int, fn func(context.Context) tea.Msg) tea.Cmd {
	c.nextCall++
	id := c.nextCall
	cctx, cancel := context.WithCancel(c.base)
	c.pending[id] = &pendingCall{owner: owner, cancel: cancel}
	return func() tea.Msg {
		msg := fn(cctx)
		// Read before cancel, or every call would look canceled.
		wasCanceled := errors.Is(cctx.Err(), context.Canceled)
		cancel()
		if _, failed := msg.(failMsg); failed && wasCanceled {
			msg = canceledMsg{}
		}
		return doneMsg{call: id, to: owner, msg: msg}
	}
}

// follow is call for a call the screen does not wait for (pendingCall's
// background): a stream it follows for as long as it is open.
func (c *ctx) follow(owner int, fn func(context.Context) tea.Msg) tea.Cmd {
	cmd := c.call(owner, fn)
	c.pending[c.nextCall].background = true
	return cmd
}

// cancelOwner cancels every call of a screen, those it follows included, as
// it is left, and reports whether any was one it waited for.
func (c *ctx) cancelOwner(owner int) bool {
	found := false
	for _, p := range c.pending {
		if p.owner == owner && !p.canceled {
			p.canceled = true
			p.cancel()
			found = found || !p.background
		}
	}
	return found
}

// cancelWaiting cancels the calls a screen is waiting for, not those it
// follows, and reports whether there were any: what esc stops.
func (c *ctx) cancelWaiting(owner int) bool {
	found := false
	for _, p := range c.pending {
		if p.owner == owner && !p.canceled && !p.background {
			p.canceled = true
			p.cancel()
			found = true
		}
	}
	return found
}

// cancelAll stops every call, on the way out.
func (c *ctx) cancelAll() {
	for _, p := range c.pending {
		p.canceled = true
		p.cancel()
	}
}

// waiting says whether any call that has not been canceled is in flight.
func (c *ctx) waiting() bool {
	for _, p := range c.pending {
		if !p.canceled && !p.background {
			return true
		}
	}
	return false
}

// push is the command that opens a screen and runs its first command.
func push(s screen, cmd tea.Cmd) tea.Cmd {
	return func() tea.Msg { return pushMsg{s: s, cmd: cmd} }
}

// back is the command that leaves the current screen.
func back(reload bool, note string) tea.Cmd {
	return func() tea.Msg { return backMsg{reload: reload, note: note} }
}

// backThen leaves n screens, tells the one it returns to that what it shows
// may have changed, and opens the screen then makes.
func backThen(n int, note string, then tea.Cmd) tea.Cmd {
	return func() tea.Msg { return backMsg{n: n, reload: true, note: note, then: then} }
}

// say is the command that shows a sentence on the bottom line.
func say(note string) tea.Cmd { return func() tea.Msg { return noteMsg{note: note} } }

// model is the root: the stack of screens, who is connected, and the line at
// the bottom. It is the one value Bubble Tea holds.
type model struct {
	c         *ctx
	stack     []screen
	who       *v1.Principal
	whoFailed bool
	err       error
	note      string
}

// top is the screen being shown.
func (m model) top() screen { return m.stack[len(m.stack)-1] }

// Init opens the menu and asks who is connected.
func (m model) Init() tea.Cmd {
	return m.c.call(rootID, func(ctx context.Context) tea.Msg {
		return whoami(ctx, m.c.client)
	})
}

// setTop records which screen is on top, so a screen's timer knows whether to
// fetch.
func (m model) setTop() model {
	m.c.topID = m.top().sid()
	return m
}

// deliver gives a message to the screen with an id, wherever it is in the
// stack, and returns what the screen asked for.
func (m model) deliver(to int, msg tea.Msg) (model, tea.Cmd) {
	for i, s := range m.stack {
		if s.sid() == to {
			var cmd tea.Cmd
			m.stack[i], cmd = s.update(m.c, msg)
			return m, cmd
		}
	}
	return m, nil
}

// Update is the root's update function: navigation, calls finishing and the
// keys every screen shares, then the rest to the top screen.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.c.w, m.c.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		m, cmd := m.deliver(m.top().sid(), msg)
		return m, cmd
	case doneMsg:
		return m.done(msg)
	case pushMsg:
		m.stack = append(m.stack, msg.s)
		return m.setTop(), msg.cmd
	case replaceMsg:
		old := m.top()
		m.c.cancelOwner(old.sid())
		m.stack[len(m.stack)-1] = msg.s
		return m.setTop(), msg.cmd
	case backMsg:
		return m.leave(msg)
	case noteMsg:
		m.err, m.note = nil, msg.note
		return m, nil
	case deliverMsg:
		m, cmd := m.deliver(msg.to, msg.msg)
		return m, cmd
	case tickMsg:
		m, cmd := m.deliver(msg.to, msg)
		return m, cmd
	}
	return m, nil
}

// done finishes a call: it is forgotten, who-am-I goes to the header, an error
// goes to the bottom line, and everything else to the screen that asked.
func (m model) done(d doneMsg) (tea.Model, tea.Cmd) {
	background := m.c.pending[d.call] != nil && m.c.pending[d.call].background
	delete(m.c.pending, d.call)
	switch inner := d.msg.(type) {
	case failMsg:
		m.err, m.note = inner.err, ""
	case canceledMsg:
		if background {
			// A stream stopped as its screen was left says nothing.
			return m, nil
		}
		m.err, m.note = nil, "canceled"
	case whoamiMsg:
		m.who = inner.principal
		m.c.acc = newAccess(inner.principal)
		// The menu is built from what the caller may do.
		m, _ = m.deliver(m.stack[0].sid(), inner)
	}
	if d.to == rootID {
		if _, failed := d.msg.(failMsg); failed {
			m.whoFailed = true
		}
		return m, nil
	}
	m, cmd := m.deliver(d.to, d.msg)
	return m, cmd
}

// leave pops screens. The first screen, the menu, stays: q quits from it.
func (m model) leave(b backMsg) (tea.Model, tea.Cmd) {
	n := max(b.n, 1)
	for ; n > 0 && len(m.stack) > 1; n-- {
		m.c.cancelOwner(m.top().sid())
		m.stack = m.stack[:len(m.stack)-1]
	}
	m = m.setTop()
	if b.note != "" {
		m.note = b.note
	}
	if b.reload {
		m, cmd := m.deliver(m.top().sid(), reloadMsg{})
		return m, tea.Batch(cmd, b.then)
	}
	return m, b.then
}

// key handles a key press. esc first stops a call the screen is waiting for;
// the quit keys and the help key work everywhere no text is being typed, except
// on the screen that asks before it is left.
func (m model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.err, m.note = nil, ""
	top := m.top()
	name := k.String()
	if name == "esc" && m.c.cancelWaiting(top.sid()) {
		m.note = "canceling"
		return m, nil
	}
	_, guarded := top.(guard)
	// On the help screen every key, q and ? included, closes it.
	_, isHelp := top.(helpScreen)
	switch {
	case name == "ctrl+c" && !guarded:
		return m, m.quit()
	case name == "q" && !top.editing() && !guarded && !isHelp:
		return m, m.quit()
	case name == "?" && !top.editing() && !isHelp:
		m.stack = append(m.stack, newHelp(m.c, top))
		return m.setTop(), nil
	}
	m, cmd := m.deliver(top.sid(), k)
	return m, cmd
}

// quit stops every call and ends the program.
func (m model) quit() tea.Cmd {
	m.c.cancelAll()
	return tea.Quit
}

// View draws the whole frame: header, title, body, status line, key hints.
func (m model) View() tea.View {
	c := m.c
	var v tea.View
	v.AltScreen = true
	if c.w < minWidth || c.h < minHeight {
		v.SetContent(strings.Join(tooSmall(c), "\n"))
		return v
	}
	body := m.top().view(c)
	if len(body) > c.bodyHeight() {
		body = body[:c.bodyHeight()]
	}
	lines := make([]string, 0, c.h)
	lines = append(lines, m.header(), fit(m.top().title(c), c.w))
	lines = append(lines, body...)
	for len(lines) < c.h-2 {
		lines = append(lines, "")
	}
	lines = append(lines, m.status(), m.hints())
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	v.SetContent(strings.Join(lines, "\n"))
	if cur, ok := m.top().(cursorer); ok {
		if x, y, show := cur.cursorAt(c); show {
			v.Cursor = tea.NewCursor(x, y+2)
		}
	}
	return v
}

// tooSmall is what is drawn when the terminal is smaller than any screen can
// use; resizing redraws it.
func tooSmall(c *ctx) []string {
	return []string{
		trunc("terminal too small", max(c.w, 1)),
		trunc(strconv.Itoa(c.w)+" by "+strconv.Itoa(c.h)+" now,", max(c.w, 1)),
		trunc("needs 40 by 10", max(c.w, 1)),
	}
}

// header is the first line: who is connected, and where to.
func (m model) header() string {
	c := m.c
	who := "connecting"
	switch {
	case m.whoFailed:
		who = "not connected"
	case m.who != nil:
		switch caller := m.who.GetCaller().(type) {
		case *v1.Principal_LocalOperator:
			who = "local operator (user " + caller.LocalOperator.GetUserId() + ")"
		case *v1.Principal_Key:
			who = "key " + output.KeyLabel(caller.Key)
		default:
			who = "unknown caller"
		}
	}
	text := "nopacs | " + clean(who) + " | " + clean(c.where)
	return c.st.bold(trunc(text, c.w))
}

// status is the line above the key hints: an error, or that a call is
// waiting, or a note about what was just done.
func (m model) status() string {
	c := m.c
	switch {
	case m.err != nil:
		return c.st.bad(errorLine(m.err, c.w))
	case c.waiting():
		return c.st.faint(trunc("waiting for the server; esc cancels", c.w))
	case m.note != "":
		return c.st.good(trunc(clean(m.note), c.w))
	}
	return ""
}

// hints is the last line: the screen's keys, then the ones every screen has.
func (m model) hints() string {
	c := m.c
	var parts []string
	for _, k := range m.top().keys() {
		parts = append(parts, k.key+" "+k.what)
	}
	// The screen that asks before it is left says what each key does itself.
	if _, guarded := m.top().(guard); !m.top().editing() && !guarded {
		parts = append(parts, "? help", "q quit")
	}
	return c.st.faint(trunc(strings.Join(parts, "  "), c.w))
}

// errorLine is the one line an error takes at the bottom of the screen: the
// text the command prints for the same failure, with the request id. When the
// line is too long for the terminal the server's message is cut, never the
// request id, because the id is what a person reports.
func errorLine(err error, w int) string {
	var ce *client.CallError
	if !errors.As(err, &ce) {
		return trunc(clean(fault.Message(err)), w)
	}
	full := clean(ce.Error())
	if width(full) <= w {
		return full
	}
	prefix := clean(ce.What) + ": "
	suffix := ""
	if ce.RequestID != "" {
		suffix = " (request id " + clean(ce.RequestID) + ")"
	}
	msg := clean(fault.Message(errors.Unwrap(ce)))
	room := w - width(prefix) - width(suffix)
	if room < 8 {
		return trunc(full, w)
	}
	return prefix + trunc(msg, room) + suffix
}
