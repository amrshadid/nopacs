package fault

import (
	"errors"
	"strconv"
)

// Kind says what sort of failure an error is.
type Kind int

// The kinds. Internal is the zero value, so an error that was never given a
// kind, and a Kind that was never set, both count as Internal.
const (
	// Internal is a failure of the archive itself: a bug, a broken invariant,
	// or a cause the caller must not be told about.
	Internal Kind = iota
	// Invalid is a request that is wrong as written: a malformed name, a
	// missing argument, a value out of range.
	Invalid
	// Unauthenticated is a request with no key, or a key that is malformed,
	// unknown, expired or revoked. All of these are one kind, so a caller
	// cannot tell which one it was.
	Unauthenticated
	// Denied is a request from a known key that holds no grant for what it
	// asks.
	Denied
	// NotFound is a request for something that does not exist, among the
	// things the caller may see.
	NotFound
	// Conflict is a request that clashes with the archive's current state,
	// such as making something that already exists.
	Conflict
	// Unavailable is a failure that may pass if the request is tried again
	// later: a database or a storage that cannot be reached.
	Unavailable
	// Precondition is a request that is well formed but names something in
	// a state that forbids it: a storage that still holds files, a file
	// already rejected, a port the installation has not allotted. Sending
	// it again unchanged fails again until that state changes.
	Precondition
	// OutOfRange is a request for a position that can no longer be
	// answered, such as a cursor older than the oldest event kept.
	// Sending it again fails again; the caller starts over.
	OutOfRange
	// Exhausted is a request refused because a bound of the server is
	// reached: a message over the size limit, or too many streams open at
	// once.
	Exhausted
	// DataLoss is archived data found missing or damaged: a file gone from
	// its storage, or bytes that do not match the SHA-256 recorded when
	// they were archived. It is the archive's failure, not the caller's,
	// and sending the request again fails again until the file is mended
	// from another copy; it is a kind of its own, apart from Internal, so
	// the caller is told which file's scan to mend rather than "internal
	// error".
	DataLoss
)

// String returns the kind's name in lowercase, as it appears in a log.
func (k Kind) String() string {
	switch k {
	case Internal:
		return "internal"
	case Invalid:
		return "invalid"
	case Unauthenticated:
		return "unauthenticated"
	case Denied:
		return "denied"
	case NotFound:
		return "not found"
	case Conflict:
		return "conflict"
	case Unavailable:
		return "unavailable"
	case Precondition:
		return "precondition"
	case OutOfRange:
		return "out of range"
	case Exhausted:
		return "exhausted"
	case DataLoss:
		return "data loss"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// ExitCode returns the status the command line exits with for an error of
// this kind. Scripts tell a usage mistake (2), a refusal (3), a missing thing
// (4), a state that forbids the change (5), a position that can no longer be
// answered (6) and a bound of the server reached (7) apart by it; every other
// failure is 1. Unauthenticated and Denied share 3 because a caller must not
// learn from the status whether its key or its grant was the problem. Each
// of 5, 6 and 7 asks the script for something different: change the state
// first, start over, or send less or later; none of them is a usage mistake
// (2), since the request was well formed.
func (k Kind) ExitCode() int {
	switch k {
	case Invalid:
		return 2
	case Unauthenticated, Denied:
		return 3
	case NotFound:
		return 4
	case Precondition:
		return 5
	case OutOfRange:
		return 6
	case Exhausted:
		return 7
	}
	return 1
}

// Error is an error with a kind, a message that is safe to show the caller,
// and optionally the error it wraps, which is for the log only.
type Error struct {
	kind    Kind
	message string
	cause   error
}

// New returns an error of the given kind with a message that is safe to show
// the caller.
func New(kind Kind, message string) error {
	return &Error{kind: kind, message: message}
}

// Wrap returns an error of the given kind that wraps cause. The message is
// what the caller is shown; cause is kept for the log and for errors.Is and
// errors.As, and is never part of the caller's message. Wrap of a nil cause
// is the same as New, so a caller that wraps unconditionally still gets an
// error.
func Wrap(kind Kind, cause error, message string) error {
	return &Error{kind: kind, message: message, cause: cause}
}

// Error returns the message followed by the wrapped error's text, for the log.
// It is not for the caller: use Message for that.
//
// A nil *Error reads as an unclassified internal failure, here and in every
// other method, so a typed nil stored in an error variable cannot panic in a
// log line.
func (e *Error) Error() string {
	if e == nil {
		return internalMessage
	}
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

// Unwrap returns the wrapped error, so errors.Is and errors.As see through an
// Error to its cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Kind returns the error's kind.
func (e *Error) Kind() Kind {
	if e == nil {
		return Internal
	}
	return e.kind
}

// Message returns the message that is safe to show the caller, without the
// wrapped error.
func (e *Error) Message() string {
	if e == nil {
		return internalMessage
	}
	return e.message
}

// KindOf returns the kind of the first Error in err's chain, and Internal
// when there is none, err is nil, or the Error found is a nil pointer. The
// first is the outermost, so a layer that wraps a lower one with a new kind
// decides what the caller sees.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind()
	}
	return Internal
}

// internalMessage is what the caller is shown for an error that carries no
// message of ours: its text was never written for a caller, so none of it is
// shown.
const internalMessage = "internal error"

// Message returns the text that is safe to show the caller for err: the
// message of the first Error in its chain, or "internal error" when there is
// none or the Error found is a nil pointer. It returns the empty string for
// a nil error.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Message()
	}
	return internalMessage
}
