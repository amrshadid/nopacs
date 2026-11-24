package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testHandoffs stands for the server's drain and owner check: it answers
// as a test sets it, and counts the drains and advances asked.
type testHandoffs struct {
	mu         sync.Mutex
	drainErr   error
	reclaimErr error
	drained    []string
	advanced   []string
	// owners, when set, takes the owner records further for Advance, as
	// the server's owner check does; generations are what each Advance
	// wrote, in order.
	owners      *backup.OwnerCheck
	generations [][]backup.Owner
	// gate, when set, holds each drain: the drain sends on it, then waits
	// to receive on it before it answers, so a test acts while an export
	// is draining its tenant, moved.
	gate chan struct{}
}

var _ api.Handoffs = (*testHandoffs)(nil)

// Drain implements api.Handoffs.
func (h *testHandoffs) Drain(_ context.Context, name string) error {
	h.mu.Lock()
	h.drained = append(h.drained, name)
	gate, err := h.gate, h.drainErr
	h.mu.Unlock()
	if gate != nil {
		gate <- struct{}{}
		<-gate
	}
	return err
}

// Advance implements api.Handoffs: through owners when set; otherwise the
// fixture's storages are never opened, and it writes no owner record.
func (h *testHandoffs) Advance(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanced = append(h.advanced, name)
	if h.owners == nil {
		return nil, nil
	}
	got, err := h.owners.Advance(ctx, id, name, st)
	if err == nil {
		h.generations = append(h.generations, got)
	}
	return got, err
}

// Reclaim implements api.Handoffs.
func (h *testHandoffs) Reclaim(context.Context, string, string, tenant.Store) ([]backup.Owner, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return nil, h.reclaimErr
}

// hold makes every drain wait on a gate until the test lets it go, and
// returns the gate (testHandoffs.gate).
func (h *testHandoffs) hold(t *testing.T) chan struct{} {
	t.Helper()
	gate := make(chan struct{})
	h.mu.Lock()
	h.gate = gate
	h.mu.Unlock()
	t.Cleanup(func() {
		h.mu.Lock()
		h.gate = nil
		h.mu.Unlock()
	})
	return gate
}

// set sets what the next drains and checks answer.
func (h *testHandoffs) set(drain, reclaim error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drainErr, h.reclaimErr = drain, reclaim
}

// exportedTenant is one export's stream, read to its end.
type exportedTenant struct {
	data      []byte
	chunks    int
	summary   *v1.TenantExportSummary
	err       error
	requestID string
}

// export reads an export of name as c to its end.
func export(c clients, name string) exportedTenant {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	var out exportedTenant
	st, err := c.backups.ExportTenant(ctx, &v1.ExportTenantRequest{Tenant: name})
	if err == nil {
		defer func() { _ = st.Close() }()
		for {
			m, rerr := st.Receive()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				err = rerr
				break
			}
			if d := m.GetData(); d != nil {
				out.data = append(out.data, d...)
				out.chunks++
			}
			if s := m.GetSummary(); s != nil {
				out.summary = s
			}
		}
	}
	out.err, out.requestID = err, info.ResponseHeader().Get(api.RequestIDHeader)
	return out
}

// code returns the stream's code, or 0 for success.
func (x exportedTenant) code() connect.Code {
	if x.err == nil {
		return 0
	}
	return connect.CodeOf(x.err)
}

// tenantState returns the tenant's state as GetTenant answers it.
func (e *env) tenantState(t *testing.T, name string) v1.TenantState {
	t.Helper()
	o := call(e.as(local).tenants.GetTenant, &v1.GetTenantRequest{Name: name})
	if o.err != nil {
		t.Fatal(o.err)
	}
	return o.res.(*v1.GetTenantResponse).GetTenant().GetState()
}

