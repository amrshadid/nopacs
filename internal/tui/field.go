package tui

import (
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// maxFieldRunes bounds what can be typed or pasted into a field. The
// contract's longest typed value, a holder, is 256 bytes; a field that took
// more would only be refused by the server later.
const maxFieldRunes = 256

// field is a one-line text input. It is written here, not taken from a
// library, because the library's input reads the system clipboard through
// another program on Linux, and a screen that handles key ids and names has no
// need to reach outside the process.
type field struct {
	val []rune
	pos int
}

// value is the text typed so far.
func (f field) value() string { return string(f.val) }

// set replaces the text and puts the cursor at its end.
func (f field) set(s string) field {
	f.val = []rune(dropControl(s))
	if len(f.val) > maxFieldRunes {
		f.val = f.val[:maxFieldRunes]
	}
	f.pos = len(f.val)
	return f
}

// insert puts text at the cursor, without control characters and within the
// limit.
func (f field) insert(s string) field {
	add := []rune(dropControl(s))
	if room := maxFieldRunes - len(f.val); len(add) > room {
		add = add[:max(room, 0)]
	}
	val := make([]rune, 0, len(f.val)+len(add))
	val = append(val, f.val[:f.pos]...)
	val = append(val, add...)
	val = append(val, f.val[f.pos:]...)
	f.val = val
	f.pos += len(add)
	return f
}

// update applies a key press or a paste and reports whether the field used it,
// so a screen can give the keys a field does not want to its own commands.
func (f field) update(msg tea.Msg) (field, bool) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		return f.insert(msg.Content), true
	case tea.KeyPressMsg:
		switch msg.String() {
		case "left", "ctrl+b":
			f.pos = max(f.pos-1, 0)
		case "right", "ctrl+f":
			f.pos = min(f.pos+1, len(f.val))
		case "home", "ctrl+a":
			f.pos = 0
		case "end", "ctrl+e":
			f.pos = len(f.val)
		case "backspace", "ctrl+h":
			if f.pos > 0 {
				f.val = append(append([]rune{}, f.val[:f.pos-1]...), f.val[f.pos:]...)
				f.pos--
			}
		case "delete", "ctrl+d":
			if f.pos < len(f.val) {
				f.val = append(append([]rune{}, f.val[:f.pos]...), f.val[f.pos+1:]...)
			}
		case "ctrl+u":
			f.val = append([]rune{}, f.val[f.pos:]...)
			f.pos = 0
		case "ctrl+k":
			f.val = append([]rune{}, f.val[:f.pos]...)
		case "ctrl+w":
			start := f.pos
			for start > 0 && unicode.IsSpace(f.val[start-1]) {
				start--
			}
			for start > 0 && !unicode.IsSpace(f.val[start-1]) {
				start--
			}
			f.val = append(append([]rune{}, f.val[:start]...), f.val[f.pos:]...)
			f.pos = start
		default:
			if msg.Text == "" || msg.Mod&(tea.ModCtrl|tea.ModAlt) != 0 {
				return f, false
			}
			return f.insert(msg.Text), true
		}
		return f, true
	}
	return f, false
}

// render returns the text that fits in avail cells, scrolled so the cursor is
// inside it, and the column of the cursor within that text. Text typed by a
// person is not cleaned: it never came from the network, and control
// characters never enter a field.
func (f field) render(avail int) (text string, cursorCol int) {
	if avail < 2 {
		return "", 0
	}
	start := 0
	for start < f.pos && width(string(f.val[start:f.pos])) > avail-1 {
		start++
	}
	end := len(f.val)
	for end > f.pos && width(string(f.val[start:end])) > avail-1 {
		end--
	}
	return string(f.val[start:end]), width(string(f.val[start:f.pos]))
}
