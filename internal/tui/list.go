package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pageAhead is how close to the end of the loaded rows the cursor may come
// before the next page is fetched. A list never loads more than it needs: a
// tenant with 300 million instances scrolls the same way as one with ten.
const pageAhead = 10

// column is one column of a list: its heading and how wide it may be. A column
// is cut with an ellipsis to fit, but never below min; when even the minimums do
// not fit, the last columns are left out.
type column struct {
	name     string
	min, max int
}

// row is one row of a list: the text of each cell, already cleaned, and the
// thing it stands for, which actions and the enter key receive.
type row struct {
	cells []string
	ref   any
}

// action is a key of a list or detail screen. do receives the screen it was
// pressed on and what the cursor is on (nil when there is no row), and returns
// the command that does it, which opens a form, a confirmation or another
// screen. No action calls the API itself.
type action struct {
	key  string
	help string
	// needsRow says the action works on the row under the cursor and does
	// nothing when there is none.
	needsRow bool
	do       func(c *ctx, self screen, ref any) tea.Cmd
}

// listScreen is every list of the program: tenants, keys, grants, settings,
// audit entries. They differ in their columns, how a page is asked for and what
// their keys do; paging, scrolling, the filter and the cursor are the same, so
// they are written once.
type listScreen struct {
	base
	heading string
	cols    []column
	empty   string

	rows       []row
	next       string
	loaded     bool
	loading    bool
	refreshing bool
	gen        int
	cursor     int
	top        int

	filter    string
	filtering bool
	input     field

	// params is what fetch needs to ask for this list: a scope, a filter. It
	// is state, so changing it is a message and a test can read it.
	params any
	// fetch makes the command that asks for the page after token ("" for the
	// first).
	fetch func(c *ctx, l listScreen, token string) tea.Cmd
	// open is what enter does on a row; nil when enter does nothing.
	open func(c *ctx, l listScreen, r row) tea.Cmd
	// note gives a line shown under the table about the selected row.
	note func(r row) string
	// live makes the list fetch its first page again on a timer.
	live bool
	// onMsg handles a message only this list knows, such as a new audit filter.
	onMsg   func(c *ctx, l listScreen, msg tea.Msg) (listScreen, tea.Cmd, bool)
	actions []action
}

// start asks for the first page afresh. A page that was still on its way is
// ignored, because its generation is old.
func (l listScreen) start(c *ctx) (listScreen, tea.Cmd) {
	l.gen++
	l.rows, l.next = nil, ""
	l.loaded, l.loading, l.refreshing = false, true, false
	l.cursor, l.top = 0, 0
	cmd := l.fetch(c, l, "")
	if l.live && c.refresh > 0 {
		cmd = tea.Batch(cmd, tick(c, l.id))
	}
	return l, cmd
}

// tick is the command that sends a screen its timer message after the refresh
// interval. It goes through ctx.after so a test can count the timers it arms
// without waiting for one.
func tick(c *ctx, to int) tea.Cmd {
	if c.after != nil {
		return c.after(to)
	}
	return tea.Tick(c.refresh, func(time.Time) tea.Msg { return tickMsg{to: to} })
}

// reload fetches the first page again and puts it over the rows already shown,
// keeping the cursor, so a change made on another screen appears without the
// list jumping to its top.
func (l listScreen) reload(c *ctx) (listScreen, tea.Cmd) {
	if !l.loaded {
		return l.start(c)
	}
	l.gen++
	l.loading, l.refreshing = true, true
	return l, l.fetch(c, l, "")
}

// visible is the indexes of the rows the filter lets through.
func (l listScreen) visible() []int {
	idx := make([]int, 0, len(l.rows))
	needle := strings.ToLower(l.filter)
	for i, r := range l.rows {
		if needle == "" || strings.Contains(strings.ToLower(strings.Join(r.cells, " ")), needle) {
			idx = append(idx, i)
		}
	}
	return idx
}

// selected is the row under the cursor, if there is one.
func (l listScreen) selected() (row, bool) {
	vis := l.visible()
	if l.cursor < 0 || l.cursor >= len(vis) {
		return row{}, false
	}
	return l.rows[vis[l.cursor]], true
}

// reserved is how many lines under the table the screen keeps for the note and
// the filter.
func (l listScreen) reserved() int {
	n := 0
	if l.note != nil {
		n++
	}
	if l.filtering || l.filter != "" {
		n++
	}
	return n
}

// rowsShown is how many rows fit: the body, less the column headings and the
// reserved lines.
func (l listScreen) rowsShown(c *ctx) int {
	return max(c.bodyHeight()-1-l.reserved(), 1)
}

// window keeps the cursor on screen: it returns the first row to draw, moving
// the stored one only as far as the cursor needs.
func (l listScreen) window(c *ctx) int {
	rows, n := l.rowsShown(c), len(l.visible())
	top := l.top
	if l.cursor < top {
		top = l.cursor
	}
	if l.cursor >= top+rows {
		top = l.cursor - rows + 1
	}
	return max(min(top, max(n-rows, 0)), 0)
}

