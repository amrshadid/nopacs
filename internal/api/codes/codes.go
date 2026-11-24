package codes

import (
	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/fault"
)

// pairs is the table: each fault kind with the one code that carries it. It is
// the only place the pairing is written, so a kind added to fault and not
// added here is caught by the test that walks every kind.
var pairs = [...]struct {
	kind fault.Kind
	code connect.Code
}{
	{fault.Internal, connect.CodeInternal},
	{fault.Invalid, connect.CodeInvalidArgument},
	{fault.Unauthenticated, connect.CodeUnauthenticated},
	{fault.Denied, connect.CodePermissionDenied},
	{fault.NotFound, connect.CodeNotFound},
	{fault.Conflict, connect.CodeAlreadyExists},
	{fault.Unavailable, connect.CodeUnavailable},
	{fault.Precondition, connect.CodeFailedPrecondition},
	{fault.OutOfRange, connect.CodeOutOfRange},
	{fault.Exhausted, connect.CodeResourceExhausted},
	{fault.DataLoss, connect.CodeDataLoss},
}

// Code returns the API code that carries a fault kind. A kind the table does
// not know is answered as INTERNAL, so a new kind never leaks out as a code
// that means something else.
func Code(kind fault.Kind) connect.Code {
	for _, p := range pairs {
		if p.kind == kind {
			return p.code
		}
	}
	return connect.CodeInternal
}

// Kind returns the fault kind of an API code. Besides the table, it maps the
// codes any method may answer without the contract naming a kind: a deadline
// or a cancellation is a failure that may pass on a later try (Unavailable).
// Every other code is Internal, a failure and exit 1: UNIMPLEMENTED, UNKNOWN
// and any code a newer server adds. DATA_LOSS is in the table as DataLoss,
// which also exits 1, with the server's message naming what to mend. None of them is shown as the
// caller's mistake, since exit 2 is for what the person typed wrong.
// RESOURCE_EXHAUSTED is in the table as Exhausted, whether a message was over
// the size limit or the server had too many streams open: either way the
// request was well formed, and sending less or later is what helps.
func Kind(code connect.Code) fault.Kind {
	for _, p := range pairs {
		if p.code == code {
			return p.kind
		}
	}
	switch code {
	case connect.CodeDeadlineExceeded, connect.CodeCanceled:
		return fault.Unavailable
	}
	return fault.Internal
}

// ToConnect turns an error into the API error a server answers: the code of
// its kind and only the message that is safe to show the caller, never the
// text of what it wraps. An error with no fault kind is INTERNAL with the
// fixed message "internal error".
func ToConnect(err error) *connect.Error {
	return connect.NewError(Code(fault.KindOf(err)), fault.Message(err))
}

// FromConnect turns an API error received by a client into a fault error of
// the matching kind. The message is the server's, which is written for the
// caller; the API error itself is kept as the cause for errors.As. A server
// message that is empty is replaced by the name of the code.
func FromConnect(err *connect.Error) error {
	msg := err.Message()
	if msg == "" {
		msg = err.Code().String()
	}
	return fault.Wrap(Kind(err.Code()), err, msg)
}
