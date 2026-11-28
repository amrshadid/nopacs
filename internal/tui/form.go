package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// formField is one line of a form: a text field, or a choice among a few words.
type formField struct {
	label string
	hint  string
	// keyID marks a field that takes a key's id. A whole key typed or pasted
	// into it is refused and wiped from the field, so a secret never sits on
	// a screen that was not made to show one.
	keyID bool
	// choices, when set, make this a choice: left and right pick one, and sel
	// is the index of the one picked.
	choices []string
	sel     int
	in      field
}

// formScreen is every screen that asks for something before it acts: making a
// key, giving a grant a role, setting a setting, filtering the audit log, and
// every confirmation. A confirmation is a form with one field and a word that
// must be typed into it exactly: that is how nothing destructive happens on one
// key.
type formScreen struct {
	base
	heading string
	intro   []string
	fields  []formField
	focus   int
	busy    bool
	// msg is why the last attempt to submit was refused, before anything was
	// sent.
	msg string
	// want, when set, is the text the first field must hold for the form to be
	// submitted.
	want string
	// confirms marks a confirmation: it can be submitted only when the first
	// field holds want, so an empty want (a name or an id the server left out)
	// can never be matched and the form refuses, instead of skipping the check
	// and letting enter go ahead on one key.
	confirms bool
	// verb names what enter does on the last field, for the key hints.
	verb string
	// pop is how many screens to leave when the change is made.
	pop int
	// submit checks the fields and returns the command that makes the change,
	// or says what is wrong with them.
	submit func(c *ctx, f formScreen) (tea.Cmd, error)
	// result handles an answer only this form knows, such as a new key.
	result func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool)
	// refusal makes a call this form made that failed show its reason on the
	// form itself (the reason code the server gave and its message), so the
	// person who typed a value sees why it was refused beside it.
	refusal bool
}

// value is what field i holds: its text, or the word chosen.
func (f formScreen) value(i int) string {
	fld := f.fields[i]
	if len(fld.choices) > 0 {
		return fld.choices[fld.sel]
	}
	return strings.TrimSpace(fld.in.value())
}

// update applies a message.
func (f formScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if f.result != nil {
		if s, cmd, ok := f.result(c, f, msg); ok {
			return s, cmd
		}
	}
	switch msg := msg.(type) {
	case actionDoneMsg:
		return f, func() tea.Msg { return backMsg{n: max(f.pop, 1), reload: true, note: msg.note} }
	case failMsg:
		f.busy = false
		if f.refusal {
			f.msg = refusalLine(msg.err)
		}
	case canceledMsg:
		f.busy = false
	case tea.PasteMsg:
		f.fields = append([]formField{}, f.fields...)
		if len(f.fields[f.focus].choices) == 0 {
			f.fields[f.focus].in, _ = f.fields[f.focus].in.update(msg)
		}
	case tea.KeyPressMsg:
		return f.key(c, msg)
	}
	return f, nil
}

// key applies a key press.
func (f formScreen) key(c *ctx, k tea.KeyPressMsg) (screen, tea.Cmd) {
	// The fields are a slice shared with older copies of the form; give this
	// copy its own before changing one.
	f.fields = append([]formField{}, f.fields...)
	cur := &f.fields[f.focus]
	switch k.String() {
	case "esc":
		return f, back(false, "")
	case "tab", "down":
		f.focus = (f.focus + 1) % len(f.fields)
		return f, nil
	case "shift+tab", "up":
		f.focus = (f.focus + len(f.fields) - 1) % len(f.fields)
		return f, nil
	case "enter":
		if f.focus < len(f.fields)-1 {
			f.focus++
			return f, nil
		}
		return f.send(c)
	}
	if len(cur.choices) > 0 {
		switch k.String() {
		case "left", "h":
			cur.sel = (cur.sel + len(cur.choices) - 1) % len(cur.choices)
		case "right", "l", "space":
			cur.sel = (cur.sel + 1) % len(cur.choices)
		}
		return f, nil
	}
	cur.in, _ = cur.in.update(k)
	f.msg = ""
	return f, nil
}

