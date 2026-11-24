package cli

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/tui"
)

// exitSignal is the status of a program ended by an interrupt: 128 plus the
// signal's number (SIGINT is 2), the shell convention.
const exitSignal = 130

// exitError is an error that carries the exit code and nothing to print.
type exitError struct{ code int }

// Error describes the exit for a caller that does print it.
func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// terminal says whether input and output are both terminals.
func (a *app) terminal() bool {
	if a.env.Terminal != nil {
		return a.env.Terminal(a.env.Stdin, a.env.Stdout)
	}
	return tui.IsTerminal(a.env.Stdin) && tui.IsTerminal(a.env.Stdout)
}

// canOpenInteractive is the one rule for opening the interactive program: no
// arguments (cobra has already refused any, so this is reached only for none)
// and a terminal at both ends. The flags that find the server do not count as
// arguments, so "nopacs --data-dir DIR" opens it too.
func (a *app) canOpenInteractive() bool { return a.terminal() }

// openInteractive makes the client the way every command does, from the same
// flags and environment, and runs the program on it. An error making the client
// is a usage error, with the pointer to the help, as for a command.
func (a *app) openInteractive(cmd *cobra.Command) error {
	c, err := a.client(cmd)
	if err != nil {
		return err
	}
	defer c.Close()
	run := a.env.Interactive
	if run == nil {
		run = tui.Run
	}
	err = run(cmd.Context(), tui.Options{
		Client:   c,
		Where:    c.Target(),
		Input:    a.env.Stdin,
		Output:   a.env.Stdout,
		Color:    ColorAllowed(true, a.env.Getenv, runtime.GOOS),
		Now:      a.env.Now,
		Location: a.env.Location,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tui.ErrInterrupted):
		return &runError{&exitError{exitSignal}}
	}
	return &runError{err}
}
