package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// menuItem is one line of the menu: what it opens and a few words about it.
type menuItem struct {
	label string
	about string
	// allow says whether the caller's grants let the screen be used; nil
	// means any caller with a grant. The server checks every call anyway:
	// this only keeps the menu from offering what would be refused.
	allow func(a *access) bool
	open  func(c *ctx) tea.Cmd
}

// menuScreen is the first screen: the nouns the program can show.
type menuScreen struct {
	base
	// all is every item the program has; items are those the caller may use.
	all    []menuItem
	items  []menuItem
	cursor int
}

// visible keeps the items the caller's grants let it use.
func visible(all []menuItem, a *access) []menuItem {
	var out []menuItem
	for _, it := range all {
		if it.allow == nil && a.anyGrant() || it.allow != nil && it.allow(a) {
			out = append(out, it)
		}
	}
	return out
}

// newMenu makes the menu, with the screens this version has.
func newMenu(c *ctx) menuScreen {
	all := []menuItem{
		{"Tenants", "the independent archives; open one for what it holds and what may be done to it", nil, func(c *ctx) tea.Cmd {
			s, cmd := newTenantList(c)
			return push(s, cmd)
		}},
		{"Keys", "who may call the API; make one, revoke one, see its grants", func(a *access) bool { return a.onInstallation(v1.Role_ROLE_ADMIN) }, func(c *ctx) tea.Cmd {
			s, cmd := newKeyList(c)
			return push(s, cmd)
		}},
		{"Settings", "the installation's defaults", func(a *access) bool { return a.onInstallation(v1.Role_ROLE_ADMIN) }, func(c *ctx) tea.Cmd {
			s, cmd := newSettingList(c, installationScope())
			return push(s, cmd)
		}},
		{"Audit log", "what was done, by which key, newest first; v verifies its seal", func(a *access) bool { return a.onInstallation(v1.Role_ROLE_ADMIN) }, func(c *ctx) tea.Cmd {
			s, cmd := newAuditList(c, installationScope())
			return push(s, cmd)
		}},
	}
	return menuScreen{base: base{c.newID()}, all: all, items: visible(all, c.acc)}
}

// update applies a message: keys move the cursor, and enter or a number opens
// an item.
func (m menuScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if _, ok := msg.(whoamiMsg); ok {
		// The caller is known now: leave out what its grants do not allow.
		m.items = visible(m.all, c.acc)
		m.cursor = max(min(m.cursor, len(m.items)-1), 0)
		return m, nil
	}
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || len(m.items) == 0 {
		return m, nil
	}
	name := k.String()
	switch name {
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, len(m.items)-1)
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = len(m.items) - 1
	case "enter":
		return m, m.items[m.cursor].open(c)
	default:
		for i, it := range m.items {
			if name == strconv.Itoa(i+1) {
				m.cursor = i
				return m, it.open(c)
			}
		}
	}
	return m, nil
}

// view draws the items.
func (m menuScreen) view(c *ctx) []string {
	lines := []string{""}
	if len(m.items) == 0 {
		return append(lines, "  "+c.st.faint("This key holds no grant, so there is nothing to open. Ask an admin for one."))
	}
	for i, it := range m.items {
		line := "  " + strconv.Itoa(i+1) + "  " + padRight(it.label, 10) + c.st.faint(trunc(it.about, max(c.w-18, 1)))
		if i == m.cursor {
			line = c.st.reverse(fit("> "+strconv.Itoa(i+1)+"  "+padRight(it.label, 10)+trunc(it.about, max(c.w-18, 1)), c.w))
		}
		lines = append(lines, line)
	}
	return lines
}

// title is the heading.
func (menuScreen) title(*ctx) string { return "Menu" }

// editing is false: the menu takes no text.
func (menuScreen) editing() bool { return false }

// keys lists the keys of the menu.
func (m menuScreen) keys() []keyHelp {
	return []keyHelp{{"up/down", "move"}, {"enter", "open"}, {"1-" + strconv.Itoa(max(len(m.items), 1)), "open"}}
}

// helpScreen lists the keys of the screen it was opened over.
type helpScreen struct {
	base
	about   string
	entries []keyHelp
}

// newHelp makes the help for a screen: its own keys, then the ones every
// screen has.
func newHelp(c *ctx, below screen) helpScreen {
	ks := append([]keyHelp{}, below.keys()...)
	if _, isList := below.(listScreen); isList && !below.editing() {
		// Too many for the hint line, which is one line wide.
		ks = append(ks, keyHelp{"pgup/pgdown", "a page at a time"}, keyHelp{"home/end", "first and last row"}, keyHelp{"ctrl+r", "fetch the first page again"})
	}
	if !below.editing() {
		ks = append(ks, keyHelp{"?", "this help"}, keyHelp{"q", "quit"})
	}
	return helpScreen{base: base{c.newID()}, about: below.title(c), entries: ks}
}

// update closes the help on any key.
func (h helpScreen) update(_ *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return h, back(false, "")
	}
	return h, nil
}

// view draws the keys, aligned.
func (h helpScreen) view(c *ctx) []string {
	keyW := 0
	for _, k := range h.entries {
		keyW = max(keyW, width(k.key))
	}
	lines := []string{""}
	for _, k := range h.entries {
		lines = append(lines, trunc("  "+c.st.bold(padRight(k.key, keyW))+"  "+k.what, c.w))
	}
	return append(lines, "", c.st.faint("  Any key closes this."))
}

// title is the heading.
func (h helpScreen) title(*ctx) string { return "Keys of: " + h.about }

// editing is false.
func (helpScreen) editing() bool { return false }

// keys lists the keys of the help screen itself.
func (helpScreen) keys() []keyHelp { return []keyHelp{{"any key", "close"}} }
