package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/master"
)

// testGetStatus guards GetStatus: reader on the tenant and operator on the
// installation; a tenant's status holds its database's state, its counts
// and feed cursor in one snapshot, each storage with its used bytes and
// capacity, and each receiver with its health and whether it admits any
// caller; every tenant's comes paged; a page size or token with one tenant
// is invalid; the entry goes to the log of the scope asked for.
func testGetStatus(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	f.failedReception(t, "scanner1", study)
	get := func(r *v1.GetStatusRequest) func(clients) outcome {
		return func(c clients) outcome { return call(c.monitor.GetStatus, r) }
	}
	one := &v1.GetStatusRequest{Scope: tenantScope("lab1")}
	every := &v1.GetStatusRequest{Scope: installation()}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: get(one), code: connect.CodePermissionDenied},
		{name: "reader on the tenant", as: f.lab1Reader, call: get(one),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"scope": "tenant/lab1"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.GetStatusResponse)
				if len(res.GetTenants()) != 1 || res.GetNextPageToken() != "" || res.GetTime() == nil {
					t.Fatalf("%v", res)
				}
				s := res.GetTenants()[0]
				if s.GetTenant() != "lab1" || s.GetDatabaseState() != v1.TenantState_TENANT_STATE_READY || s.GetEventCursor() == "" {
					t.Errorf("the tenant's state and cursor: %v", s)
				}
				if c := s.GetCounts(); c == nil || len(c.GetReceptionsInProgress()) != 0 || c.GetInstances() != 0 {
					t.Errorf("the counts: %v (a failed reception is not in progress)", c)
				}
				if len(s.GetStorages()) != 1 || s.GetStorages()[0].GetStorage() != "main" || s.GetStorages()[0].GetUsedBytes() != 0 {
					t.Errorf("the storages: %v", s.GetStorages())
				}
				if len(s.GetReceivers()) != 1 || s.GetReceivers()[0].GetReceiver() != "scanner1" || s.GetReceivers()[0].GetHealth() == nil {
					t.Errorf("the receivers: %v", s.GetReceivers())
				}
				if s.GetThroughput() == nil {
					t.Error("no throughput")
				}
			}},
		{name: "reader on the installation, every tenant", as: f.instReader, call: get(every), code: connect.CodePermissionDenied},
		{name: "operator on the installation, every tenant", as: f.instOperator, call: get(every),
			audit: &wantAudit{log: "", outcome: master.OutcomeDone, details: map[string]string{"scope": "installation"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.GetStatusResponse)
				names := []string{}
				for _, s := range res.GetTenants() {
					names = append(names, s.GetTenant())
				}
				if len(names) < 2 || names[0] != "lab1" {
					t.Errorf("every tenant's status names %v, want lab1 first of them all", names)
				}
			}},
		{name: "a page of one", as: f.instOperator, call: get(&v1.GetStatusRequest{Scope: installation(), PageSize: 1}),
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.GetStatusResponse)
				if len(res.GetTenants()) != 1 || res.GetNextPageToken() == "" {
					t.Errorf("a page of one: %d tenants, next %q", len(res.GetTenants()), res.GetNextPageToken())
				}
			}},
		{name: "a page size with one tenant", as: f.lab1Reader, call: get(&v1.GetStatusRequest{Scope: tenantScope("lab1"), PageSize: 5}),
			code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "no scope", as: f.instOperator, call: get(&v1.GetStatusRequest{}), code: connect.CodeInvalidArgument, msg: "scope"},
	})
}

