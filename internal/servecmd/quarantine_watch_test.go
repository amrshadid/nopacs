package servecmd

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// quarantineRig is a quarantine watch over a held-files figure, two
// settings and a clock the test moves, with the events it appended.
type quarantineRig struct {
	t      *testing.T
	now    time.Time
	aging  tenant.QuarantineAging
	age    time.Duration
	bytes  int64
	failed error
	events []*tenant.QuarantineAgingChanged
	woken  int
	w      *quarantineWatch
}

func newQuarantineRig(t *testing.T) *quarantineRig {
	t.Helper()
	r := &quarantineRig{t: t, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), age: 30 * 24 * time.Hour}
	id := 0
	w, err := newQuarantineWatch(quarantineConfig{
		Tenant:    "lab",
		Aging:     func(context.Context) (tenant.QuarantineAging, error) { return r.aging, nil },
		WarnAge:   func(context.Context) (time.Duration, error) { return r.age, nil },
		WarnBytes: func(context.Context) (int64, error) { return r.bytes, nil },
		Append: func(_ context.Context, evs []tenant.EventAppend) (int, error) {
			if r.failed != nil {
				return 0, r.failed
			}
			for _, e := range evs {
				p, ok := tenant.DecodeEvent(e.Event.Type(), e.Event.Payload())
				if q, isQ := p.(*tenant.QuarantineAgingChanged); ok && isQ {
					r.events = append(r.events, q)
				}
			}
			return len(evs), nil
		},
		Now:   func() time.Time { return r.now },
		NewID: func() string { id++; return "id-" + strconv.Itoa(id) },
		Wake:  func() { r.woken++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

// check looks once and fails the test on an error.
func (r *quarantineRig) check() {
	r.t.Helper()
	if err := r.w.Check(context.Background()); err != nil {
		r.t.Fatalf("a look failed: %v", err)
	}
}

// TestQuarantineWatchWritesOneEventPerCrossing guards that a warning is
// reported once when it first stands and once when no warning stands, not at
// every look: an old file past quarantine.warn-age gives one event however
// many looks follow; bytes past quarantine.warn-bytes then give one more,
// carrying both warnings; the files leaving give one recovery event; and a
// quiet quarantine gives none.
func TestQuarantineWatchWritesOneEventPerCrossing(t *testing.T) {
	r := newQuarantineRig(t)
	r.bytes = 1000
	r.aging = tenant.QuarantineAging{Oldest: r.now.Add(-24 * time.Hour), Files: 1, Bytes: 500}
	r.check()
	r.check()
	if len(r.events) != 0 {
		t.Fatalf("a day-old file under both limits wrote %d events; want none", len(r.events))
	}

	r.now = r.now.Add(40 * 24 * time.Hour)
	r.check()
	r.check()
	r.check()
	if len(r.events) != 1 || !r.events[0].AgePassed || r.events[0].BytesPassed || r.events[0].Recovered ||
		time.Duration(r.events[0].OldestAgeMicros)*time.Microsecond < 41*24*time.Hour || r.events[0].HeldFiles != 1 || r.events[0].HeldBytes != 500 {
		t.Fatalf("three looks past the age wrote %d events, the first %+v; want one with the age warning", len(r.events), r.events)
	}

	r.aging.Bytes = 5000
	r.check()
	r.check()
	if len(r.events) != 2 || !r.events[1].AgePassed || !r.events[1].BytesPassed || r.events[1].Recovered {
		t.Fatalf("bytes past the limit wrote %d events in all, the last %+v; want a second with both warnings", len(r.events), r.events)
	}

	r.aging = tenant.QuarantineAging{}
	r.check()
	r.check()
	if len(r.events) != 3 || !r.events[2].Recovered || r.events[2].AgePassed || r.events[2].BytesPassed || r.events[2].HeldFiles != 0 {
		t.Fatalf("an emptied quarantine wrote %d events in all, the last %+v; want one recovery", len(r.events), r.events)
	}
	if r.woken != 3 {
		t.Errorf("the feed's watchers were woken %d times; want once per event, 3", r.woken)
	}
}

// TestQuarantineWatchZeroBytesIsNoWarning guards that quarantine.warn-bytes
// of 0 means no byte warning however much is held, while the age warning
// still stands.
func TestQuarantineWatchZeroBytesIsNoWarning(t *testing.T) {
	r := newQuarantineRig(t)
	r.bytes = 0
	r.aging = tenant.QuarantineAging{Oldest: r.now.Add(-time.Hour), Files: 9, Bytes: 1 << 40}
	r.check()
	if len(r.events) != 0 {
		t.Fatalf("a terabyte held with no byte limit wrote %+v; want no event", r.events)
	}
	r.aging.Oldest = r.now.Add(-60 * 24 * time.Hour)
	r.check()
	if len(r.events) != 1 || !r.events[0].AgePassed || r.events[0].BytesPassed {
		t.Fatalf("an old file with no byte limit wrote %+v; want the age warning alone", r.events)
	}
}

// TestQuarantineWatchRetriesAnEventItCouldNotWrite guards that a failed
// write leaves the warning unreported, so the next look writes it, once.
func TestQuarantineWatchRetriesAnEventItCouldNotWrite(t *testing.T) {
	r := newQuarantineRig(t)
	r.aging = tenant.QuarantineAging{Oldest: r.now.Add(-60 * 24 * time.Hour), Files: 1, Bytes: 10}
	r.failed = errors.New("the feed is down")
	if err := r.w.Check(context.Background()); err == nil {
		t.Fatal("a look whose event could not be written reported success")
	}
	r.failed = nil
	r.check()
	r.check()
	if len(r.events) != 1 {
		t.Fatalf("after the feed came back %d events were written; want one", len(r.events))
	}
}

// TestQuarantineGauges guards the metrics: before a look both are described
// with no sample; after, each tenant has the oldest file's age in seconds and
// the bytes held, with its name as a label.
func TestQuarantineGauges(t *testing.T) {
	var reg quarantineWatches
	if g := reg.gauges(); len(g) != 2 || !g[0].Empty || !g[1].Empty {
		t.Fatalf("with no watch the gauges are %+v; want two described, empty", g)
	}
	r := newQuarantineRig(t)
	reg.set("lab", r.w)
	if g := reg.gauges(); len(g) != 2 || !g[0].Empty {
		t.Fatalf("before a look the gauges are %+v; want described, empty", g)
	}
	r.aging = tenant.QuarantineAging{Oldest: r.now.Add(-90 * time.Second), Files: 2, Bytes: 4096}
	r.check()
	got := map[string]float64{}
	for _, g := range reg.gauges() {
		if g.Empty || len(g.Labels) != 2 || g.Labels[0] != "tenant" || g.Labels[1] != "lab" || g.Help == "" {
			t.Errorf("the gauge %+v; want a sample labeled with the tenant lab", g)
		}
		got[g.Name] = g.Value
	}
	if got[metricQuarantineOldest] != 90 || got[metricQuarantineBytes] != 4096 {
		t.Errorf("the gauges read %v; want 90 seconds and 4096 bytes", got)
	}
	reg.drop("lab", r.w)
	if g := reg.gauges(); !g[0].Empty {
		t.Errorf("after the tenant's work stopped the gauges are %+v; want none sampled", g)
	}
}