// testExportTenant guards ExportTenant: admin on the installation only; a
// name invalid or unknown refused; a drain that fails gives the tenant back
// ready, with the entry that made it moved and the one that gave it back;
// an export streams a file the transfer key alone opens, whose SHA-256 the
// summary holds, with the tenant's section, and leaves the tenant moved
// (GetTenant says so), with its two entries; a second export, and a
// change into the moved tenant, are refused as a precondition naming
// TENANT_MOVED, while its reads are served.
func testExportTenant(t *testing.T) {
	f := newFixture(t)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: func(c clients) outcome { x := export(c, "lab1"); return outcome{err: x.err, requestID: x.requestID} },
			code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: func(c clients) outcome {
			x := export(c, "lab1")
			return outcome{err: x.err, requestID: x.requestID}
		}, code: connect.CodePermissionDenied},
		{name: "admin of the tenant only", as: f.lab1Admin, call: func(c clients) outcome {
			x := export(c, "lab1")
			return outcome{err: x.err, requestID: x.requestID}
		}, code: connect.CodePermissionDenied},
		{name: "a name that is not one", as: f.instAdmin, call: func(c clients) outcome {
			x := export(c, "Lab 1")
			return outcome{err: x.err, requestID: x.requestID}
		}, code: connect.CodeInvalidArgument, audit: failed("tenant")},
		{name: "never was", as: f.instAdmin, call: func(c clients) outcome {
			x := export(c, "nolab")
			return outcome{err: x.err, requestID: x.requestID}
		}, code: connect.CodeNotFound, audit: failed("")},
	})

	f.handoffs.set(fault.New(fault.Unavailable, "the pipeline did not stop"), nil)
	x := export(f.as(f.instAdmin), "lab1")
	if x.code() != connect.CodeUnavailable || x.summary != nil {
		t.Fatalf("an export whose drain failed: %v, summary %v", x.err, x.summary)
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("after a failed export the tenant is %v", s)
	}
	entries := byHandoff(entriesOf(f.masterLog(), x.requestID))
	if len(entries) != 2 || entries["moved"].Details["stream"] != "opened" || entries["ready"].Outcome != master.OutcomeFailed ||
		!strings.Contains(entries["ready"].Reason, "the tenant is ready again") || entries["ready"].Details[master.DetailOldState] != "moved" {
		t.Fatalf("a failed export's entries: %+v", entries)
	}

	f.handoffs.set(nil, nil)
	x = export(f.as(f.instAdmin), "lab1")
	if x.err != nil || x.summary == nil {
		t.Fatalf("an export: %v", x.err)
	}
	key := x.summary.GetTransferKey()
	sum := x.summary.GetSummary()
	digest := sha256.Sum256(x.data)
	if len(key) != secret.TransferKeySize || sum.GetFormat() != backup.ExportFormat || sum.GetSizeBytes() != int64(len(x.data)) ||
		!bytes.Equal(sum.GetSha256(), digest[:]) || sum.GetKeyVersion() != 0 || len(sum.GetSections()) != 1 ||
		sum.GetSections()[0].GetTenant() != "lab1" || sum.GetInstallationId() != testInstallation {
		t.Fatalf("the export's summary: %v (%d bytes of data, key of %d bytes)", sum, len(x.data), len(key))
	}
	tk, err := secret.TransferKeys(key)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := backup.Verify(bytes.NewReader(x.data), tk); err != nil || v.Header.Format != backup.ExportFormat || v.Starts[0].Tenant != "lab1" {
		t.Fatalf("the file under its transfer key: %+v %v", v.Header, err)
	}
	if _, err := backup.Verify(bytes.NewReader(x.data), f.keys); err == nil {
		t.Fatal("the file opened under the installation's key file")
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_MOVED {
		t.Fatalf("after the export the tenant is %v", s)
	}
	all := entriesOf(f.masterLog(), x.requestID)
	var opened, ended master.AuditEntry
	for _, en := range all {
		switch en.Details["stream"] {
		case "opened":
			opened = en
		case "ended":
			ended = en
		}
	}
	if len(all) != 2 || opened.Details["handoff"] != "moved" || opened.TenantName != "lab1" || opened.Details[master.DetailNewState] != "moved" ||
		ended.Details["ended"] != "complete" || ended.Details["sha256"] == "" || ended.Details["size_bytes"] == "" || ended.Outcome != master.OutcomeDone {
		t.Fatalf("an export's entries: %+v", all)
	}
	for _, en := range all {
		if strings.Contains(en.Reason+strings.Join(mapValues(en.Details), " "), string(key)) {
			t.Fatal("an entry holds the transfer key")
		}
	}

	again := export(f.as(f.instAdmin), "lab1")
	if again.code() != connect.CodeFailedPrecondition || !strings.Contains(again.err.Error(), master.ReasonTenantMoved) {
		t.Fatalf("a second export: %v", again.err)
	}
	f.run(t, []tcase{
		{name: "a change into the moved tenant", as: f.lab1Admin, call: func(c clients) outcome {
			return call(c.storages.AddStorage, &v1.AddStorageRequest{Tenant: "lab1", Storage: dirStorage("more", "archive", "")})
		}, code: connect.CodeFailedPrecondition, msg: master.ReasonTenantMoved},
		{name: "a read of the moved tenant", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.storages.ListStorages, &v1.ListStoragesRequest{Tenant: "lab1"})
		}},
		{name: "a catalog read of the moved tenant", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.catalog.ListSubjects, &v1.ListSubjectsRequest{Tenant: "lab1"})
		}},
	})
}

