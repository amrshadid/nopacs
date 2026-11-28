package tui

import (
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// refusalLine words an error the server answered as a form shows it: the
// reason code the server gave, when it is one of the contract's own (HELD,
// RETENTION_MINIMUM) and not only the code's name, then its message and, when it
// said so, how long to wait. An error with no answer behind it (a connection,
// a deadline of this program) is its text alone.
func refusalLine(err error) string {
	msg := fault.Message(err)
	var ce *client.CallError
	if errors.As(err, &ce) {
		msg = fault.Message(errors.Unwrap(ce))
	}
	text := clean(msg)
	d, ok := client.DetailOf(err)
	if !ok {
		return text
	}
	// A general reason is the code's own name, which says no more than the
	// message does; a reason of the contract's own already begins the message.
	if d.Reason != "" && !codes.IsGeneral(d.Reason) && !strings.HasPrefix(text, d.Reason) {
		text = d.Reason + ": " + text
	}
	if d.Retry > 0 {
		text += " (try again in " + output.Span(d.Retry) + ")"
	}
	return text
}
