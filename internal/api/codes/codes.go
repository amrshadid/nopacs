package codes

import (
	"errors"
	"strings"
	"time"

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
// fixed message "internal error". Every answer carries a google.rpc.ErrorInfo
// detail (details.go), the gRPC richer error model's statement of which case
// of its code it is:
//
//   - an error WithReason marked carries its reason and metadata, starts its
//     message with the reason (unless it is a general one, which says no
//     more than the code) and, with a retry delay, also carries a
//     google.rpc.RetryInfo detail, provided the reason fits the error's kind
//     (fitsKind);
//   - an error whose message begins with a reason of fault's table for its
//     kind, "REASON: ...", carries that reason (the refusals the master's and
//     the tenant's ports make, which cannot import this package);
//   - any other error carries its code's general reason (General).
func ToConnect(err error) *connect.Error {
	kind := fault.KindOf(err)
	msg := fault.Message(err)
	var r *reasoned
	if !errors.As(err, &r) || !fitsKind(r.reason, kind) {
		reason, ok := prefixedReason(kind, msg)
		if !ok {
			reason = General(Code(kind))
		}
		return connect.NewError(Code(kind), msg).WithDetail(errorInfo(reason, nil))
	}
	if !IsGeneral(r.reason) && !strings.HasPrefix(msg, r.reason) {
		msg = r.reason + ": " + msg
	}
	e := connect.NewError(Code(kind), msg).WithDetail(errorInfo(r.reason, r.metadata))
	if r.retry > 0 {
		e = e.WithDetail(retryInfo(r.retry))
	}
	return e
}

// fitsKind reports whether a reason WithReason marked may be answered for
// an error of kind: a case of fault's table of that very kind, or the
// general reason of kind's code. A mark that fits neither (a case of
// another kind, as when a lower layer wrapped the marked error with a new
// kind, or a word the table does not hold) is not answered: the error is
// then answered as an unmarked one, with no case metadata and no retry
// delay, so a client never reads HELD on an INTERNAL answer.
func fitsKind(reason string, kind fault.Kind) bool {
	if c, ok := fault.CaseOf(reason); ok {
		return c.Kind == kind
	}
	return reason == General(Code(kind))
}

// prefixedReason returns the reason of fault's table that a message of kind
// begins with, followed by ": ", and false when it begins with none. Both
// the kind and the prefix must match: a message of another kind that
// happens to start with the same word is not given the reason.
func prefixedReason(kind fault.Kind, msg string) (string, bool) {
	for _, c := range fault.Cases() {
		if c.Kind == kind && strings.HasPrefix(msg, c.Reason+": ") {
			return c.Reason, true
		}
	}
	return "", false
}

// General returns the general reason of an API code: its name as the
// Connect protocol writes it, in capitals (NOT_FOUND, UNAVAILABLE,
// INVALID_ARGUMENT, ...). An error with no case of its own carries it, so a
// client that reads ErrorInfo finds one on every answer; it says no more
// than the code. No reason of fault's table has the same spelling as a
// general one, so a client tells the two apart by the word alone.
//
// A code past the protocol's sixteen is UNKNOWN, as the RPC library reads
// such a code on the wire.
func General(code connect.Code) string {
	if code < connect.CodeCanceled || code > connect.CodeUnauthenticated {
		code = connect.CodeUnknown
	}
	return strings.ToUpper(code.String())
}

// IsGeneral reports whether reason is the general reason of some API code,
// as opposed to a case of fault's table or a reason this build does not
// know.
func IsGeneral(reason string) bool {
	for c := connect.CodeCanceled; c <= connect.CodeUnauthenticated; c++ {
		if General(c) == reason {
			return true
		}
	}
	return false
}

// Ensure returns e with a google.rpc.ErrorInfo detail: e itself when it
// carries one already, else a copy with its code's general reason. The
// server passes every answer through it, those the RPC library or the
// server's own front step made without a fault kind among them (a malformed
// message, a method nothing serves, a deadline), so no answer leaves without
// the detail the contract promises.
func Ensure(e *connect.Error) *connect.Error {
	if e == nil {
		return nil
	}
	for _, d := range e.Details() {
		if d.Type == ErrorInfoType {
			return e
		}
	}
	return e.WithDetail(errorInfo(General(e.Code()), nil))
}

// reasoned is an error marked with the contract's reason, its metadata and
// a retry delay (WithReason). It wraps the error it marks, so the fault
// kind and message come from that error.
type reasoned struct {
	err      error
	reason   string
	metadata map[string]string
	retry    time.Duration
}

// Error returns the wrapped error's text, for the log.
func (r *reasoned) Error() string { return r.err.Error() }

// Unwrap returns the wrapped error.
func (r *reasoned) Unwrap() error { return r.err }

// WithReason returns err marked with reason, one of the contract's fixed
// words (access.proto, "Where a kind above names a reason"), the metadata
// the contract names for it (nil for none), and retry, the delay to wait
// before trying again (zero for none): ToConnect writes them into the API
// error. Its kind and message are err's.
func WithReason(err error, reason string, metadata map[string]string, retry time.Duration) error {
	if err == nil {
		return nil
	}
	return &reasoned{err: err, reason: reason, metadata: metadata, retry: retry}
}

// ReasonOf returns the reason and the retry delay WithReason marked err
// with, and false for an error it did not mark.
func ReasonOf(err error) (string, time.Duration, bool) {
	var r *reasoned
	if !errors.As(err, &r) {
		return "", 0, false
	}
	return r.reason, r.retry, true
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
