package correct

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// hangUpAt returns a seam that ends cancel's request at the step named,
// as a client hanging up there does, and stops nothing.
func hangUpAt(step string, cancel context.CancelFunc) func(string) error {
	return func(s string) error {
		if s == step {
			cancel()
		}
		return nil
	}
}

// replaceRequest holds a corrected copy of a scan's first instance as a
// different file and returns the replace of it, and the instance's new
// revision key.
func (r *rig) replaceRequest() (ReplaceRequest, string) {
	r.t.Helper()
	s := r.scanWith(2)
	path := pathOf(s)
	in := r.held(path)[0]
	old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
	if err != nil {
		r.t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	key, err := layout.RevisionKey(path, in.SOPInstanceUID, 2)
	if err != nil {
		r.t.Fatal(err)
	}
	return ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()}, key
}

// onlyCorrection returns the rig's one correction.
func (r *rig) onlyCorrection() tenant.Correction {
	r.t.Helper()
	all, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 10})
	if err != nil || len(all) != 1 {
		r.t.Fatalf("%d corrections (%v), want one", len(all), err)
	}
	return r.state(all[0].ID)
}

// TestADeleteWhoseClientHangsUpAfterItsPrepareIsPublished guards a delete
// whose request ends (the client hangs up) right after its prepare: the
// delete goes on to its publish, apart from the request, and is answered
// committed, never left prepared holding its target.
func TestADeleteWhoseClientHangsUpAfterItsPrepareIsPublished(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(time.Hour, time.Hour, nil)
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	e.SetCrashAt(hangUpAt("prepared", cancel))
	target := tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}
	p, err := r.p.PreviewDelete(r.ctx, target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.Delete(ctx, DeleteRequest{Target: target, Reason: "x", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("a delete whose client hung up after its prepare: %s (%s), %v; want committed", c.State, c.StateReason, err)
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.RecordsState != record.StateCommitted {
		t.Fatalf("the delete is %s, its records %s", got.State, got.RecordsState)
	}
}

// TestAReplaceWhoseClientHangsUpAfterItsPrepareIsPublished guards a replace
// whose request ends right after its prepare: it writes its new revision
// and is published apart from the request, never left prepared with its
// object half written.
func TestAReplaceWhoseClientHangsUpAfterItsPrepareIsPublished(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(time.Hour, time.Hour, nil)
	req, key := r.replaceRequest()
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	e.SetCrashAt(hangUpAt("prepared", cancel))
	c, err := e.Replace(ctx, req)
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("a replace whose client hung up after its prepare: %s (%s), %v; want committed", c.State, c.StateReason, err)
	}
	if _, ok := r.objects()[key]; !ok {
		t.Error("the new revision is not stored")
	}
}

// TestThePassAbandonsADeleteARequestLeftPrepared guards what takes up a
// delete a request left prepared with no request holding it (here its
// engine call stopped right after the prepare) while the server runs: the
// pass after its first Recover abandons it, with the reason, its records
// rewritten so, and a new delete of the same target then goes through.
// Without it the delete holds its target until the next start.
func TestThePassAbandonsADeleteARequestLeftPrepared(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(20*time.Millisecond, time.Hour, nil)
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(crashAt("prepared"))
	target := tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}
	c, err := r.deleteNow(e, target)
	release()
	if err == nil || c.State != record.StatePrepared {
		t.Fatalf("the delete left: %s, %v; want prepared", c.State, err)
	}
	got := r.waitState(c.ID, record.StateAbandoned)
	if got.StateReason != reasonLeft {
		t.Errorf("abandoned for %q, want %q", got.StateReason, reasonLeft)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.state(c.ID).RecordsState != record.StateAbandoned {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned delete's records were not rewritten")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e2, release2, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if again, err := r.deleteNow(e2, target); err != nil || again.State != record.StateCommitted {
		t.Fatalf("a new delete of the same target: %s, %v", again.State, err)
	}
}

// TestThePassAbandonsAReplaceARequestLeftPrepared guards the same for a
// replace left prepared after it wrote its new revision: the pass abandons
// it and removes that revision, so the instance's bytes are the archived
// ones again.
func TestThePassAbandonsAReplaceARequestLeftPrepared(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(20*time.Millisecond, time.Hour, nil)
	req, key := r.replaceRequest()
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(crashAt("object"))
	c, err := e.Replace(r.ctx, req)
	release()
	if err == nil || c.State != record.StatePrepared {
		t.Fatalf("the replace left: %s, %v; want prepared", c.State, err)
	}
	if _, ok := r.objects()[key]; !ok {
		t.Fatal("the replace left wrote no new revision")
	}
	got := r.waitState(c.ID, record.StateAbandoned)
	if got.StateReason != reasonLeft {
		t.Errorf("abandoned for %q, want %q", got.StateReason, reasonLeft)
	}
	if _, ok := r.objects()[key]; ok {
		t.Error("the abandoned replace's new revision is still stored")
	}
}

// TestThePassLeavesARequestsPreparedDeleteAlone guards the other side of
// the pass's abandon: a delete prepared by a request still in flight is
// not abandoned under it, however many passes run, and is published.
func TestThePassLeavesARequestsPreparedDeleteAlone(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(10*time.Millisecond, time.Hour, nil)
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	e.SetCrashAt(func(step string) error {
		if step == "prepared" {
			time.Sleep(200 * time.Millisecond) // many passes
		}
		return nil
	})
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("a delete slow after its prepare: %s (%s), %v; want committed", c.State, c.StateReason, err)
	}
}

