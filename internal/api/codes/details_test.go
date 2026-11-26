package codes

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

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
// RetryInfo detail only with a delay; an error not marked carries neither.
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
	if len(plain.Details()) != 0 {
		t.Errorf("an error not marked carries details %v", plain.Details())
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
// same word carries no detail. The bytes of one are those the
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
	if e := ToConnect(fault.New(fault.Invalid, "TENANT_MOVED: a name a caller wrote")); len(e.Details()) != 0 {
		t.Errorf("a message of another kind carries details %v", e.Details())
	}
}
