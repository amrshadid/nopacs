package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// replaceCatalog makes a rebuild of replace mode of lab1 as its admin and
// works it with the rebuild supervisor until it has made its shadow the
// catalog in use, which begins a new history of the tenant's event feed.
func (f *fixture) replaceCatalog(t *testing.T) {
	t.Helper()
	made, err := f.as(f.lab1Admin).jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Rebuild{
		Rebuild: &v1.RebuildJob{Mode: v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE}}})
	if err != nil {
		t.Fatal(err)
	}
	opener := place.Opener{Values: settingvalue.Reader{Master: f.master}, Keys: f.keys, StaleUploadAge: -1}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := f.master.TenantByName(ctx, name)
		return row.ID, err
	}
	sup, err := rebuild.New(rebuild.Config{
		Tenants: envTenants{f.env},
		OpenStorages: func(name string, p tenant.Pipeline) rebuild.Storages {
			return checkedread.NewStores(opener, tenantID, p, name)
		},
		PollInterval: 20 * time.Millisecond,
		Decide:       rebuild.ByRow,
		Keys:         f.keys,
		TenantID:     tenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = sup.Stop(sctx)
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		got, err := f.as(f.lab1Admin).jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: "lab1", Id: made.GetJob().GetId()})
		if err != nil {
			t.Fatal(err)
		}
		// Done is activated for a replace: the test reads the new history
		// from the status after.
		if j := got.GetJob(); j.GetState() == v1.JobState_JOB_STATE_DONE {
			return
		} else if j.GetState() == v1.JobState_JOB_STATE_FAILED || j.GetState() == v1.JobState_JOB_STATE_CANCELED {
			t.Fatalf("the rebuild ended %v: %s", j.GetState(), j.GetReason())
		}
		if time.Now().After(deadline) {
			t.Fatal("the rebuild did not end in time")
		}
		sup.Poke("lab1")
		time.Sleep(20 * time.Millisecond)
	}
}

// statusHistory returns lab1's status cursor and history, as its reader.
func (f *fixture) statusHistory(t *testing.T) (string, string) {
	t.Helper()
	res, err := f.as(f.lab1Reader).monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: tenantScope("lab1")})
	if err != nil || len(res.GetTenants()) != 1 {
		t.Fatalf("GetStatus: %v (%v)", res, err)
	}
	return res.GetTenants()[0].GetEventCursor(), res.GetTenants()[0].GetEventHistory()
}

// endsWithHistoryChanged receives from s until it ends, and fails the test
// unless it ends with OUT_OF_RANGE and a message that begins with the
// reason EVENT_HISTORY_CHANGED and holds want.
func endsWithHistoryChanged(t *testing.T, what string, s nopacsv1connect.MonitorServiceWatchClientStream, want string) {
	t.Helper()
	for {
		m, err := s.Receive()
		if err == nil {
			if e := m.GetEvent(); e != nil && e.GetTenant() == "lab1" {
				t.Fatalf("%s: an event of the new history reached a stream of the old: %v", what, e)
			}
			continue
		}
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeOutOfRange || !strings.HasPrefix(ce.Message(), tenant.ReasonHistoryChanged+":") ||
			!strings.Contains(ce.Message(), want) {
			t.Fatalf("%s ended with %v, want OUT_OF_RANGE beginning %s and holding %q", what, err, tenant.ReasonHistoryChanged, want)
		}
		return
	}
}

// TestWatchAcrossANewHistory guards the event history on the wire: a
// status names it (event_history), and so does the keep-alive that opens a
// stream of one tenant (event_history) and of every tenant
// (tenant_event_histories, with every tenant whose cursor it names); a
// rebuild that replaces the catalog begins a new one; a stream of one
// tenant and one of every tenant open across it end with OUT_OF_RANGE and
// a message beginning EVENT_HISTORY_CHANGED, the latter naming the tenant;
// a cursor of the old history is refused the same way when a stream opens;
// and a stream from the new status's cursor carries keep-alives of the new
// history.
func TestWatchAcrossANewHistory(t *testing.T) {
	f := newFixture(t)
	oldCursor, oldHistory := f.statusHistory(t)
	if len(oldHistory) != 32 || oldCursor == "" {
		t.Fatalf("the status names the cursor %q and history %q, want both", oldCursor, oldHistory)
	}
	sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	one, err := f.as(f.lab1Reader).monitor.Watch(sctx, &v1.WatchRequest{Scope: tenantScope("lab1"), Cursor: oldCursor})
	if err != nil {
		t.Fatal(err)
	}
	first, err := one.Receive()
	if err != nil || first.GetKeepAlive().GetEventHistory() != oldHistory || first.GetKeepAlive().GetCursor() != oldCursor {
		t.Fatalf("the opening keep-alive of one tenant: %v (%v), want cursor %q of history %q", first, err, oldCursor, oldHistory)
	}
	all, err := f.as(f.instOperator).monitor.Watch(sctx, &v1.WatchRequest{Scope: installation()})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := all.Receive()
	if err != nil || !opening.GetKeepAlive().GetFull() {
		t.Fatalf("the opening keep-alive of every tenant: %v (%v)", opening, err)
	}
	k := opening.GetKeepAlive()
	if k.GetTenantEventHistories()["lab1"] != oldHistory {
		t.Errorf("the opening keep-alive of every tenant names lab1's history %q, want %q", k.GetTenantEventHistories()["lab1"], oldHistory)
	}
	for name := range k.GetTenantCursors() {
		if _, ok := k.GetTenantEventHistories()[name]; !ok {
			t.Errorf("the opening keep-alive names a cursor of %s and no history", name)
		}
	}

	f.replaceCatalog(t)

	newCursor, newHistory := f.statusHistory(t)
	if len(newHistory) != 32 || newHistory == oldHistory {
		t.Fatalf("after the rebuild the status names the history %q, before %q", newHistory, oldHistory)
	}
	endsWithHistoryChanged(t, "a stream of one tenant open across the rebuild", one, "")
	endsWithHistoryChanged(t, "a stream of every tenant open across the rebuild", all, "(tenant lab1)")
	if o := watchOnce(f.as(f.lab1Reader), &v1.WatchRequest{Scope: tenantScope("lab1"), Cursor: oldCursor}); o.code() != connect.CodeOutOfRange ||
		!strings.Contains(o.err.Error(), tenant.ReasonHistoryChanged+":") {
		t.Errorf("a stream opened from a cursor of the old history: %v, want OUT_OF_RANGE with %s", o.err, tenant.ReasonHistoryChanged)
	}
	again, err := f.as(f.lab1Reader).monitor.Watch(sctx, &v1.WatchRequest{Scope: tenantScope("lab1"), Cursor: newCursor})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := again.Receive(); err != nil || m.GetKeepAlive().GetEventHistory() != newHistory {
		t.Errorf("the opening keep-alive from the new status's cursor: %v (%v), want history %q", m, err, newHistory)
	}
	_ = again.Close()
}
