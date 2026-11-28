package servecmd

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// quotaRig is a quota watch over a use and bounds the test changes, with
// the events it appended.
type quotaRig struct {
	t      *testing.T
	use    tenant.QuotaUse
	bounds tenant.QuotaBounds
	failed error
	events []*tenant.QuotaChanged
	w      *quotaWatch
}

func newQuotaRig(t *testing.T) *quotaRig {
	t.Helper()
	r := &quotaRig{t: t, bounds: tenant.QuotaBounds{Bytes: 1000, WarnPercent: 90}}
	id := 0
	w, err := newQuotaWatch(quotaConfig{
		Tenant: "lab",
		Use:    func(context.Context) (tenant.QuotaUse, error) { return r.use, nil },
		Bounds: func(context.Context) (tenant.QuotaBounds, error) { return r.bounds, nil },
		Append: func(_ context.Context, evs []tenant.EventAppend) (int, error) {
			if r.failed != nil {
				return 0, r.failed
			}
			for _, e := range evs {
				p, ok := tenant.DecodeEvent(e.Event.Type(), e.Event.Payload())
				if q, isQ := p.(*tenant.QuotaChanged); ok && isQ {
					r.events = append(r.events, q)
				}
			}
			return len(evs), nil
		},
		Now:   func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) },
		NewID: func() string { id++; return "id-" + strconv.Itoa(id) },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

// check looks once and fails the test on an error.
func (r *quotaRig) check() {
	r.t.Helper()
	if err := r.w.Check(context.Background()); err != nil {
		r.t.Fatalf("a look failed: %v", err)
	}
}

// wantEvents fails unless the events appended so far are, in order, of
// these states.
func (r *quotaRig) wantEvents(when string, states ...tenant.QuotaState) {
	r.t.Helper()
	got := make([]tenant.QuotaState, len(r.events))
	for i, e := range r.events {
		got[i] = e.State
	}
	if strings.Join(stateWords(got), ",") != strings.Join(stateWords(states), ",") {
		r.t.Fatalf("%s: the events are %v, want %v", when, got, states)
	}
}

// stateWords returns the states as words.
func stateWords(states []tenant.QuotaState) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

// TestQuotaWatchWritesOneEventPerCrossing guards that the warning is
// written once per crossing, never once per look or per file: use under
// quota.warn writes nothing; passing it writes one warned event however
// many looks and files follow; reaching the bound writes one reached event;
// falling back under the warning writes one under event; and an event
// that could not be written is written at the next look.
func TestQuotaWatchWritesOneEventPerCrossing(t *testing.T) {
	r := newQuotaRig(t)
	r.use = tenant.QuotaUse{HeldInstances: 5, HeldBytes: 800}
	r.check()
	r.check()
	r.wantEvents("under the warning")
	r.use.HeldBytes = 900
	r.check()
	r.use = r.use.Plus(3, 50)
	r.check()
	r.check()
	r.wantEvents("past the warning", tenant.QuotaWarned)
	if e := r.events[0]; e.UsedBytes != 900 || e.UsedInstances != 5 || e.BytesBound != 1000 || e.InstancesBound != 0 {
		t.Errorf("the warning event is %+v, want the use and the bounds then", e)
	}
	r.use.PurgeBytes = 50
	r.check()
	r.check()
	r.wantEvents("at the bound", tenant.QuotaWarned, tenant.QuotaReached)
	r.failed = errors.New("the feed refuses")
	r.use = tenant.QuotaUse{HeldBytes: 100}
	if err := r.w.Check(context.Background()); err == nil {
		t.Fatal("a look whose event could not be written answered no error")
	}
	r.failed = nil
	r.check()
	r.check()
	r.wantEvents("back under", tenant.QuotaWarned, tenant.QuotaReached, tenant.QuotaUnder)
	r.bounds = tenant.QuotaBounds{WarnPercent: 90}
	r.use.HeldBytes = 1 << 40
	r.check()
	r.wantEvents("with no bound", tenant.QuotaWarned, tenant.QuotaReached, tenant.QuotaUnder)
}

// TestQuotaGauges guards the metrics of each tenant's quota: described
// with no sample before any look, then each tenant's use, bounds and state
// as its watch last measured them.
func TestQuotaGauges(t *testing.T) {
	var ws quotaWatches
	for _, g := range ws.gauges() {
		if !g.Empty {
			t.Fatalf("before any look the gauge %s has a sample", g.Name)
		}
	}
	r := newQuotaRig(t)
	r.bounds = tenant.QuotaBounds{Bytes: 1000, Instances: 10, WarnPercent: 90}
	r.use = tenant.QuotaUse{HeldInstances: 10, HeldBytes: 400}
	r.check()
	ws.set("lab", r.w)
	got := map[string]obs.Gauge{}
	for _, g := range ws.gauges() {
		got[g.Name] = g
	}
	for name, want := range map[string]float64{metricQuotaUsedBytes: 400, metricQuotaBoundBytes: 1000, metricQuotaUsedInstances: 10,
		metricQuotaBoundInstance: 10, metricQuotaState: 2} {
		g, ok := got[name]
		if !ok || g.Empty || g.Value != want || len(g.Labels) != 2 || g.Labels[1] != "lab" {
			t.Errorf("the gauge %s is %+v, want %v for the tenant lab", name, g, want)
		}
	}
}
