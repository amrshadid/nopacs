package codes

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestDetailsEncoding guards the bytes of the two error details against
// those another implementation writes for the same messages: the expected
// bytes are google.rpc.ErrorInfo(reason "HELD", domain "nopacs", metadata
// {hold: 01jh, b: x}) and google.rpc.RetryInfo(retry_delay 30s 500ns) as
// the googleapis-common-protos Python package serializes them
// (SerializeToString(deterministic=True)), so a client with its own copy of
// google/rpc/error_details.proto reads what the server wrote.
func TestDetailsEncoding(t *testing.T) {
	info := errorInfo("HELD", map[string]string{"hold": "01jh", "b": "x"})
	if got, want := hex.EncodeToString(info.Value), "0a0448454c4412066e6f706163731a060a01621201781a0c0a04686f6c64120430316a68"; got != want || info.Type != ErrorInfoType {
		t.Errorf("ErrorInfo %s %s, want %s %s", info.Type, got, ErrorInfoType, want)
	}
	retry := retryInfo(30*time.Second + 500)
	if got, want := hex.EncodeToString(retry.Value), "0a05081e10f403"; got != want || retry.Type != RetryInfoType {
		t.Errorf("RetryInfo %s %s, want %s %s", retry.Type, got, RetryInfoType, want)
	}
}

// TestWithReason guards what ToConnect makes of an error WithReason
// marked: the code of its kind, its message starting with the reason once,
// an ErrorInfo detail with the reason, the domain and the metadata, and a
// RetryInfo detail only with a delay; an error not marked carries its
// code's general reason and no RetryInfo.
func TestWithReason(t *testing.T) {
	base := fault.Wrap(fault.Unavailable, errors.New("cause"), "STORAGE_SWITCHING: the storage main is being switched")
	e := ToConnect(WithReason(base, "STORAGE_SWITCHING", nil, 30*time.Second))
	if e.Code() != connect.CodeUnavailable || strings.Count(e.Message(), "STORAGE_SWITCHING") != 1 {
		t.Errorf("code %v message %q", e.Code(), e.Message())
	}
	r, ok := ReasonOfError(e)
	if !ok || r.Reason != "STORAGE_SWITCHING" || r.Domain != Domain || len(r.Metadata) != 0 {
		t.Errorf("error info %+v %v", r, ok)
	}
	if d, ok := RetryOfError(e); !ok || d != 30*time.Second {
		t.Errorf("retry %v %v", d, ok)
	}

	e = ToConnect(WithReason(fault.New(fault.Precondition, "a hold covers it"), "HELD", map[string]string{"hold": "01jh"}, 0))
	if !strings.HasPrefix(e.Message(), "HELD: ") {
		t.Errorf("message %q does not start with its reason", e.Message())
	}
	if r, ok := ReasonOfError(e); !ok || r.Metadata["hold"] != "01jh" {
		t.Errorf("error info %+v %v", r, ok)
	}
	if _, ok := RetryOfError(e); ok {
		t.Error("a retry detail without a delay")
	}
	if reason, _, ok := ReasonOf(WithReason(base, "X", nil, 0)); !ok || reason != "X" {
		t.Errorf("ReasonOf %q %v", reason, ok)
	}

	plain := ToConnect(fault.New(fault.Unavailable, "down"))
	if r, ok := ReasonOfError(plain); !ok || r.Reason != "UNAVAILABLE" || r.Domain != Domain || len(r.Metadata) != 0 || len(plain.Details()) != 1 {
		t.Errorf("an error not marked: error info %+v %v, details %v; want the general UNAVAILABLE alone", r, ok, plain.Details())
	}
	if plain.Message() != "down" {
		t.Errorf("an error not marked has the message %q, want it unchanged", plain.Message())
	}
	if WithReason(nil, "X", nil, 0) != nil {
		t.Error("WithReason of nil is not nil")
	}
}

