package api_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestEveryKindAnswersItsErrorInfo guards, for each kind a handler answers,
// the google.rpc.ErrorInfo it carries, as a client decodes it: the general
// reason of its code for a refusal with no case of its own, and for a case
// of the table of reasons that case with its metadata. The cases whose
// setup needs archived data are asserted with their metadata where that
// data is made: RELABELED (relabeled_test.go), HELD and RETENTION_MINIMUM
// (retention_test.go), STORAGE_SWITCHING (replica_test.go), NOT_ONLINE
// (data_unit_test.go), TENANT_MOVED (handoff_test.go), EVENT_HISTORY_CHANGED
// (monitor_history_test.go), DATA_LOSS (data_test.go); every call of those
// tests also passes checkErrorInfo.
func TestEveryKindAnswersItsErrorInfo(t *testing.T) {
	f := newFixture(t)
	f.downTenant("pgdown")
	createTenant := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE})
		}
	}
	cases := []struct {
		name   string
		as     caller
		call   func(clients) outcome
		code   connect.Code
		reason string
		meta   map[string]string
	}{
		{"invalid", local, createTenant("Not A Name!"), connect.CodeInvalidArgument, "INVALID_ARGUMENT", nil},
		{"unauthenticated", caller{key: "nopacs_000000000000_0000000000000000000000000000000000000000"}, createTenant("lab9"), connect.CodeUnauthenticated, "UNAUTHENTICATED", nil},
		{"denied", f.lab1Reader, createTenant("lab9"), connect.CodePermissionDenied, "PERMISSION_DENIED", nil},
		{"not found", local, func(c clients) outcome { return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: "nosuch"}) },
			connect.CodeNotFound, "NOT_FOUND", nil},
		{"conflict", local, createTenant("lab1"), connect.CodeAlreadyExists, "ALREADY_EXISTS", nil},
		{"precondition", f.lab1Admin, addRule(&v1.RetentionRule{Name: "where", KeepAtLeast: years(1), Selector: &v1.RetentionSelector{Storage: "nowhere"}}),
			connect.CodeFailedPrecondition, "FAILED_PRECONDITION", nil},
		{"unavailable", local, func(c clients) outcome {
			return call(c.storages.ListStorages, &v1.ListStoragesRequest{Tenant: "pgdown"})
		}, connect.CodeUnavailable, "UNAVAILABLE", nil},
	}
	for _, c := range cases {
		o := c.call(f.as(c.as))
		assertErrorInfo(t, c.name, o, c.code, c.reason, c.meta)
	}

	// Internal: a request that reached its handler without the server's
	// check, answered with the fixed message.
	f.decide.Store(false)
	o := createTenant("lab9")(f.as(local))
	f.decide.Store(true)
	assertErrorInfo(t, "internal", o, connect.CodeInternal, "INTERNAL", nil)

	// Exhausted: every trial place taken.
	release := api.HoldTrialPlaces(f.handlers, api.MaxTrialsAtOnce)
	o = tryFiles(unsaved(basicDocument()), corpusFile(t, "ct5n-2062.dcm"))(f.as(f.lab1Operator))
	release()
	assertErrorInfo(t, "exhausted", o, connect.CodeResourceExhausted, "RESOURCE_EXHAUSTED", nil)
}

// assertErrorInfo fails t unless o failed with code and an ErrorInfo of
// reason, the server's domain and exactly meta.
func assertErrorInfo(t *testing.T, name string, o outcome, code connect.Code, reason string, meta map[string]string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(o.err, &ce) || ce.Code() != code {
		t.Errorf("%s: %v, want %v", name, o.err, code)
		return
	}
	r, ok := codes.ReasonOfError(ce)
	if !ok || r.Reason != reason || r.Domain != codes.Domain || len(r.Metadata) != len(meta) {
		t.Errorf("%s: error info %+v %v, want %s %v", name, r, ok, reason, meta)
		return
	}
	for k, v := range meta {
		if r.Metadata[k] != v {
			t.Errorf("%s: metadata %v, want %v", name, r.Metadata, meta)
		}
	}
}

// errorInfoChecked is a client interceptor that checks the
// google.rpc.ErrorInfo of every error a server answered (checkErrorInfo),
// so every call of every test of this package asserts the detail the
// contract promises on every answer, whatever else the test asserts.
func errorInfoChecked(t testing.TB) func(connect.ClientFunc) connect.ClientFunc {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			s, err := next(ctx, spec)
			if err != nil {
				checkErrorInfo(t, spec.Procedure, err)
				return s, err
			}
			return checkedStream{ClientStream: s, t: t, procedure: spec.Procedure}, nil
		}
	}
}

// checkedStream checks the error a stream's Receive returns.
type checkedStream struct {
	connect.ClientStream
	t         testing.TB
	procedure string
}

// Receive reads the next message and checks the error, unless it is the
// stream's clean end.
func (s checkedStream) Receive(msg any) error {
	err := s.ClientStream.Receive(msg)
	if err != nil && !errors.Is(err, io.EOF) {
		checkErrorInfo(s.t, s.procedure, err)
	}
	return err
}

// checkErrorInfo fails t unless err, when it is an answer of the server,
// carries a google.rpc.ErrorInfo in the server's domain whose reason is
// either its code's general reason, with no metadata, or a case of fault's
// table of the kind its code carries, which begins the message and gives
// only the metadata keys the case names. An error the client made itself
// (a canceled call, a deadline of its own) is not the server's and is
// passed.
func checkErrorInfo(t testing.TB, procedure string, err error) {
	var ce *connect.Error
	if !errors.As(err, &ce) || !ce.IsRemote() {
		return
	}
	t.Helper()
	r, ok := codes.ReasonOfError(ce)
	switch {
	case !ok:
		t.Errorf("%s answered %v %q with no google.rpc.ErrorInfo (details %v)", procedure, ce.Code(), ce.Message(), ce.Details())
		return
	case r.Domain != codes.Domain:
		t.Errorf("%s answered an ErrorInfo of the domain %q", procedure, r.Domain)
	}
	if codes.IsGeneral(r.Reason) {
		if r.Reason != codes.General(ce.Code()) || len(r.Metadata) != 0 {
			t.Errorf("%s answered %v with the general reason %s and metadata %v", procedure, ce.Code(), r.Reason, r.Metadata)
		}
		return
	}
	c, ok := fault.CaseOf(r.Reason)
	switch {
	case !ok:
		t.Errorf("%s answered the reason %q, which the table of reasons does not hold", procedure, r.Reason)
		return
	case codes.Code(c.Kind) != ce.Code():
		t.Errorf("%s answered the reason %s with %v, want %v", procedure, r.Reason, ce.Code(), codes.Code(c.Kind))
	case !strings.HasPrefix(ce.Message(), r.Reason+": "):
		t.Errorf("%s answered the reason %s with a message that does not begin with it: %q", procedure, r.Reason, ce.Message())
	}
	for k := range r.Metadata {
		if !slices.Contains(c.Metadata, k) {
			t.Errorf("%s answered %s with the metadata key %q, which the case does not name", procedure, r.Reason, k)
		}
	}
}