// move puts the cursor delta rows away, inside the list, and asks for the next
// page when it comes near the end of what is loaded.
func (l listScreen) move(c *ctx, delta int) (listScreen, tea.Cmd) {
	n := len(l.visible())
	l.cursor = max(min(l.cursor+delta, n-1), 0)
	l.top = l.window(c)
	return l.more(c)
}

// more asks for the next page when the cursor is within pageAhead of the end of
// the loaded rows and the server has more. Only one page is asked for at a
// time.
func (l listScreen) more(c *ctx) (listScreen, tea.Cmd) {
	if l.next == "" || l.loading || l.cursor < len(l.visible())-pageAhead {
		return l, nil
	}
	l.loading = true
	return l, l.fetch(c, l, l.next)
}

// update applies a message.
func (l listScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if l.onMsg != nil {
		if nl, cmd, ok := l.onMsg(c, l, msg); ok {
			return nl, cmd
		}
	}
	switch msg := msg.(type) {
	case pageMsg:
		return l.page(c, msg)
	case failMsg, canceledMsg:
		l.loading, l.refreshing = false, false
	case reloadMsg:
		return l.reload(c)
	case tickMsg:
		cmd := tick(c, l.id)
		if c.topID != l.id || l.loading {
			return l, cmd
		}
		nl, fetch := l.reload(c)
		return nl, tea.Batch(cmd, fetch)
	case tea.PasteMsg:
		if l.filtering {
			l.input, _ = l.input.update(msg)
			l.filter, l.cursor, l.top = l.input.value(), 0, 0
		}
	case tea.KeyPressMsg:
		return l.key(c, msg)
	}
	return l, nil
}

// page takes a page of rows, cleaning each cell: text from the server never
// reaches a view, nor the filter, with a control character in it.
func (l listScreen) page(c *ctx, p pageMsg) (screen, tea.Cmd) {
	if p.gen != l.gen {
		return l, nil
	}
	for _, r := range p.rows {
		for i := range r.cells {
			r.cells[i] = clean(r.cells[i])
		}
	}
	switch {
	case l.refreshing && len(l.rows) > pageSize:
		// More than the first page is loaded: the first page is replaced and
		// the pages after it, and the token to the one after them, are kept.
		l.rows = append(append([]row{}, p.rows...), l.rows[pageSize:]...)
	case l.refreshing:
		l.rows, l.next = p.rows, p.next
	default:
		l.rows = append(append([]row{}, l.rows...), p.rows...)
		l.next = p.next
	}
	l.loaded, l.loading, l.refreshing = true, false, false
	l.cursor = max(min(l.cursor, len(l.visible())-1), 0)
	l.top = l.window(c)
	return l, nil
}

// key applies a key press.
func (l listScreen) key(c *ctx, k tea.KeyPressMsg) (screen, tea.Cmd) {
	name := k.String()
	if l.filtering {
		return l.filterKey(c, k)
	}
	switch name {
	case "up", "k":
		return l.move(c, -1)
	case "down", "j":
		return l.move(c, 1)
	case "pgup":
		return l.move(c, -(l.rowsShown(c) - 1))
	case "pgdown":
		return l.move(c, l.rowsShown(c)-1)
	case "home":
		return l.move(c, -len(l.rows))
	case "end":
		return l.move(c, len(l.rows))
	case "/":
		l.filtering = true
		l.input = field{}.set(l.filter)
		return l, nil
	case "esc":
		if l.filter != "" {
			l.filter, l.cursor, l.top = "", 0, 0
			return l, nil
		}
		return l, back(false, "")
	case "ctrl+r":
		return l.reload(c)
	case "enter":
		if r, ok := l.selected(); ok && l.open != nil {
			return l, l.open(c, l, r)
		}
		return l, nil
	}
	for _, a := range l.actions {
		if a.key != name {
			continue
		}
		var ref any
		if r, ok := l.selected(); ok {
			ref = r.ref
		} else if a.needsRow {
			return l, nil
		}
		return l, a.do(c, l, ref)
	}
	return l, nil
}

// filterKey applies a key press while the filter is being typed: the rows are
// narrowed as each key arrives.
func (l listScreen) filterKey(c *ctx, k tea.KeyPressMsg) (screen, tea.Cmd) {
	switch k.String() {
	case "enter":
		l.filtering = false
		return l, nil
	case "esc":
		l.filtering, l.filter, l.cursor, l.top = false, "", 0, 0
		return l, nil
	case "up":
		return l.move(c, -1)
	case "down":
		return l.move(c, 1)
	}
	l.input, _ = l.input.update(k)
	l.filter, l.cursor, l.top = l.input.value(), 0, 0
	return l, nil
}

// editing is true while the filter is being typed.
func (l listScreen) editing() bool { return l.filtering }