// byHandoff returns entries by their hand-off detail.
func byHandoff(entries []master.AuditEntry) map[string]master.AuditEntry {
	out := map[string]master.AuditEntry{}
	for _, en := range entries {
		out[en.Details["handoff"]] = en
	}
	return out
}

// testReclaimTenant guards ReclaimTenant: the local operator alone, a key
// refused whatever its grants; a name invalid or unknown refused; a moved
// tenant whose owner records another installation has taken over refused
// as a precondition, left moved; one whose records still name this
// installation made ready, with its entry; a ready tenant answered as it
// stands.
func testReclaimTenant(t *testing.T) {
	f := newFixture(t)
	if x := export(f.as(f.instAdmin), "lab1"); x.err != nil {
		t.Fatal(x.err)
	}
	reclaim := func(name string) func(clients) outcome {
		return func(c clients) outcome { return call(c.tenants.ReclaimTenant, &v1.ReclaimTenantRequest{Name: name}) }
	}
	f.handoffs.set(nil, fault.New(fault.Precondition, "the storage main's owner record names installation B"))
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: reclaim("lab1"), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: reclaim("lab1"), code: connect.CodePermissionDenied},
		{name: "a key that is admin", as: f.instAdmin, call: reclaim("lab1"), code: connect.CodePermissionDenied, msg: "local operator",
			audit: &wantAudit{outcome: master.OutcomeRefused, tenant: "lab1"}},
		{name: "a name that is not one", as: local, call: reclaim("Lab 1"), code: connect.CodeInvalidArgument, audit: failed("name")},
		{name: "never was", as: local, call: reclaim("nolab"), code: connect.CodeNotFound, audit: failed("")},
		{name: "taken over", as: local, call: reclaim("lab1"), code: connect.CodeFailedPrecondition, msg: "names installation B",
			audit: &wantAudit{outcome: master.OutcomeFailed, tenant: "lab1", object: "tenant/lab1"}},
	})
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_MOVED {
		t.Fatalf("a refused reclaim left the tenant %v", s)
	}
	f.handoffs.set(nil, nil)
	f.run(t, []tcase{
		{name: "reclaimed", as: local, call: reclaim("lab1"),
			audit: &wantAudit{outcome: master.OutcomeDone, tenant: "lab1", object: "tenant/lab1", details: map[string]string{"handoff": "ready", "owner_generations": "*"}},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.ReclaimTenantResponse).GetTenant().GetState(); s != v1.TenantState_TENANT_STATE_READY {
					t.Errorf("the reclaimed tenant is %v", s)
				}
			}},
		{name: "ready already", as: local, call: reclaim("lab1"), audit: done()},
	})
	if _, err := f.registry.Store("lab1"); err != nil {
		t.Fatalf("the reclaimed tenant is not served for changes: %v", err)
	}
}