// TestARequestWaitsForTheTenantsFirstRecover guards the order of a start: a
// request is not served before its tenant's first Recover has taken up
// what the last stop left (which abandons what it finds prepared). While
// Recover cannot finish (its storage does not answer) a request waits up to
// the bound and is answered unavailable; once it has, requests are served.
func TestARequestWaitsForTheTenantsFirstRecover(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	left := r.engine()
	left.SetCrashAt(crashAt("prepared"))
	if _, err := r.deleteNow(left, tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}); err == nil {
		t.Fatal("the delete was not left prepared")
	}
	gate := make(chan struct{})
	var blocked atomic.Bool
	blocked.Store(true)
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: time.Hour, RecoverWait: 200 * time.Millisecond,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(ctx context.Context, _ string) (blob.Store, error) {
				if blocked.Load() {
					select {
					case <-gate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return r.storage, nil
			}}, nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	start := time.Now()
	if _, _, err := es.Use(r.ctx, "lab", r.p.(tenant.Store)); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a request while Recover cannot finish: %v, want unavailable", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("the request waited %v, not the bound", waited)
	}
	blocked.Store(false)
	close(gate)
	_, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatalf("a request once Recover can finish: %v", err)
	}
	release()
	if c := r.onlyCorrection(); c.State != record.StateAbandoned {
		t.Errorf("the delete a stop left is %s, want abandoned by Recover", c.State)
	}
}

// TestStopWaitsForARequestBeforeClosingStorages guards the server's stop:
// the storages an engine opened are not closed while a request still holds
// that engine; Stop waits, and closes them once the request lets it go.
func TestStopWaitsForARequestBeforeClosingStorages(t *testing.T) {
	r := newRig(t, false)
	var closed atomic.Bool
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: time.Hour,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }},
				func() error { closed.Store(true); return nil }, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- es.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while a request held its engine: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if closed.Load() {
		t.Fatal("the storages were closed under a request")
	}
	release()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the request let its engine go")
	}
	if !closed.Load() {
		t.Error("Stop did not close the storages")
	}
}

// TestAReplacedEngineIsClosedOnceLetGo guards a tenant whose database is
// opened anew (removed and made again) while a request holds its engine: a
// new engine serves the next request, the old one's storages stay open
// under the request that holds it and are closed once it lets go, not left
// open until the server stops.
func TestAReplacedEngineIsClosedOnceLetGo(t *testing.T) {
	r := newRig(t, false)
	again, err := (driver.SQLiteTenants{DataDir: r.dataDir}).Open(r.ctx, rigTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	var closes atomic.Int64
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: time.Hour,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }},
				func() error { closes.Add(1); return nil }, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	old, releaseOld, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	cur, releaseNew, err := es.Use(r.ctx, "lab", again)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	if cur == old {
		t.Fatal("the database opened anew was given the old engine")
	}
	time.Sleep(50 * time.Millisecond)
	if closes.Load() != 0 {
		t.Fatal("the replaced engine was closed under the request that holds it")
	}
	releaseOld()
	deadline := time.Now().Add(10 * time.Second)
	for closes.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the replaced engine was not closed once let go")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAUsedTokenWithAnotherTargetIsRefused guards a keyed token that made a
// delete, sent again with another target: it is refused as a token that
// does not match, never answered with the delete it made, which would tell
// the caller of a correction on a target it did not name.
func TestAUsedTokenWithAnotherTargetIsRefused(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e := r.use(r.engines(time.Hour, time.Hour, keyedConfirm(t)))
	target := tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}
	p, err := e.Preview(r.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "x", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	other := tenant.CorrectionTarget{Subject: "sub-nobody"}
	got, err := e.Delete(r.ctx, DeleteRequest{Target: other, Reason: "x", Confirm: p.Confirm, Audit: r.audit()})
	if fault.KindOf(err) != fault.Precondition || got.ID == first.ID {
		t.Fatalf("the used token with another target: %q, %v; want the precondition", got.ID, err)
	}
	if again, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "x", Confirm: p.Confirm, Audit: r.audit()}); err != nil || again.ID != first.ID {
		t.Fatalf("the used token with its own target: %q, %v; want %s", again.ID, err, first.ID)
	}
}
