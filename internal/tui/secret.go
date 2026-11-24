package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// secretScreen shows a new key, once, alone. The key is drawn here and in no
// other view; it is never written to a log, a file or an error; leaving the
// screen asks once, and then zeroes it. The server keeps only a hash and cannot
// show it again, so a person who leaves without storing it must make another
// key, and the screen says so before it lets go.
type secretScreen struct {
	base
	key    *v1.Key
	secret secret
	// asking is true after the first press of a leaving key: the screen has
	// asked whether to leave and waits for the answer.
	asking bool
	// thenQuit says the question was asked by a key that quits the program.
	thenQuit bool
}

// guarded says this screen handles the quit keys itself.
func (secretScreen) guarded() bool { return true }

// newSecretScreen makes the screen for a key just made.
func newSecretScreen(c *ctx, k *v1.Key, s secret) secretScreen {
	return secretScreen{base: base{c.newID()}, key: k, secret: s}
}

// update applies a message. The leaving keys ask once; y leaves.
func (s secretScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	name := k.String()
	if s.asking {
		if name != "y" {
			s.asking, s.thenQuit = false, false
			return s, nil
		}
		s.secret.wipe()
		s.secret = secret{}
		if s.thenQuit {
			return s, tea.Quit
		}
		return s, back(true, "the key was shown once and is gone from this screen")
	}
	switch name {
	case "enter", "esc":
		s.asking = true
	case "q", "ctrl+c":
		s.asking, s.thenQuit = true, true
	}
	return s, nil
}

// view draws the key, with the words that it will not be shown again, or the
// question about leaving.
func (s secretScreen) view(c *ctx) []string {
	var lines []string
	add := func(t string) { lines = append(lines, trunc(t, c.w)) }
	add("")
	add("  Holder   " + clean(s.key.GetHolder()))
	add("  Key id   " + clean(s.key.GetId()))
	expires := "never"
	if s.key.GetExpireTime() != nil {
		expires = output.Time(s.key.GetExpireTime(), c.loc)
	}
	add("  Expires  " + expires)
	add("")
	text := s.secret.reveal()
	if width(text)+4 > c.w {
		// A key cut or wrapped would be copied wrong and never work, so it is
		// not drawn at all when the terminal is too narrow to hold it.
		add(c.st.bad("  The terminal is too narrow to show the key on one line."))
		add("  Make it at least " + strconv.Itoa(width(text)+4) + " columns wide; the key is kept until you leave.")
	} else {
		add("  " + c.st.bold(text))
	}
	add("")
	add("  " + c.st.bad("This is the only time the key is shown. It will not be shown again."))
	add("  The server keeps only a hash of it and cannot show it a second time.")
	add("  Copy it now and store it in a file only its holder can read.")
	add("")
	if s.asking {
		add("  " + c.st.bad("Leave? The key will be gone for good.") + "  y leaves, any other key stays")
	} else {
		add("  Press enter when it is stored.")
	}
	return lines
}

// title is the heading.
func (secretScreen) title(*ctx) string { return "New key" }

// editing is false: the screen takes no text.
func (secretScreen) editing() bool { return false }

// keys lists the keys of the screen.
func (s secretScreen) keys() []keyHelp {
	if s.asking {
		return []keyHelp{{"y", "leave, the key is lost"}, {"any other", "stay"}}
	}
	return []keyHelp{{"enter", "leave (asks once)"}}
}