// send checks the form and, when it is good, makes the call. The form shows
// that it is waiting and ignores enter until the answer comes or the call is
// canceled.
func (f formScreen) send(c *ctx) (screen, tea.Cmd) {
	if f.busy {
		return f, nil
	}
	for i, fld := range f.fields {
		if fld.keyID && strings.HasPrefix(strings.TrimSpace(fld.in.value()), "nopacs_") {
			f.fields = append([]formField{}, f.fields...)
			f.fields[i].in = field{}
			f.focus = i
			f.msg = fault.Message(output.CheckKeyID("nopacs_"))
			return f, nil
		}
	}
	if f.confirms && f.want == "" {
		f.msg = "not done: there is nothing to type to confirm, so nothing is sent; go back and reload the screen"
		return f, nil
	}
	if f.want != "" && f.fields[0].in.value() != f.want {
		f.msg = "not done: type " + f.want + " exactly, as shown, to go ahead"
		return f, nil
	}
	cmd, err := f.submit(c, f)
	if err != nil {
		f.msg = fault.Message(err)
		return f, nil
	}
	f.msg, f.busy = "", true
	return f, cmd
}

// editing is true: a form is always taking text, so q and ? are letters in it.
func (f formScreen) editing() bool { return true }

// title is the form's heading.
func (f formScreen) title(*ctx) string { return f.heading }

// keys lists the keys of a form.
func (f formScreen) keys() []keyHelp {
	verb := f.verb
	if verb == "" {
		verb = "ok"
	}
	return []keyHelp{{"tab", "next field"}, {"enter", verb}, {"esc", "cancel"}}
}

// layoutLines draws the form and says where the cursor goes.
func (f formScreen) layoutLines(c *ctx) (lines []string, cx, cy int) {
	for _, l := range f.intro {
		lines = append(lines, wrap(clean(l), c.w)...)
	}
	lines = append(lines, "")
	labelW := 0
	for _, fld := range f.fields {
		labelW = max(labelW, width(clean(fld.label)))
	}
	for i, fld := range f.fields {
		label := c.st.bold(padRight(clean(fld.label), labelW))
		prefix := "  "
		if i == f.focus {
			prefix = "> "
		}
		avail := c.w - 2 - labelW - 2
		var text string
		if len(fld.choices) > 0 {
			parts := make([]string, len(fld.choices))
			for j, ch := range fld.choices {
				if j == fld.sel {
					parts[j] = "[" + ch + "]"
				} else {
					parts[j] = " " + ch + " "
				}
			}
			text = strings.Join(parts, " ")
		} else {
			shown := fld.in
			if fld.keyID && strings.HasPrefix(strings.TrimSpace(shown.value()), "nopacs_") {
				// A whole key was typed or pasted where an id belongs: it is
				// drawn as stars while it is there, never as itself.
				stars := make([]rune, len(shown.val))
				for j := range stars {
					stars[j] = '*'
				}
				shown.val = stars
			}
			t, col := shown.render(avail)
			text = t
			if i == f.focus {
				cx, cy = 2+labelW+2+col, len(lines)
			}
		}
		lines = append(lines, prefix+label+"  "+text)
		if fld.hint != "" {
			lines = append(lines, "  "+padRight("", labelW)+"  "+c.st.faint(trunc(clean(fld.hint), max(avail, 1))))
		}
	}
	lines = append(lines, "")
	switch {
	case f.busy:
		lines = append(lines, c.st.faint("waiting for the server; esc cancels"))
	case f.msg != "":
		lines = append(lines, c.st.bad(trunc(clean(f.msg), c.w)))
	}
	return lines, cx, cy
}

// view draws the form.
func (f formScreen) view(c *ctx) []string {
	lines, _, _ := f.layoutLines(c)
	return lines
}

// cursorAt puts the cursor in the field being typed in.
func (f formScreen) cursorAt(c *ctx) (int, int, bool) {
	if len(f.fields[f.focus].choices) > 0 {
		return 0, 0, false
	}
	_, x, y := f.layoutLines(c)
	return x, y, true
}
