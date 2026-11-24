package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	xterm "github.com/charmbracelet/x/term"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/fault"
)

// ErrInterrupted is returned by Run when the program was ended by an interrupt
// signal rather than by the person quitting. The terminal has been restored
// either way; the caller exits with the conventional status for a signal.
var ErrInterrupted = errors.New("interrupted")

// refreshEvery is how often the list of tenants fetches its first page again.
const refreshEvery = 5 * time.Second

// Options is everything Run takes from the process around it, so a test can
// run the program on a pipe with its own clock.
type Options struct {
	// Client reaches the API. The program makes its calls only through it, and
	// with the deadline it was made with.
	Client *client.Client
	// Where is the target shown in the header: a socket path or a server
	// address. It must hold no key.
	Where string
	// Input and Output are the terminal; os.Stdin and os.Stdout when nil.
	Input  io.Reader
	Output io.Writer
	// Color allows emphasis. The caller decides it from NO_COLOR, the terminal
	// and the operating system, as the commands do.
	Color bool
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Location is the zone times are shown in; time.Local when nil.
	Location *time.Location
	// Size, when both numbers are set, is the terminal's size to start with,
	// for a run on a pipe; a terminal reports its own.
	Width, Height int
	// NoRefresh turns off the timers by which screens refresh themselves:
	// the live lists', and a status's wait before it follows its tenant's
	// events again after a stream ended.
	NoRefresh bool

	// noInput runs the program without reading any input, for a test whose
	// program ends by itself.
	noInput bool
}

// IsTerminal reports whether r is a terminal: a file the operating system says
// is one. It asks the system (the same call the terminal library makes) and
// does not take a character device for a terminal, as /dev/null is one.
func IsTerminal(r any) bool {
	f, ok := r.(*os.File)
	return ok && xterm.IsTerminal(f.Fd())
}

// newModel makes the root model for options.
func newModel(ctxt context.Context, o Options) model {
	c := &ctx{
		base:    ctxt,
		client:  o.Client,
		where:   o.Where,
		st:      styler{on: o.Color},
		now:     o.Now,
		loc:     o.Location,
		refresh: refreshEvery,
		w:       defaultWidth,
		h:       defaultHeight,
		pending: map[int]*pendingCall{},
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.loc == nil {
		c.loc = time.Local
	}
	if o.NoRefresh {
		c.refresh = 0
	}
	if o.Width > 0 && o.Height > 0 {
		c.w, c.h = o.Width, o.Height
	}
	m := model{c: c}
	m.stack = []screen{newMenu(c)}
	return m.setTop()
}

// Run opens the interactive program on the terminal and returns when the
// person quits or a signal ends it. The alternate screen is left and the cursor
// shown whichever way it ends, a panic included: Bubble Tea restores the
// terminal before it reports one, and Run turns that into an error.
func Run(ctx context.Context, o Options) error {
	m := newModel(ctx, o)
	return runProgram(m, m.c, o)
}

// runProgram runs a model on the terminal in o and cancels c's calls when it
// ends. It is Run with the model made, so a test can run one wrapped in a model
// of its own.
func runProgram(m tea.Model, c *ctx, o Options) error {
	opts := []tea.ProgramOption{}
	if o.noInput {
		opts = append(opts, tea.WithInput(nil))
	} else if o.Input != nil {
		opts = append(opts, tea.WithInput(o.Input))
	}
	if o.Output != nil {
		opts = append(opts, tea.WithOutput(o.Output))
	}
	if o.Width > 0 && o.Height > 0 {
		opts = append(opts, tea.WithWindowSize(o.Width, o.Height))
	}
	_, err := tea.NewProgram(m, opts...).Run()
	// Whatever ended the program, no call is left running behind it.
	c.cancelAll()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tea.ErrInterrupted):
		return ErrInterrupted
	case errors.Is(err, tea.ErrProgramPanic):
		return fault.New(fault.Internal, "the interactive program stopped on an internal error; the terminal was restored")
	}
	return fault.Wrap(fault.Internal, err, "the interactive program failed: "+err.Error())
}