// TestPrefixedReasons guards that every reason the contract names travels
// as an ErrorInfo detail, not only in the message: each refusal the
// master's and the tenant's ports make with its reason at the start of its
// message decodes to that reason, with the code of its kind; the table's
// words are the ports' own; a message of another kind that starts with the
// same word carries only that kind's general reason. The bytes of one are those the
// googleapis-common-protos Python package writes for
// ErrorInfo(reason "TENANT_MOVED", domain "nopacs").
func TestPrefixedReasons(t *testing.T) {
	cases := []struct {
		err    error
		reason string
		code   connect.Code
	}{
		{master.ReadOnlyError("lab1", master.StateMoved, ""), master.ReasonTenantMoved, connect.CodeFailedPrecondition},
		{master.ReadOnlyError("lab1", master.StateFenced, "why"), master.ReasonTenantFenced, connect.CodeFailedPrecondition},
		{master.FencedReadError("lab1", ""), master.ReasonTenantFenced, connect.CodeFailedPrecondition},
		{fault.New(fault.Unavailable, "TENANT_RESTORING: the tenant lab1 is being reconciled after a restore"), "TENANT_RESTORING", connect.CodeUnavailable},
		{tenant.CursorExpired(), tenant.ReasonCursorExpired, connect.CodeOutOfRange},
		{tenant.HistoryChanged(), tenant.ReasonHistoryChanged, connect.CodeOutOfRange},
	}
	for _, c := range cases {
		e := ToConnect(c.err)
		r, ok := ReasonOfError(e)
		if e.Code() != c.code || !ok || r.Reason != c.reason || r.Domain != Domain || len(r.Metadata) != 0 {
			t.Errorf("%q: code %v, error info %+v %v; want %v %s", e.Message(), e.Code(), r, ok, c.code, c.reason)
		}
	}
	e := ToConnect(master.ReadOnlyError("lab1", master.StateMoved, ""))
	if d := e.Details(); len(d) != 1 || d[0].Type != ErrorInfoType || hex.EncodeToString(d[0].Value) != "0a0c54454e414e545f4d4f56454412066e6f70616373" {
		t.Errorf("TENANT_MOVED's details %v, want one ErrorInfo of 0a0c54454e414e545f4d4f56454412066e6f70616373", d)
	}
	if r, _ := ReasonOfError(ToConnect(fault.New(fault.Invalid, "TENANT_MOVED: a name a caller wrote"))); r.Reason != "INVALID_ARGUMENT" {
		t.Errorf("a message of another kind carries the reason %q, want INVALID_ARGUMENT", r.Reason)
	}
}

// TestEveryKindCarriesErrorInfo guards the promise that every answer
// carries a google.rpc.ErrorInfo: each kind ToConnect maps, an error with
// no kind and an API error made without a fault (Ensure) carry the general
// reason of their code, in the domain, with no metadata; Ensure leaves an
// error that has one as it is.
func TestEveryKindCarriesErrorInfo(t *testing.T) {
	want := map[fault.Kind]string{
		fault.Internal: "INTERNAL", fault.Invalid: "INVALID_ARGUMENT", fault.Unauthenticated: "UNAUTHENTICATED",
		fault.Denied: "PERMISSION_DENIED", fault.NotFound: "NOT_FOUND", fault.Conflict: "ALREADY_EXISTS",
		fault.Unavailable: "UNAVAILABLE", fault.Precondition: "FAILED_PRECONDITION", fault.OutOfRange: "OUT_OF_RANGE",
		fault.Exhausted: "RESOURCE_EXHAUSTED", fault.DataLoss: "DATA_LOSS",
	}
	for _, p := range pairs {
		r, ok := ReasonOfError(ToConnect(fault.New(p.kind, "x")))
		if !ok || r.Reason != want[p.kind] || r.Domain != Domain || len(r.Metadata) != 0 {
			t.Errorf("%v: error info %+v %v, want %s", p.kind, r, ok, want[p.kind])
		}
	}
	if r, _ := ReasonOfError(ToConnect(errors.New("cause"))); r.Reason != "INTERNAL" {
		t.Errorf("an error with no kind: reason %q, want INTERNAL", r.Reason)
	}
	for c, reason := range map[connect.Code]string{
		connect.CodeUnimplemented: "UNIMPLEMENTED", connect.CodeDeadlineExceeded: "DEADLINE_EXCEEDED",
		connect.CodeCanceled: "CANCELED", connect.CodeUnknown: "UNKNOWN", connect.CodeAborted: "ABORTED",
	} {
		e := Ensure(connect.NewError(c, "m"))
		if r, ok := ReasonOfError(e); !ok || r.Reason != reason || r.Domain != Domain || e.Code() != c || e.Message() != "m" {
			t.Errorf("Ensure of %v: %v %q, error info %+v %v; want %s", c, e.Code(), e.Message(), r, ok, reason)
		}
	}
	held := ToConnect(WithReason(fault.New(fault.Precondition, "a hold covers it"), fault.ReasonHeld, map[string]string{"hold": "h1"}, 0))
	if again := Ensure(held); len(again.Details()) != 1 {
		t.Errorf("Ensure added a second detail to an error that had one: %v", again.Details())
	}
	if Ensure(nil) != nil {
		t.Error("Ensure of nil is not nil")
	}
	if e := ToConnect(WithReason(fault.New(fault.NotFound, "no such tenant"), "NOT_FOUND", nil, 0)); e.Message() != "no such tenant" {
		t.Errorf("a general reason marked is written into the message: %q", e.Message())
	}
}

