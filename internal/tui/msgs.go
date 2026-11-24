package tui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The messages below are everything a screen can be told besides a key press.
// A call's answer, a navigation request and the timer are all messages, so a
// test can send any of them to a screen and read what it draws.

// rootID is the owner of calls the root itself makes (who is connected). No
// screen has this id: they start at 1.
const rootID = 0

// pushMsg asks the root to open a screen on top of the current one, and to run
// the screen's first command.
type pushMsg struct {
	s   screen
	cmd tea.Cmd
}

// replaceMsg asks the root to put a screen where the current one is. The form
// that makes a key is replaced by the screen that shows it, so going back from
// there never returns to a form with the holder still typed in.
type replaceMsg struct {
	s   screen
	cmd tea.Cmd
}

// backMsg asks the root to leave n screens (one when zero), and, with reload,
// to tell the screen it returns to that what it shows may have changed.
type backMsg struct {
	n      int
	reload bool
	note   string
}

// reloadMsg tells a screen to fetch what it shows again.
type reloadMsg struct{}

// deliverMsg carries a message to a screen by id, wherever it is in the
// stack. It is how a form tells the list under it what was chosen.
type deliverMsg struct {
	to  int
	msg tea.Msg
}

// doneMsg is the end of a call: the message the call made, for the screen that
// asked. The call's number lets the root forget it was pending.
type doneMsg struct {
	call int
	to   int
	msg  tea.Msg
}

// failMsg is what a call made when the server or the network did not answer
// well. The root shows its error on the bottom line and gives it to the screen
// so the screen can stop waiting.
type failMsg struct{ err error }

// canceledMsg is what a call made when the person pressed esc.
type canceledMsg struct{}

// tickMsg is the timer of a screen that refreshes itself.
type tickMsg struct{ to int }

// pageMsg is one page of a list: its rows and the token of the next page, which
// is empty at the end.
type pageMsg struct {
	gen  int
	rows []row
	next string
}

// actionDoneMsg is the answer of a change that worked, with the sentence to
// show.
type actionDoneMsg struct{ note string }

// detailMsg is a fresh copy of the thing a detail screen shows.
type detailMsg struct{ ref any }

// whoamiMsg is the answer to the call that says who is connected.
type whoamiMsg struct{ principal *v1.Principal }

// keyCreatedMsg is the answer of making a key. It is the one message that
// carries a secret, and the secret is in a type that prints as a placeholder.
type keyCreatedMsg struct {
	key    *v1.Key
	secret secret
}

// filterSetMsg tells the audit list which entries to ask for.
type filterSetMsg struct {
	outcome v1.AuditOutcome
	keyID   string
}

// secret holds a key's secret. It exists so that no accident can show the
// value: formatting it with any verb, logging it, or printing a struct that
// holds it all give a placeholder, because the bytes are behind a pointer and
// the type's own methods answer for it. wipe zeroes the bytes, which a Go string
// could not allow, so leaving the screen that showed it leaves nothing of it
// in this program's state.
type secret struct{ b *[]byte }

// newSecret copies a secret's text into a holder.
func newSecret(s string) secret {
	b := []byte(s)
	return secret{b: &b}
}

// reveal returns the text. Only the screen that shows the key calls it.
func (s secret) reveal() string {
	if s.b == nil {
		return ""
	}
	return string(*s.b)
}

// wipe overwrites the secret with zeros.
func (s secret) wipe() {
	if s.b == nil {
		return
	}
	clear(*s.b)
}

// String is the placeholder every verb prints.
func (secret) String() string { return "[secret hidden]" }

// GoString is the placeholder for %#v.
func (secret) GoString() string { return "[secret hidden]" }

// LogValue keeps the secret out of a structured log, though nothing here logs.
func (secret) LogValue() slog.Value { return slog.StringValue("[secret hidden]") }