// TestAnExportGivesItsTenantBackWhenItsKeyIsRevoked guards the end of an
// export that fails before its summary after the key that asked for it was
// revoked: the server gives the tenant back, ready, though the store
// refuses every change by that key, and the entry that says so still names
// the key and says the server gave it back.
func TestAnExportGivesItsTenantBackWhenItsKeyIsRevoked(t *testing.T) {
	f := newFixture(t)
	gate := f.handoffs.hold(t)
	f.handoffs.set(fault.New(fault.Unavailable, "the pipeline did not stop"), nil)
	done := make(chan exportedTenant, 1)
	go func() { done <- export(f.as(f.instAdmin), "lab1") }()
	<-gate
	if o := call(f.as(local).access.RevokeKey, &v1.RevokeKeyRequest{Id: f.instAdminID}); o.err != nil {
		gate <- struct{}{}
		t.Fatalf("the revoke: %v", o.err)
	}
	gate <- struct{}{}
	x := <-done
	if x.summary != nil || x.err == nil {
		t.Fatalf("an export whose drain failed: %v, summary %v", x.err, x.summary != nil)
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("an export that failed before its summary left the tenant %v", s)
	}
	back := byHandoff(entriesOf(f.masterLog(), x.requestID))["ready"]
	if back.Outcome != master.OutcomeFailed || back.KeyID != f.instAdminID || back.Details["given_back_by"] != "server" ||
		back.Details[master.DetailOldState] != "moved" {
		t.Fatalf("the entry that gave the tenant back: %+v", back)
	}
}

// TestAReclaimIsRefusedWhileAnExportRuns guards a reclaim asked while an
// export of the same tenant drains it: the reclaim is refused as a
// precondition naming the export, the tenant stays moved, and the export
// ends with its summary for a tenant still moved.
func TestAReclaimIsRefusedWhileAnExportRuns(t *testing.T) {
	f := newFixture(t)
	gate := f.handoffs.hold(t)
	done := make(chan exportedTenant, 1)
	go func() { done <- export(f.as(f.instAdmin), "lab1") }()
	<-gate
	o := call(f.as(local).tenants.ReclaimTenant, &v1.ReclaimTenantRequest{Name: "lab1"})
	gate <- struct{}{}
	x := <-done
	if connect.CodeOf(o.err) != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "an export of the tenant lab1 is running") {
		t.Fatalf("a reclaim during an export: %v", o.err)
	}
	if x.err != nil || x.summary == nil {
		t.Fatalf("the export: %v, summary %v", x.err, x.summary != nil)
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_MOVED {
		t.Fatalf("an export handed out its transfer key with the tenant %v", s)
	}
	f.handoffs.mu.Lock()
	advanced := slices.Clone(f.handoffs.advanced)
	f.handoffs.mu.Unlock()
	if !slices.Equal(advanced, []string{"lab1"}) {
		t.Fatalf("the export took the owner records further for %v", advanced)
	}
}

// TestAnExportIsNotHandedOutForATenantNoLongerMoved guards the last check
// an export makes before its summary: a tenant made ready meanwhile by
// another way than a reclaim (which is refused while the export runs) gets
// no transfer key, the export fails as a precondition, and the tenant is
// left ready, not moved again.
func TestAnExportIsNotHandedOutForATenantNoLongerMoved(t *testing.T) {
	f := newFixture(t)
	gate := f.handoffs.hold(t)
	done := make(chan exportedTenant, 1)
	go func() { done <- export(f.as(f.instAdmin), "lab1") }()
	<-gate
	entry := master.AuditEntry{ID: "test-ready-again", Time: time.Now(), LocalUser: "501", Method: "test", Outcome: master.OutcomeDone,
		RequestID: "test-ready-again", TenantName: "lab1"}
	if err := f.registry.SetHandoff(context.Background(), "lab1", master.StateReady, "", entry, master.LocalOperator()); err != nil {
		gate <- struct{}{}
		t.Fatalf("the tenant made ready: %v", err)
	}
	gate <- struct{}{}
	x := <-done
	if x.summary != nil || x.code() != connect.CodeFailedPrecondition || !strings.Contains(x.err.Error(), "no longer moved") {
		t.Fatalf("an export of a tenant no longer moved: %v, summary %v", x.err, x.summary != nil)
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("the tenant after the export: %v", s)
	}
}