// testWatch guards Watch: reader on the tenant and operator on the
// installation; a stream opens with a keep-alive, then carries an event a
// change wrote, with its tenant and payload; a cursor with every tenant,
// or tenant cursors with one, is invalid, and so is an unspecified type;
// the stream's opening and its end are each an entry in its scope's log.
func testWatch(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	open := func(c clients, r *v1.WatchRequest) (nopacsv1connect.MonitorServiceWatchClientStream, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s, err := c.monitor.Watch(ctx, r)
		if err != nil {
			cancel()
			return s, nil, err
		}
		return s, cancel, nil
	}

	// No grant, and the refusals of a malformed request, answer at once.
	for _, c := range []struct {
		name string
		as   caller
		req  *v1.WatchRequest
		code connect.Code
	}{
		{"no grant", f.noGrant, &v1.WatchRequest{Scope: tenantScope("lab1")}, connect.CodePermissionDenied},
		{"a reader on the installation, every tenant", f.instReader, &v1.WatchRequest{Scope: installation()}, connect.CodePermissionDenied},
		{"a cursor with every tenant", f.instOperator, &v1.WatchRequest{Scope: installation(), Cursor: "x"}, connect.CodeInvalidArgument},
		{"tenant cursors with one tenant", f.lab1Reader, &v1.WatchRequest{Scope: tenantScope("lab1"), TenantCursors: map[string]string{"a": ""}}, connect.CodeInvalidArgument},
		{"an unspecified type", f.lab1Reader, &v1.WatchRequest{Scope: tenantScope("lab1"), Types: []v1.EventType{v1.EventType_EVENT_TYPE_UNSPECIFIED}}, connect.CodeInvalidArgument},
		{"a cursor not made by a Watch", f.lab1Reader, &v1.WatchRequest{Scope: tenantScope("lab1"), Cursor: "not-a-cursor"}, connect.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			if o := watchOnce(f.as(c.as), c.req); o.code() != c.code {
				t.Fatalf("got %v, want %v", o.err, c.code)
			}
		})
	}

	t.Run("a reader's stream of one tenant", func(t *testing.T) {
		s, cancel, err := open(f.as(f.lab1Reader), &v1.WatchRequest{Scope: tenantScope("lab1")})
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		first, err := s.Receive()
		if err != nil || first.GetKeepAlive() == nil || first.GetKeepAlive().GetCursor() == "" {
			t.Fatalf("the first message: %v (%v), want a keep-alive with a cursor", first, err)
		}
		id := f.failedReception(t, "scanner1", study)
		var steps []v1.ReceptionStep
		for len(steps) < 2 {
			m, err := s.Receive()
			if err != nil {
				t.Fatalf("the stream ended after %v: %v", steps, err)
			}
			if e := m.GetEvent(); e != nil {
				if e.GetTenant() != "lab1" || e.GetReceptionStep().GetReception() != id || e.GetCursor() == "" {
					t.Fatalf("an event: %v", e)
				}
				steps = append(steps, e.GetReceptionStep().GetStep())
			}
		}
		if steps[0] != v1.ReceptionStep_RECEPTION_STEP_RECEIVING || steps[1] != v1.ReceptionStep_RECEPTION_STEP_FAILED {
			t.Fatalf("the steps carried: %v, want receiving then failed", steps)
		}
		cancel()
		_ = s.Close()
		// The opening, and the end with the two events it carried, in the
		// tenant's log.
		f.waitAudit(t, "lab1", map[string]string{"stream": "opened"})
		f.waitAudit(t, "lab1", map[string]string{"stream": "ended", "events": "2"})
	})

	t.Run("an operator's stream of every tenant", func(t *testing.T) {
		s, cancel, err := open(f.as(f.instOperator), &v1.WatchRequest{Scope: installation()})
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		first, err := s.Receive()
		if err != nil || !first.GetKeepAlive().GetFull() {
			t.Fatalf("the first message: %v (%v), want a full keep-alive", first, err)
		}
		id := f.failedReception(t, "scanner1", study)
		for {
			m, err := s.Receive()
			if err != nil {
				t.Fatalf("the stream of every tenant ended without the event: %v", err)
			}
			if e := m.GetEvent(); e != nil && e.GetReceptionStep().GetReception() == id {
				if e.GetTenant() != "lab1" {
					t.Fatalf("an event of every tenant's stream names %q", e.GetTenant())
				}
				return
			}
		}
	})
}

// waitAudit waits for an entry in the log of the tenant named log (the
// installation's for "") whose details hold want, failing the test after a
// bound: a stream's entries are written when it opens and when it ends,
// not at the end of a call.
func (e *env) waitAudit(t *testing.T, log string, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var entries []master.AuditEntry
		if log == "" {
			entries = e.masterLog()
		} else {
			e.flush()
			entries = e.tenantLog(log)
		}
		for _, en := range entries {
			match := true
			for k, v := range want {
				match = match && en.Details[k] == v
			}
			if match {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no entry with %v in log %q", want, log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
