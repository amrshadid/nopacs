package codes

import (
	"errors"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/fault"
)

// allKinds lists every fault kind, so a kind added to fault without a pair in
// the table fails the round-trip test below.
var allKinds = []fault.Kind{
	fault.Internal, fault.Invalid, fault.Unauthenticated, fault.Denied,
	fault.NotFound, fault.Conflict, fault.Unavailable,
	fault.Precondition, fault.OutOfRange, fault.Exhausted, fault.DataLoss,
}

// TestEveryKindRoundTrips guards the table: each kind maps to a code and that
// code maps back to the same kind, so the server and the command line agree.
func TestEveryKindRoundTrips(t *testing.T) {
	seen := map[connect.Code]fault.Kind{}
	for _, k := range allKinds {
		c := Code(k)
		if other, dup := seen[c]; dup {
			t.Errorf("kinds %v and %v share code %v", other, k, c)
		}
		seen[c] = k
		if got := Kind(c); got != k {
			t.Errorf("Kind(Code(%v)) = %v", k, got)
		}
	}
}

// TestContractCodes guards the pairing written in access.proto: each error
// kind of the contract with the code that carries it.
func TestContractCodes(t *testing.T) {
	want := map[fault.Kind]connect.Code{
		fault.Invalid:         connect.CodeInvalidArgument,
		fault.Unauthenticated: connect.CodeUnauthenticated,
		fault.Denied:          connect.CodePermissionDenied,
		fault.NotFound:        connect.CodeNotFound,
		fault.Conflict:        connect.CodeAlreadyExists,
		fault.Unavailable:     connect.CodeUnavailable,
		fault.Internal:        connect.CodeInternal,
		fault.Precondition:    connect.CodeFailedPrecondition,
		fault.OutOfRange:      connect.CodeOutOfRange,
		fault.Exhausted:       connect.CodeResourceExhausted,
		fault.DataLoss:        connect.CodeDataLoss,
	}
	for k, c := range want {
		if got := Code(k); got != c {
			t.Errorf("Code(%v) = %v, want %v", k, got, c)
		}
	}
}

// TestReceivingSideCodes guards the codes any method may answer: a deadline
// and a cancellation are unavailable, a bound of the server reached is
// Exhausted (exit 7), data loss is DataLoss (exit 1), and every other code,
// a code this version has never heard of among them, is Internal (exit 1),
// never the caller's fault.
func TestReceivingSideCodes(t *testing.T) {
	cases := map[connect.Code]fault.Kind{
		connect.CodeResourceExhausted: fault.Exhausted,
		connect.Code(99):              fault.Internal,
		connect.CodeDeadlineExceeded:  fault.Unavailable,
		connect.CodeCanceled:          fault.Unavailable,
		connect.CodeUnimplemented:     fault.Internal,
		connect.CodeUnknown:           fault.Internal,
		connect.CodeDataLoss:          fault.DataLoss,
	}
	for c, k := range cases {
		if got := Kind(c); got != k {
			t.Errorf("Kind(%v) = %v, want %v", c, got, k)
		}
		if got := Kind(c).ExitCode(); (k == fault.Unavailable || k == fault.Internal || k == fault.DataLoss) && got != 1 {
			t.Errorf("Kind(%v) exits %d, want 1", c, got)
		}
	}
	if got := Code(fault.Kind(99)); got != connect.CodeInternal {
		t.Errorf("Code of an unknown kind = %v, want internal", got)
	}
}

// TestToConnectHidesTheCause guards what a server answers: the message safe
// for the caller and never the text of the wrapped error.
func TestToConnectHidesTheCause(t *testing.T) {
	err := fault.Wrap(fault.Unavailable, errors.New("dial tcp 10.0.0.5:5432: password=hunter2"), "the tenant's database does not answer")
	ce := ToConnect(err)
	if ce.Code() != connect.CodeUnavailable || ce.Message() != "the tenant's database does not answer" {
		t.Errorf("got %v %q", ce.Code(), ce.Message())
	}
	plain := ToConnect(errors.New("secret detail"))
	if plain.Code() != connect.CodeInternal || plain.Message() != "internal error" {
		t.Errorf("unclassified error answered %v %q", plain.Code(), plain.Message())
	}
}

// TestFromConnect guards the client side: kind from the code, message from the
// server, a fallback message when it sent none, and the API error kept as the
// cause.
func TestFromConnect(t *testing.T) {
	err := FromConnect(connect.NewError(connect.CodeNotFound, "no such key"))
	if fault.KindOf(err) != fault.NotFound || fault.Message(err) != "no such key" {
		t.Errorf("got %v %q", fault.KindOf(err), fault.Message(err))
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Error("the API error is not kept as the cause")
	}
	empty := FromConnect(connect.NewError(connect.CodePermissionDenied, ""))
	if fault.Message(empty) == "" {
		t.Error("an empty server message was not replaced")
	}
}
