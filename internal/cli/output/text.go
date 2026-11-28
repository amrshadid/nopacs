package output

import (
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// TimeLayout is how a time is written for a person: the date, the time and the
// offset from UTC, so a reader in one zone can compare with a log from
// another.
const TimeLayout = "2006-01-02 15:04:05 -07:00"

// Clean returns s with every control character, a newline and an escape
// among them, and every format character (the zero-width and the right-to-left
// overrides that make text read differently from what it is), and every byte
// that is not UTF-8, replaced by "?", so text from the network cannot move the
// cursor, change colors, end the line early or pass one name off as another.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return '?'
		}
		return r
	}, s)
}

// Time writes a timestamp in the given zone with its offset, or "-" for an
// absent one.
func Time(ts *timestamppb.Timestamp, loc *time.Location) string {
	if ts == nil {
		return "-"
	}
	return ts.AsTime().In(loc).Format(TimeLayout)
}

// Dash returns s, or "-" when it is empty, so an empty cell is never a gap
// that shifts what a reader sees.
func Dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// WithheldText is what stands in a table or a list of values for one the
// server left out of an answer to a masked key.
const WithheldText = "withheld"

// Value is Dash(s), or WithheldText when the server withheld the value
// (the answer's values_withheld is true).
func Value(withheld bool, s string) string {
	if withheld {
		return WithheldText
	}
	return Dash(s)
}

// IsTerminal reports whether w is a terminal: a file that is a character
// device. It is how the command line chooses whether color is allowed; it
// takes no dependency for it.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Style says whether text may be given emphasis.
type Style struct {
	// Color allows ANSI emphasis. The caller sets it only for a terminal when
	// NO_COLOR is unset.
	Color bool
}

// Bold returns s in bold when color is allowed, and s unchanged otherwise.
func (s Style) Bold(text string) string {
	if !s.Color {
		return text
	}
	return "\x1b[1m" + text + "\x1b[0m"
}

// Table writes rows under a header as aligned columns, two spaces apart.
// Every cell is cleaned first. The last column is not padded, so a line has no
// trailing spaces. Padding is counted in runes and added before any emphasis,
// because the escape sequences of bold text have no width on the screen.
func Table(w io.Writer, style Style, header []string, rows [][]string) error {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	clean := make([][]string, len(rows))
	for r, row := range rows {
		clean[r] = make([]string, len(row))
		for i, c := range row {
			clean[r][i] = Clean(c)
			if n := utf8.RuneCountInString(clean[r][i]); i < len(widths) && n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	line := func(cells []string, bold bool) {
		for i, c := range cells {
			pad := ""
			if i < len(cells)-1 {
				pad = strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2)
			}
			if bold {
				c = style.Bold(c)
			}
			b.WriteString(c + pad)
		}
		b.WriteString("\n")
	}
	line(header, true)
	for _, row := range clean {
		line(row, false)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Pair is one labeled value of a Details block.
type Pair struct {
	// Label names the value.
	Label string
	// Value is the text of the value.
	Value string
}

// Details writes labeled values one to a line, the labels aligned, for a
// command that found one thing. Values are cleaned.
func Details(w io.Writer, style Style, pairs []Pair) error {
	width := 0
	for _, p := range pairs {
		if n := utf8.RuneCountInString(p.Label); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, p := range pairs {
		label := p.Label + ":"
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(p.Label))
		b.WriteString(style.Bold(label) + pad + " " + Clean(p.Value) + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