// TestAMarkOfAnotherKindIsNotAnswered guards that a reason is answered only
// on the code of its kind: an error marked HELD whose kind is Internal (a
// layer above wrapped it so) is answered INTERNAL with the fixed message,
// the general reason, no hold metadata and no RetryInfo; a word the table
// does not hold is answered the same way; a case of the right kind still
// carries its metadata.
func TestAMarkOfAnotherKindIsNotAnswered(t *testing.T) {
	marked := WithReason(fault.New(fault.Precondition, "a hold covers it"), fault.ReasonHeld, map[string]string{"hold": "h1"}, 30*time.Second)
	for name, err := range map[string]error{
		"wrapped as internal": fault.Wrap(fault.Internal, marked, "internal error"),
		"marked on internal":  WithReason(errors.New("boom"), fault.ReasonHeld, map[string]string{"hold": "h1"}, time.Second),
		"unknown word":        WithReason(fault.New(fault.Internal, "boom"), "NOT_A_REASON", map[string]string{"k": "v"}, time.Second),
	} {
		e := ToConnect(err)
		r, ok := ReasonOfError(e)
		if e.Code() != connect.CodeInternal || e.Message() != "internal error" && e.Message() != "boom" ||
			!ok || r.Reason != "INTERNAL" || len(r.Metadata) != 0 || len(e.Details()) != 1 {
			t.Errorf("%s: %v %q, error info %+v %v, %d details; want INTERNAL alone", name, e.Code(), e.Message(), r, ok, len(e.Details()))
		}
		if _, has := RetryOfError(e); has {
			t.Errorf("%s: a retry delay on a mark that was not answered", name)
		}
	}
	if r, _ := ReasonOfError(ToConnect(marked)); r.Reason != fault.ReasonHeld || r.Metadata["hold"] != "h1" {
		t.Errorf("a case of its own kind: %+v", r)
	}
}

// TestGeneralReasonsAreNotCases guards that a client can tell a general
// reason from a case by the word alone: no reason of fault's table is any
// code's general reason, IsGeneral says so for every code's and for none
// of the table's, and every case's kind is one the table of codes maps.
func TestGeneralReasonsAreNotCases(t *testing.T) {
	for c := connect.CodeCanceled; c <= connect.CodeUnauthenticated; c++ {
		if !IsGeneral(General(c)) {
			t.Errorf("IsGeneral(%s) is false", General(c))
		}
	}
	for _, c := range fault.Cases() {
		if IsGeneral(c.Reason) {
			t.Errorf("the case %s is spelled as a general reason", c.Reason)
		}
		if Code(c.Kind) == connect.CodeInternal {
			t.Errorf("the case %s is of a kind answered as INTERNAL", c.Reason)
		}
	}
	if General(connect.Code(99)) != "UNKNOWN" || General(0) != "UNKNOWN" {
		t.Errorf("a code past the sixteen is %s, %s; want UNKNOWN", General(connect.Code(99)), General(0))
	}
	if IsGeneral("") || IsGeneral("not_found") {
		t.Error("IsGeneral accepts a word that is no code's general reason")
	}
}

// TestStatusBytes guards the google.rpc.Status the server writes into
// grpc-status-details-bin for an answer it builds itself, against the bytes
// the googleapis-common-protos Python package writes for
// Status(code 16, message "no key", details [Any(ErrorInfo(reason
// "UNAUTHENTICATED", domain "nopacs"))]).
func TestStatusBytes(t *testing.T) {
	e := Ensure(connect.NewError(connect.CodeUnauthenticated, "no key"))
	const want = "081012066e6f206b65791a450a28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12190a0f554e41555448454e5449434154454412066e6f70616373"
	if got := hex.EncodeToString(StatusBytes(e)); got != want {
		t.Errorf("Status bytes\n got %s\nwant %s", got, want)
	}
}

// TestRetryDelayCap guards that a retry delay a server sends beyond a day, a
// negative one (a varint with its top bit set) and nanoseconds past a second
// are ignored, while a day exactly is taken.
func TestRetryDelayCap(t *testing.T) {
	build := func(secs, nanos uint64, hasNanos bool) *connect.Error {
		var dur []byte
		dur = protowire.AppendTag(dur, 1, protowire.VarintType)
		dur = protowire.AppendVarint(dur, secs)
		if hasNanos {
			dur = protowire.AppendTag(dur, 2, protowire.VarintType)
			dur = protowire.AppendVarint(dur, nanos)
		}
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, dur)
		return connect.NewError(connect.CodeUnavailable, "x").WithDetail(&connect.ErrorDetail{Type: RetryInfoType, Value: b})
	}
	if d, ok := RetryOfError(build(86400, 0, false)); !ok || d != 24*time.Hour {
		t.Errorf("a day: %v %v", d, ok)
	}
	for name, e := range map[string]*connect.Error{
		"above a day":      build(86401, 0, false),
		"years":            build(1<<40, 0, false),
		"negative":         build(^uint64(0), 0, false),
		"wraps to small":   build(1<<63+5, 0, false),
		"nanos over a sec": build(1, 2_000_000_000, true),
	} {
		if d, ok := RetryOfError(e); ok || d != 0 {
			t.Errorf("%s: delay %v taken", name, d)
		}
	}
}
