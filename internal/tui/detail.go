package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// pair is one labeled value of a detail screen.
type pair struct{ label, value string }

// detailScreen shows one thing whole: a tenant, a key, an audit entry. A long
// value wraps under its label, and the screen scrolls when there is more than
// fits. (A list never wraps a row; a detail screen exists to show what a row
// had to cut.)
type detailScreen struct {
	base
	heading string
	ref     any
	// pairs makes the labeled values of ref. It is a function of the thing
	// shown, so a fresh copy is drawn the same way.
	pairs func(c *ctx, ref any) []pair
	// refetch asks for the thing again; nil when it cannot change under the
	// person's hands, as an audit entry cannot.
	refetch func(c *ctx, id int, ref any) tea.Cmd
	loading bool
	scroll  int
	actions []action
}

// update applies a message.
func (d detailScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case detailMsg:
		d.ref, d.loading = msg.ref, false
	case failMsg, canceledMsg:
		d.loading = false
	case reloadMsg:
		if d.refetch != nil && !d.loading {
			d.loading = true
			return d, d.refetch(c, d.id, d.ref)
		}
	case tea.KeyPressMsg:
		return d.key(c, msg)
	}
	return d, nil
}

// key applies a key press.
func (d detailScreen) key(c *ctx, k tea.KeyPressMsg) (screen, tea.Cmd) {
	name := k.String()
	switch name {
	case "up", "k":
		d.scroll = max(d.scroll-1, 0)
	case "down", "j":
		d.scroll = min(d.scroll+1, d.maxScroll(c))
	case "pgup":
		d.scroll = max(d.scroll-(c.bodyHeight()-1), 0)
	case "pgdown":
		d.scroll = min(d.scroll+(c.bodyHeight()-1), d.maxScroll(c))
	case "esc":
		return d, back(false, "")
	default:
		for _, a := range d.actions {
			if a.key == name {
				return d, a.do(c, d, d.ref)
			}
		}
	}
	return d, nil
}

// lines are every line of the screen, before scrolling.
func (d detailScreen) lines(c *ctx) []string {
	pairs := append([]pair(nil), d.pairs(c, d.ref)...)
	for i := range pairs {
		// A label can be built from a name the server sent.
		pairs[i].label = clean(pairs[i].label)
	}
	labelW := 0
	for _, p := range pairs {
		labelW = max(labelW, width(p.label))
	}
	valueW := max(c.w-labelW-3, 10)
	var out []string
	for _, p := range pairs {
		wrapped := wrap(clean(p.value), valueW)
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}
		for i, w := range wrapped {
			label := strings.Repeat(" ", labelW+1)
			if i == 0 {
				label = c.st.bold(p.label+":") + strings.Repeat(" ", labelW-width(p.label))
			}
			out = append(out, label+" "+w)
		}
	}
	return out
}

// maxScroll is how far the lines can scroll.
func (d detailScreen) maxScroll(c *ctx) int {
	return max(len(d.lines(c))-c.bodyHeight(), 0)
}

// view draws the lines from the scroll position.
func (d detailScreen) view(c *ctx) []string {
	lines := d.lines(c)
	if d.loading {
		lines = append([]string{c.st.faint("refreshing")}, lines...)
	}
	from := min(d.scroll, max(len(lines)-1, 0))
	return lines[from:]
}

// title is the heading.
func (d detailScreen) title(*ctx) string { return d.heading }

// editing is false: a detail screen takes no text.
func (d detailScreen) editing() bool { return false }

// keys lists the keys of the screen.
func (d detailScreen) keys() []keyHelp {
	var ks []keyHelp
	for _, a := range d.actions {
		ks = append(ks, keyHelp{a.key, a.help})
	}
	return append(ks, keyHelp{"up/down", "scroll"}, keyHelp{"esc", "back"})
}
