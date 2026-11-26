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
// fixed message "internal error". An error WithReason marked starts its
// message with its reason and carries a google.rpc.ErrorInfo detail and,
// with a retry delay, a google.rpc.RetryInfo detail (details.go).
func ToConnect(err error) *connect.Error {
	msg := fault.Message(err)
	var r *reasoned
	if !errors.As(err, &r) {
		kind := fault.KindOf(err)
		e := connect.NewError(Code(kind), msg)
		if reason, ok := prefixedReason(kind, msg); ok {
			e = e.WithDetail(errorInfo(reason, nil))
		}
		return e
	}
	if !strings.HasPrefix(msg, r.reason) {
		msg = r.reason + ": " + msg
	}
	e := connect.NewError(Code(fault.KindOf(err)), msg).WithDetail(errorInfo(r.reason, r.metadata))
	if r.retry > 0 {
		e = e.WithDetail(retryInfo(r.retry))
	}
	return e
}

// prefixed is each reason of the contract (access.proto, "Where a kind above
// names a reason") that carries no metadata and whose refusal is made where
// this package cannot be imported (the master's and the tenant's ports),
// with the kind it belongs to. Such a refusal writes its reason at the start
// of its message, "REASON: ..."; ToConnect gives it the same
// google.rpc.ErrorInfo a WithReason mark would, so a client that reads
// details reads every reason the contract names, not only those the API
// marks itself.
var prefixed = [...]struct {
	kind   fault.Kind
	reason string
}{
	{fault.Precondition, "TENANT_MOVED"},
	{fault.Precondition, "TENANT_FENCED"},
	{fault.Unavailable, "TENANT_RESTORING"},
	{fault.OutOfRange, "CURSOR_EXPIRED"},
	{fault.OutOfRange, "EVENT_HISTORY_CHANGED"},
}

// prefixedReason returns the reason of the table above a message of kind
// begins with, followed by ": ", and false when it begins with none. Both
// the kind and the prefix must match: a message of another kind that
// happens to start with the same word is not given the reason.
func prefixedReason(kind fault.Kind, msg string) (string, bool) {
	for _, p := range prefixed {
		if p.kind == kind && strings.HasPrefix(msg, p.reason+": ") {
			return p.reason, true
		}
	}
	return "", false
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
