package tui

import (
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"

	"github.com/amrshadid/nopacs/internal/cli/output"
)

// styler gives text emphasis, or leaves it plain. It is the one place that
// decides whether an escape sequence is ever written: with it off (NO_COLOR, a
// Windows console, a dumb terminal) a view is plain text and nothing else.
type styler struct{ on bool }

// apply renders text in a style when emphasis is on.
func (s styler) apply(st lipgloss.Style, text string) string {
	if !s.on || text == "" {
		return text
	}
	return st.Render(text)
}

// bold makes a label stand out.
func (s styler) bold(t string) string { return s.apply(lipgloss.NewStyle().Bold(true), t) }

// faint dims text that is secondary: hints and the empty cells.
func (s styler) faint(t string) string { return s.apply(lipgloss.NewStyle().Faint(true), t) }

// reverse marks the selected row.
func (s styler) reverse(t string) string { return s.apply(lipgloss.NewStyle().Reverse(true), t) }

// bad marks an error or a warning.
func (s styler) bad(t string) string {
	return s.apply(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")), t)
}

// good marks a thing that worked or is healthy.
func (s styler) good(t string) string {
	return s.apply(lipgloss.NewStyle().Foreground(lipgloss.Color("2")), t)
}

// width is the number of terminal cells a string takes, ignoring escape
// sequences and counting wide characters as two.
func width(s string) int { return lipgloss.Width(s) }

// ellipsis ends a cell that was cut.
const ellipsis = "…"

// trunc cuts s to at most n cells, ending in an ellipsis when it cut
// anything. A row of a list is never allowed to wrap, so every cell goes
// through here. The input is cut to a bounded number of runes first: a
// message from the network may be long, and measuring it rune by rune is
// quadratic.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if width(s) <= n {
		return s
	}
	if n == 1 {
		return ellipsis
	}
	runes := []rune(s)
	if len(runes) > 4*n {
		runes = runes[:4*n]
	}
	var b strings.Builder
	for _, r := range runes {
		if width(b.String()+string(r)) > n-1 {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}

// padRight pads s with spaces to n cells; a longer s is left as it is.
func padRight(s string, n int) string {
	if w := width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// fit cuts and pads s to exactly n cells.
func fit(s string, n int) string { return padRight(trunc(s, n), n) }

// clean is what a screen does to every string the server sent before drawing
// it: control characters, and characters that change how text reads, become
// "?". It is the commands' own cleaner.
func clean(s string) string { return output.Clean(s) }

// wrap breaks text into lines of at most n cells, at spaces where it can and
// inside a word where it must. A detail screen uses it for a long value; a list
// never does.
func wrap(text string, n int) []string {
	if n < 1 {
		return nil
	}
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		cur := ""
		flush := func() {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
		}
		for _, w := range words {
			for width(w) > n {
				flush()
				head := cutAt(w, n)
				lines = append(lines, head)
				w = strings.TrimPrefix(w, head)
			}
			switch {
			case cur == "":
				cur = w
			case width(cur)+1+width(w) <= n:
				cur += " " + w
			default:
				flush()
				cur = w
			}
		}
		flush()
		if len(words) == 0 {
			lines = append(lines, "")
		}
	}
	return lines
}

// cutAt returns the longest prefix of s that is at most n cells wide, and at
// least one character so a loop that uses it always advances.
func cutAt(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() > 0 && width(b.String()+string(r)) > n {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dropControl removes control characters from text a person typed or pasted
// into a field, so a pasted newline or escape never becomes part of a value.
func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
