package fault

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// TestKindOf guards the kind an error reports: the kind it was made with,
// through any number of fmt.Errorf wrappings, the outermost kind when kinds
// are nested, and Internal for an error that has none or for nil.
func TestKindOf(t *testing.T) {
	plain := errors.New("plain")
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, Internal},
		{"no kind", plain, Internal},
		{"new", New(NotFound, "no such session"), NotFound},
		{"wrap", Wrap(Unavailable, plain, "the database is down"), Unavailable},
		{"under fmt.Errorf", fmt.Errorf("context: %w", New(Conflict, "exists")), Conflict},
		{"outermost wins", Wrap(Denied, New(NotFound, "inner"), "outer"), Denied},
		{"unknown value", New(Kind(42), "odd"), Kind(42)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.err); got != tt.want {
				t.Errorf("KindOf(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestExitCode guards the exit status of each kind, which scripts depend on:
// Invalid 2, Unauthenticated and Denied 3, NotFound 4, Precondition 5,
// OutOfRange 6, Exhausted 7, every other kind 1 (DataLoss among them: a
// failure of the archive, which the caller did not cause and cannot mend
// by sending something else).
func TestExitCode(t *testing.T) {
	want := map[Kind]int{
		Internal:        1,
		Invalid:         2,
		Unauthenticated: 3,
		Denied:          3,
		NotFound:        4,
		Conflict:        1,
		Unavailable:     1,
		Precondition:    5,
		OutOfRange:      6,
		Exhausted:       7,
		DataLoss:        1,
		Kind(-1):        1,
		Kind(99):        1,
	}
	for kind, code := range want {
		if got := kind.ExitCode(); got != code {
			t.Errorf("%v.ExitCode() = %d, want %d", kind, got, code)
		}
	}
}

// TestKindString guards the names kinds have in a log, and that a value with
// no name still prints as something a person can report.
func TestKindString(t *testing.T) {
	want := map[Kind]string{
		Internal:        "internal",
		Invalid:         "invalid",
		Unauthenticated: "unauthenticated",
		Denied:          "denied",
		NotFound:        "not found",
		Conflict:        "conflict",
		Unavailable:     "unavailable",
		Precondition:    "precondition",
		OutOfRange:      "out of range",
		Exhausted:       "exhausted",
		DataLoss:        "data loss",
		Kind(11):        "Kind(11)",
	}
	for kind, name := range want {
		if got := kind.String(); got != name {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, name)
		}
	}
}

// TestMessageHidesCause guards the rule that the caller's message never
// carries the wrapped error's text, which may name a file or an address,
// while the log's text (Error) still has it.
func TestMessageHidesCause(t *testing.T) {
	const detail = "open /var/lib/nopacs/master.db: permission denied"
	err := Wrap(Unavailable, errors.New(detail), "the catalog cannot be reached")

	if got := Message(err); strings.Contains(got, detail) || strings.Contains(got, "/var/lib") {
		t.Errorf("Message = %q, which carries the cause", got)
	}
	if got := Message(err); got != "the catalog cannot be reached" {
		t.Errorf("Message = %q, want the message given to Wrap", got)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("errors.As did not find an *Error")
	}
	if got := e.Message(); strings.Contains(got, detail) {
		t.Errorf("(*Error).Message = %q, which carries the cause", got)
	}
	if got := err.Error(); !strings.Contains(got, detail) {
		t.Errorf("Error() = %q, want the cause in the log's text", got)
	}
	// Further wrapping by fmt.Errorf adds to the log's text, not the caller's.
	outer := fmt.Errorf("loading tenant: %w", err)
	if got := Message(outer); got != "the catalog cannot be reached" {
		t.Errorf("Message through fmt.Errorf = %q", got)
	}
}

// TestMessageOfPlainError guards that an error with no kind shows the caller
// a fixed text and none of its own, since its text was never written for a
// caller.
func TestMessageOfPlainError(t *testing.T) {
	err := errors.New("pq: password authentication failed for user admin")
	if got := Message(err); got != "internal error" {
		t.Errorf("Message = %q, want %q", got, "internal error")
	}
	if got := Message(nil); got != "" {
		t.Errorf("Message(nil) = %q, want empty", got)
	}
}

// TestErrorsIsAndAs guards that an Error works with the standard library's
// errors package: errors.Is finds a wrapped sentinel, errors.As finds a
// wrapped error of another type and the *Error itself.
func TestErrorsIsAndAs(t *testing.T) {
	pathErr := &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}
	err := fmt.Errorf("outer: %w", Wrap(NotFound, pathErr, "no such key file"))

	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("errors.Is did not find fs.ErrNotExist through the Error")
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe != pathErr {
		t.Error("errors.As did not find the wrapped *fs.PathError")
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatal("errors.As did not find the *Error")
	}
	if fe.Kind() != NotFound {
		t.Errorf("Kind() = %v, want %v", fe.Kind(), NotFound)
	}
	var made *Error
	if !errors.As(New(Invalid, "x"), &made) || made.Unwrap() != nil {
		t.Error("an Error made by New unwraps to something")
	}
}

// TestWrapNil guards that wrapping a nil cause still yields an error, with
// the message alone as its text, so a caller cannot turn a failure into
// success by wrapping a nil.
func TestWrapNil(t *testing.T) {
	err := Wrap(Internal, nil, "something failed")
	if err == nil {
		t.Fatal("Wrap(nil) returned nil")
	}
	if got := err.Error(); got != "something failed" {
		t.Errorf("Error() = %q, want the message alone", got)
	}
}

// TestTypedNil guards that a nil *Error stored in an error, which is not a
// nil error, reads as an internal failure with the fixed message instead of
// panicking in KindOf, Message or a log line.
func TestTypedNil(t *testing.T) {
	var e *Error
	var err error = e
	wrapped := fmt.Errorf("outer: %w", err)
	for _, err := range []error{err, wrapped} {
		if got := KindOf(err); got != Internal {
			t.Errorf("KindOf = %v, want Internal", got)
		}
		if got := Message(err); got != "internal error" {
			t.Errorf("Message = %q, want %q", got, "internal error")
		}
	}
	if got := err.Error(); got != "internal error" {
		t.Errorf("Error() = %q", got)
	}
	if e.Kind() != Internal || e.Message() != "internal error" || e.Unwrap() != nil {
		t.Error("a nil *Error's methods do not read as an internal failure")
	}
}