// keys lists the keys of the list.
func (l listScreen) keys() []keyHelp {
	if l.filtering {
		return []keyHelp{{"enter", "keep"}, {"esc", "clear"}}
	}
	ks := []keyHelp{{"up/down", "move"}}
	if l.open != nil {
		ks = append(ks, keyHelp{"enter", "open"})
	}
	ks = append(ks, keyHelp{"/", "filter"})
	for _, a := range l.actions {
		ks = append(ks, keyHelp{a.key, a.help})
	}
	return append(ks, keyHelp{"esc", "back"})
}

// title is the heading with the count of rows: "100+" while the server has
// more, the exact number once the last page is in.
func (l listScreen) title(*ctx) string {
	count := "loading"
	if l.loaded {
		count = countText(len(l.rows), l.next != "")
	}
	if l.filter != "" {
		count = strconv.Itoa(len(l.visible())) + " of " + count + ", matching \"" + clean(l.filter) + "\""
	}
	return l.heading + "  [" + count + "]"
}

// countText says how many rows there are: "3 rows", or "100+ rows" while more
// remain.
func countText(n int, more bool) string {
	s := strconv.Itoa(n)
	if more {
		s += "+"
	}
	if n == 1 && !more {
		return s + " row"
	}
	return s + " rows"
}

// layout works out the width of each column for a width in cells: each takes
// the width of its widest cell up to its maximum, and when they do not fit, the
// one with the most to give is cut first, down to its minimum; the last columns
// are left out only when every column is at its minimum. It returns the widths
// of the columns that are shown.
func layout(cols []column, rows []row, avail int) []int {
	w := make([]int, len(cols))
	for i, col := range cols {
		w[i] = width(col.name)
		for _, r := range rows {
			if i < len(r.cells) {
				w[i] = max(w[i], width(r.cells[i]))
			}
		}
		if col.max > 0 {
			w[i] = min(w[i], col.max)
		}
		w[i] = max(w[i], col.min)
	}
	shown := len(cols)
	for shown > 0 {
		total := 2 * (shown - 1)
		for _, x := range w[:shown] {
			total += x
		}
		if total <= avail {
			break
		}
		best := -1
		for i := range shown {
			if w[i]-cols[i].min > 0 && (best < 0 || w[i]-cols[i].min > w[best]-cols[best].min) {
				best = i
			}
		}
		if best < 0 {
			shown--
			continue
		}
		w[best] -= min(total-avail, w[best]-cols[best].min)
	}
	return w[:shown]
}

// view draws the list: the column headings, the rows in the window with the
// selected one marked, the note about it, and the filter.
func (l listScreen) view(c *ctx) []string {
	body := c.bodyHeight()
	if !l.loaded {
		if l.loading {
			return []string{c.st.faint("loading")}
		}
		return []string{"could not load the list; ctrl+r tries again"}
	}
	vis := l.visible()
	if len(vis) == 0 {
		msg := l.empty
		if l.filter != "" {
			msg = "no loaded row matches the filter"
			if l.next != "" {
				msg += "; the server has more, which the filter has not seen: move down to load them"
			}
		}
		tail := l.tail(c, nil, body-1)
		lines := []string{c.st.faint(trunc(msg, c.w))}
		for len(lines) < body-len(tail) {
			lines = append(lines, "")
		}
		return append(lines, tail...)
	}
	widths := layout(l.cols, l.rows, c.w-2)
	lines := make([]string, 0, body)
	head := make([]string, len(widths))
	for i, w := range widths {
		head[i] = fit(l.cols[i].name, w)
	}
	lines = append(lines, c.st.bold("  "+strings.Join(head, "  ")))
	rows := l.rowsShown(c)
	top := l.window(c)
	for i := top; i < min(top+rows, len(vis)); i++ {
		r := l.rows[vis[i]]
		cells := make([]string, len(widths))
		for j, w := range widths {
			cells[j] = fit(r.cells[j], w)
		}
		line := "  " + strings.Join(cells, "  ")
		if i == l.cursor {
			line = c.st.reverse(fit("> "+line[2:], c.w))
		}
		lines = append(lines, line)
	}
	for len(lines) < 1+rows {
		lines = append(lines, "")
	}
	sel, _ := l.selected()
	return append(lines, l.tail(c, &sel, body-len(lines))...)
}

// tail draws the lines under the table: the note about the selected row and the
// filter being typed or in force.
func (l listScreen) tail(c *ctx, sel *row, room int) []string {
	var out []string
	if l.note != nil && room > len(out) {
		text := ""
		if sel != nil {
			text = l.note(*sel)
		}
		out = append(out, c.st.faint(trunc(clean(text), c.w)))
	}
	if (l.filtering || l.filter != "") && room > len(out) {
		switch {
		case l.filtering:
			text, _ := l.input.render(c.w - 2)
			out = append(out, "/ "+text)
		default:
			out = append(out, c.st.faint(trunc("filter: "+clean(l.filter)+" (esc clears)", c.w)))
		}
	}
	return out
}

// cursorAt puts the terminal's cursor in the filter while it is being typed.
func (l listScreen) cursorAt(c *ctx) (int, int, bool) {
	if !l.filtering {
		return 0, 0, false
	}
	_, col := l.input.render(c.w - 2)
	return 2 + col, c.bodyHeight() - 1, true
}
